package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperEnv 让测试二进制以"插件进程"的身份重入。
//
// 运行时就该用真实进程验证：socket 权限、pid 文件、环境变量注入、进程组回收
// 都只在真实进程上成立，用 mock 替换掉它们等于把要测的东西测没了。
const helperEnv = "PANEL_PLUGIN_TEST_MODE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		runHelper(mode)
		return
	}
	os.Exit(m.Run())
}

// runHelper 扮演插件后端进程。
func runHelper(mode string) {
	switch mode {
	case "ok":
		serveHelper(handleHelperRPC)
	case "unhealthy":
		// 监听 socket 但从不给出合法响应，用于验证就绪超时。
		serveHelper(func(http.ResponseWriter, *http.Request) {})
	case "crash":
		// 起了就崩，用于验证崩溃后的退避重启。
		os.Exit(1)
	}
	os.Exit(0)
}

func serveHelper(handler http.HandlerFunc) {
	socket := os.Getenv("PANEL_PLUGIN_SOCKET")
	if socket == "" {
		os.Exit(2)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.Exit(3)
	}
	// 一直服务到被面板终止：SIGTERM 的默认行为就是结束进程。
	_ = (&http.Server{Handler: handler}).Serve(listener)
}

func handleHelperRPC(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "health":
		writeHelperResult(w, req.ID, map[string]any{"ok": true})
	case "echo":
		params := string(req.Params)
		if params == "" {
			params = "null"
		}
		// 原样回显收到的 params，用于确认参数确实传到了插件手里。
		writeHelperRaw(w, req.ID, `{"params":`+params+`}`)
	case "identity":
		// 模拟插件读自己的令牌文件：证明面板注入的路径与文件权限真的可用。
		raw, err := os.ReadFile(os.Getenv("PANEL_PLUGIN_TOKEN_FILE"))
		if err != nil {
			writeHelperError(w, req.ID, err.Error())
			return
		}
		writeHelperRaw(w, req.ID, `{"token":`+strconv.Quote(strings.TrimSpace(string(raw)))+`}`)
	case "boom":
		writeHelperError(w, req.ID, "插件内部错误")
	default:
		writeHelperError(w, req.ID, "没有这个方法")
	}
}

func writeHelperResult(w http.ResponseWriter, id int, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	writeHelperRaw(w, id, string(raw))
}

func writeHelperRaw(w http.ResponseWriter, id int, raw string) {
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
		`,"result":` + raw + `}`))
}

func writeHelperError(w http.ResponseWriter, id int, message string) {
	raw, err := json.Marshal(message)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
		`,"error":{"code":-32601,"message":` + string(raw) + `}}`))
}

// writePluginProcess 在插件目录里放一个插件进程的替身。
//
// 替身是一个 shell 脚本，它把测试二进制以 helper 模式 exec 掉：
// 这样面板 exec 的是一个真实可执行文件，而它又能说 JSON-RPC。
func writePluginProcess(t *testing.T, pluginsRoot, org, name, version, mode string) {
	t.Helper()

	dir := filepath.Join(pluginsRoot, org, name, version, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建插件目录失败：%v", err)
	}

	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("取测试二进制路径失败：%v", err)
	}

	script := "#!/bin/sh\n" + helperEnv + "=" + mode + " exec '" + self + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "svc"), []byte(script), 0o755); err != nil {
		t.Fatalf("写入插件进程替身失败：%v", err)
	}
}

// testSpec 是一份已校验过的运行时声明，指向替身可执行文件。
// 走字面量而不是 Normalize：这里测的是运行进程，不是清单校验。
func testSpec(stopTimeoutSec int) *RuntimeSpec {
	autoStart := true
	return &RuntimeSpec{
		Kind:           RuntimeProcess,
		Exec:           "bin/svc",
		Socket:         defaultRuntimeSocket,
		Protocol:       ProtocolJSONRPC,
		AutoStart:      &autoStart,
		HealthPath:     defaultRuntimeHealth,
		StopTimeoutSec: stopTimeoutSec,
	}
}

// testTimings 把三个等待压到毫秒级：就绪超时、退避重启、空闲回收都是必须
// 验证的行为，按生产值走一遍要好几分钟。
func testTimings() RuntimeTimings {
	return RuntimeTimings{
		ReadyTimeout: 2 * time.Second,
		RestartDelay: 20 * time.Millisecond,
		IdleScan:     50 * time.Millisecond,
	}
}

