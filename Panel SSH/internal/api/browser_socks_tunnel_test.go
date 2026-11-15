package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"panelssh/internal/config"
	"panelssh/internal/secretbox"
	"panelssh/internal/sshclient"
	"panelssh/internal/store"
)

const (
	testSSHUser     = "tester"
	testSSHPassword = "tunnel-test-secret"
)

// startTestSSHServer 起一个只够测试用的 SSH 服务端：密码认证 + direct-tcpip 转发。
//
// 用进程内服务端而不是连一台真机器：这几条用例要验的是"面板能不能经 SSH 通道
// 把请求送到目标机那一侧"，那台机器具体是谁不影响结论 —— 而进程内服务端让用例
// 自包含，不必借用任何人的凭据，也不会在别人的服务器上留下连接记录。
//
// allowForwarding 为假时按 OpenSSH 的做法拒绝通道（Prohibited + "open failed"），
// 用来复现"对方 sshd 关了 AllowTcpForwarding"这类真实环境。
func startTestSSHServer(t *testing.T, allowForwarding bool) string {
	t.Helper()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成测试主机密钥失败：%v", err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("构造签名者失败：%v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if meta.User() == testSSHUser && string(password) == testSSHPassword {
				return nil, nil
			}
			return nil, errors.New("认证失败")
		},
	}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听测试 SSH 端口失败：%v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveTestSSHConn(conn, config, allowForwarding)
		}
	}()

	return listener.Addr().String()
}

func serveTestSSHConn(raw net.Conn, config *ssh.ServerConfig, allowForwarding bool) {
	defer raw.Close()

	serverConn, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(requests)

	for newChannel := range channels {
		if newChannel.ChannelType() != "direct-tcpip" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "只支持 direct-tcpip")
			continue
		}
		if !allowForwarding {
			// 与 OpenSSH 的 AllowTcpForwarding no 一致：应答码 Prohibited、说明 "open failed"。
			_ = newChannel.Reject(ssh.Prohibited, "open failed")
			continue
		}

		// direct-tcpip 的载荷就是"要连到哪"，字段顺序与 ssh 协议一致。
		var payload struct {
			Host       string
			Port       uint32
			OriginHost string
			OriginPort uint32
		}
		if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
			_ = newChannel.Reject(ssh.ConnectionFailed, "载荷解析失败")
			continue
		}

		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(channelRequests)
		go forwardTestSSHChannel(channel, net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port))))
	}
}

func forwardTestSSHChannel(channel ssh.Channel, addr string) {
	defer channel.Close()

	target, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return
	}
	defer target.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(channel, target)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(target, channel)
		done <- struct{}{}
	}()
	<-done
}

// newTunnelTestServer 把一台进程内 SSH 服务端登记成面板里的一台主机，
// 并把 Server 装配到"真的能连上它"的程度。
//
// 凭据按面板真实的方式走一遍：主密钥从文件加载、口令加密入库、取用时解密。
// known_hosts 用临时文件，因此首连的 TOFU 落盘也在被测范围内。
func newTunnelTestServer(t *testing.T) (*Server, int64) {
	t.Helper()
	return newTunnelTestServerWith(t, true)
}

// newTunnelTestServerWith 同上，但可以要求那台 SSH 服务端拒绝转发。
func newTunnelTestServerWith(t *testing.T, allowForwarding bool) (*Server, int64) {
	t.Helper()

	sshHost, sshPortText, err := net.SplitHostPort(startTestSSHServer(t, allowForwarding))
	if err != nil {
		t.Fatalf("解析测试 SSH 地址失败：%v", err)
	}
	sshPort, err := strconv.Atoi(sshPortText)
	if err != nil {
		t.Fatalf("解析测试 SSH 端口失败：%v", err)
	}

	box, err := secretbox.Load(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatalf("加载主密钥失败：%v", err)
	}
	secret, err := box.Encrypt(testSSHPassword)
	if err != nil {
		t.Fatalf("加密测试口令失败：%v", err)
	}

	st := newTestStore(t)
	hostID, err := st.CreateHost(store.HostInput{
		Name:      "测试机",
		Addr:      sshHost,
		Port:      sshPort,
		Username:  testSSHUser,
		AuthType:  sshclient.AuthPassword,
		SecretEnc: secret,
	})
	if err != nil {
		t.Fatalf("写入测试主机失败：%v", err)
	}

	s := &Server{
		cfg:          &config.Config{DevMode: true},
		store:        st,
		box:          box,
		sessions:     newTestSessionManager(t),
		knownHosts:   filepath.Join(t.TempDir(), "known_hosts"),
		mirrorTokens: newMirrorTokens(),
	}
	s.tunnels = newHostTunnels(s)
	s.socks = newHostSocks(s)
	t.Cleanup(func() {
		s.socks.closeAll()
		s.tunnels.closeAll()
	})

	return s, hostID
}

