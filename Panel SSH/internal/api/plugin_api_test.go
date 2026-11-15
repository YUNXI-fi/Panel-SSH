package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"panelssh/internal/plugin"
	"panelssh/internal/store"
)

// apiTestModeEnv 让测试二进制以"插件后端进程"的身份重入。
//
// 转发与回调这两条接口的鉴权链要靠一枚"真实签发出来的令牌"才有意义，
// 而令牌只随进程启动签发（见 plugin.RuntimeManager）。因此这里起一个真进程，
// 而不是在测试里伪造一份凭据 —— 伪造出来的那条路径生产上并不存在。
const apiTestModeEnv = "PANEL_API_TEST_MODE"

func TestMain(m *testing.M) {
	if os.Getenv(apiTestModeEnv) == "1" {
		servePluginHelper()
		return
	}
	os.Exit(m.Run())
}

// servePluginHelper 扮演插件后端：在面板给的 socket 上回答 JSON-RPC。
func servePluginHelper() {
	socket := os.Getenv("PANEL_PLUGIN_SOCKET")
	if socket == "" {
		os.Exit(2)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(3)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		params := string(req.Params)
		if params == "" {
			params = "null"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(req.ID) +
			`,"result":{"method":` + strconv.Quote(req.Method) + `,"params":` + params + `}}`))
	})

	_ = (&http.Server{Handler: handler}).Serve(listener)
}

// writeTestPlugin 在插件目录里放一份可用的插件：清单（带 runtime 段）+ 可执行的替身。
func writeTestPlugin(t *testing.T, pluginsRoot, org, name, version string) {
	t.Helper()

	dir := filepath.Join(pluginsRoot, org, name, version)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("创建插件目录失败：%v", err)
	}

	manifest := map[string]any{
		"org":     org,
		"name":    name,
		"version": version,
		"entry":   "index.html",
		"capabilities": []string{
			plugin.CapHostList, plugin.CapHostExec,
		},
		"runtime": map[string]any{
			"kind":       "process",
			"exec":       "bin/svc",
			"socket":     "app.sock",
			"healthPath": "health",
		},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("序列化清单失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatalf("写入清单失败：%v", err)
	}

	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("取测试二进制路径失败：%v", err)
	}
	script := "#!/bin/sh\n" + apiTestModeEnv + "=1 exec '" + self + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "bin", "svc"), []byte(script), 0o755); err != nil {
		t.Fatalf("写入插件进程替身失败：%v", err)
	}
}

