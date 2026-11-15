package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// SOCKS5 协议常量（RFC 1928）。
const (
	socksVersion = 0x05

	socksMethodNone     = 0x00
	socksMethodRejected = 0xFF

	socksCmdConnect = 0x01

	socksAtypIPv4   = 0x01
	socksAtypDomain = 0x03
	socksAtypIPv6   = 0x04
)

// SOCKS5 应答码。只用到这几个：其余码位浏览器不会区分，回"通用失败"即可。
const (
	socksReplySucceeded       = 0x00
	socksReplyGeneralFailure  = 0x01
	socksReplyHostUnreachable = 0x04
	socksReplyCmdUnsupported  = 0x07
	socksReplyAtypUnsupported = 0x08
)

const (
	// socksHandshakeTimeout 限制"连上到说出要访问哪里"这一段。
	//
	// 这个端口在本机是公开可连的，不能让一个只连不说的客户端把协程一直占着。
	// 握手一结束就会清掉期限，因此它不会误伤长命的 keep-alive 连接。
	socksHandshakeTimeout = 10 * time.Second
	// socksDialTimeout 是从目标机拨向最终地址的等待上限。
	socksDialTimeout = 20 * time.Second
)

// errSocksLocalHost 表示对宿主机请求代理。
//
// 它不是"做不到"，而是没有意义：宿主机就是面板所在的这台机器，而这个代理只监听
// 回环 —— 能连到这个回环的浏览器，本来就与它处在同一个网络里，经它出网等于绕一圈回到原地。
var errSocksLocalHost = errors.New("宿主机就是面板本机，浏览器与它同处一个网络，经它做代理不会改变出网路径")

// hostSocks 管理按主机开的 SOCKS5 代理。
//
// 它在面板本机监听一个端口，把每条进来的 SOCKS 连接经 SSH 的 direct-tcpip 通道
// 送到目标机，再由目标机出网。浏览器把它当作代理，于是"借这台主机访问网页"这件事
// 不需要改写任何网页内容：页面从源站原样加载。这与面板内嵌浏览器那条反代改写
// 的路子互补 —— 那条零配置但只对结构简单的站点可靠，这条要配一次浏览器但什么站都能开。
//
// 一处刻意的取舍：只监听回环、不做认证。浏览器（尤其 Chrome）没有可用的
// "SOCKS 代理带用户名密码"配置入口，加认证会把这条路的可用性直接砍掉一半；
// 而监听回环意味着能用它的只有面板本机上的进程 —— 用户按一次"启动"才会开，
// 关掉即收回，与插件隧道"只监听回环"的约定一致。
type hostSocks struct {
	s *Server

	// dial 负责把一条客户端连接送到目标地址。默认实现是"经目标机的 SSH 通道
	// direct-tcpip 拨出去"，收成一个字段是为了让 SOCKS5 协议层本身能被端到端验证：
	// 测试里把它换成直连，就能对着一个真实的本机服务跑完整条链路（协商 → 应答
	// → 双向转发），而不必先搭一台 SSH 服务器再把整条隧道拼起来。
	dial func(hostID int64, network, addr string) (net.Conn, error)

	mu      sync.Mutex
	proxies map[int64]*socksListener
}

// socksListener 是一台主机上已建立的 SOCKS5 代理。
type socksListener struct {
	socks    *hostSocks
	hostID   int64
	listener net.Listener

	// closeOnce 保证监听器只关一次：停止与面板退出可能同时发生。
	closeOnce sync.Once
}

func newHostSocks(s *Server) *hostSocks {
	socks := &hostSocks{s: s, proxies: map[int64]*socksListener{}}
	socks.dial = func(hostID int64, network, addr string) (net.Conn, error) {
		// 通道每次现取，而不是启动时抓一条存着：取用会顺带探测存活并在失效时重建，
		// 于是目标机重启、网络抖动、通道被空闲回收之后代理都能自己恢复。抓着一条不放的话，
		// 那条连接一断，代理就永久性地只报错，直到用户手动重启它。
		client, err := s.tunnels.client(hostID)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), socksDialTimeout)
		defer cancel()
		return client.DialContext(ctx, network, addr)
	}
	return socks
}

