package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"panelssh/internal/secretbox"
)

// browserPrefix 是内嵌浏览在面板上的挂载前缀。
//
// 每台主机一个镜像，因此完整前缀还要带上主机号：见 browserPrefixFor。
const browserPrefix = "/browser"

// browserPrefixFor 给出某台主机上网页镜像的反代前缀。
//
// 与 DSH 工作台同理（见 dshPrefixFor）：子路径而不是独立端口，因为面板通常被放在
// 带 TLS 的反向代理后面，HTTPS 页面里的 HTTP 子资源会被浏览器当作混合内容拦掉。
func browserPrefixFor(hostID int64) string {
	return fmt.Sprintf("%s/%d", browserPrefix, hostID)
}

// maxBrowserTextBody 是允许改写的文本正文体积上限。
//
// 改写靠字符串替换，必须把完整正文拿在手里；超过这个体量就不再尝试 ——
// 宁可如实报错，也不能把截断后的半截页面发出去。
const maxBrowserTextBody = 8 << 20

// errBrowserLocalHost 表示对宿主机请求网页镜像。
//
// 与 SOCKS 代理那条路不同，这里宿主机的确有意义：取网页的是面板自己，
// 因此哪怕浏览器在另一台机器上，也能借面板本机的网络（VPN、专线）去访问。
// 直连即可，不需要 SSH 通道。
func (s *Server) browserDialer(hostID int64) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if hostID == LocalHostID {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return s.tunnels.dialContext(ctx, hostID, network, addr)
	}
}

// browserProxy 取某台主机的镜像反代处理器，构造一次后复用。
//
// 与 DSH 一样按主机缓存：一个页面会带出几十个静态资源请求，逐次新建
// ReverseProxy 是纯浪费。缓存是安全的 —— Rewrite 每次都重新解析目标地址，
// 因此同一台主机上换目标站不会继续被转发到上一个。
func (s *Server) browserProxy(hostID int64) http.Handler {
	if cached, ok := s.browserProxies.Load(hostID); ok {
		return cached.(http.Handler)
	}
	handler := s.newBrowserProxy(hostID)
	s.browserProxies.Store(hostID, handler)
	return handler
}

type browserContextKey struct{}

// browserRequest 是一次镜像请求的解析结果。
//
// prefix 里带着本次的令牌：相对链接由浏览器按文档地址解析、会自然带上它，
// 而根相对与绝对地址由我们改写，也必须把令牌补回去 —— 少一处，页内一跳就掉线。
type browserRequest struct {
	target *url.URL
	prefix string
	// jar 是本次令牌对应的目标站 Cookie 罐，见 mirrorTokens。
	jar *cookiejar.Jar
}

// browserRequestOf 取出本次请求的目标地址与镜像前缀。
func browserRequestOf(ctx context.Context) (browserRequest, bool) {
	request, ok := ctx.Value(browserContextKey{}).(browserRequest)
	return request, ok
}

