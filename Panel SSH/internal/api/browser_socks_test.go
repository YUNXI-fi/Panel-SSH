package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTCPPair 给出一对连通的 TCP 连接（客户端侧、服务端侧）。
//
// 不用 net.Pipe：它是同步无缓冲的，写要等对端读走才返回，而 SOCKS 握手是
// 请求与应答交错的两个方向，用它会直接死锁 —— 那是脚手架的毛病，会盖掉
// 被测代码真正的行为。
func newTCPPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本机端口失败：%v", err)
	}
	defer listener.Close()

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		ch <- accepted{conn: conn, err: acceptErr}
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("连接本机端口失败：%v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("接受连接失败：%v", got.err)
	}
	return client, got.conn
}

// dialOrFail 连上一个地址，失败即终止用例。
func dialOrFail(t *testing.T, addr string) net.Conn {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接 %s 失败：%v", addr, err)
	}
	return conn
}

// negotiate 走完方法协商，返回服务端的回复字节。
func negotiate(t *testing.T, client net.Conn) []byte {
	t.Helper()

	if _, err := client.Write([]byte{socksVersion, 0x01, socksMethodNone}); err != nil {
		t.Fatalf("写出方法协商失败：%v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("读取方法协商回复失败：%v", err)
	}
	return reply
}

func TestSocksNegotiateConnect(t *testing.T) {
	ipv6 := net.ParseIP("::1").To16()

	cases := []struct {
		name string
		req  []byte
		want string
	}{
		{
			name: "IPv4",
			req:  []byte{0x05, socksCmdConnect, 0x00, socksAtypIPv4, 192, 168, 1, 10, 0x00, 0x50},
			want: "192.168.1.10:80",
		},
		{
			name: "域名",
			req:  append([]byte{0x05, socksCmdConnect, 0x00, socksAtypDomain, 11}, append([]byte("example.com"), 0x01, 0xBB)...),
			want: "example.com:443",
		},
		{
			name: "IPv6",
			req:  append([]byte{0x05, socksCmdConnect, 0x00, socksAtypIPv6}, append(ipv6, 0x00, 0x16)...),
			want: "[::1]:22",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newTCPPair(t)
			defer client.Close()
			defer server.Close()

			type result struct {
				target string
				err    error
			}
			ch := make(chan result, 1)
			go func() {
				target, err := socksNegotiate(server)
				ch <- result{target: target, err: err}
			}()

			if reply := negotiate(t, client); !bytes.Equal(reply, []byte{socksVersion, socksMethodNone}) {
				t.Fatalf("方法协商回复 = %v，期望免认证", reply)
			}
			if _, err := client.Write(tc.req); err != nil {
				t.Fatalf("写出 CONNECT 请求失败：%v", err)
			}

			got := <-ch
			if got.err != nil {
				t.Fatalf("协商失败：%v", got.err)
			}
			if got.target != tc.want {
				t.Errorf("目标地址 = %q，期望 %q", got.target, tc.want)
			}
		})
	}
}

