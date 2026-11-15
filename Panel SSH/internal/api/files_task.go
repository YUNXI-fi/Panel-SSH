package api

import (
	"fmt"
	"log"
	"strings"
	"time"

	"panelssh/internal/store"
)

// 文件操作（哈希 / 压缩 / 解压 / 上传 / 下载）的任务登记。
//
// 这些操作原先只活在"自己那个窗口"里：哈希的清单随窗口关闭就没了，压缩解压
// 更是连痕迹都不留，想回看只能重跑一遍。这里把它们接到与批量执行同一张 tasks
// 表上 —— 任务记录窗口因此只有一个入口，列表排序、LED 配色、计数聚合这些
// 逻辑一行都不用改，只需按 kind 换个措辞。
//
// 登记失败一律不阻断操作本身（口径与批量执行一致，见 batch.go 里 CreateTask
// 的说明）：用户要的是文件被处理，历史记录是辅助。

// maxFileTaskItems 是压缩/解压明细的内存上限。
//
// 这两类操作的条目数等于被处理的文件数，一个大目录轻松上万条。逐条写库会把
// 打包本身拖慢，全部攒在内存里则可能把面板撑爆，所以攒够一批就写掉、并留一个
// 上限：超出的明细丢弃，但条数照记，并在收尾说明里如实写成"明细仅保留前 N 条"。
// 哈希不设这个上限 —— 它逐条写库，内存里不留东西。
const maxFileTaskItems = 50000

// fileTask 是一次文件操作的登记句柄。
//
// 生命周期固定为 begin →（若干 addItem）→ finish/abort，
// 与批量执行的 CreateTask → AddTaskResult → FinishTask 一一对应。
type fileTask struct {
	store    *store.Store
	hostID   int64
	hostName string
	kind     string
	id       int64
	started  time.Time

	// total 是明细总数，failed 是其中失败的条数；两者都含被丢弃的明细 ——
	// 它们确实被处理过，只是明细没留下来。
	total  int
	failed int

	// items 只在压缩/解压时用，理由见 maxFileTaskItems。
	items   []store.TaskItem
	dropped int

	// settled 保证收尾只落一次：调用方在错误分支里就近收尾，正常路径由 defer 兜底。
	settled bool
}

// beginFileTask 登记一次文件操作，返回句柄。
func (s *Server) beginFileTask(hostID int64, kind, summary, algo string) *fileTask {
	task := &fileTask{store: s.store, hostID: hostID, kind: kind, started: time.Now()}
	// 主机名取当时的名字冗余存一份（与 task_results 同一个理由）：主机随时可能
	// 被改名或删除，而历史记录要说得出"当时是对哪台机器做的"。
	if host, err := s.store.GetHost(hostID); err == nil {
		task.hostName = host.Name
	}

	id, err := s.store.CreateFileTask(kind, task.hostName, summary, algo)
	if err != nil {
		// 置 0 让后续落库整体跳过，而不是每条明细都报一次同样的错。
		log.Printf("[warn] 创建文件任务记录失败: %v", err)
		return task
	}
	task.id = id
	return task
}

// summarizePaths 把一组路径压成列表页放得下的一句话。
//
// 选中几十个目录时全列出来会长得没法看，而列表页要回答的只是"这次操作的是什么"：
// 一个路径原样给出，多个只留第一个加个数。
func summarizePaths(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		return paths[0]
	}
	return fmt.Sprintf("%s 等 %d 项", paths[0], len(paths))
}

