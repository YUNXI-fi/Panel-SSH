package env

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"panelssh/internal/store"
)

// 本文件编排环境变量的变更：备份 → 改写 → 复核，结果登记进 env_ops。
//
// 为什么不复用 Runner.Run：那条流水线是给包管理写的，有两件事与环境变量相冲。
// 一是它无条件拍全机包清单快照（TakeSnapshot），对"改一行变量"完全是白花几秒到
// 几十秒的代价；二是它坚持目标机必须能提权（profile.CanPrivileged），而用户级变量
// 本来就该由登录用户自己改 —— 一台提不了权的机器上，改自己的 ~/.profile 完全可行，
// 用包管理那条路会被直接拒掉。
//
// 但审计与回滚的约定**完全沿用**同一套：操作进 env_ops（动作 env-set/env-unset/
// env-restore）、备份落同一个快照目录约定、失败同样留下可读的步骤日志。
// 于是「操作记录」页签与回滚入口对变量变更一样成立。

// VarRequest 是一次环境变量变更请求。
type VarRequest struct {
	Action string `json:"action"`
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Scope  string `json:"scope"`
	// File 是用户可选的落地文件（仅用户级有意义，取 UserVarFiles 里的某一项）。
	// 留空时用默认文件。绝不接受家目录之外的路径，校验见 VarTargetFile。
	File string `json:"file,omitempty"`
	// Dir 只在把目录加入 PATH（ActionEnvPathAdd）时使用。
	Dir string `json:"dir,omitempty"`
	// OriginOpID 只在还原（ActionEnvRestore）时使用，指向被还原的那次操作。
	OriginOpID int64 `json:"originOpId,omitempty"`
}

// VarOpResult 是一次环境变量变更的结论。
type VarOpResult struct {
	OpID   int64  `json:"opId"`
	Status string `json:"status"`
	// File 是实际改动的文件。界面必须如实显示它：用户改的是"环境变量"，
	// 落到磁盘上却是某一行，这个落差是排查问题的第一手信息。
	File string `json:"file"`
	// Verified 为真表示复核通过：新起的登录会话里该变量的状态与预期一致。
	Verified bool `json:"verified"`
	// SessionValue 是复核时在新登录会话里读到的值（unset 且确实消失时为空）。
	SessionValue string `json:"sessionValue,omitempty"`
	// Summary 是一句可读的结论，界面直接显示。
	Summary string `json:"summary"`
	// Note 解释复核未通过或结果与预期不同的原因。
	Note string `json:"note,omitempty"`
}

// varStepTimeout 是单个变量操作步骤的时限。
//
// 比采集的 25s 再宽一点：写入本身是毫秒级，但复核那一步要真起一个登录 shell，
// 而用户 rc 文件里有网络请求、条件判断的情况并不罕见。超时会如实报成失败，
// 不会假装成功。
const varStepTimeout = 30 * time.Second

// RunVar 执行一次环境变量变更。
//
// profile 由调用方传入而不是在这里探测：调用方（API 层）需要先据它给出
// "这台机器能不能改系统级变量"的结论，两处用同一份画像才不会有分歧。
// 用户级操作不使用 profile —— 见 varWrapProfile。
func (r *Runner) RunVar(ctx context.Context, t Target, profile *Profile, req VarRequest) (*VarOpResult, error) {
	switch req.Action {
	case ActionEnvSet, ActionEnvUnset, ActionEnvPathAdd:
		return r.runVarWrite(ctx, t, profile, req)
	case ActionEnvRestore:
		return r.runVarRestore(ctx, t, profile, req)
	}
	return nil, fmt.Errorf("未知的环境变量操作：%s", req.Action)
}