func (s *Server) newBrowserProxy(hostID int64) http.Handler {
	dialer := s.browserDialer(hostID)

	transport := &http.Transport{
		// 拨号单独限时：目标站可能是个没人在听的地址（内网里扫到的主机名、
		// 已下线的服务），不设上限的话这个请求会一直挂到浏览器自己放弃。
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCtx, cancel := context.WithTimeout(ctx, browserDialTimeout)
			defer cancel()
			return dialer(dialCtx, network, addr)
		},
		// 隧道一旦断开，待在池里的旧连接不会自己消失；给一个短的空闲超时，
		// 让失效的连接尽快被换掉，而不是被反复复用后每次都失败。
		IdleConnTimeout: 30 * time.Second,
	}

	return &httputil.ReverseProxy{
		// 流式响应必须即时下发，否则页面里的长轮询会一直转圈。
		FlushInterval: -1,
		Transport:     transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			request, ok := browserRequestOf(pr.In.Context())
			if !ok {
				return
			}
			target := request.target

			out := pr.Out
			out.URL.Scheme = target.Scheme
			out.URL.Host = target.Host
			out.URL.Path = target.Path
			out.URL.RawPath = ""
			out.URL.RawQuery = target.RawQuery
			out.Host = target.Host

			// 目标站看到的是"从面板这边过来的请求"。把 Origin/Referer 改写回它自己的
			// 地址，既如实反映了请求的真实来源（面板代它发起的），也满足那些会校验
			// 这两个头的内网应用。Referer 里带的是镜像地址，只取路径部分。
			origin := target.Scheme + "://" + target.Host
			out.Header.Set("Origin", origin)
			if referer := pr.In.Header.Get("Referer"); referer != "" {
				if parsed, err := url.Parse(referer); err == nil {
					out.Header.Set("Referer", origin+parsed.Path)
				}
			}

			// 目标站的登录态由面板保管（见 mirrorTokens 的 jar）。浏览器手里
			// 一个 Cookie 都没有，因此这里既不会带上面板自己的会话 Cookie，
			// 也只可能由罐里补出目标站要的那些。
			if request.jar != nil {
				if cookies := request.jar.Cookies(target); len(cookies) > 0 {
					out.Header.Set("Cookie", cookiesToHeader(cookies))
				}
			}

			// 要求明文正文：改写靠字符串替换，压缩后的字节改不了。
			// 代价是文本资源不再压缩，内网页面的体量下可以接受 ——
			// 换来的是不必在这里做一遍解压、再在响应侧做一遍重压缩。
			out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: func(resp *http.Response) error {
			header := resp.Header
			request, _ := browserRequestOf(resp.Request.Context())
			prefix := request.prefix

			// CSP 与 X-Frame-Options 会让浏览器直接拒绝把页面放进 iframe，
			// 而"能看一眼"正是这条功能的全部目的。HSTS 则会把面板自己的域
			// 也拖进强制 https —— 面板很可能只跑明文 http。
			header.Del("Content-Security-Policy")
			header.Del("Content-Security-Policy-Report-Only")
			header.Del("X-Frame-Options")
			header.Del("Strict-Transport-Security")

			target := resp.Request.URL
			if location := header.Get("Location"); location != "" {
				header.Set("Location", mirrorBrowserURL(location, target, prefix))
			}

			// 目标站的 Cookie 收进罐里，一个都不下发给浏览器。
			//
			// 下发是错的：沙箱 iframe 根本带不出去它们，留下只会在面板这个域上
			// 堆一批用不到、还会与面板自己的 Cookie 混在一起的条目。
			if values := resp.Cookies(); len(values) > 0 {
				if request.jar != nil {
					request.jar.SetCookies(target, values)
				}
				header.Del("Set-Cookie")
			}

			if browserVerbose() {
				// 排查"某个内网页面白屏"时，这是唯一能看见"页面到底请求了什么、
				// 目标站回了什么"的窗口 —— 浏览器那边的 Network 面板用户未必方便给。
				log.Printf("[browser] 镜像 %s → %d", target, resp.StatusCode)
			}

			// 压缩过的正文改不了。请求侧已经要了 identity，正常不会走到这里；
			// 真遇到不守规矩的服务端时，宁可不改写（页面照常显示，只是页内链接
			// 会指回真实地址）也不能把坏数据发出去。
			if header.Get("Content-Encoding") != "" {
				return nil
			}

			contentType := header.Get("Content-Type")
			switch {
			case strings.Contains(contentType, "text/html"):
				return rewriteBrowserHTML(resp, target, prefix)
			case strings.Contains(contentType, "text/css"):
				return rewriteBrowserCSS(resp, target, prefix)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			request, _ := browserRequestOf(r.Context())
			// 不能拿 r.URL.Path 记日志：这里拿到的是已经改写过的出站请求，
			// 它的 Path 是目标站上的路径（多半就是 "/"），记下来等于什么都没记。
			log.Printf("[browser] 镜像取页失败：主机 %d 目标 %s —— %v",
				hostID, browserTargetLabel(request.target), err)
			renderBrowserNotice(w, http.StatusBadGateway, "打不开这个地址",
				browserErrorDetail(request.target, err))
		},
	}
}

// browserErrorDetail 把失败原因转成一句用户能判断、并且知道下一步做什么的话。
//
// SSH 的原文（"ssh: rejected: administratively prohibited"）对用户没有意义，
// 而它背后是一个具体且可修的环境约束，因此单独翻译。其余错误保留原文：
// 那些多是"这台机器连不上那个地址"，原文里的地址与原因已经足够定位。
func browserErrorDetail(target *url.URL, err error) string {
	where := ""
	if target != nil {
		where = target.String() + "："
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return where + "目标站没有在 " + browserDialTimeout.String() + " 内响应。"
	case isForwardingProhibited(err):
		return where + "目标机的 SSH 服务拒绝了这条转发（sshd 的 AllowTcpForwarding / PermitOpen 限制）。" +
			"在这台机器上，SOCKS 代理与 DSH 工作台同样用不了 —— 它们走的是同一条通道。" +
			"要开通，请在目标机的 /etc/ssh/sshd_config 里设 AllowTcpForwarding yes" +
			"（并确认 PermitOpen 没有挡住该地址），然后重启 sshd。" +
			"如果这个地址其实是面板本机上的服务，把「目标主机」切成「宿主机」即可：那是直连，不经过 sshd。"
	}
	return where + err.Error()
}

