package api

import (
	"errors"
	"net/http"
	"strconv"

	"panelssh/internal/store"
)

// maxTaskListLimit 是任务列表一次返回的最大条数。
//
// 不设上限的话，随着任务累积，每次打开任务窗口都会把整张表拉一遍；
// 面板是单机工具，最近两百次批量执行足够回看。
const maxTaskListLimit = 200

// handleListTasks 返回最近的批量任务。
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.ListTasks(maxTaskListLimit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询任务失败")
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

// handleGetTask 返回单个任务的详情，含每台主机的结果。
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	// 不复用 pathID：它写死了"主机 ID 无效"的提示，放在这里会误导排查方向。
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "任务 ID 无效")
		return
	}

	detail, err := s.store.GetTask(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "任务不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "查询任务失败")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}
