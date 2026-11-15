package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// dshNativeDialTimeout 是原生地址转发时建立到目标机 dsh 的等待上限。
const dshNativeDialTimeout = 15 * time.Second

// dshNative 管理「原生地址」—— 把 dsh 的浏览器入口直接交给用户本机的浏览器。
//
// 为什么需要它：dsh 的 DeepSeek 账号登录要求授权页与回调都落在它自己的回环
// origin 上（dsh-deepseek-account-platform 的 loginOrigin 明确拒绝非回环地址
// 与子路径反代），而面板把 dsh 挂在 /dsh/{id}/ 下，这条登录链路必然断在授权
// 跳转或 /oauth/callback 上。原生地址绕开面板反代，让浏览器直接访问 dsh 监听
// 的回环端口，登录因此可用。
//
// 代价是它只对「能与面板机共享回环」的浏览器有效 —— 也就是浏览器与面板跑在
// 同一台机器上。远端主机要在面板机回环上再监听一个同端口的转发：dsh 的授权
// 页写死了它自己的端口，映射到别的端口会让授权跳转落到错误的地方。
type dshNative struct {
	s *Server

	mu        sync.Mutex
	listeners map[int64]*dshNativeListener
}

// dshNativeListener 是一条「面板机回环端口 → 目标机 dsh 端口」的转发。
type dshNativeListener struct {
	hostID   int64
	port     string
	listener net.Listener

	closeOnce sync.Once
}

func newDshNative(s *Server) *dshNative {
	return &dshNative{s: s, listeners: map[int64]*dshNativeListener{}}
}

// URL 返回某台主机上 dsh 的原生访问地址（含一次性启动令牌）。
func (n *dshNative) URL(hostID int64) (string, error) {
	endpoint, token, ok := n.s.dsh.Supervisor(hostID).Endpoint()
	if !ok || token == "" {
		return "", errors.New("dsh 尚未就绪，无法获取原生地址")
	}

	target, err := url.Parse(endpoint)
	if err != nil || target.Port() == "" {
		return "", fmt.Errorf("无法解析 dsh 的监听地址：%s", endpoint)
	}

	// 宿主机上 dsh 就监听在面板机的回环上，浏览器直接可达，不需要转发。
	host := net.JoinHostPort("127.0.0.1", target.Port())
	if hostID != LocalHostID {
		if host, err = n.listen(hostID, target.Port()); err != nil {
			return "", err
		}
	}

	return "http://" + host + "/?token=" + url.QueryEscape(token), nil
}

// listen 在面板机回环上监听与目标机 dsh 相同的端口，并转发过去。
func (n *dshNative) listen(hostID int64, port string) (string, error) {
	// 先确认 SSH 通道可用再监听端口：顺序反过来的话，凭据不对或目标机连不上时
	// 面板会先开出一个「连得上但每条都失败」的端口，用户只会看到空白页。
	if _, err := n.s.tunnels.client(hostID); err != nil {
		return "", err
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	if existing, ok := n.listeners[hostID]; ok {
		if existing.port == port {
			return existing.addr(), nil
		}
		// dsh 重启后 --port 0 会重新分配端口，旧监听已经没有意义。
		existing.close()
		delete(n.listeners, hostID)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return "", fmt.Errorf("本机端口 %s 已被占用，无法建立与目标机同端口的转发；DeepSeek 账号登录要求两端端口一致", port)
	}

	l := &dshNativeListener{hostID: hostID, port: port, listener: listener}
	n.listeners[hostID] = l
	go n.serve(l)

	return l.addr(), nil
}

func (l *dshNativeListener) addr() string {
	return net.JoinHostPort("127.0.0.1", l.port)
}

func (l *dshNativeListener) close() {
	l.closeOnce.Do(func() {
		_ = l.listener.Close()
	})
}

// serve 接受本机连接，逐条转发到目标机上的 dsh。
func (n *dshNative) serve(l *dshNativeListener) {
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			// 监听器已被关闭（dsh 停止或面板退出），转发随之结束。
			return
		}
		go n.forward(l.hostID, conn)
	}
}

// forward 把一条本机连接接到 dsh 上。目标地址每次现取：dsh 可能刚重启换了端口。
func (n *dshNative) forward(hostID int64, local net.Conn) {
	defer local.Close()

	endpoint, _, ok := n.s.dsh.Supervisor(hostID).Endpoint()
	if !ok {
		return
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return
	}
	client, err := n.s.tunnels.client(hostID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dshNativeDialTimeout)
	defer cancel()
	remote, err := client.DialContext(ctx, "tcp", target.Host)
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

// close 关掉某台主机的转发。
func (n *dshNative) close(hostID int64) {
	n.mu.Lock()
	l := n.listeners[hostID]
	delete(n.listeners, hostID)
	n.mu.Unlock()

	if l != nil {
		l.close()
	}
}

// closeAll 关掉全部转发，供面板退出与 dsh 实例整体停止时调用。
func (n *dshNative) closeAll() {
	n.mu.Lock()
	listeners := make([]*dshNativeListener, 0, len(n.listeners))
	for _, l := range n.listeners {
		listeners = append(listeners, l)
	}
	n.listeners = map[int64]*dshNativeListener{}
	n.mu.Unlock()

	for _, l := range listeners {
		l.close()
	}
}

// handleDshNative 签发 dsh 的原生访问地址。
//
// 地址里带 dsh 的一次性启动令牌，因此只对已登录的面板用户开放；地址本身只
// 在面板机回环上可达，跨机访问需要用户自己做端口转发（见 frontend 的提示）。
func (s *Server) handleDshNative(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	if !s.dshWorkspaceInstalled(hostID) {
		http.Error(w, "DSH 工作台插件未安装", http.StatusNotFound)
		return
	}

	address, err := s.dshNative.URL(hostID)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":          address,
		"loopbackOnly": true,
	})
}
