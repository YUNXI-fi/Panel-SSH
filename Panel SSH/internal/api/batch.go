package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/crypto/ssh"

	"panelssh/internal/store"
)

const (
	// maxBatchHosts 限制单次批量操作的主机数，避免一个误操作把整批机器扫一遍。
	maxBatchHosts = 200

	// defaultBatchTimeout / maxBatchTimeout 是单机命令的执行上限。
	defaultBatchTimeout = 60 * time.Second
	maxBatchTimeout     = 30 * time.Minute

	// batchConcurrency 是同时在飞的主机数。不设上限的话，一次性向几百台机器
	// 发起 SSH 握手会同时压垮面板与目标机；16 是吞吐与压力的折中。
	batchConcurrency = 16

	// maxBatchOutputBytes 限制单机回传的输出总量，防止 `yes` 这类命令撑爆内存与前端。
	maxBatchOutputBytes = 256 * 1024
	batchOutputChunk    = 8 * 1024
)

// errBatchTimeout 表示单机命令超时。
var errBatchTimeout = errors.New("执行超时")

// batchRequest 是客户端发来的启动指令。
//
// 命令放在消息体而不是 URL 查询串里：既避开 URL 长度与转义问题，
// 也避免把"执行命令"这种状态变更操作塞进 GET 的 query。
type batchRequest struct {
	Type       string  `json:"type"`
	HostIDs    []int64 `json:"hostIds"`
	Command    string  `json:"command"`
	TimeoutSec int     `json:"timeoutSec"`
}

// batchMessage 是服务端流式回传的单条消息。
//
// begin -> 每个主机 hostStart/output*/hostEnd -> done
type batchMessage struct {
	Type     string `json:"type"`
	HostID   int64  `json:"hostId,omitempty"`
	HostName string `json:"hostName,omitempty"`
	Stream   string `json:"stream,omitempty"`
	Data     string `json:"data,omitempty"`
	// ExitCode 刻意不加 omitempty：0 是"命令成功"这一有意义的值，
	// 省略后会把成功与"没有退出码"混为一谈，前端只能把它当成失败。
	ExitCode   int    `json:"exitCode"`
	Error      string `json:"error,omitempty"`
	Note       string `json:"note,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty"`
	Message    string `json:"message,omitempty"`
	Total      int    `json:"total,omitempty"`
}

type batchTarget struct {
	id   int64
	name string
}

// taskTally 统计本次任务各状态的主机数，供收尾时生成通知。
//
// 用原子计数而不是收尾时回查数据库：每台主机在自己的 goroutine 里结束，
// 计数必须并发安全；而这些数字在内存里本来就是现成的，多查一次库没有必要。
type taskTally struct {
	success atomic.Int64
	failed  atomic.Int64
	errored atomic.Int64
}

// batchWriter 串行写出消息，并在首次写失败时取消整批任务。
//
// 旧实现把所有写错误都用 `_ =` 丢掉：客户端断开后，每个主机的任务仍会
// 各自跑到自己的超时（默认 60 秒，最大 30 分钟），期间照常在目标机上执行命令 ——
// 用户已经走了，机器还在被扫。写失败即说明连接已废，没有继续下去的理由。
type batchWriter struct {
	ws     *wsWriter
	ctx    context.Context
	cancel context.CancelFunc
	failed atomic.Bool
}

func (b *batchWriter) write(msg batchMessage) {
	if b.failed.Load() {
		return
	}
	if err := b.ws.write(b.ctx, msg); err != nil {
		b.failed.Store(true)
		b.cancel()
	}
}

func (b *batchWriter) broken() bool {
	return b.failed.Load()
}

