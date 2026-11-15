package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"panelssh/internal/sysinfo"
)

const (
	// netChangeTimeoutSec 是 NetworkManager 检查点的存活时间，同时也是留给用户
	// 确认的时间窗口。窗口结束时若没收到确认，NM 会把配置回滚到变更前。
	//
	// 取 120 秒而不是更短：跨地域线路加上目标机重新激活连接的开销，
	// 恢复到能重新 SSH 常常要十几秒；窗口太短会出现"用户还没看到窗口
	// 就已经回滚了"的荒唐结果。
	netChangeTimeoutSec = 120

	// netChangeDelaySec 是变更命令延迟执行的秒数。
	//
	// 必须是正数：这些命令一旦生效就会切断面板自己那条 SSH，
	// 若与下发动作挤在同一个瞬间，会话被掐断时命令可能还没跑到，
	// 结果既没改成新的、也没留下任何线索。
	netChangeDelaySec = 3
)

// networkChange 是一次已下发、等待确认的网络变更。
type networkChange struct {
	ID      string   `json:"id"`
	HostID  int64    `json:"hostId"`
	Action  string   `json:"action"`
	Summary string   `json:"summary"`
	Risky   []string `json:"risky"`
	// Checkpoint 是 NetworkManager 检查点标识，回滚时定位用。
	Checkpoint string    `json:"-"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	// Expect 描述变更成功后的预期状态，用来区分"生效了"与"被回滚了"。
	Expect *sysinfo.NetChangeExpect `json:"-"`
}

// netChangeStore 记录每台主机当前未确认的变更。
//
// 每台主机同时只允许一个：NetworkManager 的检查点回滚不带参数时回滚的是
// 最近一个检查点，多个变更并行下发会让回滚落到错误的目标上。
type netChangeStore struct {
	mu     sync.Mutex
	byHost map[int64]*networkChange
}

func newNetChangeStore() *netChangeStore {
	return &netChangeStore{byHost: make(map[int64]*networkChange)}
}

func (s *netChangeStore) put(change *networkChange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byHost[change.HostID] = change
}

func (s *netChangeStore) get(hostID int64) *networkChange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byHost[hostID]
}

func (s *netChangeStore) delete(hostID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byHost, hostID)
}

// active 返回仍在确认窗口内的变更。已过窗口的记录由 NM 负责回滚，
// 面板这边不再把它当作"待确认"，否则用户会被一个永远不会生效的横幅卡住。
func (s *netChangeStore) active(hostID int64, now time.Time) *networkChange {
	change := s.get(hostID)
	if change == nil {
		return nil
	}
	if now.After(change.ExpiresAt) {
		return nil
	}
	return change
}

func newChangeID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// scriptPath 与 logPath 是目标机上这次变更要用的文件。
// 名字里带变更 ID，避免两次变更互相覆盖彼此的输出。
func scriptPath(id string) string { return "/tmp/panelssh-net-" + id + ".sh" }
func logPath(id string) string    { return "/tmp/panelssh-net-" + id + ".log" }

// changeScript 生成下发到目标机的延迟执行脚本。
//
// 用 heredoc 写入文件而不是把命令直接塞进 `sh -c`：命令里的连接名可能带空格
// 和中文（NM 默认连接名就是"有线连接 1"），塞进单行 shell 字符串要处理层层转义，
// 而 heredoc 里除了结束标记什么都不需要转义。
func changeScript(id string, commands []string) string {
	var body strings.Builder
	body.WriteString("cat > " + scriptPath(id) + " <<'PANELSSH_SCRIPT_EOF'\n")
	body.WriteString("#!/bin/sh\n")
	// 先删自己：脚本内容已经被 shell 读进内存，unlink 之后仍然能继续执行，
	// 这样无论成功失败都不会在 /tmp 留下垃圾脚本。
	body.WriteString("rm -f \"$0\"\n")
	// 把后续输出（含 set -x 打印的命令）落到日志里，用户能在面板上看到
	// 每条命令的退出情况 —— 否则变更失败后没有任何可查的痕迹。
	body.WriteString("exec >>" + logPath(id) + " 2>&1\n")
	// 日志随后由面板以普通用户读回（变更进行中的轮询不该为此提权），
	// 而脚本是以 root 跑的：这里显式放开读权限，不受目标机 umask 影响。
	body.WriteString("chmod 0644 " + logPath(id) + " 2>/dev/null\n")
	body.WriteString("set -x\n")
	body.WriteString("sleep " + strconv.Itoa(netChangeDelaySec) + "\n")
	for _, command := range commands {
		body.WriteString(command + "\n")
	}
	body.WriteString("PANELSSH_SCRIPT_EOF\n")
	body.WriteString("chmod +x " + scriptPath(id) + "\n")
	// setsid 让脚本进入独立会话：它必须在面板的 SSH 会话消失之后继续跑完。
	body.WriteString("setsid " + scriptPath(id) + " </dev/null >/dev/null 2>&1 &\n")
	body.WriteString("echo started\n")
	return body.String()
}

// isOvsChange 判断一次变更是否走 OVS 的自建保护。
//
// 判据是动作本身而不是目标机上有没有 OVS：同一个网口可能既有 NM 连接
// 又挂在 OVS 网桥上，按环境推断会让"保护方式"变成一件用户看不出来的事，
// 而它恰恰是这个功能里最不能含糊的部分。
func isOvsChange(action string) bool {
	switch action {
	case sysinfo.NetChangeOvsAttach,
		sysinfo.NetChangeOvsDetach,
		sysinfo.NetChangeOvsBridgeCreate,
		sysinfo.NetChangeOvsBridgeDelete:
		return true
	}
	return false
}

// confirmNetworkChange 确认一次变更，撤掉保护装置让它永久生效。
//
// 两条路的保护装置完全不同，但对外只有"确认"这一个语义：
//   - NetworkManager 类：销毁检查点，NM 便不再回滚；
//   - OVS 类：写下确认标记，让目标机上的看门狗到点后自行退场。
func (s *Server) confirmNetworkChange(ctx context.Context, hostID int64, change *networkChange) (string, error) {
	if isOvsChange(change.Action) {
		return s.runEscalated(ctx, hostID, sysinfo.OvsConfirmCommand(change.ID), commandTimeout, escalateRequired)
	}
	destroy, err := sysinfo.CheckpointDestroyCommand(change.Checkpoint)
	if err != nil {
		return "", fmt.Errorf("检查点标识异常：%w", err)
	}
	return s.runEscalated(ctx, hostID, destroy, commandTimeout, escalateRequired)
}

// revertNetworkChange 把目标机恢复到变更前，并撤掉保护装置。
//
// 两条路在这里同样只剩一个语义："回到变更之前"。OVS 那条特意先写确认标记
// 再跑恢复脚本，避免看门狗到点后与这里并发执行恢复（见 OvsRollbackCommand）。
func (s *Server) revertNetworkChange(ctx context.Context, hostID int64, change *networkChange) (string, error) {
	if isOvsChange(change.Action) {
		return s.runEscalated(ctx, hostID, sysinfo.OvsRollbackCommand(change.ID), commandTimeout, escalateRequired)
	}
	rollback, err := sysinfo.CheckpointRollbackCommand(change.Checkpoint)
	if err != nil {
		return "", fmt.Errorf("检查点标识异常：%w", err)
	}
	return s.runEscalated(ctx, hostID, rollback, commandTimeout, escalateRequired)
}

// fetchNetwork 采集一次最新的网络快照。
//
// 网络信息、提权重采的监听表、OVS 现状是三段采集：第一段按登录账号执行，
// 后两段必须换个身份（ss -p 只对 root 显示别人的 pid，读 OVS 数据库要 root，
// 见 sysinfo.ListenCommand / OvsCommand）。三段输出拼起来交给同一个解析入口 ——
// 分节标记互不重叠，Collect 不必知道自己收到的是几次采集的结果。
func (s *Server) fetchNetwork(ctx context.Context, hostID int64) (*sysinfo.NetworkSnapshot, error) {
	output, err := s.runOnHost(hostID, sysinfo.NetworkCommand())
	if err != nil {
		return nil, err
	}
	// 监听表这一段值得换个身份重采：ss -p 只有 root 才看得到别人的 pid，
	// 而界面上"结束这个监听进程"要的正是 pid。提不了权时它返回空串，解析层
	// 回落到第一段里那份（见 sysinfo.pickListenSection）—— 端口照旧显示完整，
	// 只是结束不了进程。
	//
	// 判据在拼接之前取：hasOvs 看的是第一段的 ###TOOLS，凑进后面两段的输出
	// 再判，就是拿别人的正文去回答"这台机器装没装 ovs-vsctl"。
	hasOvs := sysinfo.HasOvsTool(output)
	output += "\n" + s.fetchListenRaw(ctx, hostID)
	// 没装 ovs-vsctl 的机器上整段跳过：绝大多数目标机没有 OVS，
	// 不该为看一眼根本不存在的东西去申请一次提权。
	if hasOvs {
		output += "\n" + s.fetchOvsRaw(ctx, hostID)
	}
	return s.netCollector.Collect(hostID, output, time.Now())
}

// fetchListenRaw 以 root 重采一次监听表，提不了权就返回空串。
//
// 为什么值得多跑一次：ss 的 -p 只对"执行者有权看到的进程"输出 pid，登录用户看
// root 起的 sshd、nginx、docker-proxy 时那一栏整个是空的，而界面上"结束这个监听
// 进程"恰恰要一个 pid —— 不重采，那个按钮在大多数生产机上就是死按钮。
//
// 先看一眼提权画像再决定跑不跑：提不了权的机器上这条命令换个身份还是同一个身份，
// 跑出来的 pid 一栏照样是空的，白多一条 SSH 命令而已。这里读的是画像缓存
// （hostProfile 的第二参数为 false），不会为了判断能不能提权而多做一次探测。
//
// 返回空串时解析层回落到第一段里那份按登录用户采的监听表（见 sysinfo.pickListenSection）：
// 端口照旧显示完整，只是结束不了进程。失败也在这里咽掉 —— 它只影响"能不能结束进程"
// 这一件事，不该让整页只读信息跟着报错。
func (s *Server) fetchListenRaw(ctx context.Context, hostID int64) string {
	profile, err := s.hostProfile(ctx, hostID, false)
	if err != nil || !profile.CanPrivileged() {
		return ""
	}
	output, err := s.runEscalated(ctx, hostID, sysinfo.ListenCommand(), commandTimeout, escalatePreferred)
	if err != nil {
		return ""
	}
	return output
}

// forgetNetwork 作废一台主机的网络快照与 OVS 现状。
//
// 网络配置一旦被改动，两处缓存就同时过期了 —— 它们是按不同身份采到的两半
// 现状（见 fetchNetwork）。分开调用迟早会漏掉一处，而漏掉的表现是界面上
// 新旧掺半，那种现象很难被认出来是缓存的问题。
func (s *Server) forgetNetwork(hostID int64) {
	s.network.forget(hostID)
	s.ovs.forget(hostID)
}

// handleNetworkApply 下发一次网络变更。
//
// 执行顺序刻意如此：先建检查点，再同步跑"只改配置"的命令，最后才延迟下发
// "让配置生效"的命令。这样一半的问题会在断连之前就以报错的形式回到用户面前。
func (s *Server) handleNetworkApply(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if s.rejectLocalHostChange(w, hostID, "网络变更") {
		return
	}

	var req sysinfo.NetChangeRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}

	if active := s.netChanges.active(hostID, time.Now()); active != nil {
		writeErr(w, http.StatusConflict,
			"该主机上还有一次未确认的网络变更（"+active.Summary+"），请先确认或回滚")
		return
	}

	snapshot, err := s.network.getFresh(hostID, func() (*sysinfo.NetworkSnapshot, error) {
		return s.fetchNetwork(r.Context(), hostID)
	})
	if err != nil {
		s.writeHostError(w, err, "读取网络现状失败")
		return
	}

	plan, err := sysinfo.PlanNetworkChange(req, snapshot)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	id, err := newChangeID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成变更标识失败")
		return
	}

	change := &networkChange{
		ID:        id,
		HostID:    hostID,
		Action:    req.Action,
		Summary:   plan.Summary,
		Risky:     plan.Risky,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(netChangeTimeoutSec * time.Second),
		Expect:    plan.Expect,
	}

	// 1) 建立保护装置。这一步失败必须放弃整次变更 —— 没有保护就没有回滚
	//    能力，而"没有保护也照改"正是这个功能最不能犯的错。
	//
	//    两条路互斥，取决于变更本身而不是目标机环境：
	//    NetworkManager 类交给 NM 自己的检查点；OVS 类不受 NM 托管，
	//    改用落在目标机上的状态存档加看门狗（见 sysinfo.network_ovs.go）。
	if isOvsChange(req.Action) {
		if _, err := s.runEscalated(r.Context(), hostID,
			sysinfo.OvsGuardCommand(id, snapshot, netChangeTimeoutSec), commandTimeout, escalateRequired); err != nil {
			s.writeHostError(w, err, "建立回滚保护失败（需要 root 权限），变更已取消")
			return
		}
	} else {
		// 这一步需要 root 或 polkit 授权；面板按目标机的可用路径自动提权
		// （见 privilege.go），权限仍不足时 NM 会明确拒绝。
		created, err := s.runEscalated(r.Context(), hostID,
			sysinfo.CheckpointCreateCommand(netChangeTimeoutSec), commandTimeout, escalateRequired)
		if err != nil {
			s.writeHostError(w, err, "创建回滚检查点失败（需要 root 或 polkit 授权），变更已取消")
			return
		}
		checkpoint, err := sysinfo.NormalizeCheckpointPath(sysinfo.ParseCheckpointPath(created))
		if err != nil {
			// 没拿到检查点就没法回滚。这里尚未改动任何配置，直接中止即可。
			writeErr(w, http.StatusBadGateway, "无法识别 NetworkManager 返回的检查点标识："+err.Error()+"。变更已取消")
			return
		}
		change.Checkpoint = checkpoint
	}

	// 2) 同步执行"只改配置"的命令。这一段不碰运行态，出问题可以直接返回给用户。
	for _, command := range plan.Safe {
		if _, err := s.runEscalated(r.Context(), hostID, command, commandTimeout, escalateRequired); err != nil {
			_, _ = s.revertNetworkChange(r.Context(), hostID, change)
			s.writeHostError(w, err, "变更未生效已回滚")
			return
		}
	}

	s.netChanges.put(change)

	// 3) 延迟下发让配置生效的命令。它们会切断面板的 SSH，所以只能交给
	//    目标机上的独立进程去跑，调用方拿到的只是"已下发"。
	if len(plan.Risky) > 0 {
		if _, err := s.execEscalated(r.Context(), hostID, changeScript(id, plan.Risky), 30*time.Second, escalateRequired); err != nil {
			s.netChanges.delete(hostID)
			_, _ = s.revertNetworkChange(r.Context(), hostID, change)
			s.writeHostError(w, err, "变更下发失败，已回滚")
			return
		}
	}

	// 快照缓存必须作废：界面上的旧地址会让用户以为变更没生效。
	s.forgetNetwork(hostID)

	writeJSON(w, http.StatusOK, map[string]any{
		"change":     change,
		"timeoutSec": netChangeTimeoutSec,
	})
}

// handleNetworkCommit 确认变更：销毁检查点，让新配置永久生效。
func (s *Server) handleNetworkCommit(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	change := s.netChanges.get(hostID)
	if change == nil {
		writeErr(w, http.StatusNotFound, "该主机上没有待确认的网络变更")
		return
	}

	var body struct {
		// Force 表示"明知当前状态与预期不符，仍然保留这次配置"。
		Force bool `json:"force"`
	}
	if r.ContentLength > 0 {
		if err := decodeJSONBody(w, r, &body, 1<<16); err != nil {
			return
		}
	}

	verifyNote := ""
	verified := true
	after, err := s.fetchNetwork(r.Context(), hostID)
	if err != nil {
		// 主机还没回来。这不代表失败，也不代表成功 —— 保持检查点不动，
		// 让用户再等一轮，或直接选择回滚。
		s.writeHostError(w, err, "暂时无法确认变更结果，请稍后重试或直接回滚")
		return
	}
	if verifyErr := change.Expect.Verify(after); verifyErr != nil {
		verified = false
		verifyNote = verifyErr.Error()
		if !body.Force {
			// 状态不符时绝不撤掉保护：此时它正是把机器救回来的唯一凭借。
			// 409 让前端能把这句提示原样展示，并给出"查看日志/立即回滚"的出口。
			writeErr(w, http.StatusConflict, "变更未生效："+verifyNote+"。回滚保护仍会在窗口结束时自动恢复")
			return
		}
	}

	// 状态核对通过，撤掉保护装置：变更从此永久生效。
	if _, err := s.confirmNetworkChange(r.Context(), hostID, change); err != nil {
		s.writeHostError(w, err, "确认变更失败")
		return
	}

	// OVS 的改动还要固化到目标机的开机自恢复里：这些网桥通常由目标机自己的
	// 网络服务维护，重启后网桥上不会再有手动挂上去的网口。
	//
	// 期望状态直接取刚刚核对通过的实际快照，而不是按变更动作推算一遍 ——
	// 记下"实际达成了什么"永远比记下"我以为会达成什么"更可靠。
	//
	// 失败不算变更失败：配置已经生效并确认过了。但必须让用户知道，否则
	// 他会以为重启之后这个网口还在。
	persistNote := ""
	if isOvsChange(change.Action) {
		command := sysinfo.OvsPersistCommand(sysinfo.DesiredOvsState(after.Ovs))
		if _, err := s.runEscalated(r.Context(), hostID, command, commandTimeout, escalateRequired); err != nil {
			persistNote = "变更已生效，但开机自恢复没能更新：" + err.Error() +
				"。目标机重启后这次改动可能丢失"
		}
	}

	s.netChanges.delete(hostID)
	s.forgetNetwork(hostID)
	// 日志文件是刚才以 root 跑变更脚本时创建的，普通用户删不掉它。
	_, _ = s.runEscalated(r.Context(), hostID, "rm -f "+logPath(change.ID), commandTimeout, escalateRequired)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"verified":    verified,
		"verifyNote":  verifyNote,
		"persistNote": persistNote,
	})
}

// handleNetworkRollback 立即回滚到变更前的配置。
func (s *Server) handleNetworkRollback(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	change := s.netChanges.get(hostID)
	if change == nil {
		writeErr(w, http.StatusNotFound, "该主机上没有待确认的网络变更")
		return
	}

	output, err := s.revertNetworkChange(r.Context(), hostID, change)
	if err != nil {
		s.writeHostError(w, err, "回滚失败")
		return
	}

	s.netChanges.delete(hostID)
	s.forgetNetwork(hostID)
	_, _ = s.runEscalated(r.Context(), hostID, "rm -f "+logPath(change.ID), commandTimeout, escalateRequired)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": strings.TrimSpace(output)})
}

// handleOvsUnpersist 卸掉目标机上的开机自恢复服务。
//
// 为什么需要这个显式入口：服务是在第一次 OVS 变更被确认时自动装上去的，
// 那是个有副作用的动作 —— 用户在机器上多了一个 systemd 单元。只装不卸就是
// 一扇单向门。
//
// 为什么不靠"把端口都摘掉"来自动卸载：期望状态记的是"面板看到的物理口"，
// 而这类网桥上通常还挂着目标机自己的网络服务建的上行口，它本身就是物理口，
// 永远摘不干净。那条路在这类机器上根本走不到。
//
// 卸载后如果用户再做 OVS 变更，服务会被重新装上 —— 那是自洽的：面板对
// "我改过的桥接要活过重启"这件事的承诺，与用户是否在用它的 OVS 功能同步。
func (s *Server) handleOvsUnpersist(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	if s.rejectLocalHostChange(w, hostID, "网络变更") {
		return
	}
	// 有未决变更时不允许动它：那次变更确认时还会再写一遍期望状态，
	// 现在卸掉只会让用户以为卸成功了。
	if active := s.netChanges.active(hostID, time.Now()); active != nil {
		writeErr(w, http.StatusConflict,
			"该主机上还有一次未确认的网络变更（"+active.Summary+"），请先确认或回滚")
		return
	}

	if _, err := s.runEscalated(r.Context(), hostID, sysinfo.OvsUnpersistCommand(), commandTimeout, escalateRequired); err != nil {
		s.writeHostError(w, err, "卸载开机自恢复失败")
		return
	}
	// 服务已不在了，界面上的状态跟着变。
	s.forgetNetwork(hostID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleNetworkChange 返回当前未确认的变更，以及变更脚本在目标机上的执行日志。
// 前端重新打开窗口、或每轮轮询时用它恢复"变更进行中"的状态。
func (s *Server) handleNetworkChange(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	change := s.netChanges.get(hostID)
	if change == nil {
		writeJSON(w, http.StatusOK, map[string]any{"change": nil})
		return
	}

	// 日志读取失败不算错误：主机可能正处于断连重连的过程中，
	// 那时恰恰是最需要看到这条变更记录的时候。
	log := ""
	if out, err := s.runOnHost(hostID, "cat "+logPath(change.ID)+" 2>/dev/null"); err == nil {
		log = out
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"change": change,
		"log":    strings.TrimSpace(log),
	})
}