// start 为某台主机开一个代理并返回它的地址；已在运行则直接返回现有地址。
func (m *hostSocks) start(hostID int64) (string, error) {
	if hostID == LocalHostID {
		return "", errSocksLocalHost
	}

	// 先把通道建立起来再监听端口。顺序反过来的话，凭据不对或目标机连不上时，
	// 面板这边已经开好了一个端口对外可用，而它转发的每一条连接都只会失败 ——
	// 用户看到的是一句"代理已启动"，然后浏览器里全是错误。
	if _, err := m.s.tunnels.client(hostID); err != nil {
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.proxies[hostID]; ok {
		return existing.addr(), nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("监听本机端口失败：%w", err)
	}

	proxy := &socksListener{socks: m, hostID: hostID, listener: listener}
	m.proxies[hostID] = proxy
	go proxy.serve()

	return proxy.addr(), nil
}

// stop 关掉某台主机的代理。
func (m *hostSocks) stop(hostID int64) {
	m.mu.Lock()
	proxy := m.proxies[hostID]
	delete(m.proxies, hostID)
	m.mu.Unlock()

	if proxy != nil {
		proxy.close()
	}
}

// view 给出某台主机代理的当前状态。
func (m *hostSocks) view(hostID int64) browserSocksView {
	m.mu.Lock()
	defer m.mu.Unlock()

	if proxy, ok := m.proxies[hostID]; ok {
		return browserSocksView{Running: true, Addr: proxy.addr()}
	}
	return browserSocksView{}
}

// closeAll 关掉全部代理，供面板退出时调用。
func (m *hostSocks) closeAll() {
	m.mu.Lock()
	proxies := make([]*socksListener, 0, len(m.proxies))
	for _, proxy := range m.proxies {
		proxies = append(proxies, proxy)
	}
	m.proxies = map[int64]*socksListener{}
	m.mu.Unlock()

	for _, proxy := range proxies {
		proxy.close()
	}
}

func (l *socksListener) addr() string {
	addr, ok := l.listener.Addr().(*net.TCPAddr)
	if !ok {
		return ""
	}
	// 固定回环地址而不是直接取监听地址：监听地址可能带上 zone 之类的修饰，
	// 而这里要给出的是让用户填进浏览器的那一串。
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
}

func (l *socksListener) close() {
	l.closeOnce.Do(func() {
		_ = l.listener.Close()
	})
}

// serve 接受本机连接，逐条代理出去。
func (l *socksListener) serve() {
	for {
		conn, err := l.listener.Accept()
		if err != nil {
			// 监听器已被关闭（停止或面板退出），代理随之结束。
			return
		}
		go l.handle(conn)
	}
}

// handle 处理一条 SOCKS5 连接：握手 → 经目标机拨号 → 双向转发。
func (l *socksListener) handle(local net.Conn) {
	defer local.Close()

	_ = local.SetDeadline(time.Now().Add(socksHandshakeTimeout))
	target, err := socksNegotiate(local)
	if err != nil {
		return
	}
	// 握手完成后必须清掉期限：一条 keep-alive 网页连接会活很久，
	// 留着这个期限会在 10 秒后把它拦腰掐断，而用户只会看到页面卡住。
	_ = local.SetDeadline(time.Time{})

	remote, err := l.socks.dial(l.hostID, "tcp", target)
	if err != nil {
		// 拨号失败也要回一个码：只断开的话，浏览器只会说"代理服务器无响应"，
		// 而这里完全知道是哪一步失败，信息不该烂在服务端。
		_ = socksWriteReply(local, socksReplyForDialError(err))
		return
	}
	defer remote.Close()

	if err := socksWriteReply(local, socksReplySucceeded); err != nil {
		return
	}
	socksRelay(local, remote)
}

// socksNegotiate 完成 SOCKS5 的"方法协商 + CONNECT 请求"两步，返回目标地址。
//
// 需要立刻回一个应答码的情形（版本不对、没有可选方法、命令不支持）由它自己回完再
// 返回错误，理由同上：错误要尽可能地让对方看见。
func socksNegotiate(conn net.Conn) (string, error) {
	var head [2]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return "", err
	}
	if head[0] != socksVersion {
		return "", fmt.Errorf("不支持的 SOCKS 版本 %d", head[0])
	}

	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", err
	}
	if !bytes.Contains(methods, []byte{socksMethodNone}) {
		_, _ = conn.Write([]byte{socksVersion, socksMethodRejected})
		return "", errors.New("客户端未提供免认证方式")
	}
	if _, err := conn.Write([]byte{socksVersion, socksMethodNone}); err != nil {
		return "", err
	}

	var req [4]byte
	if _, err := io.ReadFull(conn, req[:]); err != nil {
		return "", err
	}
	if req[0] != socksVersion {
		return "", fmt.Errorf("不支持的 SOCKS 版本 %d", req[0])
	}
	if req[1] != socksCmdConnect {
		// BIND 与 UDP ASSOCIATE 都不支持：浏览器只会用到 CONNECT。
		_ = socksWriteReply(conn, socksReplyCmdUnsupported)
		return "", fmt.Errorf("不支持的 SOCKS 命令 %d", req[1])
	}

	host, err := socksReadAddr(conn, req[3])
	if err != nil {
		_ = socksWriteReply(conn, socksReplyAtypUnsupported)
		return "", err
	}

	var raw [2]byte
	if _, err := io.ReadFull(conn, raw[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(raw[:])))), nil
}

