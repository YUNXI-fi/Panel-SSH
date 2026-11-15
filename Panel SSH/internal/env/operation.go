package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"panelssh/internal/store"
)

// SnapshotKeep 是每台主机保留的快照目录数上限。
//
// 导出是因为界面要如实告诉用户"更早的快照被清理了"，这个数字必须与
// 清理逻辑同源，不能在 API 层再写一遍。
const SnapshotKeep = 20

// pruneSnapshots 按 op 序号删除最旧的快照（op-N 目录名天然按时间排序）。
func pruneSnapshots(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(e.Name(), "op-%d", &id); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) <= keep {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids[:len(ids)-keep] {
		_ = os.RemoveAll(filepath.Join(dir, fmt.Sprintf("op-%d", id)))
	}
}

// StepTimeout 是单个变更步骤的时长上限。
// 大包（docker 引擎、构建工具链组）在慢网上下载十分钟以上并不罕见，
// 给 15 分钟；超时后步骤被杀并进入补偿，不会无限挂住。
const StepTimeout = 15 * time.Minute

// Target 标识一次操作落到哪台机器（hostID 为 0 表示本机）。
type Target struct {
	HostID   int64
	HostName string
	Exec     Executor
}

// RunRequest 是一次环境操作的请求。
type RunRequest struct {
	Action        string `json:"action"`
	Specs         []Spec `json:"specs,omitempty"`
	RecipeSlug    string `json:"recipeSlug,omitempty"`
	RecipeVersion string `json:"recipeVersion,omitempty"`
	Mirror        string `json:"mirror,omitempty"`
	OriginOpID    int64  `json:"originOpId,omitempty"`
}

// PlanView 是预检结果，同时原样存入 env_ops.plan_json，
// 让"事后复盘当时预告了什么变更"成为可能。
type PlanView struct {
	Action        string     `json:"action"`
	Kind          string     `json:"kind"`
	Target        string     `json:"target"`
	RecipeSlug    string     `json:"recipeSlug,omitempty"`
	RecipeVersion string     `json:"recipeVersion,omitempty"`
	Steps         []string   `json:"steps"`
	Sim           *SimReport `json:"sim,omitempty"`
	Preview       string     `json:"preview,omitempty"`
	Note          string     `json:"note,omitempty"`
}

