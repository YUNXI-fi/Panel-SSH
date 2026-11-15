package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"panelssh/internal/store"
)

// buildRollback 根据一次历史操作的快照构造回滚计划。
//
// 回滚顺序刻意安排为：tarball 类还原 → 卸载新增包 → 装回旧版本包 → 还原源。
// 包级回退放在源还原之前，是因为旧版本（尤其第三方源里的包）往往只在
// 当前仍挂载的源里才找得到；源一旦先还原，求解器就再也看不见它们。
func (r *Runner) buildRollback(
	ctx context.Context, ex Executor, hostID int64,
	p *Profile, mgr PackageManager, originID int64, prep *prepared,
) error {
	origin, err := r.st.GetEnvOp(originID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("操作记录 #%d 不存在", originID)
		}
		return err
	}
	if origin.HostID != hostID {
		return errors.New("不能用 A 主机的快照回滚 B 主机")
	}
	if origin.Action == ActionRollback {
		return errors.New("回滚操作不能再次回滚；如需更早的状态，请直接回滚原始操作")
	}
	if origin.Status != store.EnvStatusSuccess && origin.Status != store.EnvStatusFailed {
		return fmt.Errorf("操作 #%d 处于 %s 状态，系统无法判断它实际改了什么，不能自动回滚", originID, origin.Status)
	}
	if origin.SnapshotDir == "" {
		return errors.New("该操作没有留下快照，无法回滚")
	}
	if _, err := os.Stat(origin.SnapshotDir); err != nil {
		return fmt.Errorf("快照目录已丢失（%s），无法回滚", origin.SnapshotDir)
	}

	// 环境变量变更走的是另一条编排（见 vars_run.go）：它没有包清单快照，还原入口
	// 也不同（变量页签里的"还原"）。必须在这里就分流，否则下面会先去读一份不存在
	// 的包清单，把"这个操作不该走这条路"错报成"读取快照包清单失败"。
	switch origin.Action {
	case ActionEnvSet, ActionEnvUnset, ActionEnvPathAdd, ActionEnvRestore:
		return errors.New("环境变量变更不走这条回滚通道，请在「环境变量」页签里对这条记录做还原")
	}

	snap, err := LoadSnapshot(origin.SnapshotDir, mgr)
	if err != nil {
		return fmt.Errorf("读取快照包清单失败：%w", err)
	}

	var plan PlanView
	if len(origin.Plan) > 0 {
		if err := json.Unmarshal(origin.Plan, &plan); err != nil {
			return fmt.Errorf("解析操作计划存档失败：%w", err)
		}
	}

	var (
		steps  []Step
		verify []verifyItem
		note   string
	)
	// removed 是需要卸载的"本次新增包"；reverted 是需要装回快照版本的包。
	var removed, reverted []ChangeItem
	_, sourcesExist := readSnapshotSourcesB64(origin.SnapshotDir)

	switch plan.Kind {
	case "mirror":
		// 纯换源操作：唯一动作就是把源文件解包回去。
		if !sourcesExist {
			return errors.New("快照中没有源配置备份，无法回滚")
		}

	case "recipe":
		if plan.RecipeSlug == "golang" {
			goSteps, goVerify, goErr := rollbackGo(ctx, p, snap)
			if goErr != nil {
				return goErr
			}
			steps = append(steps, goSteps...)
			verify = append(verify, goVerify...)
		} else {
			recipe := GetRecipe(plan.RecipeSlug)
			if recipe == nil {
				return fmt.Errorf("目录软件 %s 已不存在于当前版本，无法构造回滚", plan.RecipeSlug)
			}
			rp, err := recipe.Plan(ctx, p, plan.RecipeVersion)
			if err != nil {
				return err
			}
			removed, reverted = classifyBySnapshot(snap, rp.RollbackRemove)
			note = rp.Note
		}

	default: // packages / upgrade-all / 旧数据无 Kind 时
		if plan.Sim == nil {
			return errors.New("该操作没有可用的变更清单，无法逐包回滚；源配置仍可手工恢复")
		}
		// 历史计划里"新装"的要卸载；升级/降级/被卸载的都要回到快照版本。
		names := make([]string, 0)
		for _, it := range plan.Sim.Install {
			names = append(names, it.Name)
		}
		for _, it := range append(append([]ChangeItem{}, plan.Sim.Upgrade...), plan.Sim.Downgrade...) {
			names = append(names, it.Name)
		}
		for _, it := range plan.Sim.Remove {
			names = append(names, it.Name)
		}
		removed, reverted = classifyBySnapshot(snap, names)
		// Sim 里标记为"新装"但快照中其实已有旧版（求解器把升级报成了 Inst
		// 的边角情况）也要纠正：以快照为准。
		if len(plan.Sim.Remove) > 0 {
			for _, it := range plan.Sim.Remove {
				if old := snap.packageVersion(it.Name); old != "" {
					reverted = appendIfMissing(reverted, ChangeItem{Name: it.Name, From: old})
				}
			}
		}
	}

	// ---- 统一生成包级步骤 ----
	if len(removed) > 0 {
		steps = append(steps, Step{
			Name: "卸载本次新增的软件包", Command: mgr.RemoveCommand(removed), Critical: true,
		})
		for _, it := range removed {
			verify = append(verify, verifyItem{Name: it.Name, Kind: "gone"})
		}
	}
	var simReport *SimReport
	if len(reverted) > 0 {
		// 用快照版本做求解预检：旧版本已从源里移除时，冲突清单会准确指出
		// 缺哪个包，用户据此知道哪些包无法自动回退，而不是执行到一半才失败。
		revertSpecs := make([]Spec, 0, len(reverted))
		for _, it := range reverted {
			revertSpecs = append(revertSpecs, Spec{Name: it.Name, Version: it.From})
		}
		simReport, err = mgr.Simulate(ctx, ex, ActionInstall, revertSpecs)
		if err != nil {
			return err
		}
		steps = append(steps, Step{
			Name: "恢复软件包到变更前版本", Command: mgr.DowngradeCommand(reverted), Critical: true,
		})
		for _, it := range reverted {
			verify = append(verify, verifyItem{Name: it.Name, Expect: it.From, Kind: "presence"})
		}
	}

	if sourcesExist {
		b64, _ := readSnapshotSourcesB64(origin.SnapshotDir)
		steps = append(steps, Step{Name: "还原变更前的安装源与密钥", Command: restoreSourcesScript(b64), Critical: true})
	}

	if len(steps) == 0 {
		return errors.New("快照与当前状态比对后没有需要回滚的内容")
	}

	prep.steps = steps
	prep.verify = verify
	prep.plan = &PlanView{
		Action: ActionRollback,
		Kind:   "rollback",
		Target: fmt.Sprintf("回滚操作 #%d：%s", originID, origin.Target),
		Steps:  stepNames(steps),
		Sim:    simReport,
		Note:   note,
	}
	return nil
}

