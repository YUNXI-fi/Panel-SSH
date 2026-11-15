package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"panelssh/internal/env"
	"panelssh/internal/plugin"
	"panelssh/internal/store"
)

// 插件两个方向的请求体上限。与安装包同量级：插件可能一次返回一整张表的数据，
// 也可能要写一个大一点的配置文件。
const maxPluginRPCPayload = 32 << 20

// pluginAPIPrefix 是插件后端回调面板的接口前缀。
const pluginAPIPrefix = "/api/plugin-api"

// pluginTokenHeader 是插件进程回调面板时携带凭据的请求头。
//
// 用请求头而不是查询串：查询串会进访问日志、进浏览器历史与 Referer，
// 这种凭据在 URL 里出现过一次就等于泄露。
const pluginTokenHeader = "X-Plugin-Token"

// 插件相关接口的错误码。
//
// 前端要能区分"进程没在跑"（面板可以尝试拉起）与"插件自己报错"（只能如实转述），
// 光靠一句中文错误信息做不到。
const (
	codePluginNotRunning = "PLUGIN_NOT_RUNNING"
	codePluginForbidden  = "PLUGIN_FORBIDDEN"
)

// pluginAPIMethods 是插件后端可以回调的接口及其所需能力。
//
// 一张显式表，而不是"从方法名推断能力"：能力与接口的对应关系是权限模型的一部分，
// 让它可枚举、可审阅。方法名与插件前端的能力桥保持一致，插件作者只需要记一套名字。
var pluginAPIMethods = map[string]string{
	"host.list":         plugin.CapHostList,
	"host.exec":         plugin.CapHostExec,
	"host.readFile":     plugin.CapHostFileRead,
	"host.writeFile":    plugin.CapHostFileWrite,
	"host.pkg":          plugin.CapHostPkg,
	"host.service":      plugin.CapHostService,
	"host.tunnel":       plugin.CapHostTunnel,
	"host.tunnel.close": plugin.CapHostTunnel,
}

// maxPluginPkgCount 限制单次回调能装或卸几个包。
//
// 不是性能考虑，而是"一次操作要么成功要么回滚"：包越多，快照与补偿的代价越大，
// 失败后留下的半成品状态越难说清。要装一整套依赖就分几次调用，让每步都可回滚。
const maxPluginPkgCount = 32

// pluginServiceTimeout 是启停服务的等待上限。
//
// 服务启动可能很慢（数据库首次初始化），因此给得比命令执行宽松得多。
const pluginServiceTimeout = 2 * time.Minute

