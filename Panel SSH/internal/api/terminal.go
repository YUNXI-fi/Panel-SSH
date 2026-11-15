package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/crypto/ssh"

	"panelssh/internal/localterm"
)

// 浏览器与后端之间的终端消息协议。
//
// 客户端 -> 服务端：input（键盘输入）、resize（窗口尺寸变化）、ping（心跳）
// 服务端 -> 客户端：output（终端输出）、error（错误）、exit（会话结束）、pong（心跳应答）
//
// ping/pong 不是可有可无的装饰：WebSocket 的 onclose 在网络静默中断时可能
// 几分钟都不触发，前端只能靠"发出去的消息有没有回音"来判断链路是否还活着。
type wsMessage struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Message string `json:"message,omitempty"`
}

const (
	wsWriteTimeout = 10 * time.Second
	// 终端输出的读取缓冲。32KB 与 SSH 通道窗口大小同一量级，避免过度分层。
	outputBufSize = 32 * 1024
)

// wsWriter 串行化单连接上的写操作 —— websocket 不允许并发写。
// 消息类型用 any：终端与批量执行共用这条写路径，但各自的消息结构不同。
type wsWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (w *wsWriter) write(ctx context.Context, msg any) error {
	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()

	w.mu.Lock()
	defer w.mu.Unlock()
	return wsjson.Write(writeCtx, w.conn, msg)
}

// terminalChannel 抹平两种终端来源（本机 PTY / SSH 远端）的差异。
//
// 上层的消息循环只关心"读输出、写输入、改尺寸、等结束、断开时清理"，
// 至于背后是本地 fork 出来的 shell 还是 SSH 通道，与它无关。
type terminalChannel struct {
	outputs   []io.Reader
	write     func([]byte) (int, error)
	resize    func(cols, rows int) error
	wait      func() error
	terminate func(done <-chan error)
	close     func()
}

func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	hostID, err := strconv.ParseInt(r.URL.Query().Get("hostId"), 10, 64)
	if err != nil || hostID < 0 {
		writeErr(w, http.StatusBadRequest, "hostId 参数无效")
		return
	}

	// hostId 0 表示面板所在主机本身，它在主机表里没有记录，因此不做存在性校验；
	// 其余情况先确认主机存在，这样"主机被删了"会以明确的 HTTP 404 返回，
	// 而不是等 WebSocket 建好后再用消息告知。
	if hostID > 0 {
		if _, err := s.store.GetHost(hostID); err != nil {
			writeErr(w, http.StatusNotFound, "主机不存在")
			return
		}
	}

	// 留空 OriginPatterns 使用默认的同源校验。开发态由 Vite 反向代理转发，
	// 浏览器视角始终同源，因此无需放宽 Origin 限制。
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

	ctx := r.Context()
	out := &wsWriter{conn: conn}

	channel, err := s.openTerminal(hostID)
	if err != nil {
		_ = out.write(ctx, wsMessage{Type: "error", Message: err.Error()})
		_ = conn.Close(websocket.StatusInternalError, "terminal failed")
		return
	}
	defer channel.close()

	s.serveTerminal(ctx, conn, out, channel)
}

// openTerminal 按目标挑一条实现：0 是本机 PTY，其余走 SSH。
func (s *Server) openTerminal(hostID int64) (*terminalChannel, error) {
	if hostID == 0 {
		return openLocalTerminal()
	}
	return s.openSSHTerminal(hostID)
}

// openLocalTerminal 在面板所在主机上起一个登录 shell。
func openLocalTerminal() (*terminalChannel, error) {
	session, err := localterm.Start(100, 30)
	if err != nil {
		return nil, fmt.Errorf("本机终端启动失败：%w", err)
	}
	return &terminalChannel{
		outputs:   []io.Reader{session},
		write:     session.Write,
		resize:    session.Resize,
		wait:      session.Wait,
		terminate: func(<-chan error) { session.Terminate() },
		close:     session.Terminate,
	}, nil
}

// openSSHTerminal 在目标机上申请一个 PTY 并起 shell。
func (s *Server) openSSHTerminal(hostID int64) (*terminalChannel, error) {
	// 借用连接池里的连接：同一主机的终端与文件管理共用一条 SSH 连接，
	// 避免每开一个窗口都重新握手。
	lease, err := s.leaseHost(hostID)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接失败：%w", err)
	}

	session, err := lease.Client.NewSession()
	if err != nil {
		lease.Release()
		return nil, fmt.Errorf("创建 SSH 会话失败：%w", err)
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 30, 100, modes); err != nil {
		_ = session.Close()
		lease.Release()
		return nil, fmt.Errorf("申请 PTY 失败：%w", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		lease.Release()
		return nil, fmt.Errorf("获取 stdin 失败：%w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		lease.Release()
		return nil, fmt.Errorf("获取 stdout 失败：%w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		_ = session.Close()
		lease.Release()
		return nil, fmt.Errorf("获取 stderr 失败：%w", err)
	}

	if err := session.Shell(); err != nil {
		_ = session.Close()
		lease.Release()
		return nil, fmt.Errorf("启动 shell 失败：%w", err)
	}

	return &terminalChannel{
		outputs:   []io.Reader{stdout, stderr},
		write:     stdin.Write,
		resize:    func(cols, rows int) error { return session.WindowChange(rows, cols) },
		wait:      session.Wait,
		terminate: func(done <-chan error) { terminateRemote(session, done) },
		close: func() {
			_ = session.Close()
			lease.Release()
		},
	}, nil
}

// serveTerminal 是终端的主循环：输出泵 + 消息处理。
func (s *Server) serveTerminal(
	ctx context.Context,
	conn *websocket.Conn,
	out *wsWriter,
	channel *terminalChannel,
) {
	// 输出泵：把每一路输出汇入 WebSocket。
	pump := func(reader io.Reader) {
		buf := make([]byte, outputBufSize)
		for {
			n, readErr := reader.Read(buf)
			if n > 0 {
				if writeErr := out.write(ctx, wsMessage{Type: "output", Data: string(buf[:n])}); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}
	for _, reader := range channel.outputs {
		go pump(reader)
	}

	// 会话结束时主动通知前端，避免界面停在一个已经死掉的终端上。
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- channel.wait()
		_ = out.write(ctx, wsMessage{Type: "exit", Message: "会话已结束"})
		_ = conn.Close(websocket.StatusNormalClosure, "session closed")
	}()

	// 主循环：消费前端消息。
	for {
		var msg wsMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			// 前端断开（关闭窗口、刷新页面、网络中断）：必须显式终止会话。
			// 只关闭通道不够 —— 远端 sshd 不会因此结束已经起来的登录 shell，
			// 结果是用户关掉窗口后，目标机上仍挂着一个占用 PTY 的会话。
			channel.terminate(waitDone)
			return
		}
		switch msg.Type {
		case "input":
			if _, err := channel.write([]byte(msg.Data)); err != nil {
				return
			}
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				// 尺寸变化失败不影响交互，仅忽略。
				_ = channel.resize(msg.Cols, msg.Rows)
			}
		case "ping":
			// 立刻回一句，前端据此判断这条链路是否还活着。
			// 写失败不必处理：连接已经坏了，主循环下一轮 Read 就会退出。
			_ = out.write(ctx, wsMessage{Type: "pong"})
		}
	}
}
