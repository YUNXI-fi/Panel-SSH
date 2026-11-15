package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// 每主机的启动命令要能独立读写：两台机器各存各的，互不覆盖。
func TestDshCommandRoundTrip(t *testing.T) {
	st := newTestStore(t)

	// 没配过时返回空串而不是错误：调用方关心的是"配没配"，不是"查不到"。
	got, err := st.GetDshCommand(0)
	if err != nil {
		t.Fatalf("读取未配置的命令应当成功：%v", err)
	}
	if got != "" {
		t.Fatalf("未配置时应当返回空串，实际 %q", got)
	}

	if err := st.SetDshCommand(0, "/usr/local/bin/dsh"); err != nil {
		t.Fatalf("写入宿主机命令失败：%v", err)
	}
	if err := st.SetDshCommand(7, "/usr/bin/dsh"); err != nil {
		t.Fatalf("写入主机 7 的命令失败：%v", err)
	}

	if got, _ := st.GetDshCommand(0); got != "/usr/local/bin/dsh" {
		t.Fatalf("宿主机命令应为 /usr/local/bin/dsh，实际 %q", got)
	}
	if got, _ := st.GetDshCommand(7); got != "/usr/bin/dsh" {
		t.Fatalf("主机 7 的命令应为 /usr/bin/dsh，实际 %q", got)
	}

	// 覆盖写：同一台主机只留一条命令。
	if err := st.SetDshCommand(7, "/opt/dsh"); err != nil {
		t.Fatalf("覆盖写入失败：%v", err)
	}
	if got, _ := st.GetDshCommand(7); got != "/opt/dsh" {
		t.Fatalf("覆盖后应为 /opt/dsh，实际 %q", got)
	}
	// 覆盖主机 7 不该动到宿主机那份 —— 串台会让面板拉起错误的实例。
	if got, _ := st.GetDshCommand(0); got != "/usr/local/bin/dsh" {
		t.Fatalf("覆盖主机 7 影响了宿主机：%q", got)
	}

	if err := st.DeleteDshCommand(7); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if got, _ := st.GetDshCommand(7); got != "" {
		t.Fatalf("删除后应当为空，实际 %q", got)
	}
	// 记录不存在时删除同样不该报错：卸载要的是"结果是没有命令"这个事实。
	if err := st.DeleteDshCommand(7); err != nil {
		t.Fatalf("重复删除不该报错：%v", err)
	}
}

// TestMigrateDshCommandsPerHostFromLegacyMeta 验证旧的全局 meta 键能迁到宿主机名下。
//
// 两件事必须同时成立：值落到 host_id = 0，键从 meta 里消失。
// 只搬不删会留下两份真值来源 —— "界面显示 A、实际生效 B"这种错没人能一眼看穿。
func TestMigrateDshCommandsPerHostFromLegacyMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 造旧库：启动命令存在 meta 表的一个全局键里。只建这一张表即可，
	// 其余表由基础 schema 的 CREATE TABLE IF NOT EXISTS 补齐。
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("建旧库失败：%v", err)
	}
	legacy := `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '');
INSERT INTO meta(key, value) VALUES('dsh.command', 'node /opt/dsh/lib/bin.js');
`
	if _, err := raw.Exec(legacy); err != nil {
		_ = raw.Close()
		t.Fatalf("写入旧结构失败：%v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭旧库失败：%v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("迁移失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	got, err := st.GetDshCommand(0)
	if err != nil {
		t.Fatalf("读取迁移结果失败：%v", err)
	}
	if got != "node /opt/dsh/lib/bin.js" {
		t.Fatalf("旧命令应当落到宿主机名下，实际 %q", got)
	}

	left, err := st.GetMeta(legacyDshCommandKey)
	if err != nil {
		t.Fatalf("读取 meta 失败：%v", err)
	}
	if left != "" {
		t.Fatalf("旧 meta 键应当被删除，实际仍为 %q", left)
	}

	// 重开一次：迁移已经跑过，不该重复搬运、更不该把数据搬空。
	// 版本机制保证它不会重跑，这条断言守的是"万一重跑了"的后果。
	if err := st.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("二次打开失败：%v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	if got, _ := reopened.GetDshCommand(0); got != "node /opt/dsh/lib/bin.js" {
		t.Fatalf("重开之后命令不应丢失，实际 %q", got)
	}
}