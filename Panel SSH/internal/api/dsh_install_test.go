package api

import (
	"testing"

	"panelssh/internal/plugin"
	"panelssh/internal/store"
)

// 插件记录是 /dsh/{id}/ 入口的开关，而它是按主机存的：
// 装了主机 7 不代表主机 8 也有。
//
// 改造前这个判定写死了宿主机，于是"装到远端"的插件在列表里显示已安装、
// 打开却是 404 —— 一条谁都没法解释的死路。
func TestDshWorkspaceInstalledPerHost(t *testing.T) {
	st := newTestStore(t)
	s := &Server{store: st}

	if _, err := st.UpsertPlugin(store.PluginInput{
		Org:         plugin.BuiltinDshWorkspaceOrg,
		Name:        plugin.BuiltinDshWorkspaceName,
		DisplayName: "DSH 工作台",
		Version:     "0.1.0",
		Entry:       "/dsh/7/",
		Source:      plugin.SourceEmbedded,
		HostID:      7,
	}); err != nil {
		t.Fatalf("准备插件记录失败：%v", err)
	}

	if !s.dshWorkspaceInstalled(7) {
		t.Fatal("主机 7 装了插件，应当判定为已安装")
	}
	if s.dshWorkspaceInstalled(LocalHostID) {
		t.Fatal("宿主机没装插件，不该被判定为已安装")
	}
	if s.dshWorkspaceInstalled(8) {
		t.Fatal("主机 8 没装插件，不该被判定为已安装")
	}
}

// 内置插件的入口要按主机现算。
//
// 改造前存下的记录里是 /dsh/，那是一个没有主机号的地址，直接透传会让所有
// 机器都指向同一个实例；现算则让这些历史记录不需要数据迁移 ——
// 迁移漏掉一台，用户看到的就是一个打不开的图标，而且没有任何线索。
func TestBuiltinEntryNormalizedPerHost(t *testing.T) {
	legacy := store.Plugin{
		Org:    plugin.BuiltinDshWorkspaceOrg,
		Name:   plugin.BuiltinDshWorkspaceName,
		Source: plugin.SourceEmbedded,
		HostID: 7,
		Entry:  "/dsh/",
	}
	if got := builtinEntryNormalized(legacy); got.Entry != "/dsh/7/" {
		t.Fatalf("入口应当按主机现算为 /dsh/7/，实际 %q", got.Entry)
	}

	// 宿主机是 0，前缀同样要带上它。
	legacy.HostID = LocalHostID
	if got := builtinEntryNormalized(legacy); got.Entry != "/dsh/0/" {
		t.Fatalf("宿主机入口应当为 /dsh/0/，实际 %q", got.Entry)
	}

	// 第三方插件的 entry 是包内文件路径，不参与归一化。
	archive := store.Plugin{Source: plugin.SourceArchive, HostID: 7, Entry: "index.html"}
	if got := builtinEntryNormalized(archive); got.Entry != "index.html" {
		t.Fatalf("第三方插件的 entry 不该被改动，实际 %q", got.Entry)
	}
}
