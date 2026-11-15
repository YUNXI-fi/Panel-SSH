package api

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"panelssh/internal/dsh"
	"panelssh/internal/plugin"
)

// dshPrefix 是 DSH 工作台在面板上的挂载前缀。
//
// 每台主机各有一个实例，因此完整前缀还要带上主机号：见 dshPrefixFor。
const dshPrefix = "/dsh"

// dshPrefixFor 给出某台主机上 DSH 工作台的反代前缀。
//
// 为什么把主机编进路径，而不是给每台主机开一个独立端口：
// 独立端口在 HTTPS 部署下会触发浏览器的混合内容拦截（HTTPS 页面不允许嵌
// HTTP iframe），而面板通常被放在带 TLS 的反向代理后面。子路径反代保持同源，
// 没有这个问题，代价是必须修正 dsh 输出里的绝对路径（见 rewriteDshHTML）。
func dshPrefixFor(hostID int64) string {
	return fmt.Sprintf("%s/%d", dshPrefix, hostID)
}

// remoteDshWorkDir 是 dsh 在目标机上的工作目录。
//
// 用 ~ 开头而不是绝对路径：目标机的家目录只有它自己知道，展开交给那边的
// shell 做（见 dsh 包的 scriptPath）。dsh 会把启动目录当作默认的文件系统位置，
// 放在用户家目录下比放 /tmp 更贴合"这是属于这个用户的工作台"。
const remoteDshWorkDir = "~/.panelssh/dsh"

// dshWorkspaceInstalled 判断某台主机上是否装了 DSH 工作台插件。
//
// 插件记录是这道功能的开关：没装插件时那台机器的 /dsh/{id}/ 就不该存在，
// 卸载后也应立刻关闭入口并回收实例。
func (s *Server) dshWorkspaceInstalled(hostID int64) bool {
	_, err := s.store.GetPlugin(plugin.BuiltinDshWorkspaceOrg, plugin.BuiltinDshWorkspaceName, hostID)
	return err == nil
}

// dshTarget 给出某台主机上 dsh 的落脚点。
//
// 宿主机用本地子进程，工作目录固定在 data/dsh（不能让 dsh 在面板自己的源码树里
// 活动）；其余主机通过 SSH 起进程，工作目录落在那边用户家目录下。
// "在哪台机器上、用哪种方式起进程"只有 api 包知道（凭据与 known_hosts 都在这里），
// 因此集中在这一处注入，dsh 包只拿接口。
func (s *Server) dshTarget(hostID int64) dsh.Target {
	if hostID == LocalHostID {
		return dsh.Target{
			Launcher: dsh.NewLocalLauncher(),
			WorkDir:  filepath.Join(s.cfg.DataDir, "dsh"),
			// 部署时用启动参数给的默认命令，只在"这台机器还没存过命令"时兜底。
			DefaultCommand: s.cfg.DshCommand,
			LogPath:        s.dshLogPath(hostID),
		}
	}
	return dsh.Target{
		Launcher: dsh.NewRemoteLauncher(hostID, s.dshClients.client),
		WorkDir:  remoteDshWorkDir,
		LogPath:  s.dshLogPath(hostID),
	}
}

// dshLogPath 是某台主机 dsh 日志的落盘位置。
//
// 放在面板的数据目录下而不是 dsh 的工作目录里：后者是 dsh 的默认工作区，
// 日志文件混进去会被它当成用户文件。
func (s *Server) dshLogPath(hostID int64) string {
	return filepath.Join(s.cfg.DataDir, "dsh-logs", fmt.Sprintf("%d.log", hostID))
}

// dshProxy 取某台主机上的反代处理器，构造一次后复用。
//
// /dsh/{id}/ 下的每个静态资源、每个 API 调用都会走这里，逐次新建
// ReverseProxy 是纯浪费。缓存在这里是安全的：Rewrite 每次都会重新取 endpoint，
// 所以 dsh 重启换了端口之后不会继续被转发到旧地址。
func (s *Server) dshProxy(hostID int64) http.Handler {
	if cached, ok := s.dshProxies.Load(hostID); ok {
		return cached.(http.Handler)
	}
	handler := s.newDshProxy(hostID)
	s.dshProxies.Store(hostID, handler)
	return handler
}