// shortTempDir 返回一个短路径的临时目录。
//
// 不能用 t.TempDir()：它的目录名里带着测试名，而 Unix socket 的路径上限只有
// 108 字节，长测试名会让 socket 根本建不起来。这个约束在生产里同样存在 ——
// 面板的数据目录太深时也会踩到，见 plugin 包对 socket 路径长度的检查。
func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "psa")
	if err != nil {
		t.Fatalf("创建临时目录失败：%v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// newPluginServer 构造一个只挂上插件接口所需依赖的 Server。
//
// 安装包目录与运行时目录放在同一个临时根下，测试据此直接定位数据目录。
func newPluginServer(t *testing.T) (*Server, string) {
	t.Helper()

	base := shortTempDir(t)
	pluginsRoot := filepath.Join(base, "plugins")
	s := &Server{
		store:      newTestStore(t),
		pluginsDir: pluginsRoot,
		runtimes:   plugin.NewRuntimeManager(pluginsRoot, filepath.Join(base, "plugins-runtime")),
	}
	s.pluginTunnels = newPluginTunnels(s)
	t.Cleanup(s.runtimes.Shutdown)
	return s, pluginsRoot
}

// installTestPlugin 写入一条已启用的插件记录。
func installTestPlugin(t *testing.T, s *Server, org, name, version string, granted []string) *store.Plugin {
	t.Helper()

	item, err := s.store.UpsertPlugin(store.PluginInput{
		Org:          org,
		Name:         name,
		DisplayName:  "示例应用",
		Version:      version,
		Entry:        "index.html",
		Source:       plugin.SourceArchive,
		Capabilities: []string{plugin.CapHostList, plugin.CapHostExec},
		Granted:      granted,
		HostID:       LocalHostID,
	})
	if err != nil {
		t.Fatalf("写入插件记录失败：%v", err)
	}
	return item
}

// startTestProcess 拉起插件进程并返回它拿到的令牌，用来模拟插件回调面板。
func startTestProcess(t *testing.T, s *Server, org, name, version string) string {
	t.Helper()

	spec := &plugin.RuntimeSpec{
		Kind:           plugin.RuntimeProcess,
		Exec:           "bin/svc",
		Socket:         "app.sock",
		HealthPath:     "health",
		StopTimeoutSec: 5,
	}
	if err := s.runtimes.EnsureRunning(spec, org, name, version); err != nil {
		t.Fatalf("启动插件进程失败：%v", err)
	}

	raw, err := os.ReadFile(filepath.Join(
		filepath.Dir(s.pluginsDir), "plugins-runtime", org, name, "token"))
	if err != nil {
		t.Fatalf("读取令牌失败：%v", err)
	}
	return strings.TrimSpace(string(raw))
}

func TestPluginRPCRejectsUninstalledHost(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/plugins/panelssh/demo/rpc/sql.query",
		strings.NewReader(`{"hostId":7,"params":{}}`))
	req.SetPathValue("org", "panelssh")
	req.SetPathValue("name", "demo")
	req.SetPathValue("method", "sql.query")

	rec := httptest.NewRecorder()
	s.handlePluginRPC(rec, req)

	// 插件只装了宿主机，却要求操作主机 7：这是越权，不是功能。
	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d（%s），期望 403", rec.Code, rec.Body.String())
	}
}

func TestPluginRPCRejectsInvalidMethod(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/plugins/panelssh/demo/rpc/x",
		strings.NewReader(`{"hostId":0,"params":{}}`))
	req.SetPathValue("org", "panelssh")
	req.SetPathValue("name", "demo")
	req.SetPathValue("method", "../etc/passwd")

	rec := httptest.NewRecorder()
	s.handlePluginRPC(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
}

func TestPluginRPCRejectsPluginWithoutRuntime(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	// 只放清单，不放 runtime 段：这是纯静态插件，没有后端进程可转发。
	dir := filepath.Join(pluginsRoot, "panelssh", "static", "1.0.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建插件目录失败：%v", err)
	}
	raw := `{"org":"panelssh","name":"static","version":"1.0.0","entry":"index.html"}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("写入清单失败：%v", err)
	}
	installTestPlugin(t, s, "panelssh", "static", "1.0.0", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/plugins/panelssh/static/rpc/anything",
		strings.NewReader(`{"hostId":0,"params":{}}`))
	req.SetPathValue("org", "panelssh")
	req.SetPathValue("name", "static")
	req.SetPathValue("method", "anything")

	rec := httptest.NewRecorder()
	s.handlePluginRPC(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d（%s），期望 400", rec.Code, rec.Body.String())
	}
}

// 端到端：面板把调用转给插件进程，并把 hostId 作为调用上下文一并送进去。
func TestPluginRPCForwardsToProcess(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", nil)

	req := httptest.NewRequest(http.MethodPost, "/api/plugins/panelssh/demo/rpc/sql.query",
		strings.NewReader(`{"hostId":0,"params":{"sql":"select 1"}}`))
	req.SetPathValue("org", "panelssh")
	req.SetPathValue("name", "demo")
	req.SetPathValue("method", "sql.query")

	rec := httptest.NewRecorder()
	s.handlePluginRPC(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s），期望 200", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"sql.query"`) {
		t.Fatalf("响应 %s 里没有回显方法名，转发链路不对", body)
	}
	if !strings.Contains(body, `"hostId":0`) {
		t.Fatalf("响应 %s 里没有 hostId，调用上下文没有注入", body)
	}
}

