package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"panelssh/internal/config"
)

func TestBrowserTarget(t *testing.T) {
	cases := []struct {
		name    string
		rest    string
		query   string
		want    string
		wantErr bool
	}{
		{name: "带端口与路径", rest: "/http/10.0.0.5:8080/admin/index.html", want: "http://10.0.0.5:8080/admin/index.html"},
		{name: "https", rest: "/https/nas.local/", want: "https://nas.local/"},
		{name: "根路径", rest: "/http/10.0.0.5", want: "http://10.0.0.5/"},
		{name: "查询串保留", rest: "/http/10.0.0.5/a", query: "x=1&y=2", want: "http://10.0.0.5/a?x=1&y=2"},
		{name: "只有协议", rest: "/http", wantErr: true},
		{name: "不支持的协议", rest: "/ftp/10.0.0.5/x", wantErr: true},
		{name: "缺主机", rest: "/http/", wantErr: true},
		{name: "主机里混了查询串", rest: "/http/10.0.0.5?a=1/x", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := browserTarget(tc.rest, tc.query)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应当报错，实际得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			if got.String() != tc.want {
				t.Errorf("解析结果 = %q，期望 %q", got.String(), tc.want)
			}
		})
	}
}

func TestMirrorBrowserURL(t *testing.T) {
	base := &url.URL{Scheme: "http", Host: "10.0.0.5:8080", Path: "/admin/index.html"}
	const prefix = "/browser/3"
	const mirrored = "/browser/3/http/10.0.0.5:8080"

	cases := []struct {
		name string
		raw  string
		want string
	}{
		// 相对链接靠"文档地址就是目标地址的镜像"自动正确，不必改写 ——
		// 这也是目标地址编进路径而不是放进查询参数的唯一理由。
		{name: "相对路径", raw: "sub/detail.html", want: mirrored + "/admin/sub/detail.html"},
		{name: "上级路径", raw: "../up.html", want: mirrored + "/up.html"},
		{name: "根相对", raw: "/admin/list", want: mirrored + "/admin/list"},
		{name: "绝对同源", raw: "http://10.0.0.5:8080/x?y=1#z", want: mirrored + "/x?y=1#z"},
		{name: "绝对他源", raw: "https://other.example/logo.png", want: prefix + "/https/other.example/logo.png"},
		{name: "协议相对", raw: "//other.example/a", want: prefix + "/http/other.example/a"},
		{name: "已经是镜像", raw: mirrored + "/x", want: mirrored + "/x"},
		{name: "锚点", raw: "#top", want: "#top"},
		{name: "脚本伪协议", raw: "javascript:void(0)", want: "javascript:void(0)"},
		{name: "内联数据", raw: "data:image/png;base64,AAA", want: "data:image/png;base64,AAA"},
		{name: "邮件", raw: "mailto:a@b", want: "mailto:a@b"},
		{name: "空值", raw: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mirrorBrowserURL(tc.raw, base, prefix); got != tc.want {
				t.Errorf("mirrorBrowserURL(%q) = %q，期望 %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestRewriteBrowserAttrs(t *testing.T) {
	base := &url.URL{Scheme: "http", Host: "10.0.0.5:8080", Path: "/admin/index.html"}
	const prefix = "/browser/3"
	const mirrored = "/browser/3/http/10.0.0.5:8080"

	page := `<a href="/admin/list">a</a>
<img src="logo.png">
<form method="post" action="/login">
<object data="/plug.swf"></object>
<img srcset="/a.png 1x, /b.png 2x">
<div style="background:url('/bg.png')"></div>
<link rel="stylesheet" href="/a.css">`

	got := rewriteBrowserAttrs(page, base, prefix)

	for _, want := range []string{
		`href="` + mirrored + `/admin/list"`,
		`src="` + mirrored + `/admin/logo.png"`,
		`action="` + mirrored + `/login"`,
		`data="` + mirrored + `/plug.swf"`,
		`srcset="` + mirrored + `/a.png 1x, ` + mirrored + `/b.png 2x"`,
		`href="` + mirrored + `/a.css"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("改写结果缺少 %q\n---\n%s", want, got)
		}
	}
	// 注意：style="" 里的 url() 由 rewriteBrowserCSSURLs 负责，不在这一步。
	if !strings.Contains(got, `url('/bg.png')`) {
		t.Error("属性改写不应动内联样式里的 url()")
	}
}

func TestRewriteBrowserCSSURLs(t *testing.T) {
	base := &url.URL{Scheme: "http", Host: "10.0.0.5:8080", Path: "/admin/index.html"}
	const mirrored = "/browser/3/http/10.0.0.5:8080"

	got := rewriteBrowserCSSURLs(`@import "extra.css"; body{background:url(../img/a.png)} i{background:url("/b.png")}`, base, "/browser/3")

	for _, want := range []string{
		`@import "` + mirrored + `/admin/extra.css"`,
		`url(` + mirrored + `/img/a.png)`,
		`url("` + mirrored + `/b.png")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("改写结果缺少 %q\n---\n%s", want, got)
		}
	}
	// 引号形态要原样保留：漏掉引号会让 url 里含空格或括号的路径直接坏掉。
	if !strings.Contains(got, `url("`) {
		t.Error("双引号形态没有被保留")
	}
}

// meta refresh 的目标地址是纯文本，不长得像属性，必须单独覆盖。
func TestRewriteBrowserMetaRefresh(t *testing.T) {
	base := &url.URL{Scheme: "http", Host: "10.0.0.5:8080", Path: "/index.html"}
	const mirrored = "/browser/3/http/10.0.0.5:8080"

	got := rewriteBrowserMetaRefresh(
		`<meta http-equiv="refresh" content="0;url=/home"><meta charset="utf-8">`,
		base, "/browser/3")

	if !strings.Contains(got, `content="0;url=`+mirrored+`/home"`) {
		t.Errorf("meta refresh 没有被改写：%s", got)
	}
	// 与跳转无关的 meta 不能被动到。
	if !strings.Contains(got, `<meta charset="utf-8">`) {
		t.Errorf("无关的 meta 被改动了：%s", got)
	}
}

// 目标站的登录态由面板保管，浏览器一个 Cookie 都不持有。
//
// 这不是优化而是必然：iframe 的 origin 是 opaque，浏览器只给它带 SameSite=None
// 的 Cookie，而目标站的会话 Cookie 几乎都是 SameSite=Lax —— 浏览器手里那份
// 存了也带不出去。所以"谁记住登录态"只能是面板。
func TestBrowserCookieJarCarriesTargetSession(t *testing.T) {
	sawCookie := false

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "ok", Path: "/"})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<html><head></head><body>已登录</body></html>")
		case "/api/me":
			if cookie, err := r.Cookie("sid"); err == nil && cookie.Value == "ok" {
				sawCookie = true
			}
		}
	}))
	defer origin.Close()

	authority := strings.TrimPrefix(origin.URL, "http://")
	prefix := browserPrefixFor(LocalHostID) + "/testtoken"
	jar := newMirrorJar()
	proxy := (&Server{}).newBrowserProxy(LocalHostID)

	// 每个请求都自成一个"浏览器请求"：不带任何 Cookie 头，模拟沙箱 iframe。
	do := func(path string) *httptest.ResponseRecorder {
		target := &url.URL{Scheme: "http", Host: authority, Path: path}
		req := httptest.NewRequest(http.MethodGet, prefix+"/http/"+authority+path, nil)
		req = req.WithContext(context.WithValue(req.Context(), browserContextKey{}, browserRequest{
			target: target,
			prefix: prefix,
			jar:    jar,
		}))
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		return rec
	}

	login := do("/login")
	if login.Code != http.StatusOK {
		t.Fatalf("登录页状态码 = %d", login.Code)
	}
	// 下发给浏览器等于把它们堆在面板这个域上，而沙箱里又带不出去 —— 必须收走。
	if got := login.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("目标站的 Cookie 不该下发到浏览器：%s", got)
	}

	// 第二次请求浏览器依然一个 Cookie 都没有，罐里那份必须补上 ——
	// 否则页内每个接口都是 401，单页应用就是白屏。
	do("/api/me")
	if !sawCookie {
		t.Error("后续请求没有带上目标站的会话 Cookie")
	}
}

