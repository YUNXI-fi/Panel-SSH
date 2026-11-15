package env

import (
	"context"
	"regexp"
	"strings"
)

type aptManager struct{}

func (a *aptManager) Family() Family { return FamilyApt }

const (
	// aptEnv 固定语言与无人值守模式：输出格式可解析，安装过程不弹 conffile 对话框。
	aptEnv = "export LC_ALL=C DEBIAN_FRONTEND=noninteractive"
	// aptDPKGOpts 让 dpkg 在配置文件冲突时保留当前版本（confold），
	// 默认配置缺项采用维护者默认（confdef），避免升级卡在交互提示上。
	aptDPKGOpts = "-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"
)

// ---- 查询 ----

func (a *aptManager) Installed(ctx context.Context, ex Executor) ([]Package, error) {
	out, err := runQuery(ctx, ex, aptEnv+"\n"+a.PackageListCommand(), queryTimeout)
	if err != nil {
		return nil, err
	}
	return a.ParsePackages(out), nil
}

func (a *aptManager) PackageListCommand() string {
	// Status-Abbrev 第二字符为 i 才是真正安装完成的包；
	// rc 残留配置、un 未安装的行都不能出现在"已安装"里。
	return "dpkg-query -W -f='${db:Status-Abbrev}\\t${Package}\\t${Version}\\t${Architecture}\\n' 2>/dev/null"
}

func (a *aptManager) ParsePackages(raw string) []Package {
	pkgs := make([]Package, 0)
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 || len(fields[0]) < 2 || fields[0][1] != 'i' {
			continue
		}
		pkgs = append(pkgs, Package{Name: fields[1], Version: fields[2], Arch: strings.TrimSpace(fields[3])})
	}
	return pkgs
}

// 行形如：at-spi2-common/trixie 2.56.2-1 all [upgradable from: 2.56.2-0]
// 斜杠后的是 suite（trixie/trixie-security），一并取出来展示来源。
var aptUpgradeLine = regexp.MustCompile(`^(\S+?)/(\S+) (\S+) (\S+) \[upgradable from: ([^\]]+)\]`)

func (a *aptManager) Upgrades(ctx context.Context, ex Executor) ([]Upgrade, error) {
	// 不在这里主动 apt-get update：刷新索引耗时且改变系统缓存，
	// 升级流程本身的第一步会刷新。这里只报告当前已知的可升级项。
	out, err := runQuery(ctx, ex, aptEnv+"\napt list --upgradable 2>/dev/null", queryTimeout)
	if err != nil {
		return nil, err
	}
	ups := make([]Upgrade, 0)
	for _, line := range strings.Split(out, "\n") {
		m := aptUpgradeLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		ups = append(ups, Upgrade{
			Name: m[1], Repo: m[2], Candidate: m[3], Arch: m[4], Current: m[5],
		})
	}
	return ups, nil
}

var (
	aptVersionLine = regexp.MustCompile(`^     (\S+) +\d+ *$`)
	aptSourceLine  = regexp.MustCompile(`^        \d+ (\S+)`)
	aptFieldLine   = regexp.MustCompile(`^  (Installed|Candidate): (.+)$`)
)

func (a *aptManager) Candidates(ctx context.Context, ex Executor, name string) ([]Candidate, error) {
	if !validPkgToken(name) {
		return nil, ErrInvalidPackage
	}
	out, err := runQuery(ctx, ex, aptEnv+"\napt-cache policy "+shellQuote(name), queryTimeout)
	if err != nil {
		return nil, err
	}

	cands := make([]Candidate, 0)
	var installed string
	inTable := false
	for _, line := range strings.Split(out, "\n") {
		if m := aptFieldLine.FindStringSubmatch(line); m != nil {
			if m[1] == "Installed" && m[2] != "(none)" {
				installed = m[2]
			}
			continue
		}
		if strings.HasPrefix(line, "  Version table:") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if m := aptVersionLine.FindStringSubmatch(line); m != nil {
			cands = append(cands, Candidate{Version: m[1]})
			continue
		}
		if m := aptSourceLine.FindStringSubmatch(line); m != nil && len(cands) > 0 {
			// URL 后第一段作为来源展示，如 mirrors.tuna.tsinghua.edu.cn/ubuntu。
			cands[len(cands)-1].Repo = m[1]
		}
	}
	// 已装版本置顶，方便前端把"当前版本"和候选放在同一张列表里。
	if installed != "" {
		cands = append([]Candidate{{Version: installed, Repo: "(已安装)"}}, cands...)
	}
	return dedupCandidates(cands), nil
}

func dedupCandidates(in []Candidate) []Candidate {
	seen := map[string]bool{}
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if seen[c.Version] {
			continue
		}
		seen[c.Version] = true
		out = append(out, c)
	}
	return out
}