func (s *Server) handleBatchExec(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept 失败时已写好 HTTP 响应
	}
	conn.SetReadLimit(1 << 20)
	defer conn.CloseNow()

	// 登记到活跃连接表：面板关闭时要主动断开它，而不是硬切。
	if !s.registerSocket(conn) {
		_ = conn.Close(websocket.StatusGoingAway, "服务正在关闭")
		return
	}
	defer s.unregisterSocket(conn)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var req batchRequest
	if err := wsjson.Read(ctx, conn, &req); err != nil {
		return
	}

	// WebSocket 是从 HTTP 连接 Hijack 出来的，net/http 之后不再监控它，
	// 因此 r.Context() 不会因客户端断开而取消。CloseRead 会在对端关闭连接时
	// 取消返回的 ctx，用它来中止在途任务（注意它接管读取，必须在读完 start 之后调用）。
	runCtx, cancelRun := context.WithCancel(conn.CloseRead(ctx))
	defer cancelRun()

	out := &batchWriter{ws: &wsWriter{conn: conn}, ctx: runCtx, cancel: cancelRun}

	if req.Type != "start" {
		out.write(batchMessage{Type: "error", Message: "首条消息必须是 start"})
		return
	}

	command := strings.TrimSpace(req.Command)
	switch {
	case command == "":
		out.write(batchMessage{Type: "error", Message: "命令不能为空"})
		return
	case len(req.HostIDs) == 0:
		out.write(batchMessage{Type: "error", Message: "请至少选择一台主机"})
		return
	case len(req.HostIDs) > maxBatchHosts:
		out.write(batchMessage{
			Type:    "error",
			Message: fmt.Sprintf("单次最多 %d 台主机，当前选择了 %d 台", maxBatchHosts, len(req.HostIDs)),
		})
		return
	}

	timeout := defaultBatchTimeout
	if req.TimeoutSec > 0 {
		timeout = time.Duration(req.TimeoutSec) * time.Second
		if timeout > maxBatchTimeout {
			timeout = maxBatchTimeout
		}
	}
	timeoutMS := timeout.Milliseconds()

	// 解析目标主机：去重，并把已删除的主机作为独立失败项回传，
	// 而不是让整批请求直接失败。
	targets := make([]batchTarget, 0, len(req.HostIDs))
	missing := make([]int64, 0)
	seen := make(map[int64]bool, len(req.HostIDs))

	for _, id := range req.HostIDs {
		if seen[id] {
			continue
		}
		seen[id] = true

		host, err := s.store.GetHost(id)
		if err != nil {
			missing = append(missing, id)
			continue
		}
		targets = append(targets, batchTarget{id: id, name: host.Name})
	}

	total := len(targets) + len(missing)

	// 先登记任务再下发：反过来的话，"执行完"与"记录建好"之间存在一个窗口，
	// 面板恰在那时重启，这次执行就彻底没有痕迹了。
	taskID, err := s.store.CreateTask(command, int(timeout/time.Second), total)
	if err != nil {
		// 记录失败不阻断执行：用户要的是命令跑出去，历史记录是辅助。
		// 置 0 让后续落库整体跳过，而不是每台主机都报一次同样的错。
		log.Printf("[warn] 创建批量任务记录失败: %v", err)
		taskID = 0
	}

	out.write(batchMessage{
		Type:       "begin",
		Total:      total,
		DurationMS: timeoutMS, // 复用该字段传递本次超时设置，前端据此提示
	})

	tally := &taskTally{}

	for _, id := range missing {
		msg := batchMessage{
			Type:   "hostEnd",
			HostID: id,
			Error:  "主机不存在或已被删除",
		}
		out.write(msg)
		s.recordTaskResult(taskID, hostEndResult(msg), tally)
	}

	sem := make(chan struct{}, batchConcurrency)
	var wg sync.WaitGroup

	for _, target := range targets {
		wg.Add(1)
		go func(t batchTarget) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				// 连接已断或整批被取消：不再占用并发额度，直接收手。
				return
			}
			if out.broken() {
				return
			}

			out.write(batchMessage{Type: "hostStart", HostID: t.id, HostName: t.name})

			started := time.Now()
			exitCode, note, runErr := s.runBatchCommand(runCtx, t, command, timeout, out)
			elapsed := time.Since(started)

			msg := batchMessage{
				Type:       "hostEnd",
				HostID:     t.id,
				HostName:   t.name,
				ExitCode:   exitCode,
				Note:       note,
				DurationMS: elapsed.Milliseconds(),
			}
			// 退出码非 0 属于正常结果，不是错误；只有无法执行完才算错误。
			if runErr != nil {
				if _, isExit := exitStatus(runErr); !isExit {
					msg.Error = runErr.Error()
				}
			}
			out.write(msg)
			s.recordTaskResult(taskID, hostEndResult(msg), tally)
		}(target)
	}

	wg.Wait()
	out.write(batchMessage{Type: "done"})

	// 客户端中途断开时，还没轮到的主机不会有 hostEnd，结果数会少于 total。
	// 这类任务记成 interrupted 而不是 done，历史里才不会把"跑了一半"显示成"已完成"。
	status := store.TaskDone
	if runCtx.Err() != nil || out.broken() {
		status = store.TaskInterrupted
	}
	if taskID != 0 {
		if err := s.store.FinishTask(taskID, status); err != nil {
			log.Printf("[warn] 收尾批量任务 %d 失败: %v", taskID, err)
		}
		// 只有正常跑完的任务才发通知。被中途打断的任务里，未执行的主机
		// 既不是成功也不是失败，报成"完成"会误导；而取消是用户自己的动作，
		// 他已经知道了，不必再推一条通知回来。
		if status == store.TaskDone {
			s.notifyTaskFinished(
				taskID, command, total,
				int(tally.success.Load()), int(tally.failed.Load()), int(tally.errored.Load()),
			)
		}
	}
}