func TestPluginAPIRejectsMissingToken(t *testing.T) {
	s, _ := newPluginServer(t)

	req := httptest.NewRequest(http.MethodPost, pluginAPIPrefix+"/host.exec",
		strings.NewReader(`{"hostId":0,"command":"id"}`))
	req.SetPathValue("method", "host.exec")

	rec := httptest.NewRecorder()
	s.handlePluginAPI(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d（%s），期望 401", rec.Code, rec.Body.String())
	}
}

func TestPluginAPIRejectsUnknownMethod(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", []string{plugin.CapHostExec})

	token := startTestProcess(t, s, "panelssh", "demo", "1.0.0")

	// 接口表是白名单：不在表里的方法连能力映射都没有。
	req := httptest.NewRequest(http.MethodPost, pluginAPIPrefix+"/host.reboot",
		strings.NewReader(`{"hostId":0}`))
	req.SetPathValue("method", "host.reboot")
	req.Header.Set(pluginTokenHeader, token)

	rec := httptest.NewRecorder()
	s.handlePluginAPI(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d（%s），期望 404", rec.Code, rec.Body.String())
	}
}

func TestPluginAPIRejectsUngrantedCapability(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	// 只授了读主机清单，没授执行命令。
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", []string{plugin.CapHostList})

	token := startTestProcess(t, s, "panelssh", "demo", "1.0.0")

	req := httptest.NewRequest(http.MethodPost, pluginAPIPrefix+"/host.exec",
		strings.NewReader(`{"hostId":0,"command":"rm -rf /"}`))
	req.SetPathValue("method", "host.exec")
	req.Header.Set(pluginTokenHeader, token)

	rec := httptest.NewRecorder()
	s.handlePluginAPI(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d（%s），期望 403", rec.Code, rec.Body.String())
	}

	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误体失败：%v", err)
	}
	if body.Code != codePluginForbidden {
		t.Fatalf("错误码 = %q，期望 %q", body.Code, codePluginForbidden)
	}
}