// StepResult 是一个步骤的最终结果。
type StepResult struct {
	Seq        int    `json:"seq"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

// OpSummary 在操作结束时回传。
type OpSummary struct {
	OpID        int64     `json:"opId"`
	Status      string    `json:"status"`
	Compensated bool      `json:"compensated"`
	Verified    bool      `json:"verified"`
	VerifyNote  string    `json:"verifyNote,omitempty"`
	Summary     string    `json:"summary"`
	Plan        *PlanView `json:"plan,omitempty"`
}

// Events 是操作过程的回调接口（由 WebSocket 层实现）。
type Events interface {
	StepStart(seq int, name string)
	StepLog(seq int, stream, data string)
	StepEnd(r StepResult)
	Finish(s *OpSummary, err error)
}

// Runner 编排环境操作：预检 → 快照 → 执行 → 补偿 → 复核。
type Runner struct {
	st       *store.Store
	dataRoot string

	// 同一主机同时只允许一个环境变更：dpng/rpm 自身有锁，
	// 但让两个事务排队等锁会制造难以理解的失败，提前在面板侧互斥。
	locks sync.Map // hostID(int64) → *sync.Mutex
}

// NewRunner 创建编排器，快照统一落在 dataRoot 下。
func NewRunner(st *store.Store, dataRoot string) *Runner {
	return &Runner{st: st, dataRoot: dataRoot}
}

func (r *Runner) hostLock(hostID int64) *sync.Mutex {
	m, _ := r.locks.LoadOrStore(hostID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (r *Runner) snapshotDir(hostID, opID int64) string {
	return filepath.Join(r.dataRoot, fmt.Sprintf("host-%d", hostID), "snapshots", fmt.Sprintf("op-%d", opID))
}

// Plan 执行只读预检，不落任何变更。返回的 SimReport.OK 为假表示求解器
// 发现冲突，调用方应展示冲突并阻止用户确认。
func (r *Runner) Plan(ctx context.Context, ex Executor, hostID int64, req RunRequest) (*PlanView, error) {
	_, _, prep, err := r.prepare(ctx, ex, hostID, req)
	if err != nil {
		return nil, err
	}
	return prep.plan, nil
}

type prepared struct {
	plan   *PlanView
	steps  []Step
	verify []verifyItem
}

func (r *Runner) prepare(ctx context.Context, ex Executor, hostID int64, req RunRequest) (*Profile, PackageManager, *prepared, error) {
	profile, err := Probe(ctx, ex)
	if err != nil {
		return nil, nil, nil, err
	}
	mgr := NewManager(profile)
	if mgr == nil {
		return nil, nil, nil, ErrUnsupportedFamily
	}

	prep := &prepared{}
	switch req.Action {
	case ActionInstall:
		if req.RecipeSlug != "" {
			if err := r.buildRecipe(ctx, profile, mgr, ex, req, prep); err != nil {
				return nil, nil, nil, err
			}
		} else {
			if err := r.buildPackages(ctx, mgr, ex, req, prep); err != nil {
				return nil, nil, nil, err
			}
		}
	case ActionUpgrade, ActionRemove:
		if err := r.buildPackages(ctx, mgr, ex, req, prep); err != nil {
			return nil, nil, nil, err
		}
	case ActionUpgradeAll:
		sim, err := mgr.Simulate(ctx, ex, ActionUpgradeAll, nil)
		if err != nil {
			return nil, nil, nil, err
		}
		prep.plan = &PlanView{
			Action: ActionUpgradeAll, Kind: "upgrade-all",
			Target: "全部软件包", Steps: stepNames(mgr.Steps(ActionUpgradeAll, nil)), Sim: sim,
			Note: "将按包管理器求解结果升级全部可升级软件包，冲突中要求卸载的包已在上方列出",
		}
		prep.steps = mgr.Steps(ActionUpgradeAll, nil)
		prep.verify = []verifyItem{{Name: "__all__", Kind: "upgrade-all"}}
	case ActionMirror:
		mp, err := MirrorSteps(profile, req.Mirror)
		if err != nil {
			return nil, nil, nil, err
		}
		mirrorName := mirrorDisplayName(req.Mirror)
		prep.plan = &PlanView{
			Action: ActionMirror, Kind: "mirror",
			Target: mirrorName + "镜像源", Steps: stepNames(mp.Steps), Preview: mp.Preview,
		}
		prep.steps = mp.Steps
	case ActionRollback:
		if err := r.buildRollback(ctx, ex, hostID, profile, mgr, req.OriginOpID, prep); err != nil {
			return nil, nil, nil, err
		}
	default:
		return nil, nil, nil, fmt.Errorf("未知操作类型：%s", req.Action)
	}
	return profile, mgr, prep, nil
}

func mirrorDisplayName(id string) string {
	for _, m := range ListMirrors() {
		if m.ID == id {
			return m.Name
		}
	}
	return id
}

func stepNames(steps []Step) []string {
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name)
	}
	return names
}

// buildPackages 处理系统包的安装/升级/卸载预检。
func (r *Runner) buildPackages(ctx context.Context, mgr PackageManager, ex Executor, req RunRequest, prep *prepared) error {
	if len(req.Specs) == 0 {
		return ErrNoSpecs
	}
	for _, s := range req.Specs {
		if !validPkgToken(s.Name) || (s.Version != "" && !validPkgToken(s.Version)) {
			return ErrInvalidPackage
		}
	}
	sim, err := mgr.Simulate(ctx, ex, req.Action, req.Specs)
	if err != nil {
		return err
	}
	steps := mgr.Steps(req.Action, req.Specs)
	items := make([]verifyItem, 0, len(req.Specs))
	kind := "presence"
	if req.Action == ActionRemove {
		kind = "gone"
	}
	for _, s := range req.Specs {
		items = append(items, verifyItem{Name: s.Name, Expect: s.Version, Kind: kind})
	}
	prep.steps = steps
	prep.verify = items
	prep.plan = &PlanView{
		Action: req.Action, Kind: "packages",
		Target: joinSpecs(mgr, req.Specs),
		Steps:  stepNames(steps), Sim: sim,
	}
	return nil
}

// buildRecipe 处理软件目录项的预检。
func (r *Runner) buildRecipe(ctx context.Context, p *Profile, mgr PackageManager, ex Executor, req RunRequest, prep *prepared) error {
	recipe := GetRecipe(req.RecipeSlug)
	if recipe == nil {
		return fmt.Errorf("软件目录中不存在：%s", req.RecipeSlug)
	}
	if !recipe.Supports(p.Family) {
		return fmt.Errorf("%s 不支持该系统家族（%s）", recipe.Name, p.Family)
	}

	rp, err := recipe.Plan(ctx, p, req.RecipeVersion)
	if err != nil {
		return err
	}

	// 预检：有自定义模拟脚本（第三方源）就用它，否则按系统源包列表标准模拟。
	var sim *SimReport
	if rp.SimScript != "" {
		out, err := runQuery(ctx, ex, rp.SimScript, simulateTimeout)
		if err != nil {
			return err
		}
		sim = parseSimTSV(out)
	} else if len(rp.PkgSpecs) > 0 {
		sim, err = mgr.Simulate(ctx, ex, ActionInstall, rp.PkgSpecs)
		if err != nil {
			return err
		}
	} else {
		sim = &SimReport{OK: true}
	}

	version := req.RecipeVersion
	target := recipe.Name
	if version != "" {
		target += " " + version
	}
	prep.steps = rp.Steps
	prep.verify = []verifyItem{recipeVerifyItem(recipe, rp, p, version)}
	prep.plan = &PlanView{
		Action:        ActionInstall,
		Kind:          "recipe",
		Target:        target,
		RecipeSlug:    recipe.Slug,
		RecipeVersion: version,
		Steps:         stepNames(rp.Steps),
		Sim:           sim,
		Note:          rp.Note,
	}
	return nil
}

// Run 执行一次环境操作。任何失败都通过 Events.Finish 回报，不向调用方抛错。
func (r *Runner) Run(ctx context.Context, t Target, req RunRequest, ev Events) {
	lock := r.hostLock(t.HostID)
	if !lock.TryLock() {
		ev.Finish(nil, errors.New("该主机已有环境变更正在执行，请等待完成后再试"))
		return
	}
	defer lock.Unlock()

	profile, mgr, prep, err := r.prepare(ctx, t.Exec, t.HostID, req)
	if err != nil {
		ev.Finish(nil, err)
		return
	}
	if !profile.CanPrivileged() {
		ev.Finish(nil, ErrNoPrivilege)
		return
	}
	// 执行前重新求解：预检通过到用户确认之间源可能已变化。
	// 模拟与执行用的是同一份求解器，这里拦下冲突，执行步骤才有意义。
	if prep.plan.Sim != nil && !prep.plan.Sim.OK {
		ev.Finish(nil, fmt.Errorf("依赖求解未通过：%s", strings.Join(prep.plan.Sim.Conflicts, "；")))
		return
	}

	planJSON, _ := json.Marshal(prep.plan)
	opID, err := r.st.CreateEnvOp(t.HostID, t.HostName, req.Action, prep.plan.Target, string(planJSON), "", req.OriginOpID)
	if err != nil {
		ev.Finish(nil, err)
		return
	}
	snapDir := r.snapshotDir(t.HostID, opID)
	snap, snapErr := TakeSnapshot(ctx, t.Exec, profile, mgr, snapDir)
	if snapErr != nil {
		r.finishWithError(t, opID, "变更前快照失败，已中止："+snapErr.Error(), ev, prep.plan)
		return
	}
	_ = r.st.SetEnvOpSnapshotDir(opID, snapDir)
	// 快照体积小但会持续累积，只保留最近若干次，老操作记录仍可读（
	// 只是回滚时会被告知快照缺失），不影响审计。
	pruneSnapshots(filepath.Join(r.dataRoot, fmt.Sprintf("host-%d", t.HostID), "snapshots"), SnapshotKeep)

	// ---- 逐步骤执行 ----
	seq := 0
	failed := false
	for _, step := range prep.steps {
		seq++
		sr := r.runStep(ctx, t.Exec, profile, opID, seq, step, ev)
		if sr.Status == store.EnvStepFailed && step.Critical {
			failed = true
			break
		}
	}

	if failed {
		// 补偿顺序：先还原安装源（撤销新增的第三方源/镜像配置），
		// 再跑包管理器自修复（apt 的半配置状态恢复）。补偿步骤一律非关键：
		// 补偿失败本身不该再触发一轮补偿，只如实记录供人工介入。
		if b64, ok := readSnapshotSourcesB64(snap.Dir); ok {
			seq++
			r.runStep(ctx, t.Exec, profile, opID, seq, Step{
				Name: "补偿：还原变更前的安装源配置", Command: restoreSourcesScript(b64), Critical: false,
			}, ev)
		}
		if repair := mgr.RepairStep(); repair != nil {
			seq++
			r.runStep(ctx, t.Exec, profile, opID, seq, Step{
				Name: "补偿：" + repair.Name, Command: repair.Command, Critical: false,
			}, ev)
		}
		summary := &OpSummary{
			OpID: opID, Status: store.EnvStatusFailed, Compensated: true, Verified: false,
			Summary: "变更失败，已尝试自动还原安装源并修复包状态，请查看步骤日志", Plan: prep.plan,
		}
		_ = r.st.FinishEnvOp(opID, store.EnvOpResult{
			Status: store.EnvStatusFailed, Compensated: true, Verified: false, Summary: summary.Summary,
		})
		ev.Finish(summary, nil)
		return
	}

	// ---- 版本复核 ----
	verified, note := r.verify(ctx, t.Exec, profile, mgr, prep.verify)
	summary := &OpSummary{
		OpID: opID, Status: store.EnvStatusSuccess, Verified: verified, VerifyNote: note, Plan: prep.plan,
	}
	switch {
	case !verified && note != "":
		summary.Summary = "命令执行完成，但版本复核未完全通过：" + note
	case verified:
		summary.Summary = "操作成功并已通过版本复核"
	default:
		summary.Summary = "操作成功"
	}
	_ = r.st.FinishEnvOp(opID, store.EnvOpResult{
		Status: store.EnvStatusSuccess, Verified: verified, VerifyNote: note, Summary: summary.Summary,
	})
	ev.Finish(summary, nil)
}

// runStep 执行单个步骤并把结果同时推给前端与数据库。
func (r *Runner) runStep(ctx context.Context, ex Executor, p *Profile, opID int64, seq int, step Step, ev Events) StepResult {
	ev.StepStart(seq, step.Name)
	_ = r.st.StartEnvOpStep(opID, seq, step.Name)

	sink := func(stream string, data []byte) { ev.StepLog(seq, stream, string(data)) }
	started := time.Now()
	res, runErr := ex.Stream(ctx, Wrap(step.Command, p), StepTimeout, sink)
	elapsed := time.Since(started).Milliseconds()
	if runErr != nil {
		err := fmt.Errorf("%w（%s）", runErr, step.Name)
		sr := StepResult{Seq: seq, Name: step.Name, Status: store.EnvStepFailed,
			Stderr: err.Error(), DurationMS: elapsed}
		_ = r.st.FinishEnvOpStep(opID, seq, sr.Status, -1, "", sr.Stderr, elapsed)
		ev.StepEnd(sr)
		return sr
	}
	status := store.EnvStepSuccess
	if res.ExitCode != 0 {
		status = store.EnvStepFailed
	}
	sr := StepResult{Seq: seq, Name: step.Name, Status: status, ExitCode: res.ExitCode,
		Stdout: res.Stdout, Stderr: res.Stderr, DurationMS: elapsed}
	_ = r.st.FinishEnvOpStep(opID, seq, status, res.ExitCode, res.Stdout, res.Stderr, elapsed)
	ev.StepEnd(sr)
	return sr
}

// finishWithError 处理"操作登记完成但没能真正执行"的失败（如快照失败）。
func (r *Runner) finishWithError(t Target, opID int64, msg string, ev Events, plan *PlanView) {
	_ = r.st.FinishEnvOp(opID, store.EnvOpResult{Status: store.EnvStatusFailed, Verified: false, Summary: msg})
	ev.Finish(&OpSummary{OpID: opID, Status: store.EnvStatusFailed, Verified: false, Summary: msg, Plan: plan},
		errors.New(msg))
}
