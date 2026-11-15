package api

import (
	"errors"
	"net/http"

	"panelssh/internal/env"
)

// 环境变量页签的接口。
//
// 读走一次 GET（采集要启动目标机的登录 shell，所以前端打开页签时要给加载态）；
// 写走同步 POST，不走 /api/ws/env：那是给"多主机并发 + 长事务步骤流式回传"准备的，
// 而改变量是单机单文件的毫秒级动作，用一次普通请求把结论直接带回来更直接。
// 步骤明细仍然逐条写进 env_ops，用户在「操作记录」里点开这条记录即可完整复盘。

// handleEnvVars 返回目标机的环境变量（登录 shell 生效值 + 定义来源）。
func (s *Server) handleEnvVars(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	ex, _, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}
	view, err := env.CollectVars(r.Context(), ex)
	if err != nil {
		s.writeHostError(w, err, "采集环境变量失败")
		return
	}
	// 可选落地文件与系统级文件路径由后端给出：界面不该自己拼路径（拼错就是往
	// 别处写文件），也不该硬编码一个以后会变的字符串。
	writeJSON(w, http.StatusOK, map[string]any{
		"shell":      view.Shell,
		"home":       view.Home,
		"files":      view.Files,
		"vars":       view.Vars,
		"truncated":  view.Truncated,
		"userFiles":  env.UserVarFiles(),
		"systemFile": env.SystemVarFile,
	})
}

// handleEnvScan 扫描目标机上"装好了但当前 shell 用不到"的命令行环境。
//
// 只读，且刻意不缓存：扫描的答案取决于目标机此刻的状态（刚装完应用、刚删过目录），
// 而用户点这个按钮的动机就是"我怀疑刚才装的东西没进 PATH" ——
// 给一份几分钟前的缓存正好答错这个问题。
func (s *Server) handleEnvScan(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	ex, _, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}
	view, err := env.ScanEnv(r.Context(), ex)
	if err != nil {
		s.writeHostError(w, err, "扫描环境失败")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleEnvVarPathAdd 把一个目录加入 PATH。
//
// 单独一个动作而不是"用 env-set 写一个 PATH 变量"：PATH 是累加语义的变量，
// 用字面量写法会把当时的 PATH 整串冻结在文件里（见 env/vars_path.go 的说明）。
func (s *Server) handleEnvVarPathAdd(w http.ResponseWriter, r *http.Request) {
	s.envVarRequest(w, r, env.ActionEnvPathAdd)
}

// handleEnvVarSet 新增或覆盖一个环境变量。
func (s *Server) handleEnvVarSet(w http.ResponseWriter, r *http.Request) {
	s.envVarRequest(w, r, env.ActionEnvSet)
}

// handleEnvVarUnset 删除一个环境变量（只删面板可写文件里的定义）。
func (s *Server) handleEnvVarUnset(w http.ResponseWriter, r *http.Request) {
	s.envVarRequest(w, r, env.ActionEnvUnset)
}

// handleEnvVarRestore 把某个变量文件还原成某次操作之前的内容。
func (s *Server) handleEnvVarRestore(w http.ResponseWriter, r *http.Request) {
	s.envVarRequest(w, r, env.ActionEnvRestore)
}

// envVarRequest 收下请求体、定下动作，然后交给 execEnvVar。
func (s *Server) envVarRequest(w http.ResponseWriter, r *http.Request, action string) {
	var req env.VarRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	// 动作由路由决定，不认请求体里的 action：否则前端一个笔误就能让"删除"
	// 变成"写入"，而这两件事的后果完全不同。
	req.Action = action
	s.execEnvVar(w, r, req)
}

// execEnvVar 执行一次环境变量变更。
func (s *Server) execEnvVar(w http.ResponseWriter, r *http.Request, req env.VarRequest) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	// 参数不合法就没必要连目标机：白白多一次 SSH 往返只换来一个错误。
	if err := validateEnvVarReq(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 系统级变量的落地文件在 /etc 下，必须 root；用户级改的是自己的家目录，
	// 不提权也能做。这里只提前判定"前端已经说明是系统级"的情况，好让用户在
	// 点确认之前就拿到"这台机器改不了系统级变量"的结论，而不是等执行完才失败。
	//
	// 还原操作的 scope 存在原操作的计划存档里（前端并不知道），不能在这里判 ——
	// 但它同样不会出错：RunVar 内部按存档里的 scope 决定提权，提不了权时返回的是
	// 同一个 ErrNoPrivilege，下面的 envDomainError 一样会把它映射成 400。
	var (
		profile *env.Profile
		err     error
	)
	if req.Scope == env.ScopeSystem {
		profile, err = s.escalationProfile(r.Context(), hostID, escalateRequired)
	} else {
		profile, err = s.hostProfile(r.Context(), hostID, false)
	}
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "系统画像探测失败")
		}
		return
	}

	ex, hostName, err := s.envTarget(hostID)
	if err != nil {
		s.writeHostError(w, err, "连接目标主机失败")
		return
	}
	result, err := s.envRunner.RunVar(r.Context(),
		env.Target{HostID: hostID, HostName: hostName, Exec: ex}, profile, req)
	if err != nil {
		if !envDomainError(w, err) {
			writeErr(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// validateEnvVarReq 在连目标机之前把能判的参数错误判掉。
//
// 判据与环境包共用（ValidVarName / ValidateVarValue）：界面允许提交什么、
// 执行时接受什么，只该有一个定义处 —— 两处各写一份，迟早出现"界面上能填、
// 执行时被拒"或者更糟的反过来。
func validateEnvVarReq(req env.VarRequest) error {
	if req.Action == env.ActionEnvRestore {
		if req.OriginOpID <= 0 {
			return errors.New("缺少要还原的操作 ID")
		}
		return nil
	}
	if req.Scope != env.ScopeUser && req.Scope != env.ScopeSystem {
		return errors.New("请选择作用域：用户级（只对这个登录用户生效）或系统级（对这台机器上所有用户生效）")
	}
	if req.Action == env.ActionEnvPathAdd {
		return env.ValidateVarPathDir(req.Dir)
	}
	// PATH 只能走 pathAdd：字面量写入会把它冻结，按前缀删行会破坏用户的启动文件。
	// 判据放在这里是为了在连目标机之前就拒掉，文案与执行侧同一份（ValidateVarWritable）。
	if err := env.ValidateVarWritable(req.Name); err != nil {
		return err
	}
	if !env.ValidVarName(req.Name) {
		return errors.New("变量名不合法：只允许字母、数字、下划线，且不能以数字开头")
	}
	if req.Action == env.ActionEnvSet {
		return env.ValidateVarValue(req.Value)
	}
	return nil
}