func TestPluginAPIListsOwnHostOnly(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", []string{plugin.CapHostList})

	token := startTestProcess(t, s, "panelssh", "demo", "1.0.0")

	req := httptest.NewRequest(http.MethodPost, pluginAPIPrefix+"/host.list",
		strings.NewReader(`{"hostId":0}`))
	req.SetPathValue("method", "host.list")
	req.Header.Set(pluginTokenHeader, token)

	rec := httptest.NewRecorder()
	s.handlePluginAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s），期望 200", rec.Code, rec.Body.String())
	}

	var body struct {
		Hosts []struct {
			ID int64 `json:"id"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(body.Hosts) != 1 || body.Hosts[0].ID != LocalHostID {
		t.Fatalf("只应返回插件自己所在的那台主机，实际 %+v", body.Hosts)
	}
}

// 授权被撤销后，插件手里那枚还在有效期内的令牌不该还能用 ——
// 这也是权限不缓存进令牌的意义所在。
func TestPluginAPIRevokedCapabilityTakesEffectImmediately(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	item := installTestPlugin(t, s, "panelssh", "demo", "1.0.0",
		[]string{plugin.CapHostList, plugin.CapHostExec})

	token := startTestProcess(t, s, "panelssh", "demo", "1.0.0")

	if err := s.store.SetPluginGranted(item.ID, []string{plugin.CapHostList}); err != nil {
		t.Fatalf("撤销授权失败：%v", err)
	}

	req := httptest.NewRequest(http.MethodPost, pluginAPIPrefix+"/host.exec",
		strings.NewReader(`{"hostId":0,"command":"id"}`))
	req.SetPathValue("method", "host.exec")
	req.Header.Set(pluginTokenHeader, token)

	rec := httptest.NewRecorder()
	s.handlePluginAPI(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d（%s），期望 403", rec.Code, rec.Body.String())
	}
}

// pluginAPICall 以插件身份调一次回调接口，返回状态码。
func pluginAPICall(t *testing.T, s *Server, token, method, body string) int {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, pluginAPIPrefix+"/"+method, strings.NewReader(body))
	req.SetPathValue("method", method)
	req.Header.Set(pluginTokenHeader, token)

	rec := httptest.NewRecorder()
	s.handlePluginAPI(rec, req)
	return rec.Code
}

// 单位名会被拼进一条以 root 执行的命令，因此必须逐字符卡死。
// 下面这些都是真实的注入尝试，一个都不该通过。
func TestPluginServiceRejectsUnsafeUnit(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", []string{plugin.CapHostService})
	token := startTestProcess(t, s, "panelssh", "demo", "1.0.0")

	badUnits := []string{
		"nginx; rm -rf /",
		"nginx && reboot",
		"nginx$(id)",
		"nginx`id`",
		"../../etc/passwd",
		"nginx.service --now",
		"",
	}
	for _, unit := range badUnits {
		body := `{"hostId":0,"unit":` + strconv.Quote(unit) + `,"action":"start"}`
		if code := pluginAPICall(t, s, token, "host.service", body); code != http.StatusBadRequest {
			t.Fatalf("unit %q 应被拒绝，实际状态码 %d", unit, code)
		}
	}

	// 动作同样在白名单里，不认识的动作不该走到命令拼装那一步。
	if code := pluginAPICall(t, s, token, "host.service",
		`{"hostId":0,"unit":"nginx","action":"rm -rf /"}`); code != http.StatusBadRequest {
		t.Fatalf("未知动作应被拒绝，实际状态码 %d", code)
	}
}

func TestServiceCommandIsWhitelisted(t *testing.T) {
	cases := map[string]string{
		"start": "systemctl start nginx 2>/dev/null || service nginx start 2>/dev/null",
		"stop":  "systemctl stop nginx 2>/dev/null || service nginx stop 2>/dev/null",
		"enable": "systemctl enable --now nginx 2>/dev/null || " +
			"service nginx start 2>/dev/null",
		"disable": "systemctl disable --now nginx 2>/dev/null || " +
			"service nginx stop 2>/dev/null",
	}

	for action, want := range cases {
		got, ok := serviceCommand(action, "nginx")
		if !ok {
			t.Fatalf("动作 %s 应当被接受", action)
		}
		if got != want {
			t.Fatalf("动作 %s 的命令 = %q，期望 %q", action, got, want)
		}
	}

	if _, ok := serviceCommand("explode", "nginx"); ok {
		t.Fatal("未知动作不该生成命令")
	}
}

func TestPluginPkgRejectsBadInput(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", []string{plugin.CapHostPkg})
	token := startTestProcess(t, s, "panelssh", "demo", "1.0.0")

	// 只测参数校验：这些都该在触碰目标机之前就被拒掉。
	cases := []struct {
		reason string
		body   string
	}{
		{"未知动作", `{"hostId":0,"action":"purge","packages":["nginx"]}`},
		{"包列表为空", `{"hostId":0,"action":"install","packages":[]}`},
		{"包列表全是空串", `{"hostId":0,"action":"install","packages":["","  "]}`},
	}

	many := make([]string, maxPluginPkgCount+1)
	for i := range many {
		many[i] = "pkg" + strconv.Itoa(i)
	}
	raw, err := json.Marshal(many)
	if err != nil {
		t.Fatalf("序列化包列表失败：%v", err)
	}
	cases = append(cases, struct{ reason, body string }{
		"包数量超限", `{"hostId":0,"action":"install","packages":` + string(raw) + `}`,
	})

	for _, tc := range cases {
		if code := pluginAPICall(t, s, token, "host.pkg", tc.body); code != http.StatusBadRequest {
			t.Fatalf("%s：状态码 = %d，期望 400", tc.reason, code)
		}
	}
}

// 隧道目标只允许 IP 字面量里的回环与私网地址。
func TestPluginTunnelHostAllowlist(t *testing.T) {
	allowed := []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.10"}
	for _, host := range allowed {
		if !pluginTunnelHostAllowed(host) {
			t.Errorf("%s 应被允许", host)
		}
	}

	// 公网地址、域名、以及任何带杂质的串都不行：隧道只用来连目标机上
	// "只监听回环的服务"，放开公网地址等于给了插件一个借目标机出网的跳板。
	blocked := []string{
		"8.8.8.8",
		"1.1.1.1",
		"example.com",
		"localhost",
		"",
		"127.0.0.1; rm -rf /",
	}
	for _, host := range blocked {
		if pluginTunnelHostAllowed(host) {
			t.Errorf("%s 应被拒绝", host)
		}
	}
}

func TestPluginTunnelRejectsBadTarget(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", []string{plugin.CapHostTunnel})
	token := startTestProcess(t, s, "panelssh", "demo", "1.0.0")

	cases := []struct {
		reason string
		body   string
	}{
		{"宿主机不需要隧道", `{"hostId":0,"remoteHost":"127.0.0.1","remotePort":3306}`},
		{"公网地址", `{"hostId":0,"remoteHost":"8.8.8.8","remotePort":3306}`},
		{"域名而非 IP", `{"hostId":0,"remoteHost":"db.internal","remotePort":3306}`},
		{"端口越界", `{"hostId":0,"remoteHost":"127.0.0.1","remotePort":70000}`},
	}

	for _, tc := range cases {
		if code := pluginAPICall(t, s, token, "host.tunnel", tc.body); code != http.StatusBadRequest {
			t.Fatalf("%s：状态码 = %d，期望 400", tc.reason, code)
		}
	}
}

// 隧道关闭必须校验归属：id 是关闭凭据，别人的隧道不该能被关掉。
func TestPluginTunnelCloseChecksOwnership(t *testing.T) {
	s, pluginsRoot := newPluginServer(t)
	writeTestPlugin(t, pluginsRoot, "panelssh", "demo", "1.0.0")
	installTestPlugin(t, s, "panelssh", "demo", "1.0.0", []string{plugin.CapHostTunnel})
	token := startTestProcess(t, s, "panelssh", "demo", "1.0.0")

	if code := pluginAPICall(t, s, token, "host.tunnel.close",
		`{"hostId":0,"tunnelId":"nope"}`); code != http.StatusNotFound {
		t.Fatalf("未知隧道：状态码 = %d，期望 404", code)
	}

	// 手工放两条隧道进去：这里要验证的是归属判断与关闭动作，
	// 它们只用到归属信息，不必真的接到目标机（那需要一台真实主机）。
	listen := func() net.Listener {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("监听失败：%v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		return listener
	}
	s.pluginTunnels.tunnels["other-id"] = &pluginTunnel{
		id: "other-id", org: "someone", name: "else", listener: listen(),
	}
	s.pluginTunnels.tunnels["own-id"] = &pluginTunnel{
		id: "own-id", org: "panelssh", name: "demo", listener: listen(),
	}

	if code := pluginAPICall(t, s, token, "host.tunnel.close",
		`{"hostId":0,"tunnelId":"other-id"}`); code != http.StatusForbidden {
		t.Fatalf("他人的隧道：状态码 = %d，期望 403", code)
	}

	if code := pluginAPICall(t, s, token, "host.tunnel.close",
		`{"hostId":0,"tunnelId":"own-id"}`); code != http.StatusOK {
		t.Fatalf("自己的隧道：状态码 = %d，期望 200", code)
	}
	// 关掉之后再关一次应当报"不存在"，说明它真的从表里摘掉了。
	if code := pluginAPICall(t, s, token, "host.tunnel.close",
		`{"hostId":0,"tunnelId":"own-id"}`); code != http.StatusNotFound {
		t.Fatalf("重复关闭：状态码 = %d，期望 404", code)
	}
}

// 插件回调不该受 CSRF 检查约束：它的凭据来自文件，不是浏览器自动携带的 Cookie。
func TestCSRFExemptsPluginAPIPrefix(t *testing.T) {
	var reached bool
	handler := withCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, pluginAPIPrefix+"/host.exec", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !reached {
		t.Fatalf("插件回调被 CSRF 检查拦下：%d %s", rec.Code, rec.Body.String())
	}

	// 其余写接口仍然要拦。
	req = httptest.NewRequest(http.MethodPost, "/api/hosts", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通写接口状态码 = %d，期望 403", rec.Code)
	}
}
