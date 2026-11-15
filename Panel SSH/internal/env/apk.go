package env

import (
	"context"
	"regexp"
	"strings"
)

type apkManager struct{}

func (a *apkManager) Family() Family { return FamilyAPK }

// ---- 查询 ----

var (
	// apk list --installed 行：
	// musl-1.2.5-r0 x86_64 {musl} (installed) [system]
	// 版本号总是数字开头，贪婪匹配包名中可能出现的连字符。
	apkInstalledLine = regexp.MustCompile(`^(.+)-(\d\S*)\s+(\S+)`)
	// <musl current: 1.2.5-r0 vs 1.2.5-r1>
	apkUpgradeLine = regexp.MustCompile(`^<(\S+) current: (\S+) vs (\S+)>$`)
)

func (a *apkManager) PackageListCommand() string {
	return "apk list --installed 2>/dev/null"
}

func (a *apkManager) Installed(ctx context.Context, ex Executor) ([]Package, error) {
	out, err := runQuery(ctx, ex, "export LC_ALL=C\n"+a.PackageListCommand(), queryTimeout)
	if err != nil {
		return nil, err
	}
	return a.ParsePackages(out), nil
}

func (a *apkManager) ParsePackages(raw string) []Package {
	pkgs := make([]Package, 0)
	for _, line := range strings.Split(raw, "\n") {
		m := apkInstalledLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		pkgs = append(pkgs, Package{Name: m[1], Version: m[2], Arch: m[3]})
	}
	return pkgs
}

func (a *apkManager) Upgrades(ctx context.Context, ex Executor) ([]Upgrade, error) {
	// apk version 比较本地索引与已装版本；要得到新结果需要先 apk update，
	// 与其他家族一致，查询不代刷索引。
	out, err := runQuery(ctx, ex, "export LC_ALL=C\napk version -l '<' 2>/dev/null", queryTimeout)
	if err != nil {
		return nil, err
	}
	ups := make([]Upgrade, 0)
	for _, line := range strings.Split(out, "\n") {
		m := apkUpgradeLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		ups = append(ups, Upgrade{Name: m[1], Current: m[2], Candidate: m[3]})
	}
	return ups, nil
}

func (a *apkManager) Candidates(ctx context.Context, ex Executor, name string) ([]Candidate, error) {
	if !validPkgToken(name) {
		return nil, ErrInvalidPackage
	}
	// Alpine 镜像仓库只保留每个包的当前版本，没有历史版本可挑；
	// 如实返回空列表，由前端禁用版本选择，而不是伪造候选项。
	return []Candidate{}, nil
}

// ---- 模拟预检 ----

const apkSimScript = `
export LC_ALL=C
set +e
ACTION={{ACTION}}
SPECS={{SPECS}}
case "$ACTION" in
  install|upgrade) CMD="apk add --simulate --no-cache $SPECS" ;;
  remove)          CMD="apk del --simulate $SPECS" ;;
  upgrade-all)     CMD="apk upgrade --simulate --available" ;;
esac
OUT=$($CMD 2>&1)
RC=$?
echo "===CHANGES==="
printf '%s\n' "$OUT" | awk '
/^\([0-9]+\/[0-9]+\) Installing / {
  name=$3; ver=$4; gsub(/[()]/, "", ver)
  printf "install\t%s\t\t%s\t\n", name, ver
}
/^\([0-9]+\/[0-9]+\) Upgrading / {
  name=$3; oldv=$4; newv=$6; gsub(/[()]/, "", oldv)
  printf "upgrade\t%s\t%s\t%s\t\n", name, oldv, newv
}
'
echo "===CONFLICTS==="
printf '%s\n' "$OUT" | grep -E 'ERROR:|no such package|unsupported:' | head -20
echo "===RAW==="
printf '%s\n' "$OUT" | tail -12
echo "===RC==="
echo "$RC"
`

func (a *apkManager) Simulate(ctx context.Context, ex Executor, action string, specs []Spec) (*SimReport, error) {
	for _, s := range specs {
		if !validPkgToken(s.Name) || (s.Version != "" && !validPkgToken(s.Version)) {
			return nil, ErrInvalidPackage
		}
	}
	if action != ActionUpgradeAll && len(specs) == 0 {
		return nil, ErrNoSpecs
	}

	script := strings.NewReplacer(
		"{{ACTION}}", shellQuote(action),
		"{{SPECS}}", joinSpecs(a, specs),
	).Replace(apkSimScript)

	out, err := runQuery(ctx, ex, script, simulateTimeout)
	if err != nil {
		return nil, err
	}
	return parseSimTSV(out), nil
}

// ---- 变更步骤 ----

func (a *apkManager) Steps(action string, specs []Spec) []Step {
	switch action {
	case ActionInstall, ActionUpgrade:
		return []Step{
			{Name: "刷新软件包索引", Command: "export LC_ALL=C\napk update", Critical: false},
			{Name: "安装/升级软件包", Command: "apk add --no-cache " + joinSpecs(a, specs), Critical: true},
		}
	case ActionRemove:
		// del 只删包并更新 world，依赖按 solver 处理，比手工 purge 安全。
		return []Step{{Name: "卸载软件包", Command: "apk del " + joinSpecs(a, specs), Critical: true}}
	case ActionUpgradeAll:
		return []Step{
			{Name: "刷新软件包索引", Command: "export LC_ALL=C\napk update", Critical: false},
			// --available 让所有包跟随仓库最新版本，是 Alpine 全面升级的标准姿势。
			{Name: "升级全部软件包", Command: "apk upgrade --available", Critical: true},
		}
	}
	return nil
}

func (a *apkManager) RepairStep() *Step {
	return &Step{
		Name:     "修复损坏的软件包（apk fix）",
		Command:  "apk fix --no-cache",
		Critical: false,
	}
}

func (a *apkManager) DowngradeCommand(items []ChangeItem) string {
	specs := make([]Spec, 0, len(items))
	for _, it := range items {
		if it.From != "" {
			specs = append(specs, Spec{Name: it.Name, Version: it.From})
		}
	}
	// 旧版本通常已不在 Alpine 镜像里：失败的包会在执行输出中明确列出，
	// 由编排层回报，不假装回滚成功。
	return "apk add --no-cache " + joinSpecs(a, specs)
}

func (a *apkManager) ReinstallCommand(items []ChangeItem) string { return a.DowngradeCommand(items) }

func (a *apkManager) RemoveCommand(items []ChangeItem) string {
	specs := make([]Spec, 0, len(items))
	for _, it := range items {
		specs = append(specs, Spec{Name: it.Name})
	}
	return "apk del " + joinSpecs(a, specs)
}

func (a *apkManager) SourcePaths() []string {
	return []string{
		"etc/apk/repositories",
		"etc/apk/keys",
	}
}

func (a *apkManager) RenderSpec(s Spec) string {
	if s.Version != "" {
		return s.Name + "=" + s.Version
	}
	return s.Name
}