func (s *Server) newDshProxy(hostID int64) http.Handler {
	prefix := dshPrefixFor(hostID)

	// 远端主机上的 dsh 只听那边的回环地址，请求必须经 SSH 隧道送过去；
	// 宿主机直连，沿用默认 Transport 的连接池。
	transport := http.RoundTripper(http.DefaultTransport)
	if hostID != LocalHostID {
		transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return s.tunnels.dialContext(ctx, hostID, network, addr)
			},
			// 隧道一旦断开，待在池里的旧连接不会自己消失；给一个短的空闲超时，
			// 让失效的连接尽快被换掉，而不是被反复复用后每次都失败。
			IdleConnTimeout: 30 * time.Second,
		}
	}

	proxy := &httputil.ReverseProxy{
		// SSE 与流式响应必须即时下发，不能被缓冲，否则 dsh 的界面会一直转圈。
		FlushInterval: -1,
		Transport:     transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			endpoint, _, _ := s.dsh.Supervisor(hostID).Endpoint()
			target, err := url.Parse(endpoint)
			if err != nil {
				return
			}
			r.SetURL(target)

			// dsh 的 /api 有一道 browser-trust 栅栏，要求 Host 与 Origin 等于它自己的
			// authority。这条请求确实来自目标机的回环（直连是，隧道也是 ——
			// ssh.Client 的 direct-tcpip 通道在那边看来同样源自回环），把这两个头
			// 改写成 dsh 的真实地址既满足栅栏，也如实反映了这一点。
			r.Out.Host = target.Host
			if r.In.Header.Get("Origin") != "" {
				r.Out.Header.Set("Origin", endpoint)
			}
			if r.In.Header.Get("Referer") != "" {
				r.Out.Header.Set("Referer", endpoint+r.In.URL.Path)
			}

			// 剥掉面板前缀再转给 dsh。
			path := strings.TrimPrefix(r.In.URL.Path, prefix)
			if path == "" {
				path = "/"
			}
			r.Out.URL.Path = path
			r.Out.URL.RawPath = ""

			// dsh 默认对响应启用 gzip，而面板要改写入口文档里的绝对路径；
			// 压缩后的字节无法做字符串替换，因此只对入口文档要求明文，
			// 其余资源（JS/CSS 体积大得多）继续保留压缩。
			if path == "/" {
				r.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if location := resp.Header.Get("Location"); location != "" {
				if rewritten := rewriteDshLocation(location, prefix); rewritten != location {
					resp.Header.Set("Location", rewritten)
				}
			}

			// 会话未建立（或已过期）时，把 dsh 的 401 转成一次 token 兑换。
			// dsh 的浏览器会话要先走 /?token=... 换成 Cookie，不补这一步，
			// 用户与 iframe 只会看到一行 "authentication required" 的纯文本。
			// 只对入口页这么做：dsh 的 /api 返回 401 是正常语义，不能一并改道。
			//
			// token 必须取这台主机的：每台机器的 dsh 各有自己的一次性令牌。
			if resp.StatusCode == http.StatusUnauthorized && resp.Request != nil &&
				resp.Request.URL.Path == "/" && !strings.Contains(resp.Request.URL.RawQuery, "token=") {
				if _, token, ok := s.dsh.Supervisor(hostID).Endpoint(); ok && token != "" {
					return redirectToTokenExchange(resp, token, prefix)
				}
			}

			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") &&
				resp.Header.Get("Content-Encoding") == "" {
				return rewriteDshHTML(resp, prefix)
			}
			// 静态资源顺手写一份到本地缓存：预抓取没跑完时，
			// 这次（可能很慢的）加载至少能让下次直接命中。
			if resp.StatusCode == http.StatusOK && resp.Request != nil &&
				cacheableDshPath(resp.Request.URL.Path) {
				resp.Body = s.dshCache.capture(hostID, resp.Request.URL.Path,
					resp.Request.URL.RawQuery, resp.Header, resp.Body)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "无法连接 DSH 工作台："+err.Error(), http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, prefix)
		if path == "" {
			path = "/"
		}

		// 命中缓存直接本地回放：浏览器并发多少条都不会再碰到慢链路。
		if r.Method == http.MethodGet && cacheableDshPath(path) &&
			s.dshCache.serve(w, r, hostID, path) {
			return
		}

		// 远端链路上首屏资源要顺序抓一遍才可靠。入口页在这里等预抓取完成，
		// 这样浏览器拿到 HTML 时引用的资源已经全部在本地。
		// 本机直连不等待：没有慢链路问题，等反而拖慢首屏。
		if r.Method == http.MethodGet && path == "/" && hostID != LocalHostID {
			if sup := s.dsh.Supervisor(hostID); sup.Available() {
				switch sup.Status().State {
				case dsh.StateRunning, dsh.StateStarting:
					done := s.dshCache.startWarm(s, hostID, false)
					select {
					case <-done:
					case <-time.After(dshWarmWait):
					case <-r.Context().Done():
						return
					}
				}
			}
		}

		proxy.ServeHTTP(w, r)
	})
}