// shortTempDir 返回一个短路径的临时目录。
//
// 不能用 t.TempDir()：它的目录名里带着测试名，而 Unix socket 的路径上限只有
// 108 字节。长测试名会让 socket 根本建不起来，那时测的就不是运行时，
// 而是一个临时目录命名问题。
func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "psp")
	if err != nil {
		t.Fatalf("创建临时目录失败：%v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newTestManager(t *testing.T, pluginsRoot string) (*RuntimeManager, string) {
	t.Helper()
	return newTestManagerWithTimings(t, pluginsRoot, testTimings())
}

func newTestManagerWithTimings(t *testing.T, pluginsRoot string, timings RuntimeTimings) (*RuntimeManager, string) {
	t.Helper()

	runtimeRoot := filepath.Join(shortTempDir(t), "plugins-runtime")
	manager := NewRuntimeManagerWithTimings(pluginsRoot, runtimeRoot, timings)
	t.Cleanup(manager.Shutdown)
	return manager, runtimeRoot
}

func waitForStatus(t *testing.T, manager *RuntimeManager, org, name string,
	done func(RuntimeStatus) bool, timeout time.Duration) RuntimeStatus {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last RuntimeStatus
	for time.Now().Before(deadline) {
		last = manager.Status(org, name)
		if done(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待状态超时，最后状态：%+v", last)
	return last
}

// processGone 判断进程是否真的不在了。
func processGone(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) != nil }

// waitProcessGone 等进程真正退出。
//
// 停止流程可能由后台协程发起（空闲回收），此时状态先变成 stopped、
// 进程随后才退出，只断言状态会把"进程还在"漏过去。
func waitProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("进程 %d 在 %s 内没有退出", pid, timeout)
}

func TestRuntimeManagerRunsProcess(t *testing.T) {
	pluginsRoot := t.TempDir()
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "ok")
	manager, runtimeRoot := newTestManager(t, pluginsRoot)

	if err := manager.EnsureRunning(testSpec(5), "panelssh", "demo", "1.0.0"); err != nil {
		t.Fatalf("启动插件进程失败：%v", err)
	}

	status := manager.Status("panelssh", "demo")
	if status.State != StateRunning {
		t.Fatalf("状态 = %s（%s），期望 running", status.State, status.Message)
	}

	socket := manager.SocketPath("panelssh", "demo")
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("socket 不存在：%v", err)
	}
	// 同机其他用户若能连上这个 socket，就能绕开面板直接调用插件。
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket 权限 = %o，期望 600", perm)
	}

	// pid 文件与 Pdeathsig 是两道独立保险。
	raw, err := os.ReadFile(filepath.Join(runtimeRoot, "panelssh", "demo", pidFileName))
	if err != nil {
		t.Fatalf("pid 文件不存在：%v", err)
	}
	if strings.TrimSpace(string(raw)) != strconv.Itoa(status.PID) {
		t.Fatalf("pid 文件内容 = %q，期望 %d", raw, status.PID)
	}

	// 面板到插件的通道确实通了，这才是"运行中"的实际含义。
	result, err := rpcCall(context.Background(), socket, "echo", json.RawMessage(`{"hostId":7}`))
	if err != nil {
		t.Fatalf("调用插件失败：%v", err)
	}
	if !strings.Contains(string(result), `"hostId":7`) {
		t.Fatalf("插件返回 %s，参数没有传过去", result)
	}

	// 令牌是插件反向调用面板的唯一凭据：它必须落地、必须是 0600，
	// 而且插件按面板给的路径真的读得到。
	tokenPath := filepath.Join(runtimeRoot, "panelssh", "demo", tokenFileName)
	tokenInfo, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("令牌文件不存在：%v", err)
	}
	if perm := tokenInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("令牌文件权限 = %o，期望 600", perm)
	}

	identity, err := rpcCall(context.Background(), socket, "identity", nil)
	if err != nil {
		t.Fatalf("读取插件身份失败：%v", err)
	}
	var read struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(identity, &read); err != nil {
		t.Fatalf("解析插件身份失败：%v", err)
	}
	if read.Token == "" {
		t.Fatal("插件没有读到自己的令牌文件")
	}

	org, name, ok := manager.LookupToken(read.Token)
	if !ok || org != "panelssh" || name != "demo" {
		t.Fatalf("令牌查到的身份 = %s/%s（%v），期望 panelssh/demo", org, name, ok)
	}

	pid := status.PID
	manager.Stop("panelssh", "demo")

	if after := manager.Status("panelssh", "demo"); after.State != StateStopped {
		t.Fatalf("停止后状态 = %s，期望 stopped", after.State)
	}
	if !processGone(pid) {
		t.Fatalf("停止后进程 %d 仍在运行", pid)
	}
	// 停止的插件不该还能拿旧凭据调面板能力。
	if _, _, ok := manager.LookupToken(read.Token); ok {
		t.Fatal("进程停止后旧令牌应当立即失效")
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("停止后令牌文件应被清掉，Stat 返回 %v", err)
	}
}