// ---- 模拟预检 ----

// aptChangeAWK 把 apt-get -s 的事务行整理成 TSV（CHANGES 段）。
//
// 升/降级方向交给 dpkg --compare-versions 判定（在 awk 里逐条 system() 调用），
// 而不是自己在 Go 里实现 dpkg 版本算法——那套算法含 epoch、~ 后缀等规则，
// 重复实现只会和 apt 自己得出不一致的结论。
const aptChangeAWK = `awk '
/^Inst /{
  name=$2
  line=substr($0, index($0,$2)+length($2))
  oldv=""; newv=""; arch=""
  if (match(line, /^ *\[[^]]+\]/)) oldv=substr(line, RSTART+1, RLENGTH-2)
  if (match(line, /\([^)]+\)/)) {
    inside=substr(line, RSTART+1, RLENGTH-2)
    n=split(inside, parts, " ")
    newv=parts[1]
    for (i=2;i<=n;i++) if (parts[i] ~ /^\[/) { gsub(/[\[\]]/,"",parts[i]); arch=parts[i] }
  }
  kind="install"
  if (oldv != "") {
    if (system("dpkg --compare-versions " oldv " lt " newv) == 0) kind="upgrade"
    else kind="downgrade"
  }
  printf "%s\t%s\t%s\t%s\t%s\n", kind, name, oldv, newv, arch
}
/^Remv /{
  name=$2
  line=substr($0, index($0,$2)+length($2))
  oldv=""
  if (match(line, /\[[^]]+\]/)) oldv=substr(line, RSTART+1, RLENGTH-2)
  printf "remove\t%s\t%s\t\t\n", name, oldv
}
'`

// aptTailSegments 输出公共的 CHANGES/CONFLICTS/SIZE/RAW/RC 段，
// 依赖 shell 变量 OUT 与 RC。aptSimScript 与第三方源临时模拟共用。
const aptTailSegments = `
echo "===CHANGES==="
printf '%s\n' "$OUT" | ` + aptChangeAWK + `
echo "===CONFLICTS==="
printf '%s\n' "$OUT" | grep -E '^E: |Unable to locate package|no installation candidate|not going to be installed|[Bb]roken( packages| package)|Conflicts:|Breaks:' | head -20
echo "===SIZE==="
printf '%s\n' "$OUT" | grep 'Need to get' | head -1 | sed 's/^Need to get //; s/ of archives.*//'
echo "===RAW==="
printf '%s\n' "$OUT" | tail -12
echo "===RC==="
echo "$RC"
`

// aptSimScript 在目标机上跑求解器模拟，并把事务整理成 TSV。
const aptSimScript = aptEnv + `
set +e
ACTION={{ACTION}}
SPECS={{SPECS}}
case "$ACTION" in
  install|upgrade) CMD="apt-get -s -y install $SPECS" ;;
  remove)         CMD="apt-get -s -y remove $SPECS" ;;
  upgrade-all)    CMD="apt-get -s -y dist-upgrade" ;;
  *) echo "unknown action"; exit 2 ;;
esac
OUT=$($CMD 2>&1)
RC=$?
` + aptTailSegments