// rewriteDshLocation 把 dsh 的绝对路径重定向改挂到面板前缀下。
//
// dsh 的重定向都用绝对路径（如 303 -> /），直接透传会把浏览器带出 /dsh/{id}/。
// 抽成纯函数是为了能单独验证：内联在闭包里时，这条分支没有任何办法证明。
func rewriteDshLocation(location, prefix string) string {
	if !strings.HasPrefix(location, "/") || strings.HasPrefix(location, prefix) {
		return location
	}
	return prefix + location
}

// redirectToTokenExchange 把一次 401 改写成跳转到 dsh 的 token 兑换地址。
// 浏览器随后会带着新下发 Cookie 回到 /dsh/{id}/，形成一次自愈，不会循环：
// 兑换请求本身带 token 参数，不满足上面的触发条件。
func redirectToTokenExchange(resp *http.Response, token, prefix string) error {
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(""))
	resp.ContentLength = 0
	resp.Header.Del("Content-Length")

	resp.StatusCode = http.StatusSeeOther
	resp.Status = "303 See Other"
	resp.Header.Set("Location", prefix+"/?token="+url.QueryEscape(token))
	return nil
}

// maxDshEntryHTML 是允许改写的 dsh 入口文档体积上限。
// 改写靠字符串替换，必须拿到完整正文，超过这个体量就不再尝试。
const maxDshEntryHTML = 4 << 20

// rewriteDshHTML 修正 dsh 首页里的绝对路径，使其在 /dsh/{id}/ 子路径下可用。
//
// 调用方必须先确认响应未压缩：对 gzip 正文做字符串替换只会得到坏数据。
//
// 关于体积上限：这里多读 1 字节来判断是否超限 —— 超限时宁可把这次改写放弃并
// 如实报错，也不能把截断后的半截 HTML 当成改好的页面发出去（旧实现就是这样
// 静默截断的，用户会看到一个结构被拦腰砍断的页面，且完全不知道原因）。
func rewriteDshHTML(resp *http.Response, prefix string) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDshEntryHTML+1))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) > maxDshEntryHTML {
		return fmt.Errorf("dsh 入口文档超过 %d MiB，超出可安全改写的范围", maxDshEntryHTML>>20)
	}

	page := string(body)
	page = strings.ReplaceAll(page, `<base href="/">`, `<base href="`+prefix+`/">`)
	// dsh 的插件 bundle 路径以 "/plugins/ 形式同时出现在两处：HTML 属性
	// （href/src）和它注入的 __DSH_BOOT__ 配置 JSON 里。后者是客户端加载
	// 插件时真正使用的地址，只改属性会导致运行时仍然去请求 /plugins/。
	// 两种形态都以 "/plugins/ 开头，因此一次替换即可覆盖。
	page = strings.ReplaceAll(page, `"/plugins/`, `"`+prefix+`/plugins/`)

	// HTML 里能改写的只有字面量。dsh 的客户端还会在运行时用 location.origin
	// 拼出 /api/* 的地址（见其 hostBase()），这类路径不在文本里，静态替换抓不到；
	// 而 /api 正是面板自己占用的前缀。因此注入一层同源地址修正，把 dsh 页面发出的
	// 所有根路径请求改挂到当前主机的 /dsh/{id} 之下。
	shim := dshOriginShim(prefix)
	if idx := strings.Index(page, "<head>"); idx >= 0 {
		pos := idx + len("<head>")
		page = page[:pos] + shim + page[pos:]
	} else {
		page = shim + page
	}

	resp.Body = io.NopCloser(bytes.NewReader([]byte(page)))
	resp.ContentLength = int64(len(page))
	resp.Header.Set("Content-Length", fmt.Sprint(len(page)))
	return nil
}

