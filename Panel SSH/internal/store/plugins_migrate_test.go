package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigratePluginsPerHostFromLegacySchema 验证旧库能升到"每台主机一份"。
//
// 这一支迁移是整表重建（SQLite 改不了唯一约束），风险集中在两处：
//   1. 重建之后数据必须一条不少地搬过去；
//   2. 版本号必须推进，否则每次启动都重建一遍，第二次就把数据搬空了。
// 因此这里刻意先用原始 SQL 造出旧形状的库，再走一次 Open 让迁移跑起来。
func TestMigratePluginsPerHostFromLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 造旧库：plugins 是 UNIQUE(org, name)，没有 host_id，也没有 source 列
	// （source 是更晚才加的，正好覆盖"搬运时要给缺失列兜默认值"这条路径）。
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("建旧库失败：%v", err)
	}
	legacy := `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '');
CREATE TABLE plugins (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    org          TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    display_name TEXT    NOT NULL DEFAULT '',
    description  TEXT    NOT NULL DEFAULT '',
    version      TEXT    NOT NULL DEFAULT '',
    entry        TEXT    NOT NULL DEFAULT '',
    icon         TEXT    NOT NULL DEFAULT '',
    capabilities TEXT    NOT NULL DEFAULT '',
    granted      TEXT    NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    installed_at INTEGER NOT NULL,
    UNIQUE(org, name)
);
INSERT INTO plugins(org, name, display_name, version, entry, capabilities, granted, enabled, installed_at)
VALUES('panelssh', 'demo', '示例', '1.0.0', '/demo/', 'host:list,host:exec', 'host:list', 1, 1700000000);
INSERT INTO plugins(org, name, display_name, version, entry, enabled, installed_at)
VALUES('panelssh', 'off', '已停用', '2.0.0', '/off/', 0, 1700000100);
`
	if _, err := raw.Exec(legacy); err != nil {
		_ = raw.Close()
		t.Fatalf("写入旧结构失败：%v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭旧库失败：%v", err)
	}

	// 走正常的 Open：迁移应当在这里发生。
	st, err := Open(path)
	if err != nil {
		t.Fatalf("迁移失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	items, err := st.ListPluginsOnHost(0)
	if err != nil {
		t.Fatalf("读取迁移结果失败：%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("存量记录应当全部落到宿主机，实际 %d 条：%+v", len(items), items)
	}

	var demo *Plugin
	for i := range items {
		if items[i].Name == "demo" {
			demo = &items[i]
		}
		if items[i].HostID != 0 {
			t.Fatalf("存量记录的 hostID 应当是 0：%+v", items[i])
		}
	}
	if demo == nil {
		t.Fatal("demo 记录在迁移中丢失")
	}
	if demo.Version != "1.0.0" || demo.DisplayName != "示例" {
		t.Fatalf("字段没有原样搬过来：%+v", demo)
	}
	// 缺失的 source 列要落到默认值，而不是空串 —— 能力桥要靠它区分
	// "第三方包"与"内置插件"，空值会让插件被当成内置的从而绕开沙箱。
	if demo.Source != "archive" {
		t.Fatalf("缺失的 source 列应兜默认值 archive：%q", demo.Source)
	}
	if len(demo.Capabilities) != 2 || len(demo.Granted) != 1 {
		t.Fatalf("能力/授权在迁移中被改动：%+v / %+v", demo.Capabilities, demo.Granted)
	}

	// 迁移后唯一约束必须放行"同一插件装到第二台主机"。
	if _, err := st.UpsertPlugin(PluginInput{
		Org: "panelssh", Name: "demo", DisplayName: "示例", Version: "1.0.0",
		Entry: "/demo/", HostID: 7,
	}); err != nil {
		t.Fatalf("同一插件装到第二台主机应当可行：%v", err)
	}

	// 再打开一次不该重复迁移。版本号没推进的话，这里会把上面的数据搬空。
	if err := st.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("二次打开失败：%v", err)
	}
	t.Cleanup(func() { _ = again.Close() })

	after, err := again.ListPlugins()
	if err != nil {
		t.Fatalf("二次读取失败：%v", err)
	}
	if len(after) != 3 {
		t.Fatalf("二次启动不该改变数据量，期望 3 条，实际 %d 条", len(after))
	}
}