// 拼请求头只能写 name=value：Set-Cookie 那套属性会被目标站当成 Cookie 名的一部分。
func TestCookiesToHeader(t *testing.T) {
	got := cookiesToHeader([]*http.Cookie{
		{Name: "sid", Value: "abc", Path: "/", HttpOnly: true},
		{Name: "theme", Value: "dark", Secure: true},
	})
	if got != "sid=abc; theme=dark" {
		t.Fatalf("请求头 = %q，期望 sid=abc; theme=dark", got)
	}
}

// 端到端：拿一个真实的页面，确认头被剥掉、页内地址被改写、shim 被注入。
//
// 用宿主机（LocalHostID）走直连，这样不必先有一座 SSH 目标机 —— 被测的是
// 镜像这一层，隧道那一段由 hostTunnels 负责。
func TestBrowserProxyMirrorsPage(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Set-Cookie", "sid=abc; Path=/")
		_, _ = io.WriteString(w, `<html><head><title>内网面板</title></head><body>`+
			`<a href="/admin/list">列表</a><img src="logo.png">`+
			`<img src="https://cdn.example/logo.png"></body></html>`)
	}))
	defer origin.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))
	if err != nil {
		t.Fatalf("解析测试服务地址失败：%v", err)
	}
	authority := net.JoinHostPort(host, port)
	// 前缀里那一段是本机的镜像令牌。这里直接调反代，令牌校验在入口那一层
	// （handleBrowserProxy），不在反代里。
	prefix := browserPrefixFor(LocalHostID) + "/testtoken"
	mirrored := prefix + "/http/" + authority

	target := &url.URL{Scheme: "http", Host: authority, Path: "/index.html"}
	req := httptest.NewRequest(http.MethodGet, mirrored+"/index.html", nil)
	req = req.WithContext(context.WithValue(req.Context(), browserContextKey{}, browserRequest{
		target: target,
		prefix: prefix,
	}))

	rec := httptest.NewRecorder()
	(&Server{}).newBrowserProxy(LocalHostID).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rec.Code, rec.Body.String())
	}

	// 这两类头会让浏览器拒绝把页面放进 iframe，而"能看一眼"就是这条功能的全部目的。
	for _, header := range []string{"Content-Security-Policy", "X-Frame-Options"} {
		if rec.Header().Get(header) != "" {
			t.Errorf("%s 没有被剥掉：%q", header, rec.Header().Get(header))
		}
	}

	// 目标站的 Cookie 由面板收进罐里，不下发到浏览器（沙箱里带不出去）。
	if cookies := rec.Header()["Set-Cookie"]; len(cookies) != 0 {
		t.Errorf("下发的 Cookie 应当被收走：%v", cookies)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`href="` + mirrored + `/admin/list"`,
		`src="` + mirrored + `/logo.png"`,
		`src="` + prefix + `/https/cdn.example/logo.png"`,
		// shim 必须在页面自己的脚本之前落地，否则页面会先抓到原生 fetch。
		"var M=`" + mirrored + "`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("镜像页缺少 %q\n---\n%s", want, body)
		}
	}

	if idx := strings.Index(body, "var M=`"); idx > strings.Index(body, "<title>") {
		t.Error("shim 没有注入到 <head> 里，会晚于页面脚本执行")
	}
}