// dshOriginShim 在 dsh 页面最前面打一层补丁：只要请求目标是本站根路径，
// 就改挂到这台主机的 dshPrefix 之下。
//
// 为什么必须这样做：dsh 的 Web 客户端假定自己独占一个 origin 的根路径
// （hostBase() 直接返回 location.origin，再拼 "/api/remote.mux" 这类常量），
// 而面板把它挂在子路径上、且面板自己也用 /api。dsh 没有 base-path 配置项，
// 所以只能在它自己的页面里拦一层。覆盖 fetch / XHR / WebSocket / EventSource
// 四个出口即可，dsh 的 HTTP 与流式通道都从这里走。
//
// 这段脚本必须排在 dsh 自己的 <script> 之前，否则 lib 会先捕获到原生实现。
func dshOriginShim(prefix string) string {
	return `<script>(function(){
var P=` + "`" + prefix + "`" + `;
function rewrite(raw){
  var path=null,prefix="";
  if(raw.charAt(0)==="/"&&raw.charAt(1)!=="/"){path=raw;}
  else{
    var http=location.origin,ws=http.replace(/^http/,"ws");
    if(raw.indexOf(http+"/")===0){prefix=http;path=raw.slice(http.length);}
    else if(raw.indexOf(ws+"/")===0){prefix=ws;path=raw.slice(ws.length);}
  }
  if(path===null)return raw;
  if(path===P||path.indexOf(P+"/")===0||path.indexOf(P+"?")===0)return raw;
  return prefix+P+path;
}
function rewriteTarget(t){
  if(typeof t==="string")return rewrite(t);
  if(t instanceof URL)return new URL(rewrite(t.href));
  return t;
}
var of=window.fetch;
if(of){window.fetch=function(input,init){
  if(typeof input==="string"||input instanceof URL)return of.call(this,rewriteTarget(input),init);
  if(input&&input.url){var u=rewrite(input.url);if(u!==input.url)input=new Request(u,input);}
  return of.call(this,input,init);
};}
var xo=XMLHttpRequest.prototype.open;
XMLHttpRequest.prototype.open=function(){
  var a=Array.prototype.slice.call(arguments);a[1]=rewrite(a[1]);return xo.apply(this,a);
};
if(window.WebSocket){var WS=window.WebSocket;
  var W=function(url,p){return p===undefined?new WS(rewrite(url)):new WS(rewrite(url),p);};
  W.prototype=WS.prototype;
  ["CONNECTING","OPEN","CLOSING","CLOSED"].forEach(function(k){W[k]=WS[k];});
  window.WebSocket=W;}
if(window.EventSource){var ES=window.EventSource;
  window.EventSource=function(url,c){return c===undefined?new ES(rewrite(url)):new ES(rewrite(url),c);};
  window.EventSource.prototype=ES.prototype;}
})()</script>`
}

