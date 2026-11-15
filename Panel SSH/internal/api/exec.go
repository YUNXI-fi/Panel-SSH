package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// defaultExecTimeout 是单机同步执行的默认上限。
	defaultExecTimeout = 60 * time.Second
	maxExecTimeout     = 10 * time.Minute
	// maxExecOutputBytes 限制单侧输出体积。命令可能无限输出（例如 yes），
	// 必须先设上限再执行，否则内存会被写爆。
	maxExecOutputBytes = 256 * 1024
)

// execRequest 是单机同步执行的入参。
type execRequest struct {
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec"`
}

// execResult 是一次执行的结果。退出码单独给出，不与非零退出混为"接口失败"。
type execResult struct {
	Stdout        string `json:"stdout"`
	Stderr        string `json:"stderr"`
	ExitCode      int    `json:"exitCode"`
	DurationMS    int64  `json:"durationMs"`
	StdoutClipped bool   `json:"stdoutClipped"`
	StderrClipped bool   `json:"stderrClipped"`
}

// limitedBuffer 是带写入上限的缓冲：超出部分丢弃并打标记。
//
// Write 始终返回 len(p)，表示"已消费"，这样远端命令不会因为短写而报错中断。
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *limitedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func clampExecTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultExecTimeout
	}
	timeout := time.Duration(seconds) * time.Second
	if timeout > maxExecTimeout {
		return maxExecTimeout
	}
	return timeout
}

// lineWriter 把写入的字节按行拆开交给 onLine，同时原样留存给调用方事后取用。
//
// 为什么不直接在调用方数换行符：两个流（stdout / stderr）的回调分别来自
// 不同的底层拷贝协程，先后顺序不确定，而回调要往上写同一份 HTTP 响应 ——
// 并发写是错的。因此这里用一把共享锁把 onLine 串起来，调用方不必自己加锁。
type lineWriter struct {
	mu      *sync.Mutex
	buf     *limitedBuffer
	pending []byte
	onLine  func(line string)
}

// maxPendingLine 是一行的长度上限。
//
// 正常输出（tar -v 的文件名、zip 的 adding: 行）都远小于它；设这个上限是为了
// 防住"一直没有换行"的输出：那种情况下 pending 会无界增长，而它并不受
// limitedBuffer 的 256 KiB 约束（那是给事后取用的正文用的）。
const maxPendingLine = 8 * 1024

func newLineWriter(shared *sync.Mutex, buf *limitedBuffer, onLine func(string)) *lineWriter {
	return &lineWriter{mu: shared, buf: buf, onLine: onLine}
}

// isSessionNoise 判断一行是不是 SSH 会话自身的提示，而不是命令的输出。
//
// sshd 在起 shell 之前要切到登录用户的家目录，那个目录不存在时（账号建的时候没带
// 家目录、家目录被删、或者它所在的盘没挂载）就往会话里打一行
// "Could not chdir to home directory /home/admin: No such file or directory"。
// 它排在命令输出的最前面，而面板的流式消费方按"一行一个条目"解析 —— 不滤掉的话，
// 文件清单里会多出一个名字是这句话的"文件"，压缩/解压的进度也会多算一条。
func isSessionNoise(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "Could not chdir to home directory ")
}

func (w *lineWriter) Write(p []byte) (int, error) {
	// 正文照旧留存：调用方还要用它判断退出原因（例如"是否权限不足"）。
	w.buf.Write(p)
	if w.onLine == nil {
		return len(p), nil
	}

	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 && len(w.pending) < maxPendingLine {
			break
		}

		var line []byte
		if i < 0 {
			// 一直等不到换行又已经超长：把现有内容整段当作一行发出，避免无限累积。
			line, w.pending = w.pending, nil
		} else {
			line = w.pending[:i]
			w.pending = append([]byte(nil), w.pending[i+1:]...)
		}

		w.mu.Lock()
		w.onLine(string(line))
		w.mu.Unlock()
	}
	return len(p), nil
}

// execLocal 在宿主机上执行一次命令。
//
// 与远端分支的差别只有"怎么保证命令真的结束"这一件事：远端的命令挂在 sshd 下，
// 关掉会话就带走了它（terminateRemote 还会补刀）；本地没有这一层可依赖，
// 只能让 sh 独立成一个进程组，超时时杀整组 —— 否则 sh 被杀之后，管道里的
// 孙进程（curl、gpg、tar）会活下来继续写盘，而用户已经看到"执行失败"。
func execLocal(ctx context.Context, command string, timeout time.Duration) (*execResult, error) {
	return execLocalStream(ctx, command, timeout, nil)
}