// 令牌只解锁"经某台主机读网页"：换一台机器就不该认，过期之后也不该认。
//
// 这条边界是这套凭据的全部意义 —— 它替代会话 Cookie 的位置，必须比 Cookie
// 更窄：拿不到会话、也顺延不到别的机器上。
func TestMirrorTokenBindsHostAndExpires(t *testing.T) {
	tokens := newMirrorTokens()

	token, expiresAt, err := tokens.issue(7)
	if err != nil {
		t.Fatalf("签发令牌失败：%v", err)
	}
	if !expiresAt.After(time.Now()) {
		t.Error("刚签发的令牌应当是未过期的")
	}
	if _, ok := tokens.resolve(token, 7); !ok {
		t.Error("刚签发的令牌应当有效")
	}

	// 主机号是令牌的一部分：别的机器上的授权不能靠改一个数字换来。
	for _, other := range []int64{8, LocalHostID, 0} {
		if _, ok := tokens.resolve(token, other); ok {
			t.Errorf("令牌不应在主机 %d 上生效", other)
		}
	}
	if _, ok := tokens.resolve("", 7); ok {
		t.Error("空令牌必须无效")
	}
	if _, ok := tokens.resolve("凭空编的令牌", 7); ok {
		t.Error("未签发的令牌必须无效")
	}

	// 直接把记录改成过期来验证判定，不真等四个小时。
	tokens.mu.Lock()
	entry := tokens.entries[token]
	entry.expiresAt = time.Now().Add(-time.Second)
	tokens.entries[token] = entry
	tokens.mu.Unlock()

	if _, ok := tokens.resolve(token, 7); ok {
		t.Error("过期令牌必须无效")
	}
}

