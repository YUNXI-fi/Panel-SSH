package env

import (
	"context"
	"regexp"
	"strings"
)

// rpmManager 同时服务 dnf（RHEL/CentOS 8+、Fedora、Rocky、Alma）
// 与 yum（CentOS 7 等老系统）。两者都以 rpm 为包数据库，事务表格式同源，
// 差异只在模拟参数与个别子命令上，用 yum 标志位区分。
type rpmManager struct {
	yum bool
}

func (m *rpmManager) Family() Family {
	if m.yum {
		return FamilyYUM
	}
	return FamilyDNF
}

const rpmListFmt = `rpm -qa --qf '%{NAME}\t%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}\t%{ARCH}\n' 2>/dev/null`

// ---- 查询 ----

func (m *rpmManager) Installed(ctx context.Context, ex Executor) ([]Package, error) {
	out, err := runQuery(ctx, ex, "export LC_ALL=C\n"+rpmListFmt, queryTimeout)
	if err != nil {
		return nil, err
	}
	return m.ParsePackages(out), nil
}

func (m *rpmManager) PackageListCommand() string { return rpmListFmt }

func (m *rpmManager) ParsePackages(raw string) []Package {
	pkgs := make([]Package, 0)
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		arch := ""
		if len(fields) >= 3 {
			arch = fields[2]
		}
		pkgs = append(pkgs, Package{Name: fields[0], Version: fields[1], Arch: arch})
	}
	return pkgs
}

var dnfUpdateLine = regexp.MustCompile(`^([^\s.]+\.[a-z0-9_]+)\s+(\S+)\s+(\S+)\s*$`)

func (m *rpmManager) Upgrades(ctx context.Context, ex Executor) ([]Upgrade, error) {
	// check-update 约定：100=有更新，0=无更新，1=出错。脚本里不让它中断，
	// 因此直接看输出内容。
	var cmd string
	if m.yum {
		cmd = "yum check-update -y -q --color=never 2>/dev/null; true"
	} else {
		cmd = "dnf check-update -y -q --color=never 2>/dev/null; true"
	}
	out, err := runQuery(ctx, ex, "export LC_ALL=C\n"+cmd, queryTimeout)
	if err != nil {
		return nil, err
	}

	// 当前版本要从 rpm 数据库补：check-update 只给候选版本。
	installed, err := m.Installed(ctx, ex)
	if err != nil {
		return nil, err
	}
	cur := map[string]string{}
	for _, p := range installed {
		cur[p.Name] = p.Version
	}

	ups := make([]Upgrade, 0)
	for _, line := range strings.Split(out, "\n") {
		mm := dnfUpdateLine.FindStringSubmatch(strings.TrimSpace(line))
		if mm == nil {
			continue
		}
		name, arch := splitNameArch(mm[1])
		ups = append(ups, Upgrade{
			Name:      name,
			Arch:      arch,
			Candidate: mm[2],
			Repo:      mm[3],
			Current:   cur[name],
		})
	}
	return ups, nil
}

func splitNameArch(nameArch string) (name, arch string) {
	if idx := strings.LastIndex(nameArch, "."); idx > 0 {
		return nameArch[:idx], nameArch[idx+1:]
	}
	return nameArch, ""
}

func (m *rpmManager) Candidates(ctx context.Context, ex Executor, name string) ([]Candidate, error) {
	if !validPkgToken(name) {
		return nil, ErrInvalidPackage
	}
	var script string
	if m.yum {
		// 老 yum 没有 repoquery，解析 list 的 Available 段：name.arch ver repo。
		script = `export LC_ALL=C
echo "===INSTALLED==="
rpm -q --qf '%{NAME}\t%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}\n' ` + shellQuote(name) + ` 2>/dev/null
echo "===CANDIDATES==="
yum --showduplicates list ` + shellQuote(name) + ` --color=never 2>/dev/null | awk '
  /Available Packages/ {f=1; next}
  f && NF>=3 {print $2"\t"$3}
'`
	} else {
		script = `export LC_ALL=C
echo "===INSTALLED==="
rpm -q --qf '%{NAME}\t%|EPOCH?{%{EPOCH}:}:{}|%{VERSION}-%{RELEASE}\n' ` + shellQuote(name) + ` 2>/dev/null
echo "===CANDIDATES==="
dnf repoquery --showduplicates -q --color=never --qf '%{version}-%{release}\t%{reponame}\n' ` + shellQuote(name) + ` 2>/dev/null`
	}

	out, err := runQuery(ctx, ex, script, queryTimeout)
	if err != nil {
		return nil, err
	}
	sections := splitSections(out)

	cands := make([]Candidate, 0)
	// 已装版本从 rpm -q 输出的第二列取。
	for _, line := range strings.Split(sections["INSTALLED"], "\n") {
		if fields := strings.Split(strings.TrimSpace(line), "\t"); len(fields) == 2 && fields[0] == name {
			cands = append(cands, Candidate{Version: fields[1], Repo: "(已安装)"})
			break
		}
	}
	for _, line := range strings.Split(sections["CANDIDATES"], "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) >= 1 && fields[0] != "" {
			repo := ""
			if len(fields) >= 2 {
				repo = fields[1]
			}
			cands = append(cands, Candidate{Version: fields[0], Repo: repo})
		}
	}
	return dedupCandidates(cands), nil
}