// isForwardingProhibited 判断这是不是"目标机的 sshd 按策略拒绝了转发"。
//
// 认的是 SSH 通道打开失败的应答码，不是错误文本：文本随后端实现而变，
// 而码是协议里定死的。Prohibited 正是 OpenSSH 回 "administratively prohibited" 时用的那个。
func isForwardingProhibited(err error) bool {
	var openErr *ssh.OpenChannelError
	return errors.As(err, &openErr) && openErr.Reason == ssh.Prohibited
}

// browserTargetLabel 给出日志里用的目标地址。
func browserTargetLabel(target *url.URL) string {
	if target == nil {
		return "(地址未知)"
	}
	return target.String()
}

// browserDialTimeout 是从目标机拨向目标站的等待上限。
const browserDialTimeout = 20 * time.Second

// mirrorTokenTTL 是镜像令牌的有效期。
//
// 取一个工作日的量级：令牌只在"窗口开着、正在看某个内网页面"这段时间有用。
// 太短会在浏览中途失效（iframe 里看到的是一句提示，而不是页面）；
// 太长又放大了它被翻出来的代价。
const mirrorTokenTTL = 4 * time.Hour

// mirrorTokens 持有内嵌浏览用的镜像令牌。
//
// 为什么不能直接用会话：内嵌浏览的 iframe 是沙箱化的（不带 allow-same-origin），
// 它的 origin 被序列化成 null，浏览器由此把它的所有请求判定为跨站，
// 而跨站请求只带 SameSite=None 的 Cookie —— 面板的会话 Cookie 是 SameSite=Lax，
// 于是一个也带不上，每个请求都会撞 401。沙箱与"用会话鉴权"在浏览器里不能同时成立。
//
// 两条出路：去掉沙箱（被代理页面就与面板同源，能拿用户的会话去操作面板），
// 或者换一种不依赖 Cookie 的凭据。这里选后者，并把令牌的权限压到最小：
// 它只解锁"经某台主机读网页"，不含任何面板操作能力 —— 页面即使被恶意脚本控制，
// 也拿不到会话本身，也跨源碰不到面板的接口。
type mirrorTokens struct {
	mu      sync.Mutex
	entries map[string]mirrorTokenEntry
}

type mirrorTokenEntry struct {
	hostID    int64
	expiresAt time.Time
	// jar 保管目标站下发的 Cookie。
	//
	// 这是沙箱方案的必然要求：iframe 的 origin 是 opaque，浏览器只会给它带
	// SameSite=None 的 Cookie，而目标站的会话 Cookie 几乎都是 SameSite=Lax ——
	// 一个都带不过去。所以"谁记住登录态"只能由面板来做：浏览器手里一个 Cookie
	// 都没有，目标站的 Set-Cookie 由面板收进这里，后续请求再由面板补出去。
	jar *cookiejar.Jar
}

func newMirrorTokens() *mirrorTokens {
	return &mirrorTokens{entries: map[string]mirrorTokenEntry{}}
}

// newMirrorJar 造一个只服务镜像的 Cookie 罐。
//
// 不传公共后缀表：镜像面对的是内网地址与内网域名，公共后缀表对这些名字无从判断；
// 而 cookiejar.New 传 nil 不会失败（报错只可能来自一份损坏的后缀表）。
func newMirrorJar() *cookiejar.Jar {
	jar, _ := cookiejar.New(nil)
	return jar
}

// issue 签发一枚绑定到主机的令牌。
//
// 顺手清掉已过期的：令牌按窗口产生，数量很小，不值得再养一个回收协程。
func (m *mirrorTokens) issue(hostID int64) (string, time.Time, error) {
	token, err := secretbox.RandomToken(24)
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().Add(mirrorTokenTTL)

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for key, entry := range m.entries {
		if entry.expiresAt.Before(now) {
			delete(m.entries, key)
		}
	}
	m.entries[token] = mirrorTokenEntry{hostID: hostID, expiresAt: expiresAt, jar: newMirrorJar()}

	return token, expiresAt, nil
}

