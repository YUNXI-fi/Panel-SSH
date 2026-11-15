package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

// normalizeManifest 构造一份带 runtime 段的清单并跑校验。
// 走 json.Marshal/Unmarshal 而不是直接拼结构体，是为了让字段名拼写本身也被测到：
// 清单是外部输入，json tag 写错等于字段永远读不进来。
func normalizeManifest(t *testing.T, runtime any) (*Manifest, error) {
	t.Helper()

	body := baseManifest("1.0.0")
	if runtime != nil {
		body["runtime"] = runtime
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化清单失败：%v", err)
	}

	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("反序列化清单失败：%v", err)
	}
	return manifest.Normalize()
}

// normalizedRuntime 校验后取回 runtime 段，校验失败直接终止用例。
func normalizedRuntime(t *testing.T, runtime map[string]any) *RuntimeSpec {
	t.Helper()

	manifest, err := normalizeManifest(t, runtime)
	if err != nil {
		t.Fatalf("清单校验失败：%v", err)
	}
	if manifest.Runtime == nil {
		t.Fatal("清单声明了 kind=process，校验后却没有运行时配置")
	}
	return manifest.Runtime
}

// 硬判据：不填 runtime 的现有插件行为必须完全不变。
func TestManifestWithoutRuntimeStaysStatic(t *testing.T) {
	manifest, err := normalizeManifest(t, nil)
	if err != nil {
		t.Fatalf("清单校验失败：%v", err)
	}
	if manifest.Runtime != nil {
		t.Fatalf("未声明 runtime 的清单不应带运行时配置，实际得到 %+v", manifest.Runtime)
	}
	// nil 接收者也要给出可用的答案，调用方不必先判空。
	if !manifest.Runtime.AutoStartOn() {
		t.Fatal("没有运行时配置时 AutoStartOn 应为 true")
	}
}

func TestRuntimeDefaults(t *testing.T) {
	spec := normalizedRuntime(t, map[string]any{
		"kind": "process",
		"exec": "bin/mysql-manager",
	})

	if spec.Kind != RuntimeProcess {
		t.Errorf("Kind = %q，期望 %q", spec.Kind, RuntimeProcess)
	}
	if spec.Socket != defaultRuntimeSocket {
		t.Errorf("Socket = %q，期望默认 %q", spec.Socket, defaultRuntimeSocket)
	}
	if spec.Protocol != ProtocolJSONRPC {
		t.Errorf("Protocol = %q，期望默认 %q", spec.Protocol, ProtocolJSONRPC)
	}
	if spec.HealthPath != defaultRuntimeHealth {
		t.Errorf("HealthPath = %q，期望默认 %q", spec.HealthPath, defaultRuntimeHealth)
	}
	if spec.StopTimeoutSec != defaultStopTimeoutSec {
		t.Errorf("StopTimeoutSec = %d，期望默认 %d", spec.StopTimeoutSec, defaultStopTimeoutSec)
	}
	if spec.IdleStopSec != 0 {
		t.Errorf("IdleStopSec = %d，期望默认常驻 0", spec.IdleStopSec)
	}
	if !spec.AutoStartOn() {
		t.Error("未写 autoStart 时应默认自动拉起")
	}
}

func TestRuntimeExplicitValues(t *testing.T) {
	spec := normalizedRuntime(t, map[string]any{
		"kind":           "process",
		"exec":           "./bin/svc",
		"args":           []string{"--socket", "{socket}", "--data", "{dataDir}"},
		"socket":         "run/app.sock",
		"protocol":       "jsonrpc-2.0",
		"autoStart":      false,
		"idleStopSec":    300,
		"healthPath":     "system.health",
		"stopTimeoutSec": 30,
	})

	// 相对路径要经过 Clean：落位时它就是磁盘路径的一部分。
	if spec.Exec != "bin/svc" {
		t.Errorf("Exec = %q，期望去掉 ./ 前缀后的 bin/svc", spec.Exec)
	}
	if spec.Socket != "run/app.sock" {
		t.Errorf("Socket = %q，期望 run/app.sock", spec.Socket)
	}
	if len(spec.Args) != 4 || spec.Args[1] != "{socket}" {
		t.Errorf("Args = %v，期望原样保留占位符", spec.Args)
	}
	if spec.AutoStartOn() {
		t.Error("显式写了 autoStart=false 就不应被判为自动拉起")
	}
	if spec.IdleStopSec != 300 {
		t.Errorf("IdleStopSec = %d，期望 300", spec.IdleStopSec)
	}
	if spec.HealthPath != "system.health" {
		t.Errorf("HealthPath = %q，期望 system.health", spec.HealthPath)
	}
	if spec.StopTimeoutSec != 30 {
		t.Errorf("StopTimeoutSec = %d，期望 30", spec.StopTimeoutSec)
	}
}

