package api

import (
	"context"
	"net/http"
	"time"
)

const (
	// profileTTL 是系统画像（含账号角色）的缓存窗口。
	//
	// 比在线状态（10 秒）长得多：取一次画像要真的登录目标机并跑一段探测脚本，
	// 而账号权限、包管理器这些属性不会在几分钟内变化。窗口内多次请求共享同一次
	// 探测，提权决策与「角色」列读的也是这一份。
	profileTTL = 5 * time.Minute

	// roleProbeBudget 是整批探测的预算上限。
	//
	// 逐台登录比 TCP 探测贵得多（握手 + 认证 + 一段脚本），且不可达的主机会
	// 一直耗到单次探测超时。给整批设上限：到点就返回已探到的部分，其余留作
	// 「未知」由用户点刷新再试 —— 而不是让请求挂在那里等所有机器。
	roleProbeBudget = 20 * time.Second

	// roleConcurrency 是在飞的登录数上限。每次探测都要建 SSH 会话并在目标机上
	// 执行脚本，比 TCP 建连重得多，因此比状态探测的 32 低不少。
	roleConcurrency = 8
)

// hostRole 是单台主机登录账号的角色（列表页「角色」列）。
//
// PrivilegeMode 为空串表示"还没探到"：离线、凭据错误或超出探测预算。
// 它与 env.PrivNone（探到了，但这台机器上没法提权）是两回事 —— 把前者
// 当成后者，会把一台连不上的机器安静地标成"普通账号"。
type hostRole struct {
	ID            int64  `json:"id"`
	PrivilegeMode string `json:"privilegeMode"`
}

// handleHostRoles 返回全部主机的账号角色分类。
//
// 这是列表页里唯一会真的登录每一台机器的接口 —— "这台机器上的账号是什么角色"
// 只能连上去才知道。因此它带 5 分钟缓存，且整批有预算上限。
func (s *Server) handleHostRoles(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询主机失败")
		return
	}
	fresh := r.URL.Query().Get("fresh") != ""

	ctx, cancel := context.WithTimeout(r.Context(), roleProbeBudget)
	defer cancel()

	// 每台主机只写自己那一格，结果经 channel 汇总，避免并发写同一个切片。
	type probed struct {
		index int
		mode  string
	}
	results := make(chan probed, len(hosts))
	sem := make(chan struct{}, roleConcurrency)
	for i, host := range hosts {
		go func(i int, id int64) {
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- probed{i, s.roleOf(ctx, id, fresh)}
		}(i, host.ID)
	}

	modes := make([]string, len(hosts))
	for range hosts {
		select {
		case p := <-results:
			modes[p.index] = p.mode
		case <-ctx.Done():
			// 预算用完：剩下的留空（未知），前端显示为 —。
		}
	}

	out := make([]hostRole, len(hosts))
	for i, host := range hosts {
		out[i] = hostRole{ID: host.ID, PrivilegeMode: modes[i]}
	}
	writeJSON(w, http.StatusOK, out)
}

// roleOf 探测一台主机的账号角色；探测失败返回空串（未知）。
//
// 角色是系统画像的一个视点，与提权决策共用同一份缓存，因此口径天然一致 ——
// "角色列说可提权"与"变更时真的会提权"不可能各说各话。
func (s *Server) roleOf(ctx context.Context, hostID int64, fresh bool) string {
	profile, err := s.hostProfile(ctx, hostID, fresh)
	if err != nil {
		return ""
	}
	return string(profile.PrivilegeMode())
}