// resolve 校验令牌，并交出它携带的 Cookie 罐。
//
// 校验与取用合成一步：调用方要的从来不只是"这枚令牌有效吗"，而是
// "用它去取页时要带上哪份登录态"。必须同时核对主机号 —— 令牌是"经哪台机器
// 读网页"的凭据，若只看令牌本身，一台机器的授权就等于顺延到所有主机上。
func (m *mirrorTokens) resolve(token string, hostID int64) (*cookiejar.Jar, bool) {
	if token == "" {
		return nil, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[token]
	if !ok || entry.hostID != hostID || entry.expiresAt.Before(time.Now()) {
		return nil, false
	}
	return entry.jar, true
}

// browserMirrorView 是给前端的镜像凭据。
type browserMirrorView struct {
	Token string `json:"token"`
	// ExpiresAt 是毫秒时间戳，前端据此在到期前换一枚新的。
	ExpiresAt int64 `json:"expiresAt"`
}

// handleBrowserMirrorIssue 为某台主机签发一枚镜像令牌。
//
// 走 POST 而不是 GET：它在签发凭据，不该被预取或缓存。
func (s *Server) handleBrowserMirrorIssue(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	token, expiresAt, err := s.mirrorTokens.issue(hostID)
	if err != nil {
		log.Printf("[browser] 签发镜像凭据失败：主机 %d —— %v", hostID, err)
		writeErr(w, http.StatusInternalServerError, "签发镜像凭据失败")
		return
	}
	writeJSON(w, http.StatusOK, browserMirrorView{Token: token, ExpiresAt: expiresAt.UnixMilli()})
}

// handleBrowserProxy 是 /browser/{id}/{token}/... 的入口：验凭据、解析目标地址，
// 然后交给反代。
//
// 目标地址编在路径里，形如 /browser/{id}/{token}/http/10.0.0.5:8080/admin/index.html。
// 为什么不用查询参数（?url=...）：相对链接由浏览器按文档地址解析，
// 只有文档地址本身就是目标地址的镜像，页内的 "detail.html"、"../c.html"
// 才会自动落到正确的镜像路径上。用查询参数就得改写每一条相对链接。
// 根相对链接（/x/y）拿不到主机，仍然必须改写 —— 见 rewriteBrowserHTML。
//
// 凭据是路径里的那段令牌，不是会话 Cookie：见 mirrorTokens 的说明 ——
// iframe 是沙箱化的，浏览器根本不给它带会话 Cookie。
func (s *Server) handleBrowserProxy(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	prefix := browserPrefixFor(hostID)
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	token, tail, ok := strings.Cut(rest, "/")
	if !ok || token == "" {
		logBrowserFailure(r, "地址里没有凭据")
		renderBrowserNotice(w, http.StatusBadRequest, "镜像地址不完整",
			"镜像地址形如 /browser/主机号/凭据/http/内网地址/路径，请从面板的「浏览器」窗口里打开。")
		return
	}
	jar, ok := s.mirrorTokens.resolve(token, hostID)
	if !ok {
		// 过期是这里最常见的一种：窗口开得太久，令牌到期了。
		// 提示必须给出恢复动作，否则用户只能看到一个打不开的页面。
		// 日志里只留凭据的前几位：旧前端（不带凭据）发过来的那种一眼能认出来，
		// 而万一是"别的主机的凭据"，也不至于把一枚还能用的凭据整段写进日志。
		logBrowserFailure(r, "凭据无效或已过期，凭据开头为 "+browserTokenHint(token))
		renderBrowserNotice(w, http.StatusUnauthorized, "镜像凭据已失效",
			"凭据与这个窗口绑定，面板重启后失效，到期也会失效。"+
				"请关掉本窗口重新打开（或在面板页面上强制刷新 Ctrl+Shift+R）再试 —— "+
				"按窗口里的「刷新」没用，它手里还是那枚已经作废的凭据。")
		return
	}

	target, err := browserTarget("/"+tail, r.URL.RawQuery)
	if err != nil {
		logBrowserFailure(r, "地址解析失败："+err.Error())
		renderBrowserNotice(w, http.StatusBadRequest, "地址不完整", err.Error())
		return
	}

	ctx := context.WithValue(r.Context(), browserContextKey{}, browserRequest{
		target: target,
		prefix: prefix + "/" + token,
		jar:    jar,
	})
	s.browserProxy(hostID).ServeHTTP(w, r.WithContext(ctx))
}

// browserTokenHint 取凭据的前几位用于日志辨认。
func browserTokenHint(token string) string {
	if len(token) > 6 {
		return token[:6] + "…"
	}
	return token
}

// logBrowserFailure 记下镜像请求失败的原因。
//
// 镜像这条路的失败发生在 iframe 里，用户只会看到一句提示，而面板这边原本一句
// 日志都没有 —— "打不开"就只能靠猜。只记失败：成功不记，因为一个页面会带出
// 几十个子资源请求，记下来全是噪音；失败的量很小，却能在事后一眼看出卡在哪一步。
func logBrowserFailure(r *http.Request, reason string) {
	log.Printf("[browser] 镜像请求失败：%s %s —— %s", r.Method, r.URL.Path, reason)
}

// handleBrowserRootRedirect 把不带尾斜杠的镜像入口补成尾斜杠形式。
func (s *Server) handleBrowserRootRedirect(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, browserPrefixFor(hostID)+"/", http.StatusTemporaryRedirect)
}

