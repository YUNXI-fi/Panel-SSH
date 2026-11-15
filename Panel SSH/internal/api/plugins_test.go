package api

import (
	"path/filepath"
	"testing"

	"panelssh/internal/store"
)

// 这一组测试针对"重复安装内置插件把已有授权清空"这个缺陷。
// 用真实的 store（临时库）而不是打桩，是因为问题恰恰出在 UpsertPlugin
// 会把 capabilities / granted 两列一起覆盖这个行为上。
func newTestStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func strListEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPreserveExistingCapabilitiesOnReinstall(t *testing.T) {
	st := newTestStore(t)

	// 先有一条带授权的记录（模拟用户此前逐项确认过的状态）
	if _, err := st.UpsertPlugin(store.PluginInput{
		Org:          "panelssh",
		Name:         "demo",
		DisplayName:  "示例",
		Version:      "1.0.0",
		Entry:        "/demo/",
		Source:       "embedded",
		Capabilities: []string{"host:list", "host:exec"},
		Granted:      []string{"host:list"},
	}); err != nil {
		t.Fatalf("准备记录失败：%v", err)
	}

	// 重复安装：内置插件的 input 不带能力清单
	input := store.PluginInput{
		Org:         "panelssh",
		Name:        "demo",
		DisplayName: "示例",
		Version:     "1.0.0",
		Entry:       "/demo/",
		Source:      "embedded",
	}
	preserveExistingCapabilities(st, &input)

	if !strListEqual(input.Capabilities, []string{"host:list", "host:exec"}) {
		t.Fatalf("申请的能列表未被保留：%v", input.Capabilities)
	}
	if !strListEqual(input.Granted, []string{"host:list"}) {
		t.Fatalf("已有授权未被保留：%v", input.Granted)
	}

	// 落库后再读一次，确认真的写回去了而不是只在内存里改对
	if _, err := st.UpsertPlugin(input); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	saved, err := st.GetPlugin("panelssh", "demo", LocalHostID)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if !strListEqual(saved.Granted, []string{"host:list"}) {
		t.Fatalf("重复安装后授权被改动：%v", saved.Granted)
	}
}

func TestPreserveExistingCapabilitiesOnFirstInstall(t *testing.T) {
	st := newTestStore(t)

	input := store.PluginInput{Org: "panelssh", Name: "fresh", Version: "1.0.0", Source: "embedded"}
	preserveExistingCapabilities(st, &input)

	if len(input.Capabilities) != 0 || len(input.Granted) != 0 {
		t.Fatalf("首次安装不该凭空产生能力：%v / %v", input.Capabilities, input.Granted)
	}
}
