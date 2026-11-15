package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"panelssh/internal/secretbox"
	"panelssh/internal/sshclient"
)

// 插件隧道的约束。
const (
	// maxPluginTunnelsPerPlugin 限制单个插件同时持有的隧道数。
	//
	// 不是资源问题，而是"能数得清"：每条隧道都是面板替插件开着的一个本机端口，
	// 数量失控之后，"这个端口是谁开的"会变成一件查不清的事。
	maxPluginTunnelsPerPlugin = 16
	// pluginTunnelDialTimeout 是建立转发连接的等待上限。
	pluginTunnelDialTimeout = 15 * time.Second
)

// pluginTunnels 管理插件申请的端口转发。
//
// 它做的是：在面板本机监听一个端口，把每个进来的连接转发到目标机的
// remoteHost:remotePort。插件进程连本机端口就等于连上了目标机上的服务 ——
// 这是唯一一处面板替插件建立到目标机的通道的能力，所以 remoteHost 必须限死
// 在回环与私网，否则插件能借目标机做代理跳板。
//
// 连接自己持有（不走 sshclient.Pool），理由与 dshTunnels 相同：它要被长期占用，
// 池的引用计数永远不回零，空闲回收会彻底失效；主机凭据变更时也只会被标成
// retired 而一直用旧凭据服务下去。
type pluginTunnels struct {
	s *Server

	mu       sync.Mutex
	clients  map[int64]*ssh.Client      // 按主机持有的转发连接
	tunnels  map[string]*pluginTunnel   // tunnelId → 隧道
	byPlugin map[string]map[string]bool // org/name → 隧道 id 集合
}

// pluginTunnel 是一条已建立的隧道。
type pluginTunnel struct {
	id         string
	org        string
	name       string
	hostID     int64
	remoteAddr string
	listener   net.Listener

	closeOnce sync.Once
}

func newPluginTunnels(s *Server) *pluginTunnels {
	return &pluginTunnels{
		s:        s,
		clients:  map[int64]*ssh.Client{},
		tunnels:  map[string]*pluginTunnel{},
		byPlugin: map[string]map[string]bool{},
	}
}

// open 建立一条隧道并开始转发。
func (t *pluginTunnels) open(org, name string, hostID int64, remoteHost string, remotePort int) (*pluginTunnel, error) {
	if !pluginTunnelHostAllowed(remoteHost) {
		return nil, errors.New("remoteHost 只能是回环或私网地址")
	}
	if remotePort < 1 || remotePort > 65535 {
		return nil, errors.New("remotePort 必须在 1 到 65535 之间")
	}
	if hostID == LocalHostID {
		// 插件进程就跑在这台机器上，回环端口本来就连得到，没有隧道的必要。
		return nil, errors.New("宿主机不需要隧道")
	}

	key := org + "/" + name
	t.mu.Lock()
	if len(t.byPlugin[key]) >= maxPluginTunnelsPerPlugin {
		t.mu.Unlock()
		return nil, fmt.Errorf("单个插件最多同时持有 %d 条隧道", maxPluginTunnelsPerPlugin)
	}
	t.mu.Unlock()

	client, err := t.client(hostID)
	if err != nil {
		return nil, err
	}

	// 只监听回环：隧道是给面板机上的插件进程用的，监听 0.0.0.0 会让这台机器的
	// 网络邻居也借道进到目标机里。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("监听本地端口失败：%w", err)
	}

	// 隧道 id 不可枚举：它是关闭隧道时的凭据，用一个连号很容易被猜出来。
	id, err := secretbox.RandomToken(8)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("生成隧道标识失败：%w", err)
	}

	tunnel := &pluginTunnel{
		id:         id,
		org:        org,
		name:       name,
		hostID:     hostID,
		remoteAddr: net.JoinHostPort(remoteHost, strconv.Itoa(remotePort)),
		listener:   listener,
	}

	t.mu.Lock()
	t.tunnels[id] = tunnel
	if t.byPlugin[key] == nil {
		t.byPlugin[key] = map[string]bool{}
	}
	t.byPlugin[key][id] = true
	t.mu.Unlock()

	go t.serve(tunnel, client)
	return tunnel, nil
}

// localPort 返回本地监听的端口。
func (t *pluginTunnel) localPort() int {
	addr, ok := t.listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}
	return addr.Port
}

// serve 接受本机连接，逐条转发到目标机。
func (t *pluginTunnels) serve(tunnel *pluginTunnel, client *ssh.Client) {
	for {
		conn, err := tunnel.listener.Accept()
		if err != nil {
			// 监听器已被关闭（隧道关闭或面板退出），转发随之结束。
			return
		}
		go t.forward(tunnel, client, conn)
	}
}