// browserTarget 从镜像路径里解析出要访问的地址。
//
// 路径形态：/{scheme}/{host[:port]}/{path}?{query}，例如
// /http/10.0.0.5:8080/admin/index.html。
func browserTarget(rest, rawQuery string) (*url.URL, error) {
	scheme, tail, ok := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	if !ok {
		return nil, errors.New("地址里缺少主机，形如 /browser/1/http/10.0.0.5:8080/")
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("只支持 http 与 https，收到 %q", scheme)
	}

	authority, path, _ := strings.Cut(tail, "/")
	if authority == "" {
		return nil, errors.New("地址里缺少主机")
	}
	// 主机里出现 / ? # 或空白时，拼出来的地址会指向别处，直接挡下。
	if strings.ContainsAny(authority, " \t\r\n?#") {
		return nil, fmt.Errorf("主机名非法：%q", authority)
	}

	target := &url.URL{Scheme: scheme, Host: authority, Path: "/" + path, RawQuery: rawQuery}
	return target, nil
}

// mirrorBrowserURL 把一个目标站地址改写成它的镜像地址。
//
// 已经是镜像地址的原样返回：页面上常见的 "/browser/1/http/..." 会出现在
// 我们注入的 shim 与改写过的属性里，再套一层就会变成镜像的镜像。
func mirrorBrowserURL(raw string, base *url.URL, prefix string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return raw
	}
	if strings.HasPrefix(trimmed, prefix+"/") || trimmed == prefix {
		return raw
	}

	lower := strings.ToLower(trimmed)
	// 这些不是网络地址：原样留着，改写了反而会坏。
	for _, scheme := range []string{"data:", "javascript:", "mailto:", "tel:", "blob:", "about:", "file:"} {
		if strings.HasPrefix(lower, scheme) {
			return raw
		}
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return raw
	}
	absolute := base.ResolveReference(parsed)
	if absolute.Scheme != "http" && absolute.Scheme != "https" {
		return raw
	}

	mirrored := prefix + "/" + absolute.Scheme + "/" + absolute.Host + absolute.EscapedPath()
	if absolute.RawQuery != "" {
		mirrored += "?" + absolute.RawQuery
	}
	if absolute.Fragment != "" {
		mirrored += "#" + absolute.Fragment
	}
	return mirrored
}

// browserAttrRe 抓出 HTML 里指向外部资源的属性。
//
// 只认带引号的值：不带引号的写法在真实页面里极少，而为了它放宽正则会把
// 正文里恰好长得像属性的文本一起改掉，那种错误比漏改几个链接难查得多。
var browserAttrRe = regexp.MustCompile(`(?i)\b(href|src|action|formaction|poster|data|background)\s*=\s*("([^"]*)"|'([^']*)')`)

// browserSrcsetRe 抓 srcset：一个属性里塞着多组"地址 + 描述符"。
var browserSrcsetRe = regexp.MustCompile(`(?i)\bsrcset\s*=\s*("([^"]*)"|'([^']*)')`)

