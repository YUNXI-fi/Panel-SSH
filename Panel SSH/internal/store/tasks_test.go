package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestFileTaskRoundTrip 走一遍文件操作的登记、明细写入与收尾。
func TestFileTaskRoundTrip(t *testing.T) {
	st := newTestStore(t)

	id, err := st.CreateFileTask(TaskKindHash, "web-01", "计算哈希（SHA256） /var/log", "sha256")
	if err != nil {
		t.Fatalf("登记失败：%v", err)
	}

	if err := st.AddTaskItems(id, []TaskItem{
		{Path: "a.log", Size: 12, Hash: "aa"},
		// -1 是"这条路拿不到大小"（走目标机命令算哈希时 sha256sum 不输出大小），
		// 必须原样存下来：0 表示文件确实是空的，两者不能混。
		{Path: "b.log", Size: -1, Hash: "bb"},
		{Path: "c.log", Size: 3, Error: "读取被拒绝（权限不足）"},
	}); err != nil {
		t.Fatalf("写入明细失败：%v", err)
	}
	if err := st.AddTaskResult(id, TaskResult{
		HostID: 7, HostName: "web-01", Status: ResultFailed, Note: "3 个文件，1 个失败",
	}); err != nil {
		t.Fatalf("写入结果失败：%v", err)
	}
	if err := st.FinishFileTask(id, TaskDone, 3); err != nil {
		t.Fatalf("收尾失败：%v", err)
	}

	list, err := st.ListTasks(10)
	if err != nil {
		t.Fatalf("查询列表失败：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("列表应有 1 条，实得 %d", len(list))
	}
	task := list[0]
	if task.Kind != TaskKindHash || task.HostName != "web-01" || task.Algo != "sha256" {
		t.Fatalf("列表缺了种类/主机/算法：%+v", task)
	}
	if task.Total != 3 || task.Status != TaskDone {
		t.Fatalf("条数或状态不对：total=%d status=%s", task.Total, task.Status)
	}
	// 明细里的失败要能被列表的计数聚合到，任务记录窗口的 LED 就靠它上色。
	if task.FailedCount != 1 {
		t.Fatalf("失败计数应为 1，实得 %d", task.FailedCount)
	}

	detail, err := st.GetTask(id)
	if err != nil {
		t.Fatalf("查询详情失败：%v", err)
	}
	if len(detail.Items) != 3 {
		t.Fatalf("明细应有 3 条，实得 %d", len(detail.Items))
	}
	if detail.Items[0].Path != "a.log" || detail.Items[0].Hash != "aa" {
		t.Fatalf("明细顺序或内容不对：%+v", detail.Items[0])
	}
	if detail.Items[1].Size != -1 {
		t.Fatalf("拿不到大小的明细应当原样存 -1，实得 %d", detail.Items[1].Size)
	}
	if detail.Items[2].Error == "" {
		t.Fatal("单条明细的失败原因没存下来")
	}
}

// TestBatchTaskStaysEmptyItems 确认批量执行不受新表影响。
func TestBatchTaskStaysEmptyItems(t *testing.T) {
	st := newTestStore(t)

	id, err := st.CreateTask("uptime", 30, 2)
	if err != nil {
		t.Fatalf("登记批量任务失败：%v", err)
	}

	list, err := st.ListTasks(10)
	if err != nil {
		t.Fatalf("查询列表失败：%v", err)
	}
	if list[0].Kind != TaskKindBatch {
		t.Fatalf("批量执行的种类应当是 batch，实得 %q", list[0].Kind)
	}
	if list[0].HostName != "" || list[0].Algo != "" {
		t.Fatalf("批量执行不该有主机名与算法：%+v", list[0])
	}

	detail, err := st.GetTask(id)
	if err != nil {
		t.Fatalf("查询详情失败：%v", err)
	}
	// 空数组而不是 nil：前端无条件按数组渲染，null 会让它多写一段判空。
	if detail.Items == nil || len(detail.Items) != 0 {
		t.Fatalf("批量执行的明细应当是空数组，实得 %#v", detail.Items)
	}
}

// TestMigrateAddsTaskColumns 确认"只装过批量执行"的老库能升上来。
//
// 这是本次改动唯一会碰到既有数据的部分：新装库走 CREATE TABLE 一定是对的，
// 而老库靠 ensureColumn 补列，补不上会直接导致面板起不来。
func TestMigrateAddsTaskColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("打开原始库失败：%v", err)
	}
	// 建一张"加 kind / host_name / algo 之前"的 tasks 表，并按老结构塞一条记录。
	const oldSchema = `
CREATE TABLE tasks (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    command     TEXT    NOT NULL,
    timeout_sec INTEGER NOT NULL DEFAULT 0,
    status      TEXT    NOT NULL DEFAULT 'running',
    total       INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0
)`
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatalf("建老表失败：%v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO tasks(command, status, total, created_at, finished_at) VALUES('uptime','done',1,1,2)`,
	); err != nil {
		t.Fatalf("写入老记录失败：%v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关闭原始库失败：%v", err)
	}

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("升级老库失败：%v", err)
	}
	defer st.Close()

	list, err := st.ListTasks(10)
	if err != nil {
		t.Fatalf("查询列表失败：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("老记录应当还在，实得 %d 条", len(list))
	}
	// 补列时给的默认值正是"这些历史记录都是批量执行"，不需要额外的数据订正。
	if list[0].Kind != TaskKindBatch || list[0].Command != "uptime" {
		t.Fatalf("老记录的字段不对：%+v", list[0])
	}
}