// kind=static 是缺省值：其余字段一律忽略，而不是拿一份不生效的配置报错。
func TestRuntimeStaticIgnoresOtherFields(t *testing.T) {
	cases := map[string]any{
		"kind":        "static",
		"exec":        "/usr/bin/whatever",
		"idleStopSec": 999999,
	}
	manifest, err := normalizeManifest(t, cases)
	if err != nil {
		t.Fatalf("kind=static 时其余字段应被忽略，实际报错：%v", err)
	}
	if manifest.Runtime != nil {
		t.Fatalf("kind=static 不应产生运行时配置，实际得到 %+v", manifest.Runtime)
	}

	// 空对象等同于没写：kind 缺省即 static。
	manifest, err = normalizeManifest(t, map[string]any{})
	if err != nil {
		t.Fatalf("空 runtime 段应被忽略，实际报错：%v", err)
	}
	if manifest.Runtime != nil {
		t.Fatalf("空 runtime 段不应产生运行时配置，实际得到 %+v", manifest.Runtime)
	}
}

func TestRuntimeRejectsInvalidSpec(t *testing.T) {
	valid := "bin/mysql-manager"

	cases := []struct {
		reason  string
		runtime map[string]any
	}{
		{"kind 未知", map[string]any{"kind": "daemon", "exec": valid}},
		{"process 缺少 exec", map[string]any{"kind": "process"}},
		{"exec 是绝对路径", map[string]any{"kind": "process", "exec": "/usr/bin/svc"}},
		{"exec 指向包外", map[string]any{"kind": "process", "exec": "../svc"}},
		{"exec 用反斜杠", map[string]any{"kind": "process", "exec": `bin\svc`}},
		{"socket 指向包外", map[string]any{"kind": "process", "exec": valid, "socket": "../app.sock"}},
		{"socket 是绝对路径", map[string]any{"kind": "process", "exec": valid, "socket": "/tmp/app.sock"}},
		{"protocol 不支持", map[string]any{"kind": "process", "exec": valid, "protocol": "grpc"}},
		{"idleStopSec 超过上限", map[string]any{"kind": "process", "exec": valid, "idleStopSec": maxIdleStopSec + 1}},
		{"idleStopSec 为负", map[string]any{"kind": "process", "exec": valid, "idleStopSec": -1}},
		{"healthPath 含空格", map[string]any{"kind": "process", "exec": valid, "healthPath": "Health Check"}},
		{"healthPath 含连字符", map[string]any{"kind": "process", "exec": valid, "healthPath": "sys-health"}},
		{"healthPath 段尾带点", map[string]any{"kind": "process", "exec": valid, "healthPath": "sys."}},
		{"healthPath 超过长度上限", map[string]any{"kind": "process", "exec": valid, "healthPath": strings.Repeat("a", maxRPCMethodLen+1)}},
		{"stopTimeoutSec 超过上限", map[string]any{"kind": "process", "exec": valid, "stopTimeoutSec": maxStopTimeoutSec + 1}},
		{"stopTimeoutSec 为负", map[string]any{"kind": "process", "exec": valid, "stopTimeoutSec": -1}},
	}

	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			if _, err := normalizeManifest(t, tc.runtime); err == nil {
				t.Fatal("期望清单校验失败，实际通过")
			}
		})
	}
}