// 只支持 CONNECT：BIND 与 UDP ASSOCIATE 是浏览器用不到的两条命令，
// 但客户端真发过来时必须回"命令不支持"，而不是闷声断开。
func TestSocksNegotiateRejectsUnsupportedCommand(t *testing.T) {
	client, server := newTCPPair(t)
	defer client.Close()
	defer server.Close()

	ch := make(chan error, 1)
	go func() {
		_, err := socksNegotiate(server)
		ch <- err
	}()

	negotiate(t, client)
	if _, err := client.Write([]byte{0x05, 0x03, 0x00, socksAtypIPv4, 127, 0, 0, 1, 0x00, 0x50}); err != nil {
		t.Fatalf("写出请求失败：%v", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("读取应答失败：%v", err)
	}
	if reply[1] != socksReplyCmdUnsupported {
		t.Errorf("应答码 = %#x，期望 %#x（命令不支持）", reply[1], socksReplyCmdUnsupported)
	}
	if err := <-ch; err == nil {
		t.Error("不支持的命令应当返回错误")
	}
}

// 客户端没提供免认证方式时，必须明确回 0xFF 让浏览器知道无法协商，
// 否则浏览器只会看到连接被关闭。
func TestSocksNegotiateRejectsUnknownMethod(t *testing.T) {
	client, server := newTCPPair(t)
	defer client.Close()
	defer server.Close()

	ch := make(chan error, 1)
	go func() {
		_, err := socksNegotiate(server)
		ch <- err
	}()

	if _, err := client.Write([]byte{socksVersion, 0x01, 0x02}); err != nil {
		t.Fatalf("写出方法协商失败：%v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("读取方法协商回复失败：%v", err)
	}
	if reply[1] != socksMethodRejected {
		t.Errorf("回复 = %#x，期望 %#x（无可接受的方法）", reply[1], socksMethodRejected)
	}
	if err := <-ch; err == nil {
		t.Error("无可接受的方法应当返回错误")
	}
}

// 空域名要挡下：它会被拼成 ":80" 这类地址交给目标机，错误现场离原因太远。
func TestSocksNegotiateRejectsEmptyDomain(t *testing.T) {
	client, server := newTCPPair(t)
	defer client.Close()
	defer server.Close()

	ch := make(chan error, 1)
	go func() {
		_, err := socksNegotiate(server)
		ch <- err
	}()

	negotiate(t, client)
	if _, err := client.Write([]byte{0x05, socksCmdConnect, 0x00, socksAtypDomain, 0x00}); err != nil {
		t.Fatalf("写出请求失败：%v", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("读取应答失败：%v", err)
	}
	if reply[1] != socksReplyAtypUnsupported {
		t.Errorf("应答码 = %#x，期望 %#x（地址类型不支持）", reply[1], socksReplyAtypUnsupported)
	}
	if err := <-ch; err == nil {
		t.Error("空域名应当返回错误")
	}
}

// 代理要双向透传，并且在一侧关闭后立刻收摊 —— 否则浏览器换页之后，
// 本机的 fd 与目标机上的 SSH 通道会一直挂到对端自己的空闲超时。
func TestSocksRelayPassesBothWays(t *testing.T) {
	localClient, localServer := newTCPPair(t)
	remoteServer, remoteClient := newTCPPair(t)
	defer localClient.Close()
	defer remoteClient.Close()

	done := make(chan struct{})
	go func() {
		socksRelay(localServer, remoteServer)
		close(done)
	}()

	if _, err := localClient.Write([]byte("get /index.html")); err != nil {
		t.Fatalf("浏览器侧写出失败：%v", err)
	}
	request := make([]byte, len("get /index.html"))
	if _, err := io.ReadFull(remoteClient, request); err != nil {
		t.Fatalf("目标站侧读取失败：%v", err)
	}
	if string(request) != "get /index.html" {
		t.Errorf("透传内容 = %q，与原样不符", request)
	}

	if _, err := remoteClient.Write([]byte("HTTP/1.1 200 OK")); err != nil {
		t.Fatalf("目标站侧写出失败：%v", err)
	}
	response := make([]byte, len("HTTP/1.1 200 OK"))
	if _, err := io.ReadFull(localClient, response); err != nil {
		t.Fatalf("浏览器侧读取失败：%v", err)
	}
	if string(response) != "HTTP/1.1 200 OK" {
		t.Errorf("回传内容 = %q，与原样不符", response)
	}

	if err := remoteClient.Close(); err != nil {
		t.Fatalf("关闭目标站侧失败：%v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("一侧关闭后转发没有收摊，会把两端的连接一起占住")
	}
}

// 宿主机就是面板本机：经它做代理不改变出网路径，因此直接挡下，
// 而不是开一个转了等于没转的端口。
func TestSocksStartRejectsLocalHost(t *testing.T) {
	socks := newHostSocks(&Server{})

	if _, err := socks.start(LocalHostID); !errors.Is(err, errSocksLocalHost) {
		t.Fatalf("宿主机应当被挡下，实际 %v", err)
	}
	if view := socks.view(LocalHostID); view.Running {
		t.Errorf("被挡下的主机不应留下运行中的代理：%+v", view)
	}
}

// 拨号失败的应答码只能粗略映射：只有超时能确定是"连不上"，
// 其余原因（SSH 只回一段文本，拿不到 errno）一律回通用失败。
func TestSocksReplyForDialError(t *testing.T) {
	if code := socksReplyForDialError(context.DeadlineExceeded); code != socksReplyHostUnreachable {
		t.Errorf("超时应答 = %#x，期望 %#x", code, socksReplyHostUnreachable)
	}
	if code := socksReplyForDialError(errors.New("connect failed: Connection refused")); code != socksReplyGeneralFailure {
		t.Errorf("其他失败应答 = %#x，期望 %#x", code, socksReplyGeneralFailure)
	}
}

// 端口要按大端解出来。单独钉一下：端口拼错时浏览器得到一个"能连上、
// 但每一条请求都指向错误端口"的代理，排查方向会被误导到目标机那边去。
func TestSocksPortIsBigEndian(t *testing.T) {
	client, server := newTCPPair(t)
	defer client.Close()
	defer server.Close()

	ch := make(chan string, 1)
	go func() {
		target, err := socksNegotiate(server)
		if err != nil {
			ch <- ""
			return
		}
		ch <- target
	}()

	negotiate(t, client)
	req := []byte{0x05, socksCmdConnect, 0x00, socksAtypIPv4, 10, 0, 0, 1, 0x1F, 0x90}
	if _, err := client.Write(req); err != nil {
		t.Fatalf("写出请求失败：%v", err)
	}
	if got := <-ch; got != "10.0.0.1:8080" {
		t.Fatalf("目标地址 = %q，期望 10.0.0.1:8080（端口 0x1F90 = 8080）", got)
	}
}

// 端到端：把拨号换成直连，对着一个真实的本机 HTTP 服务跑完整条代理链路。
//
// 这是这条功能唯一能在没有 SSH 目标机的情况下被真正验证的部分，因此它一次覆盖
// "方法协商 → CONNECT 应答 → 双向透传 → 停止后端口释放"整条路径，
// 而不是分成几个函数各自断言 —— 分开测时全绿、拼起来不通，正是代理类代码最常见的坑。
func TestSocksProxyServesHTTP(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from target")
	}))
	defer target.Close()

	socks := newHostSocks(&Server{})
	// 直连替代 SSH 通道：这里要测的是代理本身，隧道那一段由 hostTunnels 负责。
	socks.dial = func(_ int64, network, addr string) (net.Conn, error) {
		return net.Dial(network, addr)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听本机端口失败：%v", err)
	}
	proxy := &socksListener{socks: socks, hostID: 7, listener: listener}
	socks.mu.Lock()
	socks.proxies[7] = proxy
	socks.mu.Unlock()
	go proxy.serve()

	view := socks.view(7)
	if !view.Running || view.Addr == "" {
		t.Fatalf("代理状态应为运行中，实际 %+v", view)
	}
	// 地址必须落在回环上：界面把这一串原样交给用户填进浏览器，
	// 一旦监听到别处，等于把"从这台主机出网"的通道开放给整个网络。
	if host, _, err := net.SplitHostPort(view.Addr); err != nil || host != "127.0.0.1" {
		t.Fatalf("代理地址 = %q，期望回环地址", view.Addr)
	}

	if body := fetchThroughSocks(t, view.Addr, target.Listener.Addr().String()); body != "hello from target" {
		t.Fatalf("经代理取回的内容 = %q，期望 hello from target", body)
	}

	socks.stop(7)
	if view := socks.view(7); view.Running {
		t.Errorf("停止后不应再报运行中：%+v", view)
	}
	// 端口必须真的被释放：否则用户以为已经关了，通道其实还开着。
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		_ = conn.Close()
		t.Error("停止后端口仍然可连")
	}
}

