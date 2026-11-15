package api

import (
	"net/http"
	"time"

	"panelssh/internal/sysinfo"
)

// netTTL 是网络信息的最短复用窗口。
//
// 必须小于前端的轮询间隔（3 秒）：大于等于它时每一轮轮询都会命中缓存，
// 采集器拿不到新的计数快照，速率会一直显示同一个值，看起来像卡住了。
// 这个窗口只用于合并"多个窗口同时打开"带来的重复请求。
const netTTL = 2 * time.Second

// handleNetwork 返回目标主机的网络现状：接口、地址、路由、DNS、监听端口。
//
// 这个接口只读，不做任何变更 —— 改地址、建网桥会在改动的瞬间断开 SSH，
// 必须与确认回滚机制配套提供，不能混在采集接口里。
func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	fetch := func() (*sysinfo.NetworkSnapshot, error) {
		return s.fetchNetwork(r.Context(), hostID)
	}

	var (
		snapshot *sysinfo.NetworkSnapshot
		err      error
	)
	if r.URL.Query().Get("fresh") == "1" {
		snapshot, err = s.network.getFresh(hostID, fetch)
	} else {
		snapshot, err = s.network.get(hostID, fetch)
	}
	if err != nil {
		s.writeHostError(w, err, "读取网络信息失败")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