// classifyBySnapshot 按快照明细把包名分成"新装（快照无）"与"变更（快照有旧版）"。
func classifyBySnapshot(snap *Snapshot, names []string) (removed, reverted []ChangeItem) {
	for _, name := range names {
		if old := snap.packageVersion(name); old != "" {
			reverted = append(reverted, ChangeItem{Name: name, From: old})
		} else {
			removed = append(removed, ChangeItem{Name: name})
		}
	}
	return removed, reverted
}

func appendIfMissing(items []ChangeItem, item ChangeItem) []ChangeItem {
	for _, it := range items {
		if it.Name == item.Name {
			return items
		}
	}
	return append(items, item)
}

// rollbackGo 处理 tarball 安装的 Go：变更前没有就删目录，有就下载旧版重装。
func rollbackGo(ctx context.Context, p *Profile, snap *Snapshot) ([]Step, []verifyItem, error) {
	oldGo := snap.Facts["GO_VERSION"]
	if oldGo == "" {
		return []Step{{
			Name:     "移除 /usr/local/go 与命令符号链接",
			Command:  "rm -rf /usr/local/go\nrm -f /usr/local/bin/go /usr/local/bin/gofmt",
			Critical: true,
		}}, []verifyItem{{Name: "golang", Kind: "recipe-removed"}}, nil
	}

	rp, err := planGo(ctx, p, oldGo)
	if err != nil {
		return nil, nil, err
	}
	// rp.Steps 的最后一步是 tarball 下载解压，前置工具步骤在回滚里不需要重复。
	tarStep := rp.Steps[len(rp.Steps)-1]
	return []Step{{Name: "重装变更前的 Go " + oldGo, Command: tarStep.Command, Critical: true}},
		[]verifyItem{{Name: "golang", Expect: oldGo, Kind: "recipe", Detect: detectGo(nil)}}, nil
}