// fetchThroughSocks 用一个最小 SOCKS5 客户端取回 http://targetAddr/ 的响应正文。
//
// 手写而不是引一个代理客户端库：这里要验证的正是这套协议本身，
// 用别人的实现来对答案，等于拿同族实现互相印证。
func fetchThroughSocks(t *testing.T, proxyAddr, targetAddr string) string {
	t.Helper()

	conn := dialOrFail(t, proxyAddr)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte{socksVersion, 0x01, socksMethodNone}); err != nil {
		t.Fatalf("写出方法协商失败：%v", err)
	}
	method := make([]byte, 2)
	if _, err := io.ReadFull(conn, method); err != nil {
		t.Fatalf("读取方法协商回复失败：%v", err)
	}
	if !bytes.Equal(method, []byte{socksVersion, socksMethodNone}) {
		t.Fatalf("方法协商回复 = %v，期望免认证", method)
	}

	host, portText, err := net.SplitHostPort(targetAddr)
	if err != nil {
		t.Fatalf("解析目标地址失败：%v", err)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		t.Fatalf("目标地址不是 IPv4：%s", targetAddr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("解析目标端口失败：%v", err)
	}

	request := append([]byte{socksVersion, socksCmdConnect, 0x00, socksAtypIPv4}, ip...)
	request = append(request, byte(port>>8), byte(port))
	if _, err := conn.Write(request); err != nil {
		t.Fatalf("写出 CONNECT 请求失败：%v", err)
	}

	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("读取 CONNECT 应答失败：%v", err)
	}
	if reply[1] != socksReplySucceeded {
		t.Fatalf("CONNECT 应答码 = %#x，期望成功", reply[1])
	}

	// 用 HTTP/1.0：让服务端回完就关连接，这样"响应读完"与"转发收摊"能被一起验证。
	if _, err := io.WriteString(conn, "GET / HTTP/1.0\r\nHost: "+targetAddr+"\r\n\r\n"); err != nil {
		t.Fatalf("写出 HTTP 请求失败：%v", err)
	}
	response, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("读取 HTTP 响应失败：%v", err)
	}

	_, body, ok := strings.Cut(string(response), "\r\n\r\n")
	if !ok {
		t.Fatalf("响应不是合法 HTTP：%q", response)
	}
	return body
}