// runBatchCommand 在单台主机上执行命令，并把输出实时流回客户端。
func (s *Server) runBatchCommand(
	ctx context.Context,
	target batchTarget,
	command string,
	timeout time.Duration,
	out *batchWriter,
) (exitCode int, note string, runErr error) {
	lease, err := s.leaseHost(target.id)
	if err != nil {
		return -1, "", err
	}
	defer lease.Release()

	session, err := lease.Client.NewSession()
	if err != nil {
		return -1, "", err
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return -1, "", err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return -1, "", err
	}
	if err := session.Start(command); err != nil {
		return -1, "", err
	}

	// 输出用管道实时回流。若等命令结束再一次性取结果，
	// 一条耗时命令会让前端在整个执行期间都是空白。
	var (
		pumpWG    sync.WaitGroup
		mu        sync.Mutex
		sentBytes int
		truncated bool
		overflow  bool
	)

	// 输出触顶后主动结束这条命令。
	//
	// 旧实现只是停止读取，远端进程仍在写：SSH 通道窗口很快被填满，进程阻塞在
	// write 上，session.Wait() 于是只能等到超时才返回 —— 用户看到的是
	// "输出被截断了，但这条命令还占着 60 秒"。
	//
	// 这里只负责"通知主流程去终止"，不在 pump 里自己等待会话结束：
	// waitDone 只能有一个消费者，否则会和主 select 抢同一个结果。
	stopRequest := make(chan struct{}, 2)
	requestStop := func() {
		mu.Lock()
		already := overflow
		overflow = true
		mu.Unlock()
		if already {
			return
		}
		select {
		case stopRequest <- struct{}{}:
		default:
		}
	}

	pump := func(reader io.Reader, stream string) {
		defer pumpWG.Done()

		buf := make([]byte, batchOutputChunk)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				chunk, hitLimit := reserveOutput(&mu, &sentBytes, &truncated, buf[:n])
				if len(chunk) > 0 {
					out.write(batchMessage{
						Type:   "output",
						HostID: target.id,
						Stream: stream,
						Data:   string(chunk),
					})
				}
				if hitLimit {
					requestStop()
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}

	pumpWG.Add(2)
	go pump(stdout, "stdout")
	go pump(stderr, "stderr")

	waitDone := make(chan error, 1)
	go func() { waitDone <- session.Wait() }()

	select {
	case err := <-waitDone:
		runErr = err
	case <-time.After(timeout):
		terminateRemote(session, waitDone)
		runErr = errBatchTimeout
	case <-ctx.Done():
		// 客户端断开（或整批被取消）：必须显式终止远端命令。
		// 只关闭会话时，真实 sshd 会让命令照常跑完 —— 用户已经走了，机器还在被扫。
		terminateRemote(session, waitDone)
		runErr = ctx.Err()
	case <-stopRequest:
		// 输出触顶：同样走统一的终止流程。
		terminateRemote(session, waitDone)
	}

	pumpWG.Wait()

	mu.Lock()
	wasOverflow := overflow
	mu.Unlock()

	switch {
	case wasOverflow:
		// 是我们主动掐掉的，不是远端执行失败，因此以"已完成但被截断"回报。
		return -1, fmt.Sprintf("输出超过 %d KiB，已终止该主机的命令", maxBatchOutputBytes/1024), nil
	case truncated:
		note = fmt.Sprintf("输出超过 %d KiB，已截断", maxBatchOutputBytes/1024)
	}

	switch {
	case runErr == nil:
		return 0, note, nil
	default:
		// 命令跑完了但退出码非 0，属于正常结果而非执行失败。
		if code, ok := exitStatus(runErr); ok {
			return code, note, nil
		}
		return -1, note, runErr
	}
}

// reserveOutput 在输出总量上限内分配本次可发送的字节数。
func reserveOutput(mu *sync.Mutex, sent *int, truncated *bool, chunk []byte) ([]byte, bool) {
	mu.Lock()
	defer mu.Unlock()

	remaining := maxBatchOutputBytes - *sent
	if remaining <= 0 {
		*truncated = true
		return nil, true
	}
	if len(chunk) > remaining {
		chunk = chunk[:remaining]
		*truncated = true
	}
	*sent += len(chunk)
	return chunk, *truncated
}

// exitStatus 从错误中提取远端进程的退出码。
func exitStatus(err error) (int, bool) {
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return int(exitErr.ExitStatus()), true
	}
	return 0, false
}