// serviceUnitPattern 限定 systemd 单位名。
//
// 这个值最终会被拼进一条以 root 执行的命令，所以逐字符卡死：允许的字符集就是
// 单位名本身允许的那些，连空格都不放。
var serviceUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]{0,63}$`)

// pluginRPCBody 是插件前端经由面板转发时的请求体。
type pluginRPCBody struct {
	HostID int64           `json:"hostId"`
	Params json.RawMessage `json:"params"`
}

// pluginAPIBody 是插件后端回调面板时的请求体。
//
// 各接口的字段并在一起：每个接口都只有两三个字段，为它们各定义一个结构体
// 并不会让代码更清楚，反而多出三处需要与插件文档对齐的地方。
type pluginAPIBody struct {
	HostID     int64    `json:"hostId"`
	Command    string   `json:"command"`
	TimeoutSec int      `json:"timeoutSec"`
	Path       string   `json:"path"`
	Content    string   `json:"content"`
	Action     string   `json:"action"`
	Packages   []string `json:"packages"`
	Unit       string   `json:"unit"`
	RemoteHost string   `json:"remoteHost"`
	RemotePort int      `json:"remotePort"`
	TunnelID   string   `json:"tunnelId"`
}

// handlePluginRPC 把插件前端的调用转发给插件的后端进程。
//
// 这个接口要求会话身份：插件前端跑在 opaque origin 的沙箱里，它自己发的请求
// 不带会话 Cookie，所以由主应用代为发起（见 PluginFrame.vue 的能力桥）。
// 谁能调插件，这件事因此仍由面板把关，而不是由沙箱里的代码自己声明。
func (s *Server) handlePluginRPC(w http.ResponseWriter, r *http.Request) {
	org, name := r.PathValue("org"), r.PathValue("name")
	method := r.PathValue("method")

	// method 会进入插件进程的请求，因此在这里就按命名空间规则卡住。
	if !plugin.IsValidRPCMethod(method) {
		writeErr(w, http.StatusBadRequest, "方法名非法")
		return
	}

	var body pluginRPCBody
	if err := decodeJSONBody(w, r, &body, maxPluginRPCPayload); err != nil {
		return
	}

	// 插件可以装在多台主机上，版本也可能不同，取调用方所在的那一台。
	item, ok := s.pluginOnHost(w, org, name, body.HostID)
	if !ok {
		return
	}

	manifest, err := plugin.ReadInstalledManifest(filepath.Join(s.pluginsDir, org, name, item.Version))
	if err != nil {
		writeErr(w, http.StatusNotFound, "读取插件清单失败："+err.Error())
		return
	}
	if manifest.Runtime == nil || manifest.Runtime.Kind != plugin.RuntimeProcess {
		writeErr(w, http.StatusBadRequest, "该插件没有后端进程")
		return
	}

	if err := s.runtimes.EnsureRunning(manifest.Runtime, org, name, item.Version); err != nil {
		writeErrCode(w, http.StatusBadGateway, codePluginNotRunning, "插件后端进程未就绪："+err.Error())
		return
	}

	params, err := pluginHostParams(body.HostID, body.Params)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := s.runtimes.Call(r.Context(), org, name, method, params)
	if err != nil {
		s.writePluginCallError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// handlePluginAPI 是插件后端进程回调面板的入口。
//
// 它不走会话鉴权（插件进程不是浏览器，没有会话 Cookie），身份由令牌头给出；
// 也不受 CSRF 检查约束（见 withCSRF）：CSRF 的前提是浏览器会自动携带凭据，
// 而这枚令牌是插件自己去读一个 0600 文件拿到的，跨站页面不可能有它。
func (s *Server) handlePluginAPI(w http.ResponseWriter, r *http.Request) {
	org, name, ok := s.runtimes.LookupToken(r.Header.Get(pluginTokenHeader))
	if !ok {
		writeErr(w, http.StatusUnauthorized, "插件令牌无效或已失效")
		return
	}

	method := r.PathValue("method")
	capability, ok := pluginAPIMethods[method]
	if !ok {
		writeErr(w, http.StatusNotFound, "没有这个接口")
		return
	}

	var body pluginAPIBody
	if err := decodeJSONBody(w, r, &body, maxPluginRPCPayload); err != nil {
		return
	}

	item, err := s.store.GetPlugin(org, name, body.HostID)
	if err != nil {
		writeErrCode(w, http.StatusForbidden, codePluginForbidden, "插件未安装在请求指定的主机上")
		return
	}
	if !item.Enabled {
		writeErrCode(w, http.StatusForbidden, codePluginForbidden, "插件已停用")
		return
	}
	// 权限逐次判定，不缓存在令牌里：令牌只回答"你是谁"，"你能做什么"以当前
	// 授权为准。缓存快照会让"撤销授权"要等插件进程重启才生效，中间那段窗口
	// 恰恰是撤销授权最需要它生效的时候。
	if !contains(item.Granted, capability) {
		writeErrCode(w, http.StatusForbidden, codePluginForbidden, "插件未获授权："+capability)
		return
	}

	switch method {
	case "host.list":
		s.pluginAPIHostList(w, item)
	case "host.exec":
		s.pluginAPIExec(w, r, body)
	case "host.readFile":
		s.pluginAPIReadFile(w, body)
	case "host.writeFile":
		s.pluginAPIWriteFile(w, body)
	case "host.pkg":
		s.pluginAPIPkg(w, r, body)
	case "host.service":
		s.pluginAPIService(w, r, body)
	case "host.tunnel":
		s.pluginAPITunnel(w, item, body)
	case "host.tunnel.close":
		s.pluginAPITunnelClose(w, item, body)
	}
}

// pluginOnHost 取插件在某台主机上的安装记录，并确认它是启用的。
//
// "只能操作自己被安装到的主机"这条边界来自前端能力桥的 requireOwnHost，
// 后端进程模型沿用同一条：安装记录就是插件的活动范围，没装它的机器不该被它碰到。
func (s *Server) pluginOnHost(w http.ResponseWriter, org, name string, hostID int64) (*store.Plugin, bool) {
	items, err := s.store.ListPluginsByName(org, name)
	if err != nil || len(items) == 0 {
		writeErr(w, http.StatusNotFound, "插件不存在")
		return nil, false
	}

	for i := range items {
		if items[i].HostID != hostID {
			continue
		}
		if !items[i].Enabled {
			writeErr(w, http.StatusForbidden, "插件已停用")
			return nil, false
		}
		return &items[i], true
	}

	writeErr(w, http.StatusForbidden, "插件未安装在请求指定的主机上")
	return nil, false
}

// pluginHostParams 把调用上下文与插件自己的参数拼成 JSON-RPC 的 params。
//
// 面板不解释 params 的内容，但必须把 hostId 一并送进去：插件要操作目标机时
// 得知道"这一次是哪台"。它由面板注入，插件无法伪造。
func pluginHostParams(hostID int64, params json.RawMessage) (json.RawMessage, error) {
	if len(params) == 0 || strings.TrimSpace(string(params)) == "null" {
		params = json.RawMessage("{}")
	} else {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(params, &probe); err != nil {
			return nil, errors.New("params 必须是 JSON 对象")
		}
	}

	return json.Marshal(struct {
		HostID int64           `json:"hostId"`
		Params json.RawMessage `json:"params"`
	}{HostID: hostID, Params: params})
}

// writePluginCallError 把插件侧的失败转成 HTTP 响应。
//
// 面板不解释插件错误的含义，但必须把"进程没在跑"与"插件自己报错"分开：
// 前者是面板能处理的状态，后者只能如实转给前端。
func (s *Server) writePluginCallError(w http.ResponseWriter, err error) {
	var rpcErr *plugin.RPCError

	switch {
	case errors.Is(err, plugin.ErrPluginNotRunning):
		writeErrCode(w, http.StatusBadGateway, codePluginNotRunning, "插件后端进程未运行")
	case errors.As(err, &rpcErr):
		writeErr(w, http.StatusBadRequest, rpcErr.Message)
	default:
		writeErr(w, http.StatusBadGateway, "调用插件失败："+err.Error())
	}
}

// pluginAPIHostList 只回插件自己被安装到的那一台主机。
//
// 不回全部清单：那等于给插件一份"还能去哪些机器"的靶子列表，
// 而它本来只需要知道自己脚下这一台。
func (s *Server) pluginAPIHostList(w http.ResponseWriter, item *store.Plugin) {
	if item.HostID == LocalHostID {
		writeJSON(w, http.StatusOK, map[string]any{
			"hosts": []any{map[string]any{"id": LocalHostID, "name": "宿主机"}},
		})
		return
	}

	host, err := s.store.GetHost(item.HostID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "主机不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hosts": []any{map[string]any{
			"id":       host.ID,
			"name":     host.Name,
			"addr":     host.Addr,
			"port":     host.Port,
			"username": host.Username,
			"group":    host.Group,
		}},
	})
}

func (s *Server) pluginAPIExec(w http.ResponseWriter, r *http.Request, body pluginAPIBody) {
	command := strings.TrimSpace(body.Command)
	if command == "" {
		writeErr(w, http.StatusBadRequest, "command 为必填")
		return
	}

	result, err := s.execOnHost(r.Context(), body.HostID, command, clampExecTimeout(body.TimeoutSec))
	if err != nil {
		s.writeHostError(w, err, "执行命令失败")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// pluginAPIReadFile 读取目标机上的文本文件。
//
// 与界面上的文件编辑器有一处刻意的差别：不提权。插件是第三方代码，
// 让它借面板的身份去写 /etc 下的文件，等于把提权链路交给了插件作者；
// 界面上"被拒绝就用 root 再试一次"是给坐在面板前的人的便利，不该复制到这条路径。
func (s *Server) pluginAPIReadFile(w http.ResponseWriter, body pluginAPIBody) {
	target, ok := pluginAbsPath(w, body.Path)
	if !ok {
		return
	}
	if body.HostID == LocalHostID {
		writeErr(w, http.StatusBadRequest, "宿主机不支持文件管理，请选择一台远端主机")
		return
	}

	sess, guard, ok := s.openSFTPNoResponse(body.HostID)
	if !ok {
		writeErr(w, http.StatusBadGateway, "无法建立到目标主机的 SFTP 连接")
		return
	}
	defer sess.Close()

	info, err := guard.Stat(target)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在："+err.Error())
		return
	}
	if info.IsDir() {
		writeErr(w, http.StatusBadRequest, "目标是目录，无法作为文本读取")
		return
	}
	if info.Size() > maxTextEditSize {
		writeErr(w, http.StatusRequestEntityTooLarge, "文件超过 1 MiB")
		return
	}

	file, err := guard.Open(target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打开文件失败："+err.Error())
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxTextEditSize))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取文件失败："+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":    target,
		"content": string(data),
		"size":    len(data),
	})
}

// pluginAPIWriteFile 把内容写入目标机上的文件。
func (s *Server) pluginAPIWriteFile(w http.ResponseWriter, body pluginAPIBody) {
	target, ok := pluginAbsPath(w, body.Path)
	if !ok {
		return
	}
	if body.HostID == LocalHostID {
		writeErr(w, http.StatusBadRequest, "宿主机不支持文件管理，请选择一台远端主机")
		return
	}
	if len(body.Content) > maxTextEditSize {
		writeErr(w, http.StatusRequestEntityTooLarge, "内容超过 1 MiB")
		return
	}

	sess, guard, ok := s.openSFTPNoResponse(body.HostID)
	if !ok {
		writeErr(w, http.StatusBadGateway, "无法建立到目标主机的 SFTP 连接")
		return
	}
	defer sess.Close()

	// 先记住原权限位：SFTP 的 Create 是 O_TRUNC 重写，且没有传 mode 的入口，
	// 会把 0600 的配置、0755 的脚本一律写成默认权限。改内容是改内容，
	// 不该顺手把权限也改掉。
	var originalMode os.FileMode
	existed := false
	if info, err := guard.Stat(target); err == nil {
		if info.IsDir() {
			writeErr(w, http.StatusBadRequest, "目标是目录，无法写入")
			return
		}
		originalMode = info.Mode().Perm()
		existed = true
	}

	file, err := guard.Create(target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打开文件失败："+err.Error())
		return
	}
	if _, err := io.WriteString(file, body.Content); err != nil {
		_ = file.Close()
		writeErr(w, http.StatusInternalServerError, "写入失败："+err.Error())
		return
	}
	if err := file.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, "关闭文件失败："+err.Error())
		return
	}

	if existed {
		if err := guard.Chmod(target, originalMode); err != nil {
			// 内容已经写成功，此时报错会让插件以为没写进去，因此只提示权限未还原。
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "path": target, "warning": "内容已写入，但未能还原原权限位",
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": target})
}

// pluginAbsPath 校验插件给出的路径。
//
// 必须是绝对路径：插件在目标机上操作的是那台机器的文件系统，
// 相对路径要依赖一个我们从未约定过的工作目录。
func pluginAbsPath(w http.ResponseWriter, raw string) (string, bool) {
	clean := strings.TrimSpace(raw)
	if clean == "" || !path.IsAbs(clean) {
		writeErr(w, http.StatusBadRequest, "path 必须是以 / 开头的绝对路径")
		return "", false
	}
	return path.Clean(clean), true
}

// pluginAPIPkg 在目标机上安装或卸载软件包。
//
// 走的是面板既有的环境流水线，而不是自己拼一条 apt install：装包必须提权、
// 必须可回滚、必须防并发，这三件事只有那条流水线有（hostLock + 快照 + 补偿）。
// 插件由此拿到的不是「能执行任意命令」，而是「能装包，并且装坏了能退回去」。
func (s *Server) pluginAPIPkg(w http.ResponseWriter, r *http.Request, body pluginAPIBody) {
	action := strings.TrimSpace(body.Action)
	if action != env.ActionInstall && action != env.ActionRemove {
		writeErr(w, http.StatusBadRequest, "action 只能是 install 或 remove")
		return
	}

	specs := make([]env.Spec, 0, len(body.Packages))
	for _, name := range body.Packages {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		specs = append(specs, env.Spec{Name: name})
	}
	if len(specs) == 0 {
		writeErr(w, http.StatusBadRequest, "packages 不能为空")
		return
	}
	if len(specs) > maxPluginPkgCount {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("单次最多 %d 个软件包", maxPluginPkgCount))
		return
	}

	exec, hostName, err := s.envTarget(body.HostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}

	events := &pluginPkgEvents{}
	s.envRunner.Run(r.Context(),
		env.Target{HostID: body.HostID, HostName: hostName, Exec: exec},
		env.RunRequest{Action: action, Specs: specs}, events)

	// Run 是同步的：跑完全部步骤并做完补偿之后才会返回，此时结果已经齐了。
	if events.err != nil {
		writeErr(w, http.StatusBadGateway, "软件包操作失败："+events.err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      events.summary != nil && events.summary.Status == store.EnvStatusSuccess,
		"steps":   events.steps,
		"summary": events.summary,
	})
}

// pluginPkgEvents 收集环境流水线的回调，供同步的插件回调一次性返回。
//
// 流水线本身是流式的（界面上要边跑边显示），而插件进程调面板只是一个同步 HTTP
// 请求。这里把过程记下来、跑完一并交回：插件侧不必自己实现事件通道，
// 但它仍然拿得到每一步的输出与退出码，失败原因不至于只剩一句"失败了"。
type pluginPkgEvents struct {
	mu      sync.Mutex
	steps   []env.StepResult
	summary *env.OpSummary
	err     error
}

func (e *pluginPkgEvents) StepStart(int, string) {}

func (e *pluginPkgEvents) StepLog(int, string, string) {}

func (e *pluginPkgEvents) StepEnd(result env.StepResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.steps = append(e.steps, result)
}

func (e *pluginPkgEvents) Finish(summary *env.OpSummary, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.summary, e.err = summary, err
}

// pluginAPIService 启停目标机上的服务。
//
// 它给的是「把已装好的服务的开关交出去」：单位名与动作都在白名单里，插件拼不出
// 任意命令 —— 这正是不直接给它提权执行的意义。
func (s *Server) pluginAPIService(w http.ResponseWriter, r *http.Request, body pluginAPIBody) {
	unit := strings.TrimSpace(body.Unit)
	if !serviceUnitPattern.MatchString(unit) {
		writeErr(w, http.StatusBadRequest, "unit 非法")
		return
	}

	action := strings.TrimSpace(body.Action)
	command, ok := serviceCommand(action, unit)
	if !ok {
		writeErr(w, http.StatusBadRequest,
			"action 只能是 enable、disable、start、stop、restart 或 reload")
		return
	}

	output, err := s.runEscalated(r.Context(), body.HostID, command, pluginServiceTimeout, escalateRequired)
	if err != nil {
		s.writeHostError(w, err, "操作服务失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "unit": unit, "action": action, "output": output,
	})
}

// serviceCommand 拼出启停服务的命令。
//
// 先 systemctl 再回退 service：目标机可能是 systemd，也可能是 OpenRC/SysV，
// 两条都试比"先探测 init 系统再决定"少一次往返。动作取自白名单、单位名逐字符
// 校验过，因此这条命令里不含插件可控的任意内容。
func serviceCommand(action, unit string) (string, bool) {
	fallback := "service " + unit + " " + action + " 2>/dev/null"

	switch action {
	case "enable":
		return "systemctl enable --now " + unit + " 2>/dev/null || service " + unit + " start 2>/dev/null", true
	case "disable":
		return "systemctl disable --now " + unit + " 2>/dev/null || service " + unit + " stop 2>/dev/null", true
	case "start", "stop", "restart", "reload":
		return "systemctl " + action + " " + unit + " 2>/dev/null || " + fallback, true
	}
	return "", false
}

// ClosePluginRuntimes 停掉全部插件后端进程，供面板退出时调用。
//
// 本地子进程有 Pdeathsig 兜底，但那只管到"面板进程消失"为止：面板被强杀或
// 机器断电时来不及收的进程，只能靠下次启动时的 pid 文件清理（见 RuntimeManager）。
func (s *Server) ClosePluginRuntimes() {
	if s.runtimes != nil {
		s.runtimes.Shutdown()
	}
}

// ClosePluginTunnels 关掉全部插件隧道与转发连接，供面板退出时调用。
func (s *Server) ClosePluginTunnels() {
	if s.pluginTunnels != nil {
		s.pluginTunnels.closeAll()
	}
}

// pluginAPITunnel 建立一条到目标机上的隧道。
//
// 契约见设计文档 5.6：remoteHost 限回环与私网，隧道归插件所有，
// 进程停止或插件卸载时无条件全部关闭。
func (s *Server) pluginAPITunnel(w http.ResponseWriter, item *store.Plugin, body pluginAPIBody) {
	tunnel, err := s.pluginTunnels.open(item.Org, item.Name, item.HostID, body.RemoteHost, body.RemotePort)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 每次开放都留痕：这是面板替插件开的门，事后必须能回答
	// "谁在什么时候把哪台机器的哪个端口连了出去"。
	s.notifySecurity(store.LevelInfo, "插件建立隧道："+item.DisplayName,
		fmt.Sprintf("%s · %s → 127.0.0.1:%d",
			s.hostLabel(item.HostID), tunnel.remoteAddr, tunnel.localPort()))

	writeJSON(w, http.StatusOK, map[string]any{
		"tunnelId":  tunnel.id,
		"localHost": "127.0.0.1",
		"localPort": tunnel.localPort(),
		// 隧道与插件进程同生命周期，进程一停就被无条件关掉，因此不承诺到期时间：
		// 给一个假的到期值只会让插件去做无用的续期。
		"expiresAt": nil,
	})
}

// pluginAPITunnelClose 关闭一条隧道。
func (s *Server) pluginAPITunnelClose(w http.ResponseWriter, item *store.Plugin, body pluginAPIBody) {
	tunnel, ok := s.pluginTunnels.get(strings.TrimSpace(body.TunnelID))
	if !ok {
		writeErr(w, http.StatusNotFound, "隧道不存在或已关闭")
		return
	}
	// 只能关自己开的那条。隧道 id 不可枚举，归属检查是第二道闸。
	if tunnel.org != item.Org || tunnel.name != item.Name {
		writeErrCode(w, http.StatusForbidden, codePluginForbidden, "隧道不属于该插件")
		return
	}

	s.pluginTunnels.close(tunnel)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