// ---- 模拟预检 ----

// rpmChangeAWK 解析 dnf/yum 的事务表并输出 TSV（CHANGES 段）。
//
// 事务表包行统一为五列：name arch version repo size；
// 旧版本号从 BEGIN 阶段加载的 rpm 已装清单里补，事务表本身不打印 from。
const rpmChangeAWK = `awk -v rpmq='` + rpmListFmt + `' '
BEGIN {
  while ((rpmq) | getline) {
    n = split($0, a, "\t")
    if (n >= 2) cur[a[1]] = a[2]
  }
  close(rpmq)
}
function start_section(title) {
  if (title ~ /^Upgrading/) return "upgrade"
  if (title ~ /^Downgrading/) return "downgrade"
  if (title ~ /^Removing/) return "remove"
  if (title ~ /^Replacing/) return "install"
  if (title ~ /^Installing/) return "install"
  return ""
}
/^(Installing|Upgrading|Downgrading|Removing|Replacing)/ && /:$/ {
  section = start_section($1)
  next
}
/^(Transaction Summary|Skipping|Dependencies resolved|Nothing to do|Complete!)/ {
  section = ""
  next
}
section != "" && /^[[:space:]]/ {
  line = $0
  sub(/^[[:space:]]+/, "", line)
  n = split(line, f, /[[:space:]]+/)
  if (n < 3) next
  # 老 yum 把名字和架构挤在一列（nginx.x86_64）。
  pname = f[1]; parch = f[2]; pver = f[3]
  if (pver !~ /[0-9]/) next
  if (pname ~ /\.(x86_64|aarch64|noarch|i686|ppc64le|s390x|armv7hl)$/) {
    ix = match(pname, /\.[^.]+$/)
    parch = substr(pname, ix+1)
    pname = substr(pname, 1, ix-1)
  }
  oldv = (pname in cur) ? cur[pname] : ""
  kind = section
  if (section == "install" && oldv != "") kind = "upgrade"
  printf "%s\t%s\t%s\t%s\t%s\n", kind, pname, oldv, pver, parch
}
'`

// rpmTailSegments 是 dnf/yum 模拟公共的输出段，依赖 OUT 与 RC。
const rpmTailSegments = `
echo "===CHANGES==="
printf '%s\n' "$OUT" | ` + rpmChangeAWK + `
echo "===CONFLICTS==="
printf '%s\n' "$OUT" | grep -E '^Error:|^Errors encountered|nothing provides|Unable to find a match|No match for argument|No package [^ ]+ available|conflicting requests' | head -20
echo "===SIZE==="
printf '%s\n' "$OUT" | grep 'Total download size' | head -1 | sed -E 's/.*Total download size:? *//'
echo "===RAW==="
printf '%s\n' "$OUT" | tail -12
echo "===RC==="
echo "$RC"
`

// rpmSimCommand 按家族/动作生成模拟命令本身。
func rpmSimCommand(yum bool, action string, specs []Spec) string {
	spec := joinSpecs(&rpmManager{yum: yum}, specs)
	if yum {
		switch action {
		case ActionInstall, ActionUpgrade:
			return "yum install -y " + spec + " --color=never </dev/null"
		case ActionRemove:
			return "yum remove -y " + spec + " --color=never </dev/null"
		case ActionUpgradeAll:
			return "yum update -y --color=never </dev/null"
		}
	}
	switch action {
	case ActionInstall, ActionUpgrade:
		return "dnf install -y --assumeno --color=never " + spec
	case ActionRemove:
		return "dnf remove -y --assumeno --color=never " + spec
	case ActionUpgradeAll:
		return "dnf upgrade -y --assumeno --color=never"
	}
	return ""
}