func TestTokenStoreReissueInvalidatesOldToken(t *testing.T) {
	store := newTokenStore()
	dataDir := t.TempDir()

	first, err := store.issue("panelssh", "demo", dataDir)
	if err != nil {
		t.Fatalf("签发令牌失败：%v", err)
	}
	second, err := store.issue("panelssh", "demo", dataDir)
	if err != nil {
		t.Fatalf("重新签发令牌失败：%v", err)
	}

	if first == second {
		t.Fatal("重新签发应当得到一个不同的令牌")
	}
	if _, ok := store.lookup(first); ok {
		t.Fatal("重新签发后旧令牌应当失效")
	}
	if identity, ok := store.lookup(second); !ok || identity.Org != "panelssh" || identity.Name != "demo" {
		t.Fatalf("新令牌应能查到身份，实际 %+v（%v）", identity, ok)
	}

	store.revoke("panelssh", "demo")
	if _, ok := store.lookup(second); ok {
		t.Fatal("撤销后令牌应当失效")
	}
}

func TestRuntimeManagerRetriesAfterCrash(t *testing.T) {
	pluginsRoot := t.TempDir()
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "crash")
	manager, _ := newTestManager(t, pluginsRoot)

	if err := manager.EnsureRunning(testSpec(5), "panelssh", "demo", "1.0.0"); err == nil {
		t.Fatal("进程起了就崩，启动不应报成功")
	}

	// 重试是异步的：等它试完上限次数后停下来。
	status := waitForStatus(t, manager, "panelssh", "demo", func(s RuntimeStatus) bool {
		return s.Failures > restartLimit
	}, 5*time.Second)

	if status.State != StateFailed {
		t.Fatalf("状态 = %s，期望 failed", status.State)
	}
	if !strings.Contains(status.Message, "不再自动重试") {
		t.Fatalf("失败信息 = %q，期望说明不再重试", status.Message)
	}
}

func TestRuntimeManagerReportsReadyTimeout(t *testing.T) {
	pluginsRoot := t.TempDir()
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "unhealthy")

	timings := testTimings()
	timings.ReadyTimeout = 300 * time.Millisecond
	manager, _ := newTestManagerWithTimings(t, pluginsRoot, timings)

	err := manager.EnsureRunning(testSpec(2), "panelssh", "demo", "1.0.0")
	if err == nil {
		t.Fatal("进程始终不就绪，启动不应报成功")
	}
	if !strings.Contains(err.Error(), "就绪") {
		t.Fatalf("错误 = %q，期望说明未就绪", err.Error())
	}
}

func TestRuntimeManagerReapsIdleProcess(t *testing.T) {
	pluginsRoot := t.TempDir()
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "ok")
	manager, _ := newTestManager(t, pluginsRoot)

	spec := testSpec(5)
	spec.IdleStopSec = 1
	if err := manager.EnsureRunning(spec, "panelssh", "demo", "1.0.0"); err != nil {
		t.Fatalf("启动插件进程失败：%v", err)
	}
	pid := manager.Status("panelssh", "demo").PID

	waitForStatus(t, manager, "panelssh", "demo", func(s RuntimeStatus) bool {
		return s.State == StateStopped
	}, 5*time.Second)

	waitProcessGone(t, pid, 3*time.Second)
}

func TestRuntimeManagerSwitchesVersion(t *testing.T) {
	pluginsRoot := t.TempDir()
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "ok")
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "2.0.0", "ok")
	manager, _ := newTestManager(t, pluginsRoot)

	if err := manager.EnsureRunning(testSpec(5), "panelssh", "demo", "1.0.0"); err != nil {
		t.Fatalf("启动 1.0.0 失败：%v", err)
	}
	first := manager.Status("panelssh", "demo").PID

	// 装了另一个版本：一个插件只有一条进程，应当换成新版本。
	if err := manager.EnsureRunning(testSpec(5), "panelssh", "demo", "2.0.0"); err != nil {
		t.Fatalf("启动 2.0.0 失败：%v", err)
	}

	status := manager.Status("panelssh", "demo")
	if status.Version != "2.0.0" {
		t.Fatalf("版本 = %q，期望 2.0.0", status.Version)
	}
	if status.PID == first {
		t.Fatal("换版本后应当是一个新的进程")
	}
	if !processGone(first) {
		t.Fatalf("旧版本的进程 %d 没有被收掉", first)
	}
}

