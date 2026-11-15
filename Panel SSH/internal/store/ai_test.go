package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestAIConversationRoundTrip 走一遍读、写、覆盖写、清空。
func TestAIConversationRoundTrip(t *testing.T) {
	st := newTestStore(t)

	session, err := st.CreateAISession("")
	if err != nil {
		t.Fatalf("创建会话失败：%v", err)
	}

	// 会话存在但还没聊过时返回零值而不是 ErrNotFound：让"还没有对话"变成
	// 一个需要调用方处理的错误，只会让每个调用点都多一段判空。
	conv, err := st.GetAIConversation(session.ID)
	if err != nil {
		t.Fatalf("空会话读取不该报错：%v", err)
	}
	if conv.HistoryJSON != "" {
		t.Fatalf("空会话不该有历史：%q", conv.HistoryJSON)
	}
	if conv.ToolStatusJSON != "{}" {
		t.Fatalf("空会话的工具状态应当是空对象：%q", conv.ToolStatusJSON)
	}

	history := `[{"role":"system","content":"提示"},{"role":"user","content":"你好"}]`
	status := `{"call_1":true,"call_2":false}`
	if err := st.SaveAIConversation(session.ID, history, status, "你好"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	conv, err = st.GetAIConversation(session.ID)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if conv.HistoryJSON != history || conv.ToolStatusJSON != status {
		t.Fatalf("读回的内容不一致：%+v", conv)
	}
	if conv.UpdatedAt == 0 {
		t.Fatal("updated_at 没有被写入")
	}

	// 标题只在为空时补写：第二轮不该把它改掉。
	got, err := st.GetAISession(session.ID)
	if err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	if got.Title != "你好" {
		t.Fatalf("标题应当取第一句用户提问，实际 %q", got.Title)
	}
	if err := st.SaveAIConversation(session.ID, `[{"role":"user","content":"第二条"}]`, "{}", "第二条"); err != nil {
		t.Fatalf("覆盖写入失败：%v", err)
	}
	got, _ = st.GetAISession(session.ID)
	if got.Title != "你好" {
		t.Fatalf("已有标题不该被后续对话覆盖，实际 %q", got.Title)
	}

	// 每个会话至多一行：覆盖写变成插入的话，读取方会因为"取哪一行"而拿到
	// 旧对话 —— 现象是"聊了新的，重开窗口看到的还是旧的"。
	var rows int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM ai_conversation WHERE session_id = ?`, session.ID).Scan(&rows); err != nil {
		t.Fatalf("统计行数失败：%v", err)
	}
	if rows != 1 {
		t.Fatalf("会话应当只有一行记录，实际 %d 行 —— 覆盖写变成了插入", rows)
	}
	conv, _ = st.GetAIConversation(session.ID)
	if conv.HistoryJSON != `[{"role":"user","content":"第二条"}]` {
		t.Fatalf("覆盖写入没有生效：%q", conv.HistoryJSON)
	}

	// 清空只删记录，会话本身保留；标题一并重置，免得列表里留着对不上的旧标题。
	if err := st.ClearAIConversation(session.ID); err != nil {
		t.Fatalf("清空失败：%v", err)
	}
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM ai_conversation WHERE session_id = ?`, session.ID).Scan(&rows); err != nil {
		t.Fatalf("统计行数失败：%v", err)
	}
	if rows != 0 {
		t.Fatalf("清空后不该还有行，实际 %d 行", rows)
	}
	conv, err = st.GetAIConversation(session.ID)
	if err != nil {
		t.Fatalf("清空后读取失败：%v", err)
	}
	if conv.HistoryJSON != "" {
		t.Fatalf("清空后不该还有历史：%q", conv.HistoryJSON)
	}
	got, err = st.GetAISession(session.ID)
	if err != nil {
		t.Fatalf("清空后会话不该消失：%v", err)
	}
	if got.Title != "" {
		t.Fatalf("清空后标题应当重置，实际 %q", got.Title)
	}
}