// browserCSSURLRe 抓 CSS 里的 url(...)，同时覆盖 <style> 块与 style="" 内联样式。
//
// 不用反向引用去配成对的引号：Go 的正则（RE2）不支持反向引用。
// 因此这里括号内的内容整体抓下来，引号在改写函数里再剥 ——
// 代价是路径里出现未编码的右括号时会提前收尾，而这种写法在样式表里本就非法。
var browserCSSURLRe = regexp.MustCompile(`(?i)url\(\s*([^)]*?)\s*\)`)

// browserCSSImportRe 抓 CSS 的 @import "x.css"（url(...) 那种写法已被上面覆盖）。
var browserCSSImportRe = regexp.MustCompile(`(?i)@import\s+["'][^"']+["']`)

// rewriteBrowserHTML 把页面里的地址改写成本地镜像。
func rewriteBrowserHTML(resp *http.Response, base *url.URL, prefix string) error {
	body, err := readBrowserBody(resp)
	if err != nil {
		return err
	}

	page := rewriteBrowserAttrs(string(body), base, prefix)
	// <style> 块与 style="" 都在这条规则里：两者的 url() 形态一致。
	page = rewriteBrowserCSSURLs(page, base, prefix)
	page = rewriteBrowserMetaRefresh(page, base, prefix)

	replaceBrowserBody(resp, injectBrowserShim(page, base, prefix))
	return nil
}

// rewriteBrowserCSS 改写样式表正文。
func rewriteBrowserCSS(resp *http.Response, base *url.URL, prefix string) error {
	body, err := readBrowserBody(resp)
	if err != nil {
		return err
	}
	replaceBrowserBody(resp, rewriteBrowserCSSURLs(string(body), base, prefix))
	return nil
}

// rewriteBrowserAttrs 改写 HTML 属性里的地址。
func rewriteBrowserAttrs(page string, base *url.URL, prefix string) string {
	page = browserAttrRe.ReplaceAllStringFunc(page, func(match string) string {
		eq := strings.IndexByte(match, '=')
		name := match[:eq]
		value, quote := match[eq+1:], match[eq+1]
		return name + "=" + string(quote) + mirrorBrowserURL(value[1:len(value)-1], base, prefix) + string(quote)
	})

	page = browserSrcsetRe.ReplaceAllStringFunc(page, func(match string) string {
		eq := strings.IndexByte(match, '=')
		name := match[:eq]
		value, quote := match[eq+1:], match[eq+1]
		inner := value[1 : len(value)-1]

		entries := strings.Split(inner, ",")
		for i, entry := range entries {
			fields := strings.Fields(entry)
			if len(fields) == 0 {
				continue
			}
			fields[0] = mirrorBrowserURL(fields[0], base, prefix)
			entries[i] = strings.Join(fields, " ")
		}
		return name + "=" + string(quote) + strings.Join(entries, ", ") + string(quote)
	})

	return page
}

// browserCSSURLs 改写 CSS 里引用到的资源地址。
func rewriteBrowserCSSURLs(text string, base *url.URL, prefix string) string {
	text = browserCSSURLRe.ReplaceAllStringFunc(text, func(match string) string {
		open := strings.IndexByte(match, '(')
		inner := strings.TrimSpace(match[open+1 : len(match)-1])

		// 引号形态原样保留：去掉了会让路径里的空格、括号把 url() 拆坏。
		quote := ""
		if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0] {
			quote = string(inner[0])
			inner = inner[1 : len(inner)-1]
		}
		return "url(" + quote + mirrorBrowserURL(inner, base, prefix) + quote + ")"
	})

	// @import url(x.css) 已被上面那条覆盖，这里只管 @import "x.css" 写法。
	return browserCSSImportRe.ReplaceAllStringFunc(text, func(match string) string {
		at := strings.IndexAny(match, `"'`)
		quote := match[at]
		inner := match[at+1 : len(match)-1]
		return "@import " + string(quote) + mirrorBrowserURL(inner, base, prefix) + string(quote)
	})
}

// rewriteBrowserMetaRefresh 改写 <meta http-equiv="refresh" content="0;url=/home">。
//
// 这种跳转的目标地址是纯文本，不长的像属性，因此上面那两条正则覆盖不到。
func rewriteBrowserMetaRefresh(page string, base *url.URL, prefix string) string {
	return browserMetaTagRe.ReplaceAllStringFunc(page, func(tag string) string {
		if indexFold(tag, "refresh") < 0 {
			return tag
		}
		at := indexFold(tag, "url=")
		if at < 0 {
			return tag
		}
		// url= 之后到引号、分号、空白或标签结尾之间就是目标地址。
		rest := tag[at+len("url="):]
		end := strings.IndexAny(rest, "\"';> \t")
		if end < 0 {
			end = len(rest)
		}
		return tag[:at+len("url=")] + mirrorBrowserURL(rest[:end], base, prefix) + rest[end:]
	})
}