// execLocalStream 是 execLocal 的流式版本：多出来的只有 onLine。
//
// onLine 为 nil 时两者完全等价 —— 需要过程可见的调用方（压缩、解压）传回调，
// 其余的传 nil，不必各自维护一份"怎么跑本地命令"的实现。
func execLocalStream(ctx context.Context, command string, timeout time.Duration, onLine func(line string)) (*execResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		// 告诉标准库进程已由我们清掉，后续收尾不必再报错。
		return os.ErrProcessDone
	}

	stdout := newLimitedBuffer(maxExecOutputBytes)
	stderr := newLimitedBuffer(maxExecOutputBytes)
	var streamMu sync.Mutex
	cmd.Stdout = newLineWriter(&streamMu, stdout, onLine)
	cmd.Stderr = newLineWriter(&streamMu, stderr, onLine)

	started := time.Now()
	runErr := cmd.Run()

	result := &execResult{
		Stdout:        stdout.String(),
		Stderr:        stderr.String(),
		DurationMS:    time.Since(started).Milliseconds(),
		StdoutClipped: stdout.Truncated(),
		StderrClipped: stderr.Truncated(),
	}

	if runErr != nil {
		// 超时与"没跑起来"必须分开：前者是上游没响应（504），后者是启动失败。
		if cctx.Err() != nil {
			if errors.Is(cctx.Err(), context.DeadlineExceeded) {
				return nil, errExecTimeout
			}
			return nil, cctx.Err()
		}
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return nil, fmt.Errorf("启动命令失败：%w", runErr)
		}
		// 退出码非 0 不算失败 —— 调用方要用它区分"跑完了但命令自己报错"。
		result.ExitCode = exitErr.ExitCode()
		if result.ExitCode < 0 {
			// 被信号打断时 ExitCode 是 -1。按 shell 惯例折成 128+signal，
			// 避免把一个负数抛给界面（远端分支经 sshd 拿到的正是这个口径）。
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				result.ExitCode = 128 + int(ws.Signal())
			}
		}
	}
	return result, nil
}

// execOnHost 在目标主机上同步执行命令并返回结果。
//
// 这里是唯一一份"单机同步执行"的实现，HTTP 接口与 AI 工具层共用，
// 避免两处各写一遍 SSH 会话与超时处理。
func (s *Server) execOnHost(ctx context.Context, hostID int64, command string, timeout time.Duration) (*execResult, error) {
	return s.execOnHostStream(ctx, hostID, command, timeout, nil)
}

// execOnHostStream 是 execOnHost 的流式版本：在执行过程中把输出的每一行交给 onLine。
//
// 为什么需要它：压缩、解压可能要跑上几分钟，而 execOnHost 要等命令跑完才返回，
// 面板在这期间对用户是完全沉默的。要让过程可见，只能边跑边把输出交出去。
//
// 除此之外两者的口径完全一致（退出码、输出正文、错误分类），因为判断"要不要提权重试"
// 靠的正是 stderr 与退出码 —— 一旦分叉，同一个操作在两条路径上会得出不同结论。
//
// onLine 由本函数串行调用，传入 nil 表示只要最终结果。
func (s *Server) execOnHostStream(ctx context.Context, hostID int64, command string, timeout time.Duration, onLine func(line string)) (*execResult, error) {
	// 宿主机没有 SSH 会话可借，直接落到本地进程。
	if hostID == LocalHostID {
		return execLocalStream(ctx, command, timeout, onLine)
	}

	lease, err := s.leaseHost(hostID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	session, err := lease.Client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("创建 SSH 会话失败：%w", err)
	}
	defer session.Close()

	stdout := newLimitedBuffer(maxExecOutputBytes)
	stderr := newLimitedBuffer(maxExecOutputBytes)
	var streamMu sync.Mutex
	// 会话噪音在这一层滤掉：下游的流式消费方（文件清单、压缩/解压的逐条进度）按
	// "一行等于命令的一行输出"解析，混进一句 sshd 的提示就会变成一个不存在的文件。
	emit := func(line string) {
		if onLine == nil || isSessionNoise(line) {
			return
		}
		onLine(line)
	}
	session.Stdout = newLineWriter(&streamMu, stdout, emit)
	session.Stderr = newLineWriter(&streamMu, stderr, emit)

	started := time.Now()
	if err := session.Start(command); err != nil {
		return nil, fmt.Errorf("启动命令失败：%w", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(timeout):
		terminateRemote(session, done)
		runErr = errBatchTimeout
	case <-ctx.Done():
		// 这里同样必须显式终止：只关闭会话不会让真实 sshd 停掉远端命令。
		terminateRemote(session, done)
		runErr = ctx.Err()
	}

	exitCode := 0
	if runErr != nil {
		code, isExit := exitStatus(runErr)
		if !isExit {
			// 只有"没能执行完"才算失败；退出码非 0 是正常结果。
			return nil, fmt.Errorf("执行失败：%w", runErr)
		}
		exitCode = code
	}

	return &execResult{
		Stdout:        stdout.String(),
		Stderr:        stderr.String(),
		ExitCode:      exitCode,
		DurationMS:    time.Since(started).Milliseconds(),
		StdoutClipped: stdout.Truncated(),
		StderrClipped: stderr.Truncated(),
	}, nil
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	var req execRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}

	command := strings.TrimSpace(req.Command)
	if command == "" {
		writeErr(w, http.StatusBadRequest, "命令不能为空")
		return
	}

	result, err := s.execOnHost(r.Context(), hostID, command, clampExecTimeout(req.TimeoutSec))
	if err != nil {
		s.writeHostError(w, err, "执行命令失败")
		return
	}
	writeJSON(w, http.StatusOK, result)
}