// 签发时顺手清掉过期的：令牌按窗口产生，不清理会随开关窗口越积越多。
func TestMirrorTokensPurgeExpiredOnIssue(t *testing.T) {
	tokens := newMirrorTokens()

	stale, _, err := tokens.issue(1)
	if err != nil {
		t.Fatalf("签发令牌失败：%v", err)
	}
	tokens.mu.Lock()
	entry := tokens.entries[stale]
	entry.expiresAt = time.Now().Add(-time.Minute)
	tokens.entries[stale] = entry
	tokens.mu.Unlock()

	if _, _, err := tokens.issue(1); err != nil {
		t.Fatalf("再次签发失败：%v", err)
	}
	if _, ok := tokens.entries[stale]; ok {
		t.Error("签发新令牌时应当顺手清掉已过期的条目")
	}
}

// 这条翻译很要紧：用户看到的原文是一句 SSH 内部错误，而它背后是一个具体且可修的
// 环境约束（对方 sshd 不允许转发）。认的是协议里的应答码而不是错误文本 ——
// 文本随后端实现而变，码是定死的。
func TestBrowserErrorDetailExplainsProhibitedForwarding(t *testing.T) {
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:5666", Path: "/"}

	// 这正是 sshd 拒绝打开转发通道时，x/crypto 交到我们手里的那个错误。
	prohibited := &ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "open failed"}
	if !isForwardingProhibited(prohibited) {
		t.Fatal("应当认出这是转发被拒")
	}
	// 实际拿到它时多半被包了一层，因此必须能穿透包装识别。
	if !isForwardingProhibited(fmt.Errorf("取页失败：%w", prohibited)) {
		t.Error("包装之后仍应认出转发被拒")
	}

	detail := browserErrorDetail(target, prohibited)
	for _, want := range []string{"AllowTcpForwarding", "宿主机", "SOCKS"} {
		if !strings.Contains(detail, want) {
			t.Errorf("提示里缺少 %q，用户就不知道该改什么：%s", want, detail)
		}
	}

	// 别的失败原因不能被当成转发被拒，否则会把用户引向错误的方向。
	refused := &ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "connect failed: Connection refused"}
	if isForwardingProhibited(refused) {
		t.Error("连接被拒不该被认成转发被拒")
	}
	if isForwardingProhibited(context.DeadlineExceeded) {
		t.Error("超时不该被认成转发被拒")
	}
	if got := browserErrorDetail(target, context.DeadlineExceeded); !strings.Contains(got, "没有在") {
		t.Errorf("超时应当给超时提示，实际：%s", got)
	}
}

// 走真实路由表跑一遍：路径注册、鉴权、以及"前缀剥掉之后目标地址解析对不对"。
//
// 只调处理器是测不出路径写错这类问题的（与 privilege_test.go 同一个理由），
// 而这条功能的地址全在路径里，路径写错就等于功能不可用。
func TestBrowserRouteServesMirroredPage(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<html><head></head><body><a href="/admin">管理</a></body></html>`)
	}))
	defer origin.Close()

	authority := strings.TrimPrefix(origin.URL, "http://")

	s := &Server{
		cfg:          &config.Config{DevMode: true},
		sessions:     newTestSessionManager(t),
		mirrorTokens: newMirrorTokens(),
	}

	// 借会话签发逻辑拿一份 Cookie，不必真跑登录接口。
	issued := httptest.NewRecorder()
	s.sessions.Issue(issued, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil), 1)

	// 镜像凭据由接口签发（它才要求会话身份），镜像路径本身只认这段令牌。
	token, _, err := s.mirrorTokens.issue(LocalHostID)
	if err != nil {
		t.Fatalf("签发镜像令牌失败：%v", err)
	}
	mirrored := browserPrefixFor(LocalHostID) + "/" + token + "/http/" + authority

	req := httptest.NewRequest(http.MethodGet, mirrored+"/index.html", nil)
	for _, cookie := range issued.Result().Cookies() {
		req.AddCookie(cookie)
	}

	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `href="`+mirrored+`/admin"`) {
		t.Errorf("页内地址没有被改写到本机镜像下：%s", body)
	}

	// 没有令牌一律拒绝：镜像路径不再讲会话，拿不到令牌（没登录就签不出来）就走不进来。
	anon := httptest.NewRequest(http.MethodGet,
		browserPrefixFor(LocalHostID)+"/forged/http/"+authority+"/index.html", nil)
	anonRec := httptest.NewRecorder()
	s.Router().ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Errorf("伪造令牌访问镜像的状态码 = %d，期望 401", anonRec.Code)
	}
}