// forward 把一条本地连接与目标机上的连接对接起来。
//
// 任一方向结束就关掉两端：这里适配的是请求-响应式的数据库/HTTP 连接，
// 半关闭语义在它们身上没有意义，留着半个连接只会白白占着目标机的句柄。
func (t *pluginTunnels) forward(tunnel *pluginTunnel, client *ssh.Client, local net.Conn) {
	defer local.Close()

	ctx, cancel := context.WithTimeout(context.Background(), pluginTunnelDialTimeout)
	defer cancel()

	remote, err := client.DialContext(ctx, "tcp", tunnel.remoteAddr)
	if err != nil {
		return
	}
	defer remote.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, local)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, remote)
		done <- struct{}{}
	}()
	<-done
}

// get 按 id 取一条隧道。
func (t *pluginTunnels) get(id string) (*pluginTunnel, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tunnel, ok := t.tunnels[id]
	return tunnel, ok
}

// close 关闭一条隧道。
func (t *pluginTunnels) close(tunnel *pluginTunnel) {
	tunnel.closeOnce.Do(func() {
		_ = tunnel.listener.Close()
	})

	key := tunnel.org + "/" + tunnel.name

	t.mu.Lock()
	delete(t.tunnels, tunnel.id)
	if set := t.byPlugin[key]; set != nil {
		delete(set, tunnel.id)
		if len(set) == 0 {
			delete(t.byPlugin, key)
		}
	}
	t.mu.Unlock()
}

// closePlugin 关掉某个插件的全部隧道。
//
// 插件进程停止或插件卸载时无条件调用：隧道是面板替它开的门，
// 门开着而它已经不在，等于留下一条没人认领的通道。
func (t *pluginTunnels) closePlugin(org, name string) {
	t.mu.Lock()
	ids := make([]string, 0, len(t.byPlugin[org+"/"+name]))
	for id := range t.byPlugin[org+"/"+name] {
		ids = append(ids, id)
	}
	t.mu.Unlock()

	for _, id := range ids {
		if tunnel, ok := t.get(id); ok {
			t.close(tunnel)
		}
	}
}

// closeAll 关掉全部隧道与转发连接，供面板退出时调用。
func (t *pluginTunnels) closeAll() {
	t.mu.Lock()
	tunnels := make([]*pluginTunnel, 0, len(t.tunnels))
	for _, tunnel := range t.tunnels {
		tunnels = append(tunnels, tunnel)
	}
	clients := make([]*ssh.Client, 0, len(t.clients))
	for _, client := range t.clients {
		clients = append(clients, client)
	}
	t.tunnels = map[string]*pluginTunnel{}
	t.byPlugin = map[string]map[string]bool{}
	t.clients = map[int64]*ssh.Client{}
	t.mu.Unlock()

	for _, tunnel := range tunnels {
		tunnel.closeOnce.Do(func() {
			_ = tunnel.listener.Close()
		})
	}
	for _, client := range clients {
		_ = client.Close()
	}
}

// client 取一条到目标机的可用转发连接，必要时新建。
func (t *pluginTunnels) client(hostID int64) (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if cached, ok := t.clients[hostID]; ok {
		if sshclient.Alive(cached) {
			return cached, nil
		}
		// 判否时连接已被 Alive 自己关掉，这里只需把它从表里摘掉，
		// 下面会重新拨一条，隧道因此能从"连接断了"里自愈。
		delete(t.clients, hostID)
	}

	host, err := t.s.store.GetHost(hostID)
	if err != nil {
		return nil, err
	}
	secret, err := t.s.hostSecret(hostID)
	if err != nil {
		return nil, err
	}
	client, err := sshclient.Dial(sshclient.Target{
		Addr:     host.Addr,
		Port:     host.Port,
		Username: host.Username,
		AuthType: host.AuthType,
		Secret:   secret,
	}, t.s.knownHosts, dialTimeout)
	if err != nil {
		return nil, err
	}

	t.clients[hostID] = client
	return client, nil
}

// pluginTunnelHostAllowed 判断隧道目标地址是否在允许范围内。
//
// 只允许回环与私网：插件要连的是目标机上"只监听 127.0.0.1 的服务"，
// 放开公网地址等于给了它一个借目标机出网的跳板 —— 那是代理，不是隧道。
func pluginTunnelHostAllowed(host string) bool {
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
