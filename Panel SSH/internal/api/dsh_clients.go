package api

import (
	"sync"

	"golang.org/x/crypto/ssh"

	"panelssh/internal/sshclient"
)

// dshClients 按主机持有 dsh 实例专用的 SSH 连接。
//
// 为什么不能复用 hostTunnels：那条共享连接有 3 分钟空闲回收，而 dsh 是长驻
// 服务，进程就挂在承载它的 SSH 会话上。只要几分钟没人打开工作台，连接被
// janitor 回收，会话随之断开，dsh 就被带走了 —— 远端用户看到的是
// 「dsh 进程已退出：wait: remote command exited without exit status or exit signal」。
// 宿主机用本地子进程，不经过这条连接，所以这个坑只在远端出现。
//
// dsh 的运行期本身就是长期占用，因此这里不做空闲回收：连接随实例生命周期走，
// dsh 停止时由 api 层显式关闭，面板退出时统一关闭。
type dshClients struct {
	s *Server

	mu      sync.Mutex
	clients map[int64]*ssh.Client
}

func newDshClients(s *Server) *dshClients {
	return &dshClients{s: s, clients: map[int64]*ssh.Client{}}
}

// client 取一条到目标机的专用连接，必要时新建。
func (c *dshClients) client(hostID int64) (*ssh.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if cached, ok := c.clients[hostID]; ok {
		if sshclient.Alive(cached) {
			return cached, nil
		}
		// 判否时连接已被 Alive 自己关掉，这里只需把它从表里摘掉。
		delete(c.clients, hostID)
	}

	host, err := c.s.store.GetHost(hostID)
	if err != nil {
		return nil, err
	}
	secret, err := c.s.hostSecret(hostID)
	if err != nil {
		return nil, err
	}
	client, err := sshclient.Dial(sshclient.Target{
		Addr:     host.Addr,
		Port:     host.Port,
		Username: host.Username,
		AuthType: host.AuthType,
		Secret:   secret,
	}, c.s.knownHosts, dialTimeout)
	if err != nil {
		return nil, err
	}

	c.clients[hostID] = client
	return client, nil
}

// close 关掉某台主机的专用连接。必须在 dsh 停止之后调用：停止动作本身
// 要靠这条连接把信号送到目标机。
func (c *dshClients) close(hostID int64) {
	c.mu.Lock()
	client := c.clients[hostID]
	delete(c.clients, hostID)
	c.mu.Unlock()

	if client != nil {
		_ = client.Close()
	}
}

// closeAll 关掉全部专用连接，供面板退出时调用。
func (c *dshClients) closeAll() {
	c.mu.Lock()
	clients := make([]*ssh.Client, 0, len(c.clients))
	for _, client := range c.clients {
		clients = append(clients, client)
	}
	c.clients = map[int64]*ssh.Client{}
	c.mu.Unlock()

	for _, client := range clients {
		_ = client.Close()
	}
}