// browserMetaTagRe 匹配单个 <meta ...> 标签。
var browserMetaTagRe = regexp.MustCompile(`(?i)<meta\s[^>]*>`)

// indexFold 是不区分大小写的 strings.Index（返回 -1 表示没有）。
func indexFold(haystack, needle string) int {
	return strings.Index(strings.ToLower(haystack), strings.ToLower(needle))
}

// readBrowserBody 完整读出正文，超限即报错（不截断）。
func readBrowserBody(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBrowserTextBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(body) > maxBrowserTextBody {
		return nil, fmt.Errorf("正文超过 %d MiB，超出可安全改写的范围", maxBrowserTextBody>>20)
	}
	return body, nil
}

func replaceBrowserBody(resp *http.Response, body string) {
	resp.Body = io.NopCloser(bytes.NewReader([]byte(body)))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	// 长度变了，原响应上的分块/压缩标记都必须清掉。
	resp.Header.Del("Transfer-Encoding")
	resp.Header.Del("Content-Encoding")
	resp.TransferEncoding = nil
}

// injectBrowserShim 在页面最前面注入一层地址修正。
//
// 为什么必须注入：页面的 JavaScript 会在运行时拼出地址（fetch('/api/x')、
// location.origin + '/login'、new WebSocket(...)），这些字符串不在 HTML 里，
// 静态替换抓不到。它们带着面板的域，直接发出去会打到面板自己的接口上。
// 覆盖 fetch / XHR / WebSocket / EventSource 四个出口即可。
//
// 注入位置必须在页面自己的 <script> 之前，否则库会先捕获到原生实现。
func injectBrowserShim(page string, base *url.URL, prefix string) string {
	mirrorBase := prefix + "/" + base.Scheme + "/" + base.Host
	shim := browserShim(prefix, base.Scheme+"://"+base.Host, mirrorBase)
	if idx := strings.Index(page, "<head>"); idx >= 0 {
		pos := idx + len("<head>")
		return page[:pos] + shim + page[pos:]
	}
	return shim + page
}

