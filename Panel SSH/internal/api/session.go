package api

import (
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// 远端命令的终止策略。
//
// 为什么不能只靠"关闭 SSH 会话"：会话通道关闭只是让对端不再有数据通道，
// 真实 OpenSSH 并不会因此杀掉已经在跑的子进程。这一点在真实 sshd 上验证过 ——
// 只关闭会话时，`sleep 8 && touch 标记文件` 会照常跑完并创建标记文件。
// 而先发 SIGTERM 再关闭会话时，命令会被真正终止。
//
// 因此凡是"要中止远端命令"的地方，都必须显式发信号，并且必须复用同一份实现，
// 否则很容易出现某个分支漏发信号（这正是一次真实回归的成因）。
const (
	// remoteTermGrace 是收到 SIGTERM 后等待远端自行收尾的时长。
	remoteTermGrace = 2 * time.Second
	// remoteKillGrace 是发出 SIGKILL 后等待会话收尾的时长。
	remoteKillGrace = 2 * time.Second
)

// terminateRemote 终止远端命令并等待本次会话收尾。
//
// done 必须是调用方已经启动的 session.Wait() 的结果通道。
// 顺序：SIGTERM（给远端清理机会）→ SIGKILL（对无响应者强制）→ 关闭会话（兜底）。
// 每一步之间都用 done 判断是否已结束，正常响应时整个流程只需一次往返。
func terminateRemote(session *ssh.Session, done <-chan error) {
	// 会话已经自然结束（例如用户在终端里执行了 exit）时不该再补一发信号：
	// 那条命令已经跑完了，再发 SIGTERM 只是无谓的往返。
	if alreadyDone(done) {
		return
	}

	_ = session.Signal(ssh.SIGTERM)
	if waitDone(done, remoteTermGrace) {
		return
	}

	_ = session.Signal(ssh.SIGKILL)
	if waitDone(done, remoteKillGrace) {
		return
	}

	// 远端连信号都不响应（例如已失去网络）：关闭会话强行结束本次等待。
	_ = session.Close()
	<-done
}

// alreadyDone 非阻塞地判断会话是否已经结束。
func alreadyDone(done <-chan error) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// waitDone 在时限内等待会话结束，返回是否已结束。
func waitDone(done <-chan error, timeout time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// wsRegistry 记录活跃的 WebSocket 连接（终端、批量执行、AI 助手）。
//
// 为什么需要它：这些连接都是从 HTTP 连接 Hijack 出来的，http.Server.Shutdown
// 既不等待也不关闭它们。没有这个注册表，重启面板时前端只能看到一个被硬切断的
// 连接（一直转圈），而不是"连接已断开"。
type wsRegistry struct {
	mu      sync.Mutex
	conns   map[*websocket.Conn]struct{}
	closing bool
}

func newWSRegistry() *wsRegistry {
	return &wsRegistry{conns: make(map[*websocket.Conn]struct{})}
}

// add 登记一条连接。返回 false 表示服务正在关闭，调用方应立即拒绝该连接。
func (r *wsRegistry) add(conn *websocket.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.conns[conn] = struct{}{}
	return true
}

func (r *wsRegistry) remove(conn *websocket.Conn) {
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
}

// closeAll 关闭全部活跃连接并禁止后续新连接。
func (r *wsRegistry) closeAll(reason string) {
	r.mu.Lock()
	r.closing = true
	conns := make([]*websocket.Conn, 0, len(r.conns))
	for conn := range r.conns {
		conns = append(conns, conn)
	}
	r.conns = make(map[*websocket.Conn]struct{})
	r.mu.Unlock()

	for _, conn := range conns {
		// 用 GoingAway 让前端能区分"服务端主动关闭"与网络故障。
		_ = conn.Close(websocket.StatusGoingAway, reason)
	}
}

// CloseSockets 主动关闭所有活跃的 WebSocket 连接，供优雅退出时调用。
func (s *Server) CloseSockets(reason string) {
	s.sockets.closeAll(reason)
}

// registerSocket 登记一条 WebSocket 连接。返回 false 表示服务正在关闭。
func (s *Server) registerSocket(conn *websocket.Conn) bool {
	return s.sockets.add(conn)
}

// unregisterSocket 注销一条 WebSocket 连接。
func (s *Server) unregisterSocket(conn *websocket.Conn) {
	s.sockets.remove(conn)
}