// addItem 记一条明细；登记失败（id 为 0）时直接丢弃。
func (t *fileTask) addItem(path string, size int64, hash, errText string) {
	if t == nil || t.id == 0 {
		return
	}
	t.total++
	if errText != "" {
		t.failed++
	}
	item := store.TaskItem{Path: path, Size: size, Hash: hash, Error: errText}

	// 哈希的清单本身就是用户要回看的产物，逐条入库：一个几万文件的目录要算上
	// 很久，中途被打断时已经算完的那部分必须留得下来。
	if t.kind == store.TaskKindHash {
		t.write([]store.TaskItem{item})
		return
	}
	// 压缩/解压同样在过程中逐条解析，但条目动辄上万，逐条 INSERT 会把一次
	// 本来只是"打包"的操作拖慢，所以先攒在内存里。
	if len(t.items) >= maxFileTaskItems {
		t.dropped++
		return
	}
	t.items = append(t.items, item)
}

// setTotal 在明细没能留下来时用统计阶段的分母兜底。
//
// 压缩/解压在"权限不足、转 root 重做"那条路上拿不到逐条输出，明细表会是空的，
// 但条目数在统计阶段已经数出来了 —— 列表里显示 N 项，好过显示 0 项。
func (t *fileTask) setTotal(n int) {
	if t == nil || t.total > 0 || n <= 0 {
		return
	}
	t.total = n
}

// finish 正常收尾；errText 非空表示整个操作失败了。
func (t *fileTask) finish(errText string) {
	t.settle(store.TaskDone, errText)
}

// abort 收尾成"中断"：客户端先走了（关掉窗口、断开连接）。
//
// 与批量执行同一口径 —— 历史里才不会把"跑了半截"显示成"已完成"。
func (t *fileTask) abort() {
	t.settle(store.TaskInterrupted, "")
}

func (t *fileTask) settle(taskStatus, errText string) {
	if t == nil || t.id == 0 || t.settled {
		return
	}
	t.settled = true

	// 攒下的明细先于状态写入：任务一旦变成终态，界面就会去拉明细，
	// 顺序反过来会出现"已完成但清单是空的"这一瞬间。
	if len(t.items) > 0 {
		t.write(t.items)
		t.items = nil
	}

	status := store.ResultSuccess
	switch {
	case errText != "":
		status = store.ResultError
	case t.failed > 0:
		// 有个别明细失败：整体确实做完了，但列表里必须能一眼看出"这批不干净"，
		// 所以记成 failed —— 它在界面上对应黄色告警，与批量执行里
		// "命令跑完了但退出码非 0"是同一个位置。
		status = store.ResultFailed
	}

	if err := t.store.AddTaskResult(t.id, store.TaskResult{
		HostID:     t.hostID,
		HostName:   t.hostName,
		Status:     status,
		ExitCode:   0,
		Error:      errText,
		Note:       t.note(),
		DurationMS: time.Since(t.started).Milliseconds(),
	}); err != nil {
		log.Printf("[warn] 写入文件任务 %d 的主机结果失败: %v", t.id, err)
	}
	if err := t.store.FinishFileTask(t.id, taskStatus, t.total); err != nil {
		log.Printf("[warn] 收尾文件任务 %d 失败: %v", t.id, err)
	}
}

// write 落一批明细。写库失败只记日志，不影响正在进行的操作。
func (t *fileTask) write(items []store.TaskItem) {
	if err := t.store.AddTaskItems(t.id, items); err != nil {
		log.Printf("[warn] 写入任务 %d 的明细失败: %v", t.id, err)
	}
}

// note 是详情页那句"这批做了多少、有几个不干净"。
func (t *fileTask) note() string {
	// 哈希与上下传面对的就是文件本身，说"条目"反而绕。
	noun := "个条目"
	switch t.kind {
	case store.TaskKindHash, store.TaskKindUpload, store.TaskKindDownload, store.TaskKindTransfer:
		noun = "个文件"
	}

	parts := []string{fmt.Sprintf("%d %s", t.total, noun)}
	if t.failed > 0 {
		parts = append(parts, fmt.Sprintf("%d 个失败", t.failed))
	}
	if t.dropped > 0 {
		parts = append(parts, fmt.Sprintf("明细仅保留前 %d 条", maxFileTaskItems))
	}
	return strings.Join(parts, "，")
}