// browserShim 返回注入脚本。
//
// 三个值：P 是本主机的镜像前缀（/browser/1），M 是当前目标站的镜像基址
// （/browser/1/http/10.0.0.5:8080），T 是目标站自身的源（http://10.0.0.5:8080）。
// 页面里的地址要么是根相对的、要么是自己那个源开头的，两者都靠 M 还原成镜像地址。
//
// 它做两件事：
//   - 把页面发出的根相对/绝对地址改挂到本主机的镜像路径下；
//   - 把当前页面地址用 postMessage 报给外层窗口 —— 面板里的 iframe 是沙箱化的
//     （不与面板同源），外层读不到它的 location，地址栏因此只能靠页内回报同步。
func browserShim(prefix, targetOrigin, mirrorBase string) string {
	return `<script>(function(){
var P=` + "`" + prefix + "`" + `;
var M=` + "`" + mirrorBase + "`" + `;
var T=` + "`" + targetOrigin + "`" + `;
function mirror(raw){
  if(typeof raw!=="string"||raw==="")return raw;
  if(raw.charAt(0)==="#")return raw;
  if(raw.indexOf(M)===0)return raw;
  if(raw.charAt(0)==="/"&&raw.charAt(1)==="/"){
    var scheme=location.protocol.replace(":","");
    return P+"/"+scheme+raw.slice(1);
  }
  if(raw.charAt(0)==="/")return M+raw;
  var m=/^(https?):\/\/(.*)$/.exec(raw);
  if(m)return P+"/"+m[1]+"/"+m[2];
  return raw;
}
/*
 * 沙箱里访问 localStorage / sessionStorage 会直接抛 SecurityError（origin 是 opaque），
 * 而相当多的单页应用启动第一件事就是读它 —— 一抛就是整页白屏。这里换成内存兜底：
 * 写进去的东西只活在当前文档里（换页即丢），但至少不会把页面打死。
 * 拿不到属性时才替换，原生可用的场合一律不动。
 */
function memoryStorage(){
  var data={};
  return {
    getItem:function(k){k=String(k);return Object.prototype.hasOwnProperty.call(data,k)?data[k]:null;},
    setItem:function(k,v){data[String(k)]=String(v);},
    removeItem:function(k){delete data[String(k)];},
    clear:function(){data={};},
    key:function(i){var ks=Object.keys(data);return i<ks.length?ks[i]:null;},
    get length(){return Object.keys(data).length;}
  };
}
["localStorage","sessionStorage"].forEach(function(name){
  var native=null;
  try{native=window[name];}catch(e){}
  if(native)return;
  try{Object.defineProperty(window,name,{value:memoryStorage(),configurable:true});}catch(e){}
});
function mirrorTarget(t){
  if(typeof t==="string")return mirror(t);
  if(t instanceof URL)return new URL(mirror(t.href));
  return t;
}
var of=window.fetch;
if(of){window.fetch=function(input,init){
  if(typeof input==="string"||input instanceof URL)return of.call(this,mirrorTarget(input),init);
  if(input&&input.url){var u=mirror(input.url);if(u!==input.url)input=new Request(u,input);}
  return of.call(this,input,init);
};}
var xo=XMLHttpRequest.prototype.open;
XMLHttpRequest.prototype.open=function(){
  var a=Array.prototype.slice.call(arguments);a[1]=mirror(a[1]);return xo.apply(this,a);
};
if(window.WebSocket){var WS=window.WebSocket;
  var W=function(u,p){return p===undefined?new WS(mirror(u)):new WS(mirror(u),p);};
  W.prototype=WS.prototype;
  ["CONNECTING","OPEN","CLOSING","CLOSED"].forEach(function(k){W[k]=WS[k];});
  window.WebSocket=W;}
if(window.EventSource){var ES=window.EventSource;
  window.EventSource=function(u,c){return c===undefined?new ES(mirror(u)):new ES(mirror(u),c);};
  window.EventSource.prototype=ES.prototype;}
function report(){
  var p=location.pathname;
  if(p.indexOf(M)===0)p=p.slice(M.length);
  if(p===""||p.charAt(0)!=="/")p="/"+p;
  try{parent.postMessage({panelsshBrowser:1,url:T+p+location.search+location.hash},"*");}catch(e){}
}
["pushState","replaceState"].forEach(function(k){var f=history[k];history[k]=function(){var r=f.apply(this,arguments);report();return r;};});
window.addEventListener("load",report);
window.addEventListener("popstate",report);
report();
})()</script>`
}

// cookiesToHeader 把 Cookie 罐里的条目拼成请求头。
//
// 不能直接用 http.Cookie.String()：那是 Set-Cookie 的格式（会带上 Path、
// HttpOnly 这些属性），而请求头里只认 name=value。
func cookiesToHeader(cookies []*http.Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		parts = append(parts, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(parts, "; ")
}

// browserVerboseEnv 打开后逐个记录镜像请求的应答状态。
//
// 平时不开：一个页面会带出几十个子资源请求，全记下来是噪音。但排查"某个内网页面
// 白屏"时，它是唯一能看见"页面到底请求了什么、目标站回了什么"的窗口。
const browserVerboseEnv = "PANEL_BROWSER_VERBOSE"

func browserVerbose() bool {
	return os.Getenv(browserVerboseEnv) != ""
}

// renderBrowserNotice 输出一个自包含的提示页。
//
// 不返回 JSON、也不留空白：这个地址是被放进 iframe 里看的，
// 用户看到的必须是一句人能读懂的话和下一步怎么做。
func renderBrowserNotice(w http.ResponseWriter, status int, title, detail string) {
	body := `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<title>` + html.EscapeString(title) + `</title><style>
body{margin:0;padding:32px;background:#0e0f12;color:#b9bec6;
 font:13px/1.7 system-ui,-apple-system,"Segoe UI","Noto Sans CJK SC",sans-serif}
h1{margin:0 0 10px;font-size:15px;color:#eceef1}
p{margin:0;color:#8b919b;word-break:break-word}
.hint{margin-top:14px;font-size:12px;color:#5c626c}
</style></head><body><h1>` + html.EscapeString(title) + `</h1><p>` +
		html.EscapeString(detail) + `</p>` +
		`<p class="hint">内嵌浏览只适合结构简单的内网页面。整站浏览请改用窗口里的「系统代理」。</p>` +
		`</body></html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