// aptTempRepoSimScript 用临时源做预检，不改 /etc/apt：
// 第三方软件（Node/Docker）的正式源要到执行阶段才写入，这里把仓库行放进
// 一个临时 sourceparts 目录，与系统现有源一起参与求解。
//
// 三个必须做对的点：
//  1. 预检是以登录用户跑的，不能写 /var/lib/apt/lists。放在临时目录里更新索引，
//     否则 apt-get update 会因权限不足静默失败，随后报的"Unable to locate package"
//     会把网络/权限问题伪装成"官方源里没有这个包"。
//  2. 必须同时带上系统源。只挂第三方源时，docker-ce 依赖的 iptables、pigz 这类
//     发行版包会被判成"无法安装"，预检同样过不了。
//  3. 只下载第三方仓库的索引，系统包复用目标机上已有的索引。全量更新会把
//     deb-src 一并拉下来（实测有机器 /var/lib/apt/lists 有 153 MB），
//     预检会在 90 秒预算内超时，而这只是一次"能不能装"的问询。
//
// repoLine 是一条 deb 源行（已由面板侧按发行版拼好），specs 是待模拟包。
func aptTempRepoSimScript(repoLine string, specs []Spec) string {
	return aptEnv + `
set +e
SIMDIR=$(mktemp -d)
trap 'rm -rf "$SIMDIR"' EXIT
mkdir -p "$SIMDIR/lists/partial" "$SIMDIR/cache/archives/partial" "$SIMDIR/extra-src" "$SIMDIR/all-src"
cat > "$SIMDIR/extra-src/panelssh-extra.list" <<'LISTEOF'
` + repoLine + `
LISTEOF
apt-get update -qq -o Dir::Etc::sourcelist=/dev/null -o Dir::Etc::sourceparts="$SIMDIR/extra-src" -o Dir::State::lists="$SIMDIR/lists" -o Dir::Cache="$SIMDIR/cache" >/dev/null 2>&1
# 系统已有索引直接链进来：模拟要看到发行版自己的包，但不必重新下载。
SYSLISTS=0
for f in /var/lib/apt/lists/*; do
  [ "$f" = "/var/lib/apt/lists/lock" ] && continue
  [ -f "$f" ] && ln -s "$f" "$SIMDIR/lists/" 2>/dev/null && SYSLISTS=1
done
# 模拟时的源 = 系统源 + 第三方源（系统源同样用软链，避免复制后漂移）。
for f in /etc/apt/sources.list.d/*; do
  [ -f "$f" ] && ln -s "$f" "$SIMDIR/all-src/" 2>/dev/null
done
ln -s "$SIMDIR/extra-src/panelssh-extra.list" "$SIMDIR/all-src/panelssh-extra.list" 2>/dev/null
MAIN=/etc/apt/sources.list
[ -f "$MAIN" ] || MAIN=/dev/null
OPTS="-o Dir::Etc::sourcelist=$MAIN -o Dir::Etc::sourceparts=$SIMDIR/all-src -o Dir::State::lists=$SIMDIR/lists -o Dir::Cache=$SIMDIR/cache"
# 没有系统索引缓存（极少见）时退回完整更新：宁可慢，也不给出假结论。
if [ "$SYSLISTS" = 0 ]; then
  apt-get update -qq $OPTS >/dev/null 2>&1
fi
OUT=$(apt-get -s -y $OPTS install ` + joinSpecs(&aptManager{}, specs) + ` 2>&1)
RC=$?
` + aptTailSegments
}

func (a *aptManager) Simulate(ctx context.Context, ex Executor, action string, specs []Spec) (*SimReport, error) {
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
	).Replace(aptSimScript)

	out, err := runQuery(ctx, ex, script, simulateTimeout)
	if err != nil {
		return nil, err
	}
	return parseSimTSV(out), nil
}

// ---- 变更步骤 ----

func (a *aptManager) Steps(action string, specs []Spec) []Step {
	switch action {
	case ActionInstall, ActionUpgrade:
		return []Step{
			{Name: "刷新软件包索引", Command: aptEnv + "\napt-get update -qq", Critical: false},
			{Name: "安装/升级软件包", Command: aptEnv + "\napt-get install -y " + aptDPKGOpts + " " + joinSpecs(a, specs), Critical: true},
		}
	case ActionRemove:
		return []Step{
			{Name: "卸载软件包（保留配置）", Command: aptEnv + "\napt-get remove -y " + joinSpecs(a, specs), Critical: true},
		}
	case ActionUpgradeAll:
		return []Step{
			{Name: "刷新软件包索引", Command: aptEnv + "\napt-get update -qq", Critical: false},
			{Name: "升级全部软件包", Command: aptEnv + "\napt-get dist-upgrade -y " + aptDPKGOpts, Critical: true},
		}
	}
	return nil
}

func (a *aptManager) RepairStep() *Step {
	// dpkg 中断后包状态可能停在 half-configured/unpacked：先 configure -a
	// 再让 apt 补齐缺失依赖，这是 dpkg/apt 中断恢复的标准两步。
	return &Step{
		Name:     "修复中断的包状态（dpkg --configure -a && apt-get -f install）",
		Command:  aptEnv + "\ndpkg --configure -a\napt-get -f install -y " + aptDPKGOpts,
		Critical: false,
	}
}

func (a *aptManager) DowngradeCommand(items []ChangeItem) string {
	specs := make([]Spec, 0, len(items))
	for _, it := range items {
		if it.From != "" {
			specs = append(specs, Spec{Name: it.Name, Version: it.From})
		}
	}
	return aptEnv + "\napt-get install -y --allow-downgrades " + aptDPKGOpts + " " + joinSpecs(&aptManager{}, specs)
}

func (a *aptManager) ReinstallCommand(items []ChangeItem) string {
	return a.DowngradeCommand(items)
}

func (a *aptManager) RemoveCommand(items []ChangeItem) string {
	return aptEnv + "\napt-get remove -y " + joinNames(items)
}

func (a *aptManager) SourcePaths() []string {
	return []string{
		"etc/apt/sources.list",
		"etc/apt/sources.list.d",
		"etc/apt/keyrings",
		"etc/apt/trusted.gpg.d",
	}
}

func (a *aptManager) RenderSpec(s Spec) string {
	if s.Version != "" {
		return s.Name + "=" + s.Version
	}
	return s.Name
}