// TestAISessionIsolation 是多会话的核心约束：写一个会话不能影响另一个。
//
// 之前只有一行固定 id 的记录，所有对话共用同一份历史；改成多会话之后，
// 任何一处漏带 session_id 都会表现为"切了会话还看到上一个会话的内容"。
func TestAISessionIsolation(t *testing.T) {
	st := newTestStore(t)

	first, err := st.CreateAISession("")
	if err != nil {
		t.Fatalf("创建会话失败：%v", err)
	}
	second, err := st.CreateAISession("")
	if err != nil {
		t.Fatalf("创建会话失败：%v", err)
	}

	firstHistory := `[{"role":"user","content":"第一个会话"}]`
	secondHistory := `[{"role":"user","content":"第二个会话"}]`
	if err := st.SaveAIConversation(first.ID, firstHistory, `{"a":true}`, "第一个会话"); err != nil {
		t.Fatalf("写入会话 1 失败：%v", err)
	}
	if err := st.SaveAIConversation(second.ID, secondHistory, `{"b":false}`, "第二个会话"); err != nil {
		t.Fatalf("写入会话 2 失败：%v", err)
	}

	conv, err := st.GetAIConversation(first.ID)
	if err != nil {
		t.Fatalf("读取会话 1 失败：%v", err)
	}
	if conv.HistoryJSON != firstHistory || conv.ToolStatusJSON != `{"a":true}` {
		t.Fatalf("会话 1 的内容被污染了：%+v", conv)
	}
	conv, err = st.GetAIConversation(second.ID)
	if err != nil {
		t.Fatalf("读取会话 2 失败：%v", err)
	}
	if conv.HistoryJSON != secondHistory || conv.ToolStatusJSON != `{"b":false}` {
		t.Fatalf("会话 2 的内容被污染了：%+v", conv)
	}

	// 删除一个会话，另一个必须原样还在。
	if err := st.DeleteAISession(first.ID); err != nil {
		t.Fatalf("删除会话失败：%v", err)
	}
	if _, err := st.GetAIConversation(first.ID); err == nil {
		t.Fatal("已删除的会话不该还能读出记录")
	}
	conv, err = st.GetAIConversation(second.ID)
	if err != nil {
		t.Fatalf("删除一个会话不该影响另一个：%v", err)
	}
	if conv.HistoryJSON != secondHistory {
		t.Fatalf("另一个会话的内容被改动了：%q", conv.HistoryJSON)
	}
}

// 会话不存在时，写入必须失败而不是留下一条无人认领的记录。
func TestSaveAIConversationRejectsMissingSession(t *testing.T) {
	st := newTestStore(t)

	err := st.SaveAIConversation(9999, `[]`, "{}", "")
	if err != ErrNotFound {
		t.Fatalf("写入不存在的会话应当返回 ErrNotFound，实际 %v", err)
	}

	var rows int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM ai_conversation`).Scan(&rows); err != nil {
		t.Fatalf("统计行数失败：%v", err)
	}
	if rows != 0 {
		t.Fatalf("不该留下孤立的历史记录，实际 %d 行", rows)
	}
}

// TestMigrateAISessionsKeepsLegacyConversation 验证老库升级不丢对话。
//
// 改造前助手只有一行固定 id 的对话记录。升级时必须把它原样搬进一个新会话，
// 否则用户打开助手会发现之前聊过的内容全没了 —— 这是不可接受的回归。
func TestMigrateAISessionsKeepsLegacyConversation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// 手工造一个旧形状的库：只有 ai_conversation(id ...)，没有 ai_sessions。
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("打开旧库失败：%v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE ai_conversation (
    id               INTEGER PRIMARY KEY,
    history_json     TEXT    NOT NULL DEFAULT '',
    tool_status_json TEXT    NOT NULL DEFAULT '{}',
    updated_at       INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("建旧表失败：%v", err)
	}
	history := `[{"role":"user","content":"升级前聊的内容"}]`
	if _, err := legacy.Exec(
		`INSERT INTO ai_conversation(id, history_json, tool_status_json, updated_at) VALUES(1, ?, ?, ?)`,
		history, `{"call_old":false}`, int64(1700000000)); err != nil {
		t.Fatalf("写入旧记录失败：%v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("关闭旧库失败：%v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	sessions, err := st.ListAISessions()
	if err != nil {
		t.Fatalf("读取会话失败：%v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("旧对话应当变成一个会话，实际 %d 个", len(sessions))
	}

	conv, err := st.GetAIConversation(sessions[0].ID)
	if err != nil {
		t.Fatalf("读取对话失败：%v", err)
	}
	if conv.HistoryJSON != history {
		t.Fatalf("旧对话内容没有搬过来：%q", conv.HistoryJSON)
	}
	if conv.ToolStatusJSON != `{"call_old":false}` {
		t.Fatalf("工具状态没有搬过来：%q", conv.ToolStatusJSON)
	}
}

// 会话列表按最近活动排序，最近聊过的排在最前面。
func TestListAISessionsOrdersByRecentActivity(t *testing.T) {
	st := newTestStore(t)

	older, _ := st.CreateAISession("旧会话")
	newer, _ := st.CreateAISession("新会话")
	// 两个会话在同一秒内建出来时，仅按时间排序无法区分先后。
	// 直接把时间戳摆成想要的样子，比 sleep 一秒稳。
	now := time.Now().Unix()
	if _, err := st.db.Exec(`UPDATE ai_sessions SET updated_at = ? WHERE id = ?`, now-10, newer.ID); err != nil {
		t.Fatalf("调整时间戳失败：%v", err)
	}
	if _, err := st.db.Exec(`UPDATE ai_sessions SET updated_at = ? WHERE id = ?`, now, older.ID); err != nil {
		t.Fatalf("调整时间戳失败：%v", err)
	}

	sessions, err := st.ListAISessions()
	if err != nil {
		t.Fatalf("读取会话列表失败：%v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("应当有 2 个会话，实际 %d 个", len(sessions))
	}
	if sessions[0].ID != older.ID {
		t.Fatalf("刚聊过的会话应当排在第一位，实际 %d（新会话 id=%d）", sessions[0].ID, newer.ID)
	}
}
