package api

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"panelssh/internal/store"
)

// maxNotificationListLimit 是通知列表一次返回的最大条数。
//
// 通知按用户要求永久保留、不自动清理，表会持续增长，
// 因此查询侧必须自己收口：面板一次只看最新一屏，没必要把历史全拉回来。
const maxNotificationListLimit = 200

// notify 写入一条通知。
//
// 通知属于辅助信息，写失败只记日志、绝不向上传播 ——
// 批量任务不该因为"通知没记上"而报错，改密码也不该因此失败。
func (s *Server) notify(in store.NotificationInput) {
	if _, err := s.store.CreateNotification(in); err != nil {
		log.Printf("[warn] 写入通知失败: %v", err)
	}
}

// notificationsPayload 把列表与未读数一次返回。
//
// 两者总是被一起使用（面板要列表，角标要计数），分两个接口会让前端
// 每次刷新多一次往返，且两个响应可能来自不同瞬间，角标与列表对不上。
type notificationsPayload struct {
	Items  []store.Notification `json:"items"`
	Unread int                  `json:"unread"`
}

// handleListNotifications 返回最近的通知与未读数。
func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListNotifications(maxNotificationListLimit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询通知失败")
		return
	}
	unread, err := s.store.CountUnreadNotifications()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询通知失败")
		return
	}
	writeJSON(w, http.StatusOK, notificationsPayload{Items: items, Unread: unread})
}

// readPayload 是标记已读的请求体。
type readPayload struct {
	IDs []int64 `json:"ids"`
}

// handleMarkNotificationsRead 把指定通知标为已读。
//
// ids 为空表示全部已读：前端"全部已读"按钮不必先拉一次列表再回传所有 ID。
func (s *Server) handleMarkNotificationsRead(w http.ResponseWriter, r *http.Request) {
	var req readPayload
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	if err := s.store.MarkNotificationsRead(req.IDs); err != nil {
		writeErr(w, http.StatusInternalServerError, "标记已读失败")
		return
	}
	s.writeUnread(w)
}

// handleMarkAllNotificationsRead 把全部未读通知标为已读。
func (s *Server) handleMarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	if err := s.store.MarkNotificationsRead(nil); err != nil {
		writeErr(w, http.StatusInternalServerError, "标记已读失败")
		return
	}
	s.writeUnread(w)
}

// writeUnread 回传最新未读数。
//
// 标记已读后返回新计数，前端不必再拉一次列表就能更新角标 ——
// 否则用户点"全部已读"后角标会停在旧数字上，直到下一次轮询才归零。
func (s *Server) writeUnread(w http.ResponseWriter) {
	unread, err := s.store.CountUnreadNotifications()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询通知失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"unread": unread})
}

// ---------- 事件源 ----------

// notifyTaskFinished 在批量任务收尾时产生一条通知。
//
// 内容按结果分支：全成功给 success，有非 0 退出码给 warning，有执行错误给 error ——
// 这样铃铛角标的颜色本身就说明了严重性，不必点开才知道。
func (s *Server) notifyTaskFinished(taskID int64, command string, total, success, failedExit, errored int) {
	level := store.LevelSuccess
	switch {
	case errored > 0:
		level = store.LevelError
	case failedExit > 0:
		level = store.LevelWarning
	}

	// 命令可能很长，标题只取首行并截断；完整命令留在任务详情里，
	// 通知不该变成第二处展示命令全文的地方。
	summary := firstLineOf(command)
	if len(summary) > 60 {
		summary = summary[:60] + "…"
	}

	title := fmt.Sprintf("任务 #%d 完成：%s", taskID, summary)
	if failedExit == 0 && errored == 0 {
		title = fmt.Sprintf("任务 #%d 全部成功：%s", taskID, summary)
	}

	s.notify(store.NotificationInput{
		Category: store.NotifyTask,
		Level:    level,
		Title:    title,
		Body:     fmt.Sprintf("共 %d 台 · 成功 %d · 退出码非 0 %d · 失败 %d", total, success, failedExit, errored),
		// 指回任务，面板里点这条通知可以直接打开任务详情。
		Ref: taskID,
	})
}

// notifyHostOffline / notifyHostOnline 产生主机上下线通知。
//
// 各只给一台主机的一条记录，而不是攒成"3 台主机下线"：
// 下线时间点不同、原因也可能不同，合并后无法回答"哪台什么时候掉的"。
func (s *Server) notifyHostOffline(name string, addr string) {
	s.notify(store.NotificationInput{
		Category: store.NotifyHost,
		Level:    store.LevelError,
		Title:    fmt.Sprintf("主机离线：%s", name),
		Body:     fmt.Sprintf("%s 连续多次探测均无法连接，请确认机器与网络状态", addr),
	})
}

// formatLatency 把毫秒读数写成给人看的形式：两位小数，800 毫秒以上改用秒。
//
// 必须与前端 web/src/api/format.ts 的 formatLatency 保持一致 —— 同一个读数
// 在通知里和界面上显示成两个样子，用户会怀疑哪一个才是对的。
func formatLatency(ms float64) string {
	if ms >= 800 {
		return fmt.Sprintf("%.2f s", ms/1000)
	}
	return fmt.Sprintf("%.2f ms", ms)
}

func (s *Server) notifyHostOnline(name string, addr string, latencyMs float64) {
	s.notify(store.NotificationInput{
		Category: store.NotifyHost,
		Level:    store.LevelSuccess,
		Title:    fmt.Sprintf("主机恢复：%s", name),
		Body:     fmt.Sprintf("%s 已恢复连接（%s）", addr, formatLatency(latencyMs)),
	})
}

// notifySecurity 产生安全/管理类通知。
//
// 只覆盖有实际意义的操作：凭据变更、主机增删、插件装卸与授权变更。
// 刻意不记"登录成功"这类高频且低信息量的事件 —— 它们会把真正需要
// 注意的条目从列表里挤走，通知中心一旦充满噪声就没人再看了。
func (s *Server) notifySecurity(level, title, body string) {
	s.notify(store.NotificationInput{
		Category: store.NotifySecurity,
		Level:    level,
		Title:    title,
		Body:     body,
	})
}

// firstLineOf 取文本首个非空行。
func firstLineOf(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(text)
}