func TestRuntimeManagerRemoveDeletesDataDir(t *testing.T) {
	pluginsRoot := t.TempDir()
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "ok")
	manager, runtimeRoot := newTestManager(t, pluginsRoot)

	if err := manager.EnsureRunning(testSpec(5), "panelssh", "demo", "1.0.0"); err != nil {
		t.Fatalf("启动插件进程失败：%v", err)
	}
	pid := manager.Status("panelssh", "demo").PID

	dataDir := filepath.Join(runtimeRoot, "panelssh", "demo")
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("运行时目录应当存在：%v", err)
	}

	if err := manager.Remove("panelssh", "demo"); err != nil {
		t.Fatalf("卸载清理失败：%v", err)
	}

	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("运行时目录应当被删除，Stat 返回 %v", err)
	}
	if !processGone(pid) {
		t.Fatalf("卸载后进程 %d 仍在运行", pid)
	}
}

// 孤儿防护：面板被强杀（SIGKILL / 断电）时子进程未必跟着走，
// 下一次启动要按 pid 文件把这些残留收掉。
func TestRuntimeManagerCleansUpOrphans(t *testing.T) {
	pluginsRoot := t.TempDir()
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "ok")
	runtimeRoot := filepath.Join(t.TempDir(), "plugins-runtime")

	// 起好进程后不停止，直接丢掉这个管理器 —— 这就是面板被强杀后的现场。
	// 它的空闲回收协程会一直留着，随测试二进制退出一起结束。
	crashed := NewRuntimeManager(pluginsRoot, runtimeRoot)
	if err := crashed.EnsureRunning(testSpec(5), "panelssh", "demo", "1.0.0"); err != nil {
		t.Fatalf("启动插件进程失败：%v", err)
	}
	pid := crashed.Status("panelssh", "demo").PID

	// 新实例启动时会按 pid 文件把上一个实例留下的进程收掉。
	fresh := NewRuntimeManager(pluginsRoot, runtimeRoot)
	t.Cleanup(fresh.Shutdown)

	waitProcessGone(t, pid, 3*time.Second)
}

func TestRPCCallSurfacesPluginError(t *testing.T) {
	pluginsRoot := t.TempDir()
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "ok")
	manager, _ := newTestManager(t, pluginsRoot)

	if err := manager.EnsureRunning(testSpec(5), "panelssh", "demo", "1.0.0"); err != nil {
		t.Fatalf("启动插件进程失败：%v", err)
	}

	_, err := rpcCall(context.Background(), manager.SocketPath("panelssh", "demo"), "boom", nil)

	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("错误 = %v，期望 RPCError", err)
	}
	if rpcErr.Message != "插件内部错误" {
		t.Fatalf("错误信息 = %q，期望原样透传", rpcErr.Message)
	}
}

func TestRPCCallDetectsMissingProcess(t *testing.T) {
	_, err := rpcCall(context.Background(), filepath.Join(t.TempDir(), "nope.sock"), "health", nil)
	if !errors.Is(err, ErrPluginNotRunning) {
		t.Fatalf("错误 = %v，期望 ErrPluginNotRunning", err)
	}
}

// socket 路径超过内核上限时，必须给出人话：否则插件进程只会以一个看不懂的
// 退出码死掉，而排查方向会被引到插件自己身上去。
func TestRuntimeManagerRejectsOverlongSocketPath(t *testing.T) {
	pluginsRoot := shortTempDir(t)
	writePluginProcess(t, pluginsRoot, "panelssh", "demo", "1.0.0", "ok")

	deep := filepath.Join(shortTempDir(t), strings.Repeat("d", 80), strings.Repeat("e", 80))
	manager := NewRuntimeManagerWithTimings(pluginsRoot, deep, testTimings())
	t.Cleanup(manager.Shutdown)

	err := manager.EnsureRunning(testSpec(5), "panelssh", "demo", "1.0.0")
	if err == nil {
		t.Fatal("socket 路径超限时不应报成功")
	}
	if !strings.Contains(err.Error(), "socket 路径过长") {
		t.Fatalf("错误 = %q，期望指明 socket 路径过长", err.Error())
	}
}
