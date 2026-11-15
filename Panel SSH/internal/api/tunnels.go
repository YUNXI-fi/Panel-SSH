package api

import (
	"context"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"panelssh/internal/sshclient"
)

const (
	// tunnelIdleTTL 是常驻通道在无人使用多久之后回收。
	//
	// 取值与 sshclient.Pool 一致：同样是"指向生产机的 SSH 连接不该长期占着"。
	// 空闲回收对使用者是透明的 —— 下一次取用会重新拨一条。
	tunnelIdleTTL = 3 * time.Minute
	// tunnelReapInterval 是空闲回收的检查周期。
	tunnelReapInterval = time.Minute
)

// hostTunnels 按主机持有长期占用的 SSH 连接。
//
// 它服务的是所有"需要一条持续通道"的功能：DSH 反代、内嵌浏览器反代、SOCKS5 代理。
// 这些功能要的通道形态各不相同（http.Transport 的拨号器、每条客户端连接一次
// direct-tcpip），但"到哪台机器、用哪份凭据、断了怎么办"是同一件事，因此只做一次。
//
// 为什么不复用 sshclient.Pool：
//
//   - 池的租约按引用计数回收，而这里的连接要被长期占用（反代的每个请求、SOCKS 的
//     每条客户端连接都从它上面开通道），refs 永远不回零，空闲回收对它彻底失效；
//   - 主机凭据变更时池只把连接标成 retired，等 refs 归零才关，于是这条连接会一直
//     用旧凭据服务下去。
//
// 因此自己持有，且生命周期由自己管：一台主机一条，取用前探测存活，失效即重建，
// 长期没人用就回收。回收权必须留在这里 —— 通道是共用的，任何一个消费者都没有
// 立场关掉别人可能正在用的连接，所以「停止 dsh」「停掉代理」这类动作只停它自己的
// 东西，不自作主张去关通道。
type hostTunnels struct {
	s *Server

	mu      sync.Mutex
	clients map[int64]*tunnelConn

	stop chan struct{}
	once sync.Once
}

// tunnelConn 是一条常驻通道当前的状态。
type tunnelConn struct {
	client   *ssh.Client
	lastUsed time.Time
}

func newHostTunnels(s *Server) *hostTunnels {
	t := &hostTunnels{
		s:       s,
		clients: map[int64]*tunnelConn{},
		stop:    make(chan struct{}),
	}
	go t.janitor()
	return t
}

// client 取一条到目标机的可用连接，必要时新建。
func (t *hostTunnels) client(hostID int64) (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if cached, ok := t.clients[hostID]; ok {
		if sshclient.Alive(cached.client) {
			cached.lastUsed = time.Now()
			return cached.client, nil
		}
		// 判否时连接已被 Alive 自己关掉，这里只需要把它从表里摘掉；
		// 下面会重新拨一条，依赖它的功能因此能从"隧道断了"里自愈。
		delete(t.clients, hostID)
	}

	client, err := t.dial(hostID)
	if err != nil {
		return nil, err
	}
	t.clients[hostID] = &tunnelConn{client: client, lastUsed: time.Now()}
	return client, nil
}

func (t *hostTunnels) dial(hostID int64) (*ssh.Client, error) {
	host, err := t.s.store.GetHost(hostID)
	if err != nil {
		return nil, err
	}
	secret, err := t.s.hostSecret(hostID)
	if err != nil {
		return nil, err
	}
	return sshclient.Dial(sshclient.Target{
		Addr:     host.Addr,
		Port:     host.Port,
		Username: host.Username,
		AuthType: host.AuthType,
		Secret:   secret,
	}, t.s.knownHosts, dialTimeout)
}

// dialContext 让它能当 http.Transport 的拨号器。
//
// 连接由 ssh.Client 直接给出：请求走 direct-tcpip 通道，在目标机看来就是
// "从回环连过来的"。
func (t *hostTunnels) dialContext(ctx context.Context, hostID int64, network, addr string) (net.Conn, error) {
	client, err := t.client(hostID)
	if err != nil {
		return nil, err
	}
	return client.DialContext(ctx, network, addr)
}

// closeAll 关掉全部通道并停止回收协程，供面板退出时调用。
func (t *hostTunnels) closeAll() {
	t.once.Do(func() { close(t.stop) })

	t.mu.Lock()
	clients := make([]*ssh.Client, 0, len(t.clients))
	for _, cached := range t.clients {
		clients = append(clients, cached.client)
	}
	t.clients = map[int64]*tunnelConn{}
	t.mu.Unlock()

	for _, client := range clients {
		_ = client.Close()
	}
}

// janitor 定期回收长期没人用的通道。
//
// 需要一个定时协程而不是在取用时顺手清理：真正要处理的恰恰是"再也没人取用"
// 的那些 —— 用户看完一个内网页面就关了窗口、停掉代理之后不再打开，惰性清理
// 永远等不到下一次取用。
func (t *hostTunnels) janitor() {
	ticker := time.NewTicker(tunnelReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			t.reapIdle()
		}
	}
}

func (t *hostTunnels) reapIdle() {
	deadline := time.Now().Add(-tunnelIdleTTL)

	t.mu.Lock()
	var stale []*ssh.Client
	for hostID, cached := range t.clients {
		if cached.lastUsed.Before(deadline) {
			stale = append(stale, cached.client)
			delete(t.clients, hostID)
		}
	}
	t.mu.Unlock()

	for _, client := range stale {
		_ = client.Close()
	}
}