func TestRuntimeRejectsOversizedArgs(t *testing.T) {
	tooMany := make([]string, maxRuntimeArgs+1)
	for i := range tooMany {
		tooMany[i] = "--flag"
	}
	if _, err := normalizeManifest(t, map[string]any{
		"kind": "process", "exec": "bin/svc", "args": tooMany,
	}); err == nil {
		t.Errorf("超过 %d 项启动参数应被拒绝", maxRuntimeArgs)
	}

	if _, err := normalizeManifest(t, map[string]any{
		"kind": "process", "exec": "bin/svc",
		"args": []string{"--data", strings.Repeat("a", maxRuntimeArgLen+1)},
	}); err == nil {
		t.Errorf("单项超过 %d 字符的启动参数应被拒绝", maxRuntimeArgLen)
	}
}

// IsValidRPCMethod 被清单校验与转发接口共用，规则错了会同时影响两处。
func TestIsValidRPCMethod(t *testing.T) {
	valid := []string{"health", "connection.list", "sql.query.plan", "a1.b2", strings.Repeat("a", maxRPCMethodLen)}
	for _, method := range valid {
		if !IsValidRPCMethod(method) {
			t.Errorf("%q 应被判为合法方法名", method)
		}
	}

	invalid := []string{"", "Health", "health.", ".health", "a..b", "a-b", "a b", "sql.query!", strings.Repeat("a", maxRPCMethodLen+1)}
	for _, method := range invalid {
		if IsValidRPCMethod(method) {
			t.Errorf("%q 应被判为非法方法名", method)
		}
	}
}

// 能力分布在四张表里（known / label / order / risk），少一处就会出现
// "清单声明被接受、界面上没有文案"或"授予了却查不到风险等级"这类半截状态。
func TestCapabilityTablesCoverEachOther(t *testing.T) {
	if len(CapabilityOrder) != len(knownCapabilities) {
		t.Fatalf("顺序表 %d 项、已知能力表 %d 项，两者必须一致",
			len(CapabilityOrder), len(knownCapabilities))
	}

	seen := make(map[string]bool, len(CapabilityOrder))
	for _, name := range CapabilityOrder {
		if seen[name] {
			t.Errorf("顺序表里 %s 出现了两次", name)
		}
		seen[name] = true

		if !IsKnownCapability(name) {
			t.Errorf("%s 在顺序表里，却不在已知能力表里", name)
		}
		if CapabilityLabel[name] == "" {
			t.Errorf("%s 没有界面文案", name)
		}
		if CapabilityRisk[name] == "" {
			t.Errorf("%s 没有风险等级", name)
		}
	}

	for name := range knownCapabilities {
		if !seen[name] {
			t.Errorf("%s 是已知能力，却不在展示顺序表里", name)
		}
	}
}

// 授权界面按 CapabilityOrder 排版，因此它必须是风险升序：
// 高危能力排在前面，等于在诱导用户顺手把它勾上。
func TestCapabilityOrderIsRiskAscending(t *testing.T) {
	rank := map[string]int{RiskLow: 0, RiskMedium: 1, RiskHigh: 2}

	prev := -1
	for _, name := range CapabilityOrder {
		level, ok := rank[CapabilityRisk[name]]
		if !ok {
			t.Fatalf("%s 的风险等级无法识别：%q", name, CapabilityRisk[name])
		}
		if level < prev {
			t.Fatalf("%s（%s）排在了风险更低的能力之后", name, CapabilityRisk[name])
		}
		prev = level
	}
}

// 新增的 3 项能力必须真的被清单校验接受，否则插件声明它们时会被当成未知能力拒绝。
func TestManifestAcceptsPluginCapabilities(t *testing.T) {
	body := baseManifest("1.0.0")
	body["capabilities"] = []string{CapHostPkg, CapHostService, CapHostTunnel}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化清单失败：%v", err)
	}

	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("反序列化清单失败：%v", err)
	}
	normalized, err := manifest.Normalize()
	if err != nil {
		t.Fatalf("清单校验失败：%v", err)
	}
	if len(normalized.Capabilities) != 3 {
		t.Fatalf("能力 = %v，期望 3 项", normalized.Capabilities)
	}
}
