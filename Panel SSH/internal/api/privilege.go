package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"panelssh/internal/env"
)

// 本文件是"用户不该关心 root 这件事"的落点。
//
// 环境管理（装/升级/换源）早就是这么做的：由 env.Wrap 按目标机情况自动选
// root / su。这里把同一套能力提到 api 层，供存储变更、网络变更、结束进程
// 这些同样需要 root 的模块复用 —— 面板自己知道哪条命令需要提权（按命令性质
// 判定，不靠"失败了再试一次"），用户只需要看到结果。

// escalateIntent 决定"目标机没有可用提权路径时怎么办"。
type escalateIntent int

const (
	// escalateRequired：这条命令离了 root 一定失败（格式化、挂载、改网卡配置）。
	// 拿不到 root 时直接报错，而不是让用户对着 "Permission denied" 猜。
	escalateRequired escalateIntent = iota

	// escalatePreferred：有 root 更稳、没有也能试（结束进程：杀自己的进程不需要 root）。
	// 拿不到 root 时按普通用户执行，保住今天已经能用的那些场景。
	escalatePreferred
)

// hostProfile 返回目标机的系统画像，按主机缓存。
//
// 提权决策与列表页「角色」列都读这一份，因此缓存窗口内不会再打扰目标机；
// 唯一的例外见 escalationProfile 的"准备拒绝时重探"。
func (s *Server) hostProfile(ctx context.Context, hostID int64, fresh bool) (*env.Profile, error) {
	load := func() (*env.Profile, error) {
		ex, _, err := s.envTarget(hostID)
		if err != nil {
			return nil, err
		}
		return env.Probe(ctx, ex)
	}
	if fresh {
		return s.profiles.getFresh(hostID, load)
	}
	return s.profiles.get(hostID, load)
}

// escalationProfile 决定这条命令走不走提权：返回 nil 表示"按普通用户执行"。
func (s *Server) escalationProfile(ctx context.Context, hostID int64, intent escalateIntent) (*env.Profile, error) {
	profile, err := s.hostProfile(ctx, hostID, false)
	if err != nil {
		return nil, err
	}
	if profile.CanPrivileged() {
		return profile, nil
	}

	// 缓存里的结论可能是几分钟前的，而用户很可能刚在面板里给这台主机补上提权口令就来点变更。
	// 因此只在"准备拒绝"这条路上重探一次，别让过期画像挡住刚配好的机器。
	fresh, ferr := s.hostProfile(ctx, hostID, true)
	if ferr == nil && fresh.CanPrivileged() {
		return fresh, nil
	}
	if intent == escalatePreferred {
		return nil, nil
	}
	// 不再缀一句解释：ErrNoPrivilege 自身已经把三条路径讲清楚了，
	// 重复一遍只会让同一句话在界面上出现两次。
	return nil, env.ErrNoPrivilege
}

// execEscalated 以 root 执行一段脚本：按目标机的可用路径自动提权。
//
// 返回值与 execOnHost 同口径：退出码非 0 不算错误，只有"没能跑完"才返回 error。
func (s *Server) execEscalated(ctx context.Context, hostID int64, script string, timeout time.Duration, intent escalateIntent) (*execResult, error) {
	profile, err := s.escalationProfile(ctx, hostID, intent)
	if err != nil {
		// escalationProfile 只在两件事上返回错误：拿不到画像（连不上），
		// 或"必须提权却提不了"。后者是给用户的结论，必须原样上报；
		// 前者对"能提就提"这类命令可以退化为普通执行，让真正的失败原因露出来。
		if intent == escalateRequired || errors.Is(err, env.ErrNoPrivilege) {
			return nil, err
		}
		return s.execOnHost(ctx, hostID, script, timeout)
	}
	if profile == nil {
		// 目标机提不了权，但这条命令本来就不强求 root。
		return s.execOnHost(ctx, hostID, script, timeout)
	}

	ex, _, err := s.envTarget(hostID)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	res, err := ex.Run(ctx, env.Wrap(script, profile), timeout)
	result := &execResult{DurationMS: time.Since(started).Milliseconds()}
	if res != nil {
		result.Stdout, result.Stderr, result.ExitCode = res.Stdout, res.Stderr, res.ExitCode
		// 超限提示已由 env 侧写进正文（"[输出超过上限，已截断]"），
		// 这里不再重复置 Clipped 标志，否则同一条消息会说两遍。
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// runEscalated 以 root 执行并返回标准输出；非零退出视为错误（与 runOnHost 同口径）。
func (s *Server) runEscalated(ctx context.Context, hostID int64, command string, timeout time.Duration, intent escalateIntent) (string, error) {
	res, err := s.execEscalated(ctx, hostID, command, timeout, intent)
	if err != nil {
		return "", err
	}
	if res.ExitCode == 0 {
		return res.Stdout, nil
	}
	message := strings.TrimSpace(res.Stderr)
	if message == "" {
		message = strings.TrimSpace(res.Stdout)
	}
	if message == "" {
		message = fmt.Sprintf("命令以退出码 %d 结束", res.ExitCode)
	}
	return res.Stdout, errors.New(message)
}

// privilegeView 是"这台机器会怎么提权"的接口返回。
type privilegeView struct {
	Mode string `json:"mode"`
	// Label 是给界面直接显示的说法，不让前端再翻译一遍 mode。
	Label string `json:"label"`
	// Escalatable 为假表示这台机器上根本提不了权：需要在操作前就告诉用户，
	// 而不是让他点了变更才看到失败。
	Escalatable bool `json:"escalatable"`
}

// handleHostPrivilege 返回一台主机的提权方式。
//
// 界面在做需要 root 的操作前用它弹提醒；画像走缓存，不会因为打开一个窗口就连两次目标机。
func (s *Server) handleHostPrivilege(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	profile, err := s.hostProfile(r.Context(), hostID, r.URL.Query().Get("fresh") != "")
	if err != nil {
		if !envDomainError(w, err) {
			s.writeHostError(w, err, "系统画像探测失败")
		}
		return
	}
	mode := profile.PrivilegeMode()
	writeJSON(w, http.StatusOK, privilegeView{
		Mode:        string(mode),
		Label:       privilegeModeLabel(mode),
		Escalatable: mode != env.PrivNone,
	})
}

// privilegeModeLabel 把提权方式写成界面上能读的话。
func privilegeModeLabel(mode env.PrivilegeMode) string {
	switch mode {
	case env.PrivRoot:
		return "已以 root 登录"
	case env.PrivSu:
		return "su + 提权口令"
	case env.PrivSudoNoPass:
		return "免密 sudo"
	case env.PrivSudo:
		return "sudo + 登录口令"
	}
	return "无法提权"
}