// socksReadAddr 按地址类型读出目标主机。
func socksReadAddr(conn net.Conn, atyp byte) (string, error) {
	switch atyp {
	case socksAtypIPv4:
		var raw [4]byte
		if _, err := io.ReadFull(conn, raw[:]); err != nil {
			return "", err
		}
		return net.IP(raw[:]).String(), nil
	case socksAtypIPv6:
		var raw [16]byte
		if _, err := io.ReadFull(conn, raw[:]); err != nil {
			return "", err
		}
		return net.IP(raw[:]).String(), nil
	case socksAtypDomain:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return "", err
		}
		if size[0] == 0 {
			return "", errors.New("域名为空")
		}
		raw := make([]byte, size[0])
		if _, err := io.ReadFull(conn, raw); err != nil {
			return "", err
		}
		// 域名原样交给目标机解析，这里不做形态校验：地址最终由 ssh 的
		// DialContext 按 "host:port" 解析，畸形输入在那一层就会失败。
		return string(raw), nil
	default:
		return "", fmt.Errorf("不支持的地址类型 %d", atyp)
	}
}

// socksWriteReply 回一条 SOCKS5 应答。
//
// 绑定地址一律填 0.0.0.0:0：这个字段是给"目标机回连客户端"用的（BIND 场景），
// 而代理承载的 CONNECT 不需要它，客户端也不会读它 —— 填真实的本端地址反而要多写
// 一套 IPv4/IPv6 编码分支。
func socksWriteReply(conn net.Conn, code byte) error {
	_, err := conn.Write([]byte{socksVersion, code, 0x00, socksAtypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// socksReplyForDialError 把拨号失败映射成应答码。
//
// 只能粗略映射：SSH 把失败原因以一段文本（"connect failed: ..."）带回来，
// 拿不到 errno。除超时外一律回"通用失败" —— 浏览器对这几个码的展示差别很小，
// 猜一个具体的反而误导。
func socksReplyForDialError(err error) byte {
	if errors.Is(err, context.DeadlineExceeded) {
		return socksReplyHostUnreachable
	}
	return socksReplyGeneralFailure
}

// socksRelay 双向转发，任一方向结束就收摊。
//
// 这里的取舍与插件隧道不同（那边连接是请求-响应式的，半关闭没有意义），
// 但结论一样：浏览器与目标站之间是 keep-alive 长连接，浏览器这一侧关闭时
// （换页、空闲回收）目标站还会把连接挂到它自己的空闲超时上。真要等它，两条通道
// ——本机的一个 fd 与目标机上的一个 SSH 通道——就要白占几十秒。而浏览器在等响应
// 期间不会半关闭，所以"先关一端就收摊"不会截断任何正在读的响应。
func socksRelay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	pump := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go pump(a, b)
	go pump(b, a)

	// 只等第一个方向结束就返回，由调用方的 defer 关掉两端；
	// 另一个方向的 io.Copy 会因为读写端被关而立刻出错退出。
	<-done
}

// browserSocksView 是给前端的代理状态。
type browserSocksView struct {
	Running bool `json:"running"`
	// Addr 形如 127.0.0.1:38271，未运行时为空。
	Addr string `json:"addr"`
}

func (s *Server) handleBrowserSocksStatus(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.socks.view(hostID))
}

// handleBrowserSocksStart 为某台主机启动浏览器代理。
func (s *Server) handleBrowserSocksStart(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	addr, err := s.socks.start(hostID)
	if err != nil {
		if errors.Is(err, errSocksLocalHost) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.writeHostError(w, err, "启动浏览器代理失败")
		return
	}
	writeJSON(w, http.StatusOK, browserSocksView{Running: true, Addr: addr})
}

// handleBrowserSocksStop 停止某台主机的浏览器代理。
func (s *Server) handleBrowserSocksStop(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	s.socks.stop(hostID)
	writeJSON(w, http.StatusOK, browserSocksView{})
}

// CloseBrowserProxies 停掉全部浏览器代理，供面板退出时调用。
//
// 代理本身只在本机监听，进程退出时端口自然就没了；显式关掉是为了让正在转发的
// 连接立刻断开，而不是等进程把文件描述符一起带走。
func (s *Server) CloseBrowserProxies() {
	s.socks.closeAll()
}

// CloseHostTunnels 关闭全部常驻 SSH 通道，供面板退出时调用。
//
// 必须在所有用它的消费者之后调用：停远端 dsh 实例正要靠这条通道下发信号。
func (s *Server) CloseHostTunnels() {
	s.tunnels.closeAll()
}
