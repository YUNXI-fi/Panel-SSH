package env

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ErrTimeout 表示目标机在规定时间内没有返回。
var ErrTimeout = errors.New("执行超时")

// exitCodeOf 从进程退出错误中取出退出码。
func exitCodeOf(err error) (int, bool) {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// ExecResult 是一次脚本执行的结果。退出码非 0 不算错误：
// 与批量执行的口径一致，"跑完了但返回非零"是调用方要解读的正常结果，
// 只有启动失败、超时这类"没能跑完"才返回 error。
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// LogSink 接收流式输出。stream 为 "stdout" 或 "stderr"。
type LogSink func(stream string, data []byte)

// Command 是一条待投递到目标机的命令，连同投递它所需的交互要求。
//
// Secret 与 TTY 由 Wrap 按提权路径决定：su 从 /dev/tty 读口令，且要等它把
// 提示打出来；sudo -S 从 stdin 读，而它的提示被清空了（见 Wrap）。
type Command struct {
	// Line 是实际投递的命令行。
	Line string
	// Secret 为真表示执行通道必须把提权口令写进会话标准输入。
	Secret bool
	// TTY 为真表示执行前必须申请伪终端。
	TTY bool
}

// SecretFeeder 由"能拿到提权口令"的执行通道实现。
//
// SSH 通道从主机记录里取口令；本机通道从宿主机的提权账号配置里取。
// 两者都没有口令时返回 ok=false，调用方据此判定"提不了权"。
type SecretFeeder interface {
	// EscalationSecret 返回提权口令、它的来源，以及有没有可用口令。
	//
	// dedicated 为真表示这是主机上单独配置的「提权口令」（root 口令），
	// 为假表示退回的登录凭据。两者的去向不能互换：su 认的是 root 的口令，
	// sudo 认的是执行者自己的口令，也就是登录口令。
	EscalationSecret() (secret string, dedicated bool, ok bool)
}

// Executor 是目标机的命令执行通道。本机与远程 SSH 各有一个实现，
// 编排层只面向这个接口，因此"本机也纳入管理"不需要任何特判分支。
type Executor interface {
	// Run 一次性执行并收集全部输出。
	Run(ctx context.Context, cmd Command, timeout time.Duration) (*ExecResult, error)
	// Stream 在执行同时把输出实时交给 sink（可为 nil）。
	Stream(ctx context.Context, cmd Command, timeout time.Duration, sink LogSink) (*ExecResult, error)
}

// MaxStepOutputBytes 限制单个步骤单侧输出的保留量。
// 安装命令在异常时可能刷屏（apt 的依赖树、curl 进度），不设上限会撑爆内存与数据库。
const MaxStepOutputBytes = 64 * 1024

// cappedWriter 是带上限的写入缓冲：写满后丢弃多余内容并置截断标记。
// 同时把每一块转发给 sink，用于 WebSocket 实时回传（实时流同样截断，
// 避免一条失控命令把浏览器打挂）。
type cappedWriter struct {
	buf       bytes.Buffer
	limit     int
	sent      int
	stream    string
	sink      LogSink
	truncated bool
}

func newCappedWriter(stream string, sink LogSink) *cappedWriter {
	return &cappedWriter{stream: stream, sink: sink, limit: MaxStepOutputBytes}
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.sink != nil && len(p) > 0 && w.sent < w.limit {
		chunk := p
		if w.sent+len(chunk) > w.limit {
			chunk = chunk[:w.limit-w.sent]
		}
		w.sink(w.stream, chunk)
		w.sent += len(chunk)
	}
	if w.sent >= w.limit {
		w.truncated = true
		// 始终宣称已消费：否则上游管道写阻塞会让远端命令挂住。
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		w.buf.Write(p[:remaining])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// Wrap 把一段 POSIX sh 脚本包装成实际投递到目标机的命令。
//
// 用 base64 传输而非引号转义：脚本里既有单引号又有双引号（apt/dnf 源配置），
// 任何转义方案都要和外层 SSH、提权命令两层引号搏斗；base64 字符集天然安全。
//
// 解码后直接管道给 sh，避免脚本落临时文件（不留垃圾、不挑 /tmp 权限）。
// 提权路径有三种：
//
//   - 已是 root：直接执行；
//   - su：su 的 -c 直接吃脚本文本，口令由伪终端投喂（见 Command.TTY）；
//   - sudo：sudo -S 要独占 stdin 读口令，脚本因此只能走 sh -c 传入；
//     免密 sudo 不需要口令，照旧管道给 sh -n 就行。
func Wrap(script string, profile *Profile) Command {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	pipe := "echo " + encoded + " | base64 -d | "
	if profile == nil {
		return Command{Line: pipe + "sh"}
	}
	switch profile.PrivilegeMode() {
	case PrivSu:
		// LC_ALL=C 把 su 自己的提示固定成英文，执行器据此认出"该答口令了"。
		return Command{Line: "LC_ALL=C su -c '" + pipe + "sh'", Secret: true, TTY: true}
	case PrivSudoNoPass:
		// -n 明确要求非交互：万一免密其实不可用（画像过期、sudoers 被改回），
		// 它会立刻报错，而不是停在那里等一个永远不会来的口令。
		return Command{Line: pipe + "LC_ALL=C sudo -n sh"}
	case PrivSudo:
		// -S 让它从 stdin 读口令；-p '' 清掉它自己的提示 —— 那句提示不带
		// 换行，会和随后的报错挤在同一行，被当成目标机说的话展示给用户。
		return Command{Line: "LC_ALL=C sudo -S -p '' sh -c '" + pipe + "sh'", Secret: true}
	default:
		// 已是 root，或根本没有可用路径（调用方自行判断该不该往下跑）。
		return Command{Line: pipe + "sh"}
	}
}

// LocalExecutor 通过本机进程执行命令。
type LocalExecutor struct {
	// account / secret 是宿主机的提权账号与口令。
	// secret 为空表示没有配置提权，markEscalation 会据此判定"提不了权"。
	account string
	secret  string
}

// NewLocalExecutor 返回本机执行器。
func NewLocalExecutor() *LocalExecutor { return &LocalExecutor{} }

// 编译期确认本机执行器也能提供提权口令：宿主机的 su/sudo 两条路径靠它成立。
var _ SecretFeeder = (*LocalExecutor)(nil)

// NewLocalExecutorWithSecret 返回带提权口令的本机执行器。
func NewLocalExecutorWithSecret(account, secret string) *LocalExecutor {
	return &LocalExecutor{account: account, secret: secret}
}

// EscalationSecret 实现 SecretFeeder。
//
// 账号是 root 时 dedicated 为真：su 认的是 root 自己的口令。其余情况按
// "面板运行用户自己的口令"处理，走 sudo -S —— 与远端主机的取舍一致。
func (e *LocalExecutor) EscalationSecret() (string, bool, bool) {
	if e.secret == "" {
		return "", false, false
	}
	return e.secret, e.account == "root", true
}

// secretFeedDelay 是 su 路径下投喂口令前的等待时间。
//
// su 从 /dev/tty 读口令，而 pty 由 script 建立、回显由前置的 stty -echo 关闭。
// 立刻写入口令会赶在这两步之前，既有被回显进输出的风险，也可能被行规程丢弃。
const secretFeedDelay = 400 * time.Millisecond

// localCommandEnv 返回本机系统命令的环境变量，剔除动态链接器注入变量。
//
// LD_LIBRARY_PATH / LD_PRELOAD 只对"启动面板的那个终端/IDE"有意义。
// 把它们传给 apt-get、dpkg 这类系统程序，会让后者加载到非系统的 libstdc++，
// 表现为 GLIBCXX 版本缺失 —— 错误看起来像系统坏了，实际是环境污染。
func localCommandEnv() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, item := range env {
		if strings.HasPrefix(item, "LD_LIBRARY_PATH=") || strings.HasPrefix(item, "LD_PRELOAD=") {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func (e *LocalExecutor) Run(ctx context.Context, cmd Command, timeout time.Duration) (*ExecResult, error) {
	return e.Stream(ctx, cmd, timeout, nil)
}

func (e *LocalExecutor) Stream(ctx context.Context, cmd Command, timeout time.Duration, sink LogSink) (*ExecResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stdinSecret string
	if cmd.Secret {
		if e.secret == "" {
			return nil, errors.New("提权口令不可用：宿主机未配置提权账号")
		}
		stdinSecret = e.secret + "\n"
	}

	program, args := "sh", []string{"-c", cmd.Line}
	if cmd.Secret && cmd.TTY {
		// su 没有终端会直接拒绝执行。script 给它一个 pty，并在同一条命令里
		// 先关回显 —— 口令不能出现在执行输出里。
		program, args = "script", []string{"-q", "-e", "-c", "stty -echo; " + cmd.Line, "/dev/null"}
	}

	// 独立进程组：超时时杀整个组，curl | gpg 这类管道里的孙进程才会一起结束，
	// 否则只杀 sh 会留下孤儿继续写文件。
	proc := exec.CommandContext(cctx, program, args...)
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// 不让启动面板的那个终端把动态链接器变量带进系统命令。
	// 典型故障：面板从 IDE 启动，继承到 LD_LIBRARY_PATH，apt-get 因此加载
	// 了非系统的 libstdc++，报 GLIBCXX_x.y.z not found。
	proc.Env = localCommandEnv()
	proc.Cancel = func() error {
		if proc.Process != nil {
			_ = syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
		}
		// 返回 ErrProcessDone：告诉标准库进程已被我们清掉，无需再 Wait 报错。
		return os.ErrProcessDone
	}

	if stdinSecret != "" {
		stdin, err := proc.StdinPipe()
		if err != nil {
			return nil, err
		}
		go func() {
			if cmd.TTY {
				time.Sleep(secretFeedDelay)
			}
			_, _ = io.WriteString(stdin, stdinSecret)
			_ = stdin.Close()
		}()
	}

	stdout := newCappedWriter("stdout", sink)
	stderr := newCappedWriter("stderr", sink)
	proc.Stdout = stdout
	proc.Stderr = stderr

	runErr := proc.Run()

	result := &ExecResult{
		Stdout: stdout.buf.String(),
		Stderr: stderr.buf.String(),
	}
	if stdout.truncated {
		result.Stdout += "\n[输出超过上限，已截断]\n"
	}
	if stderr.truncated {
		result.Stderr += "\n[输出超过上限，已截断]\n"
	}

	if runErr != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return result, ErrTimeout
		}
		if code, ok := exitCodeOf(runErr); ok {
			result.ExitCode = code
			return result, nil
		}
		return result, runErr
	}
	return result, nil
}

// TailLines 返回至多最后 n 行非空文本，用于把长输出压缩成错误摘要。
func TailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			out = append([]string{line}, out...)
		}
	}
	return strings.Join(out, "\n")
}