// dshStatus 是给前端的 dsh 状态。
type dshStatusView struct {
	Available bool      `json:"available"`
	State     string    `json:"state"`
	Message   string    `json:"message"`
	URL       string    `json:"url"`
	Command   string    `json:"command"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	Logs      []string  `json:"logs"`
}

func (s *Server) dshStatusViewValue(hostID int64) dshStatusView {
	sup := s.dsh.Supervisor(hostID)
	status := sup.Status()
	view := dshStatusView{
		Available: sup.Available(),
		State:     string(status.State),
		Message:   status.Message,
		Command:   sup.Command(),
		PID:       status.PID,
		StartedAt: status.StartedAt,
		Logs:      status.Logs,
	}
	if status.State == dsh.StateRunning {
		view.URL = dshPrefixFor(hostID) + "/"
	}
	return view
}

func (s *Server) handleDshStatus(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.dshStatusViewValue(hostID))
}

// handleDshSetCommand 保存某台主机上 dsh 的启动命令。
//
// 命令按主机落库而不是只留在内存里：用户填一次就该跨重启一直有效，
// 否则每次重启面板都要重填，等于把配置成本转嫁给用户。
func (s *Server) handleDshSetCommand(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}

	command := strings.TrimSpace(req.Command)
	if command == "" {
		writeErr(w, http.StatusBadRequest, "启动命令不能为空")
		return
	}

	if err := s.dsh.SetCommand(hostID, command); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存启动命令失败")
		return
	}

	writeJSON(w, http.StatusOK, s.dshStatusViewValue(hostID))
}

// dshDetectScript 在目标机上找出 dsh 可执行文件的绝对路径。
//
// 取绝对路径而不是直接写 "dsh"：面板跑命令是非登录 shell，不读 /etc/profile.d，
// PATH 里未必有 npm 的全局 bin 目录（与 detectBun / detectCUDA 用绝对路径同理）。
// 也不用 npx 兜底：那会让每次启动都多走一遍 npx 解析，慢且可能联网。
//
// 用户级安装（~/.panelssh/dsh-runtime）要输出 "<node> <bin.js>" 两段：
// 用户前缀里的 dsh 首行是 `#!/usr/bin/env node`，而 Node 24 不在面板的 PATH 上，
// 只给 shim 路径会得到一个"Node 版本不对"的失败。
const dshDetectScript = `U="$HOME/.panelssh/dsh-runtime"
B="$U/lib/node_modules/@deepseek-ai/dsh/lib/bin.js"
N=$(ls -d "$HOME/.panelssh/node"/node-v24.*/bin/node 2>/dev/null | sort -V | tail -n1)
if [ -f "$B" ] && [ -n "$N" ] && [ -x "$N" ]; then echo "$N $B"; exit 0; fi
p=$(command -v dsh 2>/dev/null)
if [ -n "$p" ]; then echo "$p"; exit 0; fi
for d in /usr/local/bin /usr/bin; do [ -x "$d/dsh" ] && { echo "$d/dsh"; exit 0; }; done
exit 1`

// handleDshDetectCommand 探测目标机上 dsh 的安装位置并保存为该主机的启动命令。
//
// 装在哪儿只有目标机知道（npm 的全局前缀因发行版与安装方式而异），
// 让用户自己去翻路径是把面板能做的事推给用户，因此这里探测完直接落库。
func (s *Server) handleDshDetectCommand(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	res, err := s.execEscalated(r.Context(), hostID, dshDetectScript, defaultExecTimeout, escalatePreferred)
	if err != nil {
		s.writeHostError(w, err, "探测 dsh 安装路径失败")
		return
	}
	command := firstLine(res.Stdout)
	if res.ExitCode != 0 || command == "" {
		writeErr(w, http.StatusBadRequest, "目标机上没有找到 dsh 可执行文件，请先完成安装")
		return
	}

	if err := s.dsh.SetCommand(hostID, command); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存启动命令失败")
		return
	}
	// 回填命令的目的是"让它能用"，因此顺手拉起一次；失败详情由状态接口与
	// 运行日志给出，这里不阻塞请求。
	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "已回填启动命令，准备启动")
	s.startDshAsync(hostID)
	writeJSON(w, http.StatusOK, s.dshStatusViewValue(hostID))
}

// firstLine 取输出的第一行并去掉首尾空白。
//
// 探测脚本正常情况下只输出一行，多余的行是目标机 shell 被打上的噪音
// （如 sshd 的家目录提示），不能让它混进启动命令里。
func firstLine(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(line)
}