// hostEndResult 把一条 hostEnd 消息翻译成落库用的记录。
//
// 判定口径与前端 BatchWindow 完全一致：有 error 即"没跑起来"，
// 否则退出码为 0 算成功、非 0 算命令跑完了但没跑对。
// 两者必须一致，否则同一个任务在实时窗口和历史记录里会显示成两种结果。
func hostEndResult(msg batchMessage) store.TaskResult {
	status := store.ResultFailed
	switch {
	case msg.Error != "":
		status = store.ResultError
	case msg.ExitCode == 0:
		status = store.ResultSuccess
	}
	return store.TaskResult{
		HostID:     msg.HostID,
		HostName:   msg.HostName,
		Status:     status,
		ExitCode:   msg.ExitCode,
		Error:      msg.Error,
		Note:       msg.Note,
		DurationMS: msg.DurationMS,
	}
}

// recordTaskResult 落库单台主机的执行结果，并累计本次任务的结果计数。
//
// taskID 为 0 表示任务登记本身就失败了，此时跳过落库。
// 写库失败只记日志、不向客户端报错也不中断整批：历史记录是辅助功能，
// 不该让正在跑的批量执行跟着失败。
func (s *Server) recordTaskResult(taskID int64, result store.TaskResult, tally *taskTally) {
	// 计数先于落库，且不受落库成败影响：它反映的是"实际执行结果"，
	// 而写库失败只说明"这条没存下来"。若把两者绑在一起，
	// 收尾通知里的"几台失败"会与真实执行结果对不上。
	switch result.Status {
	case store.ResultSuccess:
		tally.success.Add(1)
	case store.ResultFailed:
		tally.failed.Add(1)
	case store.ResultError:
		tally.errored.Add(1)
	}

	if taskID == 0 {
		return
	}
	if err := s.store.AddTaskResult(taskID, result); err != nil {
		log.Printf("[warn] 写入任务 %d 的主机结果失败: %v", taskID, err)
	}
}