// issueSession 借会话签发逻辑拿一份 Cookie，不必真跑登录接口。
func issueSession(t *testing.T, s *Server, req *http.Request) {
	t.Helper()

	issued := httptest.NewRecorder()
	s.sessions.Issue(issued, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil), 1)
	for _, cookie := range issued.Result().Cookies() {
		req.AddCookie(cookie)
	}
}

// SOCKS5 → 常驻 SSH 通道（direct-tcpip）→ 目标站。
//
// 这里覆盖前面那条用例换掉的那一段：那里的拨号是直连，这里走真实 SSH 通道，
// 因此 hostTunnels 的取用、known_hosts 的 TOFU 落盘、ssh.Client 的
// direct-tcpip 转发都在被测范围内。
func TestSocksProxyOverSSHTunnel(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello through tunnel")
	}))
	defer target.Close()

	s, hostID := newTunnelTestServer(t)

	addr, err := s.socks.start(hostID)
	if err != nil {
		t.Fatalf("启动代理失败：%v", err)
	}
	if view := s.socks.view(hostID); !view.Running || view.Addr != addr {
		t.Fatalf("代理状态 = %+v，期望运行中且地址为 %s", view, addr)
	}

	if body := fetchThroughSocks(t, addr, target.Listener.Addr().String()); body != "hello through tunnel" {
		t.Fatalf("经 SSH 通道取回的内容 = %q，期望 hello through tunnel", body)
	}

	// 通道应当被复用而不是每条连接重拨：第二次仍然要通。
	if body := fetchThroughSocks(t, addr, target.Listener.Addr().String()); body != "hello through tunnel" {
		t.Fatalf("第二次经代理取回的内容 = %q", body)
	}

	// 首连必须把指纹落盘：否则"指纹变了就报错"这条保护根本无从谈起。
	known, err := os.ReadFile(s.knownHosts)
	if err != nil || len(bytes.TrimSpace(known)) == 0 {
		t.Errorf("首次连接后 known_hosts 里没有记录：%v", err)
	}
}

// 内嵌浏览的取页面同样要走隧道：目标机不是宿主机时，browserDialer 选的
// 就是 SSH 通道 —— 这条路径与宿主机直连那条是两套代码。
func TestBrowserProxyOverSSHTunnel(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><head></head><body><a href="/next">下一页</a></body></html>`)
	}))
	defer origin.Close()

	s, hostID := newTunnelTestServer(t)

	authority := strings.TrimPrefix(origin.URL, "http://")
	token, _, err := s.mirrorTokens.issue(hostID)
	if err != nil {
		t.Fatalf("签发镜像令牌失败：%v", err)
	}
	mirrored := browserPrefixFor(hostID) + "/" + token + "/http/" + authority

	req := httptest.NewRequest(http.MethodGet, mirrored+"/index.html", nil)
	issueSession(t, s, req)

	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `href="`+mirrored+`/next"`) {
		t.Errorf("经隧道取回的页面没有被改写：%s", body)
	}
}

// 对方 sshd 关了转发时，用户不该只看到一句 SSH 内部错误：那背后是一个具体、
// 可修的环境约束，提示里必须写清改什么、以及"本机服务可以走宿主机"这条捷径。
//
// 走的是真实路由，因此从应答码、错误翻译到提示页全都是备测的。
func TestBrowserProxyExplainsProhibitedForwarding(t *testing.T) {
	s, hostID := newTunnelTestServerWith(t, false)

	authority := "127.0.0.1:5666"
	token, _, err := s.mirrorTokens.issue(hostID)
	if err != nil {
		t.Fatalf("签发镜像令牌失败：%v", err)
	}
	mirrored := browserPrefixFor(hostID) + "/" + token + "/http/" + authority

	req := httptest.NewRequest(http.MethodGet, mirrored+"/", nil)
	issueSession(t, s, req)

	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502：%s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	// 原始 SSH 错误要不要保留都行，但"改什么"必须写到。
	for _, want := range []string{"AllowTcpForwarding", "宿主机", authority} {
		if !strings.Contains(body, want) {
			t.Errorf("提示里缺少 %q：%s", want, body)
		}
	}
}