// handleDshEntry 是 /dsh/{id}/ 的入口：未就绪时给出可读的状态页并触发懒启动，
// 就绪后交给反代。
//
// 带 config=1 时无条件渲染设置页。dsh 一旦跑起来，反代就接管了该前缀，
// 这个参数是运行期改启动命令的唯一入口。
//
// 懒启动的理由：dsh 常驻约 320MB 内存，面板启动时不该先付这笔开销，
// 只有用户真正打开某台主机的工作台时才在那边把它拉起来。
func (s *Server) handleDshEntry(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	if !s.dshWorkspaceInstalled(hostID) {
		http.Error(w, "DSH 工作台插件未安装", http.StatusNotFound)
		return
	}

	settings := r.URL.Query().Get("config") != ""
	sup := s.dsh.Supervisor(hostID)

	if sup.Available() {
		if _, _, ready := sup.Endpoint(); !ready {
			// 已经在启动中就不要再起一个等待协程：状态页每 3 秒自动刷新一次，
			// 每次刷新都新起一个等待者，几十秒下来会白占几十个协程。
			if sup.Status().State != dsh.StateStarting {
				go func() {
					// 失败详情通过状态页与 /api/hosts/{id}/dsh/status 暴露，
					// 这里无需额外处理。
					_ = sup.EnsureRunning()
				}()
			}
		} else if !settings {
			s.dshProxy(hostID).ServeHTTP(w, r)
			return
		}
	}

	s.renderDshStatusPage(w, hostID, "")
}

// handleDshRedirect 把不带尾斜杠的入口补成尾斜杠形式。
func (s *Server) handleDshRedirect(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, dshPrefixFor(hostID)+"/", http.StatusTemporaryRedirect)
}

// handleDshRootRedirect 把不带主机号的入口指向宿主机。
//
// /dsh/ 是改造前唯一的入口，它的语义就是"面板自己所在的那台机器"，
// 因此重定向到宿主机而不是报错 —— 老书签与旧链接照常能用。
func (s *Server) handleDshRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, dshPrefixFor(LocalHostID)+"/", http.StatusTemporaryRedirect)
}