// rpmSimScript 在目标机上跑求解器模拟。
//
// dnf 用 --assumeno：有可行方案时打印事务后以 1 退出；yum 没有等价选项，
// 把 stdin 接到 /dev/null，读到 EOF 的 yum 会按 N 中止，同样安全不落变更。
func rpmSimScript(yum bool, action string, specs []Spec) string {
	yumFlag := "0"
	if yum {
		yumFlag = "1"
	}
	return `
export LC_ALL=C
YUM=` + yumFlag + `
CMD="` + rpmSimCommand(yum, action, specs) + `"
set +e
OUT=$(sh -c "$CMD" 2>&1)
RC=$?
` + rpmTailSegments
}

// rpmTempRepoSimScript 用临时候选仓库做预检（Node 等第三方源场景），
// 通过 --repofrompath 临时挂载，不写 /etc/yum.repos.d。--nogpgcheck 仅用于模拟。
func rpmTempRepoSimScript(yum bool, repoID, baseURL string, specs []Spec) string {
	bin := "dnf"
	if yum {
		bin = "yum"
	}
	return `
export LC_ALL=C
set +e
OUT=$(` + bin + ` install -y --assumeno --color=never --nogpgcheck --repofrompath=` + repoID + `,` + baseURL + ` --enablerepo=` + repoID + ` ` + joinSpecs(&rpmManager{yum: yum}, specs) + ` 2>&1 </dev/null)
RC=$?
` + rpmTailSegments
}

func (m *rpmManager) Simulate(ctx context.Context, ex Executor, action string, specs []Spec) (*SimReport, error) {
	for _, s := range specs {
		if !validPkgToken(s.Name) || (s.Version != "" && !validPkgToken(s.Version)) {
			return nil, ErrInvalidPackage
		}
	}
	if action != ActionUpgradeAll && len(specs) == 0 {
		return nil, ErrNoSpecs
	}

	out, err := runQuery(ctx, ex, rpmSimScript(m.yum, action, specs), simulateTimeout)
	if err != nil {
		return nil, err
	}
	return parseSimTSV(out), nil
}

// ---- 变更步骤 ----

func (m *rpmManager) bin() string {
	if m.yum {
		return "yum"
	}
	return "dnf"
}

func (m *rpmManager) Steps(action string, specs []Spec) []Step {
	bin := m.bin()
	switch action {
	case ActionInstall, ActionUpgrade:
		// dnf/yum 会自动刷新过期元数据，不需要独立的 makecache 步骤。
		return []Step{{
			Name:     "安装/升级软件包",
			Command:  "export LC_ALL=C\n" + bin + " install -y " + joinSpecs(m, specs),
			Critical: true,
		}}
	case ActionRemove:
		return []Step{{
			Name:     "卸载软件包",
			Command:  "export LC_ALL=C\n" + bin + " remove -y " + joinSpecs(m, specs),
			Critical: true,
		}}
	case ActionUpgradeAll:
		sub := "upgrade"
		if m.yum {
			sub = "update"
		}
		return []Step{{
			Name:     "升级全部软件包",
			Command:  "export LC_ALL=C\n" + bin + " " + sub + " -y",
			Critical: true,
		}}
	}
	return nil
}

func (m *rpmManager) RepairStep() *Step {
	// rpm/dnf 没有 apt-get -f 那种成熟的自动修复；强跑 rpm --rebuilddb
	// 反而有风险，因此不提供自动修复步骤，回滚只还原源与包版本。
	return nil
}

func (m *rpmManager) DowngradeCommand(items []ChangeItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if it.From != "" {
			parts = append(parts, shellQuote(it.Name+"-"+it.From))
		}
	}
	// 直接以旧 NEVR 安装：dnf 对仓库中仍存在的旧版本会接受，
	// 比 downgrade 子命令覆盖面更广（后者要求包还在本地缓存）。
	return "export LC_ALL=C\n" + m.bin() + " install -y " + strings.Join(parts, " ")
}

func (m *rpmManager) ReinstallCommand(items []ChangeItem) string { return m.DowngradeCommand(items) }

func (m *rpmManager) RemoveCommand(items []ChangeItem) string {
	return "export LC_ALL=C\n" + m.bin() + " remove -y " + joinNames(items)
}

func (m *rpmManager) SourcePaths() []string {
	return []string{
		"etc/yum.repos.d",
		"etc/pki/rpm-gpg",
	}
}

func (m *rpmManager) RenderSpec(s Spec) string {
	if s.Version != "" {
		return s.Name + "-" + s.Version
	}
	return s.Name
}