// 并发取舍：这里刻意不取 hostLock。
//
// 那把锁的语义是"同一主机同时只允许一个环境变更事务"，它要防的是包管理器
// 互相踩（两个 apt 同时跑确实会坏）。变量写入是单文件的毫秒级读写，与包管理
// 之间没有共享状态，套上同一把锁只会让用户在装软件时连一个变量都改不了。
// 同一个文件的并发写由界面保证（编辑弹窗一次只提交一次），不在这里造锁。
func (r *Runner) runVarWrite(ctx context.Context, t Target, profile *Profile, req VarRequest) (*VarOpResult, error) {
	pathAdd := req.Action == ActionEnvPathAdd
	setting := req.Action == ActionEnvSet

	// 加入 PATH 时"名字"是固定的 PATH，被操作的对象是那个目录：操作记录里显示的
	// 应该是"/var/apps/x/target/bin"，而不是又一条叫 PATH 的记录。
	target := req.Name
	if pathAdd {
		if err := ValidateVarPathDir(req.Dir); err != nil {
			return nil, err
		}
		req.Name, req.Value, target = VarPathName, req.Dir, req.Dir
	} else if err := ValidateVarWritable(req.Name); err != nil {
		// PATH 不走"设置 / 删除变量"这条路：字面量写入会把它冻结进文件，按 "PATH="
		// 前缀删行又会连用户写在 if 判断里的累加行一起删掉（见 ValidateVarWritable）。
		// 上面那个分支就是 PATH 的唯一入口，所以这里只判"非 pathAdd"的其余动作。
		return nil, err
	}
	if !pathAdd && !ValidVarName(req.Name) {
		return nil, fmt.Errorf("变量名不合法（只允许字母、数字、下划线，且不能以数字开头）：%s", req.Name)
	}
	if setting {
		if err := ValidateVarValue(req.Value); err != nil {
			return nil, err
		}
	}

	file, wrap, err := r.varTarget(ctx, t.Exec, profile, req.Scope, req.File)
	if err != nil {
		return nil, err
	}

	// 步骤名在这里先定下来，plan 与步骤日志共用：用户在操作详情里看到的顺序
	// 与 plan 里预告的完全一致。
	backupStep := "读取并保存变更前的文件内容"
	writeStep := "从文件中删除该变量"
	verifyStep := "在新起的登录会话中读取该变量"
	switch req.Action {
	case ActionEnvSet:
		writeStep = "写入变量定义（先清掉文件里的旧定义）"
	case ActionEnvPathAdd:
		writeStep = "把这个目录写入 PATH（整组重写面板管理的行）"
		verifyStep = "在新起的登录会话中核对 PATH"
	}

	plan := VarPlan{
		Action: req.Action,
		Kind:   "env",
		Target: target,
		Steps:  []string{backupStep, writeStep, verifyStep},
		Name:   req.Name,
		Value:  req.Value,
		Scope:  req.Scope,
		File:   file,
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	opID, err := r.st.CreateEnvOp(t.HostID, t.HostName, req.Action, target, string(planJSON), "", 0)
	if err != nil {
		return nil, err
	}
	snapDir := r.snapshotDir(t.HostID, opID)
	if err := r.st.SetEnvOpSnapshotDir(opID, snapDir); err != nil {
		return nil, err
	}

	// ---- 步骤 1：备份 ----
	// 备份文件落在面板侧：目标机上不留面板的痕迹，备份本身也不会被目标机上的
	// 任何操作改动。内容为空与文件不存在是两件事，靠标记文件区分（见 WriteVarBackup）。
	//
	// 内容同时留在这份内存里：加入 PATH 要据它算出"面板原先管着哪些目录"，
	// 而集合运算放在面板侧做才测得动（见 vars_path.go 的说明）。
	var backupContent []byte
	if _, err := r.runVarStep(ctx, t.Exec, wrap, opID, 1, backupStep, VarBackupScript(file),
		func(stdout string) error {
			content, existed, perr := ParseVarBackup(stdout)
			if perr != nil {
				return perr
			}
			backupContent = content
			return WriteVarBackup(snapDir, content, existed)
		}); err != nil {
		return nil, r.failVarOp(opID, req, err)
	}

	// ---- 步骤 2：改写 ----
	var script string
	if pathAdd {
		// 新目录放最前：用户做这件事的动机就是"我要用这个应用里的 node"，
		// 排到后面会被 /usr/bin 里的同名命令一直挡住，等于没加。
		script, err = VarPathSetScript(file, append([]string{req.Dir}, VarPathDirs(backupContent)...))
	} else {
		script, err = varWriteScript(setting, file, req.Name, req.Value)
	}
	if err != nil {
		return nil, r.failVarOp(opID, req, err)
	}
	if _, err := r.runVarStep(ctx, t.Exec, wrap, opID, 2, writeStep, script, nil); err != nil {
		return nil, r.failVarOp(opID, req, err)
	}

	// ---- 步骤 3：复核（新登录会话里到底有没有、是什么） ----
	probe := VarProbeResult{}
	if _, err := r.runVarStep(ctx, t.Exec, wrap, opID, 3, verifyStep, varProbeScript(req.Name),
		func(stdout string) error {
			probe = ParseVarProbe(stdout)
			return nil
		}); err != nil {
		return nil, r.failVarOp(opID, req, err)
	}

	verified, note, summary := judgeVarWrite(setting, req, file, probe)
	if pathAdd {
		verified, note, summary = judgeVarPathAdd(req.Dir, file, probe)
	}
	status := store.EnvStatusSuccess
	finishErr := r.st.FinishEnvOp(opID, store.EnvOpResult{
		Status: status, Verified: verified, VerifyNote: note, Summary: summary,
	})
	if finishErr != nil {
		return nil, finishErr
	}
	return &VarOpResult{
		OpID: opID, Status: status, File: file,
		Verified: verified, SessionValue: probe.Value, Summary: summary, Note: note,
	}, nil
}

// judgeVarPathAdd 判定"目录已加入 PATH"的复核结论。
//
// 只认一件事：新起的登录会话里，那条 PATH 里到底有没有这个目录。
// 不去逐字符比对 PATH 的值 —— 目标机上还有别的东西在改 PATH（rc 文件、pam、
// systemd 会话），比出"不一样"没有任何可操作的结论；"有没有这个目录"才有。
func judgeVarPathAdd(dir, file string, probe VarProbeResult) (bool, string, string) {
	if !probe.Set {
		note := fmt.Sprintf("文件已写入，但新登录会话里读不到 PATH —— 无法确认 %s 是否生效", dir)
		return false, note, fmt.Sprintf("已写入 %s，但没能复核", file)
	}
	for _, got := range strings.Split(probe.Value, ":") {
		if got != "" && strings.TrimSuffix(got, "/") == strings.TrimSuffix(dir, "/") {
			return true, "", fmt.Sprintf("已把 %s 写入 %s，并确认新的登录会话里它已在 PATH 中", dir, file)
		}
	}
	note := fmt.Sprintf("文件已写入，但新登录会话的 PATH 里没有 %s：该文件可能不在这个 shell 的启动路径上"+
		"（例如写进了 ~/.profile 而登录用的是 zsh）", dir)
	return false, note, fmt.Sprintf("已写入 %s，但新登录会话的 PATH 里没有它", file)
}

// judgeVarWrite 判定写入结果的复核结论。
//
// 三种情形要分开说，因为它们对应三种完全不同的处置方式：
//   - 与预期一致 → 成功；
//   - 写进去了但值不是设的那个 → 有别处更晚的定义盖住了它，用户需要去看变量列表里的来源；
//   - 写进去了但读不到 → 这个文件不在该 shell 的启动路径上（或条件分支没走到）。
func judgeVarWrite(setting bool, req VarRequest, file string, probe VarProbeResult) (bool, string, string) {
	if setting {
		switch {
		case probe.Set && probe.Value == req.Value:
			return true, "", fmt.Sprintf("已在 %s 写入 %s，并确认新的登录会话里已生效", file, req.Name)
		case probe.Set:
			note := fmt.Sprintf("文件已写入，但新登录会话里读到的是 %q，与设置的值不同 —— 说明有更晚的定义覆盖了它。"+
				"回到列表里查看 %s 的来源", probe.Value, req.Name)
			return false, note, fmt.Sprintf("已写入 %s，但被更晚的定义覆盖", file)
		default:
			note := fmt.Sprintf("文件已写入，但新登录会话里读不到 %s：该文件可能不在这个 shell 的启动路径上"+
				"（例如写进了 ~/.profile 而登录用的是 zsh，或定义在条件分支里）", req.Name)
			return false, note, fmt.Sprintf("已写入 %s，但新登录会话里读不到", file)
		}
	}
	if !probe.Set {
		return true, "", fmt.Sprintf("已从 %s 删除 %s 的定义，并确认新的登录会话里它已不存在", file, req.Name)
	}
	note := fmt.Sprintf("已从 %s 删除，但新登录会话里它依然生效（当前值 %q）—— 还有别的文件定义它，"+
		"可在变量列表里查看它的来源", file, probe.Value)
	return false, note, fmt.Sprintf("已从 %s 删除，但变量仍由别处定义", file)
}

// runVarRestore 把变量文件还原成某次操作之前的内容。
//
// 还原的依据只有两样东西：那次操作留下的备份内容，以及"备份时文件是否存在"。
// 前者回答"写回什么"，后者回答"该不该有你"—— 面板新建的文件（系统的
// /etc/profile.d/panel-env.sh 就是典型）在当时并不存在，还原时应当删掉它，
// 而不是写回一份空文件：空文件虽然内容为空，却会继续参与每个登录会话的启动，
// 面板留下的痕迹并没有被清掉。
func (r *Runner) runVarRestore(ctx context.Context, t Target, profile *Profile, req VarRequest) (*VarOpResult, error) {
	origin, err := r.st.GetEnvOp(req.OriginOpID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("操作记录 #%d 不存在", req.OriginOpID)
		}
		return nil, err
	}
	if origin.HostID != t.HostID {
		return nil, errors.New("不能用 A 主机的记录还原 B 主机的文件")
	}
	switch origin.Action {
	case ActionEnvSet, ActionEnvUnset, ActionEnvPathAdd:
	default:
		return nil, fmt.Errorf("操作 #%d 的动作是 %s，不是环境变量变更，无法在这里还原", origin.ID, origin.Action)
	}
	if origin.SnapshotDir == "" {
		return nil, errors.New("该操作没有留下备份，无法还原")
	}

	var vp VarPlan
	if len(origin.Plan) > 0 {
		if err := json.Unmarshal(origin.Plan, &vp); err != nil {
			return nil, fmt.Errorf("解析操作计划存档失败：%w", err)
		}
	}
	if vp.File == "" {
		return nil, errors.New("操作记录里没有目标文件，无法还原")
	}
	content, existed, err := ReadVarBackup(origin.SnapshotDir)
	if err != nil {
		return nil, err
	}
	wrap := varWrapProfile(profile, vp.Scope)
	if vp.Scope == ScopeSystem && wrap == nil {
		return nil, ErrNoPrivilege
	}

	plan := VarPlan{
		Action: ActionEnvRestore,
		Kind:   "env",
		Target: vp.Name,
		Steps:  []string{"把文件还原成变更前的内容", "核对文件内容与备份是否一致"},
		Name:   vp.Name,
		Scope:  vp.Scope,
		File:   vp.File,
		Note:   fmt.Sprintf("还原操作 #%d 改动过的 %s", origin.ID, vp.File),
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	opID, err := r.st.CreateEnvOp(t.HostID, t.HostName, ActionEnvRestore, vp.Name, string(planJSON), origin.SnapshotDir, origin.ID)
	if err != nil {
		return nil, err
	}
	// 还原操作自己没有新备份：它把文件带回"更早"的状态，而那个状态已经在
	// 被还原那次操作的目录里了，再把同一份内容复制一遍没有意义。
	_ = r.st.SetEnvOpSnapshotDir(opID, origin.SnapshotDir)

	restoreStep := "把文件还原成变更前的内容"
	if _, err := r.runVarStep(ctx, t.Exec, wrap, opID, 1, restoreStep,
		VarRestoreScript(vp.File, content, existed), nil); err != nil {
		return nil, r.failVarOp(opID, req, err)
	}

	verifyStep := "核对文件内容与备份是否一致"
	var gotContent []byte
	var gotExisted bool
	if _, err := r.runVarStep(ctx, t.Exec, wrap, opID, 2, verifyStep, VarBackupScript(vp.File),
		func(stdout string) error {
			c, e, perr := ParseVarBackup(stdout)
			if perr != nil {
				return perr
			}
			gotContent, gotExisted = c, e
			return nil
		}); err != nil {
		return nil, r.failVarOp(opID, req, err)
	}

	verified := gotExisted == existed && bytes.Equal(gotContent, content)
	note, summary := "", ""
	switch {
	case verified:
		if existed {
			summary = fmt.Sprintf("已把 %s 还原成操作 #%d 之前的内容", vp.File, origin.ID)
		} else {
			summary = fmt.Sprintf("已删除面板当初新建的 %s（操作 #%d 之前它并不存在）", vp.File, origin.ID)
		}
	case existed:
		note = "还原后读回的内容与备份不一致，文件可能被其它程序同时改动了"
		summary = fmt.Sprintf("已写入 %s，但核对未通过", vp.File)
	default:
		note = "本应删掉这个文件，但它仍然存在，可能被其它程序同时创建了"
		summary = fmt.Sprintf("已删除 %s，但核对未通过", vp.File)
	}
	if err := r.st.FinishEnvOp(opID, store.EnvOpResult{
		Status: store.EnvStatusSuccess, Verified: verified, VerifyNote: note, Summary: summary,
	}); err != nil {
		return nil, err
	}
	return &VarOpResult{
		OpID: opID, Status: store.EnvStatusSuccess, File: vp.File,
		Verified: verified, Summary: summary, Note: note,
	}, nil
}

// varWriteScript 按动作选改写脚本。
func varWriteScript(setting bool, file, name, value string) (string, error) {
	if setting {
		return VarSetScript(file, name, value)
	}
	return VarUnsetScript(file, name)
}

// varWrapProfile 决定这条变量操作用什么身份执行：返回 nil 表示按登录用户执行。
//
// 只有"能提权的系统级操作"才返回 profile，其余一律按登录用户执行：提权去改
// 家目录文件会把文件属主变成 root，那台机器上该用户下次登录可能就读不到了 ——
// 面板宁可改不了，也不能把用户的文件改成 root 所有。
func varWrapProfile(profile *Profile, scope string) *Profile {
	if scope == ScopeSystem && profile != nil && profile.CanPrivileged() {
		return profile
	}
	return nil
}

// varTarget 决定这次改动落在哪个文件，并返回执行它所需的身份。
//
// 返回的 *Profile 为 nil 表示"按登录用户执行"（见 varWrapProfile）。
func (r *Runner) varTarget(ctx context.Context, ex Executor, profile *Profile, scope, requested string) (string, *Profile, error) {
	switch scope {
	case ScopeSystem:
		wrap := varWrapProfile(profile, ScopeSystem)
		if wrap == nil {
			return "", nil, ErrNoPrivilege
		}
		file, err := VarTargetFile(ScopeSystem, "", requested)
		return file, wrap, err
	case ScopeUser:
		// 家目录必须问目标机要：前端传来的路径不能用于决定写哪儿（那是把"删改用户文件"
		// 变成一次点击），面板侧也不能缓存它（同一份缓存同时对应多台机器）。
		home, err := CollectHome(ctx, ex)
		if err != nil {
			return "", nil, err
		}
		file, err := VarTargetFile(ScopeUser, home, requested)
		return file, varWrapProfile(profile, ScopeUser), err
	}
	return "", nil, fmt.Errorf("未知的变量作用域：%s", scope)
}

// failVarOp 把一次失败的操作收尾，并把错误原样返回给调用方。
//
// 步骤日志已经由 runVarStep 逐条写入（含失败那一步的退出码与 stderr），
// 这里只补操作级的结论，不做任何"再试一次"的动作：变量写入要么写成了、
// 要么没写成，没有需要补偿的中间状态（改写脚本本身是"先写临时文件再覆盖"，
// 失败时原文件仍是改写前的样子）。
func (r *Runner) failVarOp(opID int64, req VarRequest, cause error) error {
	summary := fmt.Sprintf("%s 未完成：%s", varActionText(req.Action), cause)
	_ = r.st.FinishEnvOp(opID, store.EnvOpResult{
		Status: store.EnvStatusFailed, Verified: false, Summary: summary,
	})
	return cause
}

// varActionText 给动作一个中文说法，用于失败结论。
func varActionText(action string) string {
	switch action {
	case ActionEnvSet:
		return "设置变量"
	case ActionEnvUnset:
		return "删除变量"
	case ActionEnvRestore:
		return "还原变量文件"
	case ActionEnvPathAdd:
		return "加入 PATH"
	}
	return "环境变量操作"
}

// runVarStep 执行一个变量操作步骤：结果写进数据库，同时把该步骤的输出交给
// after 做后续处理（解析结果、落盘备份等）。
//
// after 返回的错误算这一步的失败：把"跑完了但结果不可用"（备份解码失败、
// 备份落盘失败）与"命令本身失败"放在同一个位置收口，界面上就不会出现
// "这一步显示成功、操作却失败了"这种对不上的记录。
func (r *Runner) runVarStep(
	ctx context.Context, ex Executor, p *Profile,
	opID int64, seq int, name, script string, after func(stdout string) error,
) (string, error) {
	_ = r.st.StartEnvOpStep(opID, seq, name)

	started := time.Now()
	res, runErr := ex.Run(ctx, Wrap(script, p), varStepTimeout)
	elapsed := time.Since(started).Milliseconds()

	var (
		stdout, stderr string
		exitCode       int
	)
	if res != nil {
		stdout, stderr, exitCode = res.Stdout, res.Stderr, res.ExitCode
	}

	var stepErr error
	switch {
	case runErr != nil:
		stepErr = runErr
		exitCode = -1
		stderr = runErr.Error()
	case exitCode != 0:
		message := strings.TrimSpace(stderr)
		if message == "" {
			message = strings.TrimSpace(stdout)
		}
		if message == "" {
			message = fmt.Sprintf("退出码 %d", exitCode)
		}
		stepErr = errors.New(message)
	case after != nil:
		stepErr = after(stdout)
		if stepErr != nil {
			stderr = stepErr.Error()
		}
	}

	status := store.EnvStepSuccess
	if stepErr != nil {
		status = store.EnvStepFailed
	}
	_ = r.st.FinishEnvOpStep(opID, seq, status, exitCode, stdout, stderr, elapsed)
	if stepErr != nil {
		return stdout, fmt.Errorf("%s失败：%w", name, stepErr)
	}
	return stdout, nil
}