// renderDshStatusPage 输出一个自包含的状态页：自动刷新，并把最近日志一并展示，
// 否则 dsh 启动失败时用户只能看到一个空白窗口，无从排查。
func (s *Server) renderDshStatusPage(w http.ResponseWriter, hostID int64, hint string) {
	sup := s.dsh.Supervisor(hostID)
	status := sup.Status()
	prefix := dshPrefixFor(hostID)
	commandPath := fmt.Sprintf("/api/hosts/%d/dsh/command", hostID)

	title := "DSH 工作台"
	detail := status.Message
	if hint != "" {
		detail = hint
	}
	if detail == "" && status.State == dsh.StateStopped {
		detail = "DSH 工作台尚未启动，正在拉起…"
	}

	var body strings.Builder
	body.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	body.WriteString(`<title>` + html.EscapeString(title) + `</title>`)
	// 仅在启动中/未启动时自动刷新，进入失败态后停下来，避免无意义地反复打日志。
	if status.State == dsh.StateStarting || status.State == dsh.StateStopped {
		body.WriteString(`<meta http-equiv="refresh" content="3">`)
	}
	body.WriteString(`<style>
body{margin:0;padding:32px;background:#0b1020;color:#cbd5e1;
 font:14px/1.7 system-ui,-apple-system,"Segoe UI","Noto Sans CJK SC",sans-serif}
h1{margin:0 0 6px;font-size:16px;color:#e2e8f0}
.state{display:inline-block;margin:0 0 14px;padding:2px 8px;border-radius:999px;
 font-size:12px;background:rgba(148,163,184,.16);color:#94a3b8}
.state-running{background:rgba(52,211,153,.16);color:#34d399}
.state-failed,.state-disabled{background:rgba(248,113,113,.16);color:#fca5a5}
.state-starting{background:rgba(56,189,248,.16);color:#7dd3fc}
p{margin:0 0 14px}
pre{margin:0;padding:12px;max-height:320px;overflow:auto;border-radius:8px;
 background:rgba(0,0,0,.35);font:12px/1.6 ui-monospace,Consolas,monospace;color:#94a3b8;
 white-space:pre-wrap;word-break:break-word}
.hint{margin-top:16px;font-size:12px;color:#64748b}
form{margin:0 0 16px}
label{display:block;margin:0 0 6px;font-size:12px;color:#94a3b8}
.row{display:flex;gap:8px}
input{flex:1;min-width:0;padding:8px 10px;border-radius:8px;border:1px solid rgba(148,163,184,.28);
 background:rgba(0,0,0,.32);color:#e2e8f0;font:13px/1.4 ui-monospace,Consolas,monospace}
input:focus{outline:none;border-color:#38bdf8}
button{padding:8px 14px;border-radius:8px;border:0;cursor:pointer;
 background:#38bdf8;color:#0b1020;font-size:13px;font-weight:600}
button:disabled{opacity:.6;cursor:default}
.err{color:#fca5a5;font-size:12px;margin:6px 0 0}
a{color:#38bdf8}
code{padding:1px 5px;border-radius:4px;background:rgba(148,163,184,.16);
 font:12px/1.4 ui-monospace,Consolas,monospace;color:#cbd5e1}
</style></head><body>`)

	body.WriteString(`<h1>` + html.EscapeString(title) + `</h1>`)
	body.WriteString(`<div class="state state-` + html.EscapeString(string(status.State)) + `">` +
		html.EscapeString(string(status.State)) + `</div>`)
	body.WriteString(`<p>` + html.EscapeString(detail) + `</p>`)

	// 未就绪时给出配置入口。命令由用户填写并立即生效，不需要重启面板。
	body.WriteString(`<form id="cfg"><label>dsh 启动命令</label><div class="row">`)
	body.WriteString(`<input id="cmd" name="command" spellcheck="false" placeholder="node /path/to/@deepseek-ai/dsh/lib/bin.js" value="` +
		html.EscapeString(sup.Command()) + `">`)
	body.WriteString(`<button id="save" type="submit">保存并启动</button></div>`)
	body.WriteString(`<p class="hint" id="err"></p></form>`)
	body.WriteString(`<p class="hint">面板会在该命令后补上 <code>web --no-open --host 127.0.0.1 --port 0</code>。` +
		`命令按空格拆分，因此路径里不要带空格。</p>`)

	// 设置页在 dsh 已经跑起来时也会渲染，这时得给一条回到工作台的路，
	// 否则用户改完命令就出不去了。
	if status.State == dsh.StateRunning {
		body.WriteString(`<p><a href="` + html.EscapeString(prefix) + `/">打开工作台</a></p>`)
	}

	if len(status.Logs) > 0 {
		body.WriteString(`<pre>` + html.EscapeString(strings.Join(status.Logs, "\n")) + `</pre>`)
	}

	body.WriteString(`<p class="hint">更新于 ` + time.Now().Format("15:04:05") + `</p>`)
	body.WriteString(`<script>
// 自动刷新是为了等 dsh 起来，但用户开始填命令时必须停掉，
// 否则每 3 秒一次的刷新会把他正在输入的半截命令清掉。
const autoRefresh = document.querySelector('meta[http-equiv="refresh"]');
const cmdInput = document.getElementById("cmd");
for (const event of ["focus", "input"]) {
  cmdInput.addEventListener(event, () => autoRefresh && autoRefresh.remove());
}

const form = document.getElementById("cfg");
form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = document.getElementById("save");
  const err = document.getElementById("err");
  err.textContent = "";
  button.disabled = true;
  try {
    const res = await fetch("` + commandPath + `", {
      method: "PUT",
      headers: { "content-type": "application/json", "X-PanelSSH-Request": "1" },
      body: JSON.stringify({ command: document.getElementById("cmd").value }),
    });
    if (!res.ok) {
      err.textContent = (await res.text()).trim() || ("保存失败（HTTP " + res.status + "）");
      button.disabled = false;
      return;
    }
    location.reload();
  } catch (cause) {
    err.textContent = "保存失败：" + cause;
    button.disabled = false;
  }
});
</script>`)
	body.WriteString(`</body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, body.String())
}

// CloseDshInstances 停掉全部 dsh 实例，供面板退出时调用。
//
// 本地实例还有 Pdeathsig 兜底，远端那些没有 —— 不显式停掉的话，远端会一直
// 挂着一个占几百 MB 内存的 node，而面板这边已经什么都不知道了。
//
// 这里不关常驻通道：停远端实例正要靠那条通道下发信号，先关掉就停不动了。
// 通道由 main 在全部消费者停完之后统一关闭（见 CloseHostTunnels）。
func (s *Server) CloseDshInstances() {
	// 先收掉原生地址的转发端口：否则 dsh 停掉之后，本机仍留着一个
	// 「连得上但没有任何后端」的端口。
	s.dshNative.closeAll()
	// 停实例要靠专用连接下发信号，因此关闭必须排在 StopAll 之后。
	s.dsh.StopAll()
	s.dshClients.closeAll()
}
