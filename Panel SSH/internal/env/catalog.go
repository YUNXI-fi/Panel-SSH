package env

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// VersionOption 是软件目录中一个可选版本。
type VersionOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// RecipePlan 是一个目录软件在目标家族上的执行计划。
//
// SimScript 非空时用它（自定义第三方源模拟）做预检；否则用包管理器
// 对 PkgSpecs 的标准模拟。Steps 是用户确认后实际执行的完整步骤。
type RecipePlan struct {
	Steps    []Step `json:"steps"`
	PkgSpecs []Spec `json:"pkgSpecs,omitempty"`
	// RollbackRemove 是该软件的"主体包"：回滚时若变更前未安装则卸载、
	// 已安装则装回快照版本。只放软件本体，不放 ca-certificates 这类
	// 通用前置包——自动卸掉它们会误伤机器上的其他东西。
	RollbackRemove []string `json:"rollbackRemove,omitempty"`
	SimScript      string   `json:"-"`
	Verify         string   `json:"-"`
	VerifyLabel    string   `json:"verifyLabel,omitempty"`
	Note           string   `json:"note,omitempty"`
}

// LaunchSpec 描述一个目录条目安装后如何"打开"。
//
// 只给能落到 Panel SSH 窗口里的条目填：目前是网络安全类命令行工具，
// 打开即在那台主机的终端窗口里执行 Command。系统运行时、工具链、数据库
// 这类环境软件不填，界面因此不会给它们生成桌面图标或"打开"入口。
type LaunchSpec struct {
	// Command 是打开时在目标机终端里自动执行的命令（通常是一条只读状态命令）。
	Command string `json:"command"`
	// ConfigPaths 是主要配置文件的绝对路径，供详情页展示。
	ConfigPaths []string `json:"configPaths,omitempty"`
}

// Recipe 描述软件目录中的一项（语言运行时或工具链）。
type Recipe struct {
	Slug        string
	Name        string
	Category    string // runtime / toolchain / network / security / …
	Description string
	// UseCase 说明"这套软件通常拿来干什么"，与 Description（它是什么）互补。
	// 目前只有网络安全分类需要单列，其余留空。
	UseCase string
	// Note 是安装前后的注意事项，随目录状态一起带给界面。
	Note string
	// Launch 非空表示安装后可以"打开"；为空表示只提供安装/重装。
	Launch *LaunchSpec
	// Families 为空表示全家族支持；不支持的家族在目录状态里直接标记。
	Families []Family
	// DetectCommand 生成版本探测命令：输出的第一行即版本号，无输出视为未安装。
	DetectCommand func(p *Profile) string
	// DetectLabel 是版本探测结果的人话名称；目录条目未声明时退回 Name。
	DetectLabel string
	// ServiceUnits 是各家族的守护进程单元名，供"应用包"生成状态/启停动作。
	// 留空表示这个条目不是常驻服务。
	ServiceUnits map[Family]string
	// ConfigPaths 是主要配置文件的绝对路径，供应用包与详情页复用。
	ConfigPaths []string
	// Versions 返回可选版本；nil 表示该软件跟随系统仓库，不提供版本选择。
	Versions func(ctx context.Context, p *Profile) ([]VersionOption, error)
	// Plan 生成指定家族上的安装/升级计划。version 为空表示默认版本。
	Plan func(ctx context.Context, p *Profile, version string) (*RecipePlan, error)
	// CheckCommand 生成"安装后复核脚本"：输出 OK 表示通过，其它内容原样作为
	// 问题描述回给用户。留空则退回"取版本号"这种弱复核。
	//
	// 需要它的是"装上了不等于能用"的条目：数据库、Web 服务器这类服务，
	// 版本命令会照常打出版本，而服务根本没起来。
	CheckCommand func(p *Profile) string
}

// ---- 版本探测命令 ----

func firstLineVersion(cmd string) string {
	return cmd + " 2>/dev/null | head -1"
}

func detectNode(*Profile) string { return firstLineVersion("node -v 2>/dev/null | sed 's/^v//'") }
func detectPython(*Profile) string {
	// 必须先 command -v 守卫：python3 -V 的错误走 stderr 又被 2>&1 合并时，
	// "sh: python3: not found" 会被当成版本输出。
	return firstLineVersion(`command -v python3 >/dev/null 2>&1 && python3 -V 2>&1 | awk '{print $2}'`)
}
func detectGo(*Profile) string {
	return firstLineVersion("go version 2>/dev/null | awk '{print $3}' | sed 's/^go//'")
}
func detectJava(*Profile) string {
	// openjdk 输出：openjdk version "17.0.9" 2024-...，取引号里的主版本。
	// command -v 守卫理由同 detectPython（java -version 输出在 stderr）。
	return firstLineVersion(`command -v java >/dev/null 2>&1 && java -version 2>&1 | head -1 | sed -E 's/.*"([^"]+)".*/\1/'`)
}
func detectDocker(*Profile) string {
	return firstLineVersion("docker --version 2>/dev/null | sed -E 's/Docker version ([^ ,]+).*/\\1/'")
}
func detectGCC(*Profile) string { return firstLineVersion("gcc -dumpversion 2>/dev/null") }

// detectBun / detectCUDA 用绝对路径：这两个都是脚本或官方包直接落到
// /usr/local 的，非登录 shell（面板跑命令的方式）不会读 /etc/profile.d，
// 靠 PATH 找会时灵时不灵。
func detectBun(*Profile) string { return firstLineVersion("/usr/local/bin/bun --version") }
func detectCUDA(*Profile) string {
	return firstLineVersion("/usr/local/cuda/bin/nvcc --version 2>/dev/null | tail -1 | sed -E 's/.*release ([0-9.]+).*/\\1/'")
}
func detectMongo(*Profile) string {
	return firstLineVersion("mongod --version 2>/dev/null | head -1 | sed -E 's/.*v([0-9.]+).*/\\1/'")
}

// detectDsh 用 npm 自己的全局包列表回答"装没装 dsh"。
//
// 不用 `dsh --version`：面板跑命令是非登录 shell，不读 /etc/profile.d，
// npm 的全局 bin 目录未必在 PATH 上，那样会把"装了但 PATH 里没有"误报成没装。
// npm ls -g 问的是 npm 自己认不认这个包，与 PATH 无关。
func detectDsh(*Profile) string {
	return firstLineVersion(
		`npm ls -g --depth=0 @deepseek-ai/dsh 2>/dev/null | sed -n 's/.*@deepseek-ai\/dsh@\(.*\)/\1/p'`)
}

// ---- 版本候选 ----

// nodeLTS 是 NodeSource 仍在维护的大版本线（2026 年视角：20 维护、22/24 LTS）。
var nodeLTS = []string{"24", "22", "20"}

func staticVersions(versions []string) func(context.Context, *Profile) ([]VersionOption, error) {
	return func(_ context.Context, _ *Profile) ([]VersionOption, error) {
		opts := make([]VersionOption, 0, len(versions))
		for _, v := range versions {
			opts = append(opts, VersionOption{Value: v, Label: v + ".x（官方仓库）"})
		}
		return opts, nil
	}
}

// fetchGoVersions 从 go.dev 拉取稳定版本列表（面板侧联网，不是目标机）。
func fetchGoVersions(ctx context.Context, _ *Profile) ([]VersionOption, error) {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// 默认 mode=json 只回最新两个 minor 系列的当前版本；include=all 才给
	// 完整历史（其中 rc/beta 的 stable=false，下方统一过滤）。
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, "https://go.dev/dl/?mode=json&include=all", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var releases []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, err
	}
	opts := make([]VersionOption, 0, 12)
	for _, r := range releases {
		if !r.Stable || !strings.HasPrefix(r.Version, "go") {
			continue
		}
		v := strings.TrimPrefix(r.Version, "go")
		opts = append(opts, VersionOption{Value: v, Label: v})
		if len(opts) >= 12 {
			break
		}
	}
	if len(opts) == 0 {
		return nil, fmt.Errorf("官方版本列表为空")
	}
	return opts, nil
}

// ---- Recipe 实现 ----

func planNode(_ context.Context, p *Profile, version string) (*RecipePlan, error) {
	if version == "" {
		version = "22"
	}
	if !inList(version, nodeLTS) {
		return nil, fmt.Errorf("不支持的 Node.js 大版本：%s", version)
	}
	plan := &RecipePlan{
		Verify:      "node -v 2>/dev/null | sed 's/^v//'",
		VerifyLabel: "node",
	}
	switch p.Family {
	case FamilyApt:
		repoLine := fmt.Sprintf("deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_%s.x nodistro main", version)
		plan.PkgSpecs = []Spec{{Name: "nodejs"}}
		plan.RollbackRemove = []string{"nodejs"}
		plan.SimScript = aptTempRepoSimScript(
			fmt.Sprintf("deb [trusted=yes] https://deb.nodesource.com/node_%s.x nodistro main", version),
			plan.PkgSpecs)
		plan.Steps = []Step{
			{Name: "安装前置工具（ca-certificates/curl/gnupg）", Command: aptEnv + "\napt-get update -qq\napt-get install -y ca-certificates curl gnupg", Critical: false},
			{Name: "添加 NodeSource 官方签名密钥", Command: "set -e\ninstall -d -m 0755 /etc/apt/keyrings\ncurl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --batch --yes --dearmor -o /etc/apt/keyrings/nodesource.gpg\nchmod 644 /etc/apt/keyrings/nodesource.gpg", Critical: true},
			{Name: "添加 NodeSource 官方软件源", Command: "set -e\necho " + shellQuote(repoLine) + " > /etc/apt/sources.list.d/nodesource.list", Critical: true},
			{Name: "刷新索引并安装 Node.js", Command: aptEnv + "\napt-get update -qq\napt-get install -y nodejs", Critical: true},
		}
	case FamilyDNF, FamilyYUM:
		yum := p.Family == FamilyYUM
		bin := "dnf"
		if yum {
			bin = "yum"
		}
		baseURL := fmt.Sprintf("https://rpm.nodesource.com/pub_%s.x/nodistro/el/%s/%s", version, majorVersion(p.Version), p.PkgArch)
		plan.PkgSpecs = []Spec{{Name: "nodejs"}}
		plan.RollbackRemove = []string{"nodejs"}
		plan.SimScript = rpmTempRepoSimScript(yum, "nodesource", baseURL, plan.PkgSpecs)
		plan.Steps = []Step{
			// 官方 setup 脚本负责装密钥、写 repo（与官方文档一致，避免手工
			// 拼仓库路径在各 RHEL 系发行版之间漂移）。
			{Name: "运行 NodeSource 官方仓库配置脚本", Command: "set -e\ncurl -fsSL https://rpm.nodesource.com/setup_" + version + ".x | bash -", Critical: true},
			{Name: "安装 Node.js", Command: "export LC_ALL=C\n" + bin + " install -y nodejs", Critical: true},
		}
	case FamilyAPK:
		plan.PkgSpecs = []Spec{{Name: "nodejs"}, {Name: "npm"}}
		plan.Note = "Alpine 仓库只提供当前版本，大版本选择不生效"
		plan.Steps = []Step{
			{Name: "安装 Node.js 与 npm", Command: "apk add --no-cache nodejs npm", Critical: true},
		}
		plan.RollbackRemove = []string{"nodejs", "npm"}
	}
	return plan, nil
}

func planPython(_ context.Context, p *Profile, _ string) (*RecipePlan, error) {
	plan := &RecipePlan{
		Verify:      "python3 -V | awk '{print $2}'",
		VerifyLabel: "python3",
	}
	var specs []Spec
	switch p.Family {
	case FamilyApt:
		// venv 是现代 Python 开发的刚需（pip 外部管理环境限制），随主包一起装。
		specs = []Spec{{Name: "python3"}, {Name: "python3-pip"}, {Name: "python3-venv"}}
	case FamilyDNF, FamilyYUM:
		specs = []Spec{{Name: "python3"}, {Name: "python3-pip"}}
	case FamilyAPK:
		specs = []Spec{{Name: "python3"}, {Name: "py3-pip"}}
	}
	plan.PkgSpecs = specs
	plan.RollbackRemove = specNames(specs)
	plan.Steps = []Step{{Name: "安装 Python 3 与 pip", Command: managerSteps(p, ActionInstall, specs)}}
	plan.Note = "版本由发行版官方仓库决定（不同发行版自带版本不同）"
	return plan, nil
}

func planGo(_ context.Context, p *Profile, version string) (*RecipePlan, error) {
	if version == "" {
		return nil, fmt.Errorf("请选择或输入 Go 版本号")
	}
	if !isValidGoVersion(version) {
		return nil, fmt.Errorf("Go 版本号格式应为 1.x.y，如 1.26.1")
	}

	prereq := []Spec{{Name: "ca-certificates"}, {Name: "curl"}, {Name: "tar"}}
	plan := &RecipePlan{
		PkgSpecs:    prereq,
		Note:        fmt.Sprintf("将从 go.dev 下载官方 tarball（约 70MB）并安装到 /usr/local/go，版本 %s", version),
		Verify:      "go version 2>/dev/null | sed 's/^go//' | awk '{print $1}'",
		VerifyLabel: "go",
	}

	tarball := fmt.Sprintf("set -e\ncurl -fL --retry 3 -o /tmp/panelssh-go.tar.gz https://go.dev/dl/go%s.linux-%s.tar.gz\nrm -rf /usr/local/go\ntar -C /usr/local -xzf /tmp/panelssh-go.tar.gz\nrm -f /tmp/panelssh-go.tar.gz\nln -sf /usr/local/go/bin/go /usr/local/bin/go\nln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt",
		version, p.Arch)

	plan.Steps = []Step{
		{Name: "安装下载与解压工具", Command: managerSteps(p, ActionInstall, prereq), Critical: false},
		{Name: "下载并安装 Go " + version + " 到 /usr/local/go", Command: tarball, Critical: true},
	}
	return plan, nil
}

var javaLTS = []string{"21", "17"}

func planJava(_ context.Context, p *Profile, version string) (*RecipePlan, error) {
	if version == "" {
		version = "21"
	}
	if !inList(version, javaLTS) {
		return nil, fmt.Errorf("仅支持 Java LTS 版本：%s", strings.Join(javaLTS, "、"))
	}
	var specs []Spec
	switch p.Family {
	case FamilyApt:
		specs = []Spec{{Name: "openjdk-" + version + "-jdk"}}
	case FamilyDNF, FamilyYUM:
		specs = []Spec{{Name: "java-" + version + "-openjdk-devel"}}
	case FamilyAPK:
		// Alpine 包名没有 -jdk 后缀，openjdk21 即完整 JDK。
		specs = []Spec{{Name: "openjdk" + version}}
	}
	plan := &RecipePlan{
		PkgSpecs:       specs,
		Steps:          []Step{{Name: "安装 OpenJDK " + version + "（JDK）", Command: managerSteps(p, ActionInstall, specs)}},
		Verify:         `java -version 2>&1 | head -1 | sed -E 's/.*"([^"]+)".*/\1/'`,
		VerifyLabel:    "java",
		RollbackRemove: specNames(specs),
	}
	return plan, nil
}

// dockerKeyScript 获取 Docker 的 apt 签名密钥。
//
// 官方 download.docker.com 在部分网络里会被重置（解析到 IPv6 的线路尤其明显），
// 而国内镜像分发的是同一把密钥。依次尝试，任一成功即可；全部失败才报错，
// 而不是让 gpg 对空输入说一句 no valid OpenPGP data。
func dockerKeyScript(osName string) string {
	urls := []string{
		"https://mirrors.ustc.edu.cn/docker-ce/linux/" + osName + "/gpg",
		"https://mirrors.aliyun.com/docker-ce/linux/" + osName + "/gpg",
		"https://mirrors.tuna.tsinghua.edu.cn/docker-ce/linux/" + osName + "/gpg",
		"https://download.docker.com/linux/" + osName + "/gpg",
	}

	var b strings.Builder
	// --batch --yes：这一步没有伪终端，gpg 默认会去开 /dev/tty 而直接失败。
	// GNUPGHOME 放在临时目录：只是把 ASCII 密钥转成二进制，不该在 root 家目录留下钥匙箱。
	b.WriteString("set -e\nWORK=$(mktemp -d)\ntrap 'rm -rf \"$WORK\"' EXIT\n")
	b.WriteString("export GNUPGHOME=\"$WORK/gnupg\"\nmkdir -p \"$GNUPGHOME\"\nchmod 700 \"$GNUPGHOME\"\n")
	b.WriteString("KEY=\"$WORK/docker.asc\"\ninstall -d -m 0755 /etc/apt/keyrings\nfor u in")
	for _, u := range urls {
		b.WriteString(" " + shellQuote(u))
	}
	b.WriteString("; do\n")
	b.WriteString(`  if curl -fsSL --connect-timeout 8 --max-time 60 "$u" -o "$KEY" && [ -s "$KEY" ]; then break; fi` + "\n")
	b.WriteString("done\n")
	b.WriteString(`if [ ! -s "$KEY" ]; then echo '无法获取 Docker 签名密钥：官方源与国内镜像均不可达' >&2; exit 1; fi` + "\n")
	b.WriteString("gpg --batch --yes --dearmor -o /etc/apt/keyrings/docker.gpg < \"$KEY\"\n")
	b.WriteString("chmod 644 /etc/apt/keyrings/docker.gpg")
	return b.String()
}

func planDocker(_ context.Context, p *Profile, _ string) (*RecipePlan, error) {
	plan := &RecipePlan{
		Verify:      "docker --version | sed -E 's/Docker version ([^ ,]+).*/\\1/'",
		VerifyLabel: "docker",
	}
	pkgs := []Spec{
		{Name: "docker-ce"}, {Name: "docker-ce-cli"}, {Name: "containerd.io"},
		{Name: "docker-buildx-plugin"}, {Name: "docker-compose-plugin"},
	}

	switch p.Family {
	case FamilyApt:
		if p.OS != "ubuntu" && p.OS != "debian" {
			return nil, fmt.Errorf("Docker 官方源仅支持 Ubuntu/Debian（当前为 %s），可改用系统仓库安装", p.OS)
		}
		// 官方源在部分网络里会建立连接却拿不到数据（CloudFront 大文件尤其明显），
		// 国内镜像分发的是同一套包。这里默认走中科大镜像，并在计划说明里写明。
		const aptMirror = "https://mirrors.ustc.edu.cn/docker-ce/linux/"
		repoLine := fmt.Sprintf("deb [arch=%s signed-by=/etc/apt/keyrings/docker.gpg] %s%s %s stable",
			p.Arch, aptMirror, p.OS, p.Codename)
		plan.PkgSpecs = pkgs
		plan.Note = "使用中科大 Docker 镜像下载（官方源在部分网络会限速或重置连接）"
		plan.SimScript = aptTempRepoSimScript(
			fmt.Sprintf("deb [trusted=yes arch=%s] %s%s %s stable", p.Arch, aptMirror, p.OS, p.Codename),
			pkgs)
		plan.Steps = []Step{
			{Name: "安装前置工具", Command: aptEnv + "\napt-get update -qq\napt-get install -y ca-certificates curl gnupg", Critical: false},
			{Name: "添加 Docker 官方签名密钥", Command: dockerKeyScript(p.OS), Critical: true},
			{Name: "添加 Docker 官方软件源", Command: "set -e\necho " + shellQuote(repoLine) + " > /etc/apt/sources.list.d/docker.list", Critical: true},
			{Name: "安装 Docker Engine 与插件", Command: aptEnv + "\napt-get update -qq\napt-get install -y " + joinSpecs(&aptManager{}, pkgs), Critical: true},
			// 容器内没有 systemd，enable 失败不应判整次安装失败——二进制已就位。
			{Name: "设置 Docker 开机自启并启动", Command: "systemctl enable --now docker 2>/dev/null || true", Critical: false},
		}
	case FamilyDNF, FamilyYUM:
		yum := p.Family == FamilyYUM
		bin := "dnf"
		if yum {
			bin = "yum"
		}
		major := majorVersion(p.Version)
		// Rocky/Alma/RHEL 都使用 Docker 的 CentOS 兼容仓库。
		plan.PkgSpecs = pkgs
		plan.SimScript = rpmTempRepoSimScript(yum, "docker-ce",
			fmt.Sprintf("https://download.docker.com/linux/centos/%s/%s/stable", major, p.PkgArch), pkgs)
		plan.Steps = []Step{
			{Name: "添加 Docker 官方软件源", Command: "set -e\ncurl -fsSL -o /etc/yum.repos.d/docker-ce.repo https://download.docker.com/linux/centos/docker-ce.repo\nsed -i 's/\\$releasever/" + major + "/g' /etc/yum.repos.d/docker-ce.repo", Critical: true},
			{Name: "安装 Docker Engine 与插件", Command: "export LC_ALL=C\n" + bin + " install -y " + joinSpecs(&rpmManager{yum: yum}, pkgs), Critical: true},
			{Name: "设置 Docker 开机自启并启动", Command: "systemctl enable --now docker 2>/dev/null || true", Critical: false},
		}
	case FamilyAPK:
		plan.PkgSpecs = []Spec{{Name: "docker"}, {Name: "docker-cli-compose"}}
		plan.RollbackRemove = specNames(plan.PkgSpecs)
		plan.Steps = []Step{
			{Name: "安装 Docker 与 compose 插件", Command: "apk add --no-cache docker docker-cli-compose", Critical: true},
			{Name: "加入开机服务并启动（OpenRC）", Command: "rc-update add docker default 2>/dev/null || true\nservice docker start 2>/dev/null || true", Critical: false},
		}
	}
	if plan.RollbackRemove == nil {
		plan.RollbackRemove = specNames(pkgs)
	}
	return plan, nil
}

func planToolchain(_ context.Context, p *Profile, _ string) (*RecipePlan, error) {
	plan := &RecipePlan{
		Verify:      "gcc -dumpversion 2>/dev/null",
		VerifyLabel: "gcc",
	}
	switch p.Family {
	case FamilyApt:
		specs := []Spec{{Name: "build-essential"}}
		plan.PkgSpecs = specs
		plan.RollbackRemove = []string{"build-essential"}
		plan.Steps = []Step{{Name: "安装编译工具链（build-essential：gcc/g++/make）", Command: managerSteps(p, ActionInstall, specs)}}
	case FamilyDNF, FamilyYUM:
		// 包组没有普通包的 NEVRA 事务行格式，模拟与执行都走 group 子命令。
		cmd := "dnf group install -y " + shellQuote("Development Tools")
		if p.Family == FamilyYUM {
			cmd = "yum groupinstall -y " + shellQuote("Development Tools")
		}
		simCmd := cmd + " --assumeno --color=never </dev/null"
		if p.Family == FamilyYUM {
			simCmd = cmd + " --color=never </dev/null"
		}
		plan.SimScript = "export LC_ALL=C\nset +e\nOUT=$(" + simCmd + " 2>&1)\nRC=$?\n" + rpmTailSegments
		plan.Steps = []Step{{Name: "安装开发工具组（Development Tools）", Command: "export LC_ALL=C\n" + cmd, Critical: true}}
		// 开发工具组是几十个编译器/库的集合，自动 group remove 会连带移除
		// 其他软件正在用的组件，回滚不做这件事，只还原源并给出说明。
		plan.Note = "RHEL 系的工具链以包组安装，回滚时不会自动卸载编译器集合，需人工评估"
	case FamilyAPK:
		specs := []Spec{{Name: "build-base"}, {Name: "linux-headers"}}
		plan.PkgSpecs = specs
		plan.RollbackRemove = specNames(specs)
		plan.Steps = []Step{{Name: "安装编译工具链（build-base/linux-headers）", Command: managerSteps(p, ActionInstall, specs)}}
	}
	return plan, nil
}

// planBun 用官方安装脚本把 Bun 装到 /usr/local。
//
// Bun 没有进入任何发行版的官方仓库，只能走官方脚本 —— 与 Node 在 RPM 系上用
// 官方 setup 脚本是同一性质的取舍。安装前缀固定在 /usr/local（而不是脚本默认的
// 用户家目录），这样面板自己、root 与所有用户都能用同一条路径。
func planBun(_ context.Context, p *Profile, _ string) (*RecipePlan, error) {
	if p.PkgArch != "x86_64" && p.PkgArch != "aarch64" {
		return nil, fmt.Errorf("Bun 官方安装脚本只提供 x86_64 与 aarch64 版本（当前 %s）", p.PkgArch)
	}
	prereq := []Spec{{Name: "curl"}, {Name: "unzip"}}
	install := "set -e\n" +
		"export BUN_INSTALL=/usr/local\n" +
		"curl -fsSL https://bun.sh/install | bash"

	return &RecipePlan{
		PkgSpecs:    prereq,
		Verify:      "/usr/local/bin/bun --version",
		VerifyLabel: "bun",
		Note:        "通过官方脚本装到 /usr/local（约 90MB）。脚本安装不进包管理器，因此回滚不会自动卸载它 —— 需要时手工删除 /usr/local/bin/bun 与 /usr/local/bin/bunx",
		Steps: []Step{
			{Name: "安装下载与解压工具（curl/unzip）", Command: managerSteps(p, ActionInstall, prereq), Critical: false},
			{Name: "下载并安装 Bun 到 /usr/local", Command: install, Critical: true},
		},
	}, nil
}

// dshNodeVersion 是 dsh 所依赖的 Node.js 大版本。
//
// 取 24 而不是 planNode 的默认值 22：上游的包元数据是用 Node 24 构建的，
// 它没有声明 engines 约束，也就没有理由在一个更旧的运行时上碰运气。
const dshNodeVersion = "24"

// dshVersionCommand 从 npm 的全局包列表里取 dsh 的版本号。
//
// 与 detectDsh 是同一句命令，只是不带"取首行"那层包装：
// 计划里 Verify 的契约是"输出第一行即版本号"，与其它条目保持一致。
const dshVersionCommand = `npm ls -g --depth=0 @deepseek-ai/dsh 2>/dev/null | sed -n 's/.*@deepseek-ai\/dsh@\(.*\)/\1/p'`

// dshNpmStep 通过 npm 全局安装 dsh。
func dshNpmStep() Step {
	return Step{
		Name: "通过 npm 全局安装 @deepseek-ai/dsh",
		Command: "set -e\nexport LC_ALL=C\n" +
			"npm install -g @deepseek-ai/dsh@latest\n" +
			// 刚装上的 bin 在这个 shell 的命令缓存里可能还指向旧路径。
			"hash -r 2>/dev/null || true",
		Critical: true,
	}
}

// planDsh 安装 DeepSeek Harness（dsh）：先确保 Node.js 就位，再全局安装 dsh。
//
// 为什么把 Node 一起装，而不是让调用方串两次环境操作：
//   - Runner 的主机锁在两次操作之间会释放，中间态（Node 装了、dsh 没装）
//     的补偿语义说不清；
//   - 用户视角这是一个动作，应该只有一次确认、一次进度、一次补偿；
//   - "前置依赖作为配方内的普通步骤"已有先例（planBun 的 curl/unzip）。
//
// Node 步骤无条件执行：它本身是幂等的，而且能顺手修掉"机器上已有旧版 Node、
// 而 dsh 需要更新的版本"这种预检查不出来的情况。
func planDsh(ctx context.Context, p *Profile, _ string) (*RecipePlan, error) {
	node, err := planNode(ctx, p, dshNodeVersion)
	if err != nil {
		return nil, err
	}

	steps := append([]Step{}, node.Steps...)
	steps = append(steps, dshNpmStep())

	return &RecipePlan{
		Steps: steps,
		// 预检脚本与包规格都继承 Node 那份：apt/dnf 系用它做临时仓库模拟，
		// apk 没有模拟脚本、靠包列表回答 nodejs/npm 能不能装。
		// dsh 自己不是系统包，包管理器模拟不了它 —— 这是配方机制本来的边界，
		// 它的可安装性因此由"Node 与 npm 能不能装"决定。
		SimScript: node.SimScript,
		PkgSpecs:  node.PkgSpecs,
		// 回滚只撤 Node（它是系统包）；dsh 是 npm 全局包，包管理器不认识它，
		// 撤不掉，只能在 Note 里如实告知。
		RollbackRemove: node.RollbackRemove,
		Verify:         dshVersionCommand,
		VerifyLabel:    "dsh",
		Note: "会先确保 Node.js " + dshNodeVersion + ".x（NodeSource 官方源）就位，再全局安装 " +
			"@deepseek-ai/dsh。Node 步骤是幂等的；dsh 是 npm 全局包，包管理器不认识它，" +
			"因此回滚不会自动卸载。上游当前把预发布版标为 latest，装到的是 developer preview 版本。",
	}, nil
}

// cudaRepoDir 推导目标机对应的 NVIDIA CUDA 仓库目录名。
//
// NVIDIA 按"发行版+版本"发仓库（ubuntu2404 / debian12 / rhel9 / fedora40），
// 路径写死任何一个都会在别的版本上 404，因此按画像现算。
func cudaRepoDir(p *Profile) (string, error) {
	switch p.OS {
	case "ubuntu":
		return "ubuntu" + strings.ReplaceAll(p.Version, ".", ""), nil
	case "debian":
		return "debian" + majorVersion(p.Version), nil
	case "fedora":
		return "fedora" + majorVersion(p.Version), nil
	case "rhel", "rocky", "alma", "centos":
		return "rhel" + majorVersion(p.Version), nil
	}
	return "", fmt.Errorf("NVIDIA 官方仓库不覆盖 %s：CUDA 只提供 Ubuntu/Debian/Fedora/RHEL 系（Alpine 需自行用容器或 wsl）", p.OS)
}

// planCUDA 安装 NVIDIA CUDA Toolkit（官方仓库）。
//
// 只装 toolkit，不装驱动：驱动必须与具体显卡型号匹配，且装错会让整台机器
// 失去图形/计算能力，不该由"点一下装个工具链"来完成。
func planCUDA(_ context.Context, p *Profile, _ string) (*RecipePlan, error) {
	if p.PkgArch != "x86_64" {
		return nil, fmt.Errorf("NVIDIA 的 CUDA 仓库只提供 x86_64 包（当前架构 %s）", p.PkgArch)
	}
	repo, err := cudaRepoDir(p)
	if err != nil {
		return nil, err
	}
	base := "https://developer.download.nvidia.com/compute/cuda/repos/" + repo + "/x86_64"

	plan := &RecipePlan{
		PkgSpecs:    []Spec{{Name: "cuda-toolkit"}},
		Verify:      "/usr/local/cuda/bin/nvcc --version | tail -1 | sed -E 's/.*release ([0-9.]+).*/\\1/'",
		VerifyLabel: "nvcc",
		Note:        "只装 CUDA Toolkit（约 3~4GB），不含 NVIDIA 驱动；跑 GPU 程序前需要先按显卡型号装好匹配版本的驱动",
	}

	switch p.Family {
	case FamilyApt:
		plan.SimScript = aptTempRepoSimScript("deb [trusted=yes] "+base+"/ /", plan.PkgSpecs)
		plan.Steps = []Step{
			{Name: "安装密钥导入所需工具", Command: aptEnv + "\napt-get update -qq\napt-get install -y ca-certificates curl", Critical: false},
			{Name: "导入 NVIDIA CUDA 仓库签名密钥", Command: "set -e\ncurl -fsSL -o /tmp/cuda-keyring.deb " + base + "/cuda-keyring_1.1-1_all.deb\ndpkg -i /tmp/cuda-keyring.deb\nrm -f /tmp/cuda-keyring.deb", Critical: true},
			{Name: "刷新索引并安装 CUDA Toolkit", Command: aptEnv + "\napt-get update -qq\napt-get install -y cuda-toolkit", Critical: true},
		}
	case FamilyDNF, FamilyYUM:
		yum := p.Family == FamilyYUM
		bin := "dnf"
		if yum {
			bin = "yum"
		}
		repoFile := "/etc/yum.repos.d/cuda-" + repo + ".repo"
		plan.SimScript = rpmTempRepoSimScript(yum, "cuda", base+"/", plan.PkgSpecs)
		plan.Steps = []Step{
			{Name: "添加 NVIDIA CUDA 官方软件源", Command: "set -e\ncurl -fsSL -o " + repoFile + " " + base + "/cuda-" + repo + ".repo", Critical: true},
			{Name: "安装 CUDA Toolkit", Command: "export LC_ALL=C\n" + bin + " install -y cuda-toolkit", Critical: true},
		}
	}
	return plan, nil
}

// planMongo 安装 MongoDB 官方社区版（官方仓库）。
//
// 发行版仓库里没有 mongodb-org：Debian/Ubuntu 只有已被淘汰的 mongodb 包，
// RHEL 系完全没有，因此只能按官方文档加 MongoDB 自己的仓库。
func planMongo(_ context.Context, p *Profile, _ string) (*RecipePlan, error) {
	plan := &RecipePlan{
		PkgSpecs:    []Spec{{Name: "mongodb-org"}},
		Verify:      "mongod --version 2>/dev/null | head -1 | sed -E 's/.*v([0-9.]+).*/\\1/'",
		VerifyLabel: "mongod",
		Note:        "mongod 默认只监听 127.0.0.1 且未开启鉴权；要对外提供服务，请先创建管理员账号并开启 authorization",
	}

	switch p.Family {
	case FamilyApt:
		keyURL := "https://www.mongodb.org/static/pgp/server-8.0.asc"
		// Ubuntu 与 Debian 的仓库路径只差发行版名与组件名（multiverse / main）。
		distro, component := "ubuntu", "multiverse"
		if p.OS != "ubuntu" {
			distro, component = "debian", "main"
		}
		repoURL := "https://repo.mongodb.org/apt/" + distro + " " + p.Codename + "/mongodb-org/8.0 " + component
		repoLine := "deb [ arch=amd64,arm64 signed-by=/usr/share/keyrings/mongodb-server-8.0.gpg ] " + repoURL
		plan.SimScript = aptTempRepoSimScript("deb [trusted=yes] "+repoURL, plan.PkgSpecs)
		plan.Steps = []Step{
			{Name: "安装密钥导入所需工具", Command: aptEnv + "\napt-get update -qq\napt-get install -y ca-certificates curl gnupg", Critical: false},
			{Name: "导入 MongoDB 8.0 签名密钥", Command: "set -e\ncurl -fsSL " + keyURL + " | gpg --batch --yes --dearmor -o /usr/share/keyrings/mongodb-server-8.0.gpg", Critical: true},
			{Name: "添加 MongoDB 官方软件源", Command: "set -e\necho " + shellQuote(repoLine) + " > /etc/apt/sources.list.d/mongodb-org-8.0.list", Critical: true},
			{Name: "刷新索引并安装 MongoDB", Command: aptEnv + "\napt-get update -qq\napt-get install -y mongodb-org", Critical: true},
		}
	case FamilyDNF, FamilyYUM:
		yum := p.Family == FamilyYUM
		bin := "dnf"
		if yum {
			bin = "yum"
		}
		baseURL := "https://repo.mongodb.org/yum/redhat/" + majorVersion(p.Version) + "/mongodb-org/8.0/x86_64/"
		repoBody := "[mongodb-org-8.0]\nname=MongoDB Repository\nbaseurl=" + baseURL +
			"\ngpgcheck=1\nenabled=1\ngpgkey=https://www.mongodb.org/static/pgp/server-8.0.asc"
		plan.SimScript = rpmTempRepoSimScript(yum, "mongodb-org-8.0", baseURL, plan.PkgSpecs)
		plan.Steps = []Step{
			{Name: "添加 MongoDB 官方软件源", Command: "set -e\ncat > /etc/yum.repos.d/mongodb-org-8.0.repo <<'PANELSSH_MONGO_REPO'\n" + repoBody + "\nPANELSSH_MONGO_REPO", Critical: true},
			{Name: "安装 MongoDB", Command: "export LC_ALL=C\n" + bin + " install -y mongodb-org", Critical: true},
		}
	}

	plan.Steps = append(plan.Steps, serviceStep(p.Family, "mongod"))
	return plan, nil
}

// managerSteps 生成"单步包操作"的完整命令，供 Recipe 嵌入自己的步骤列表。
func managerSteps(p *Profile, action string, specs []Spec) string {
	m := NewManager(p)
	steps := m.Steps(action, specs)
	cmds := make([]string, 0, len(steps))
	for _, s := range steps {
		cmds = append(cmds, s.Command)
	}
	return strings.Join(cmds, "\n")
}

func majorVersion(versionID string) string {
	if i := strings.Index(versionID, "."); i > 0 {
		return versionID[:i]
	}
	return versionID
}

// specNames 取 Spec 列表的包名。
func specNames(specs []Spec) []string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	return names
}

func inList(s string, list []string) bool {
	for _, v := range list {
		if s == v {
			return true
		}
	}
	return false
}

// goVersionRe 严格校验 Go tarball 版本号（1.x.y，不含任何 shell 元字符）。
var goVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func isValidGoVersion(s string) bool { return goVersionRe.MatchString(s) }

// catalog 是软件目录的静态注册表。
//
// 分成两部分：需要第三方源或官方 tarball 的条目在这里逐条实现，
// 直接由发行版仓库提供的条目由 catalog_system.go 的数据表生成。
var catalog = append([]*Recipe{
	{
		Slug: "nodejs", Name: "Node.js", Category: "runtime",
		Description:   "Node.js 运行时与 npm，通过 NodeSource 官方仓库安装可选 LTS 大版本",
		DetectCommand: detectNode,
		Versions:      staticVersions(nodeLTS),
		Plan:          planNode,
	},
	{
		Slug: "python3", Name: "Python 3", Category: "runtime",
		Description:   "Python 3 解释器、pip 与 venv 虚拟环境支持",
		DetectCommand: detectPython,
		Plan:          planPython,
	},
	{
		Slug: "golang", Name: "Go", Category: "runtime",
		Description:   "Go 语言官方 tarball，安装到 /usr/local/go 并注册到 PATH，可选近期稳定版本",
		DetectCommand: detectGo,
		Versions:      fetchGoVersions,
		Plan:          planGo,
	},
	{
		Slug: "openjdk", Name: "OpenJDK", Category: "runtime",
		Description:   "Eclipse OpenJDK（完整 JDK），支持 21/17 两个 LTS 版本",
		DetectCommand: detectJava,
		Versions:      staticVersions(javaLTS),
		Plan:          planJava,
	},
	{
		Slug: "docker", Name: "Docker Engine", Category: "runtime",
		Description:   "Docker CE 引擎、containerd、buildx 与 compose 插件，使用 Docker 官方仓库",
		DetectCommand: detectDocker,
		CheckCommand:  checkDocker,
		Plan:          planDocker,
	},
	{
		Slug: "bun", Name: "Bun", Category: "runtime",
		Description:   "Bun 运行时、包管理器与打包器，官方脚本安装到 /usr/local（CentOS 7 的 glibc 过旧，不在此支持）",
		Families:      []Family{FamilyApt, FamilyDNF, FamilyAPK},
		DetectCommand: detectBun,
		CheckCommand:  checkBun,
		Plan:          planBun,
	},
	{
		Slug: "dsh", Name: "DeepSeek Harness", Category: "toolchain",
		Description:   "DeepSeek Harness（dsh）命令行与 Web 工作台：先确保 Node.js 就位，再通过 npm 全局安装",
		DetectCommand: detectDsh,
		CheckCommand:  checkDsh,
		Plan:          planDsh,
	},
	{
		Slug: "cuda", Name: "CUDA Toolkit", Category: "toolchain",
		Description:   "NVIDIA CUDA Toolkit（nvcc 等），用于 GPU 计算与编译；仅 x86_64，且不含显卡驱动",
		Families:      []Family{FamilyApt, FamilyDNF, FamilyYUM},
		DetectCommand: detectCUDA,
		CheckCommand:  checkCUDA,
		Plan:          planCUDA,
	},
	{
		Slug: "mongodb", Name: "MongoDB", Category: "database",
		Description:   "MongoDB 8.0 官方社区版（mongod），通过 MongoDB 自己的仓库安装（Alpine 仓库不提供）",
		Families:      []Family{FamilyApt, FamilyDNF, FamilyYUM},
		DetectCommand: detectMongo,
		CheckCommand:  checkMongo,
		Plan:          planMongo,
	},
	{
		Slug: "build-toolchain", Name: "构建工具链", Category: "toolchain",
		Description:   "gcc/g++/make 等基础编译工具（Debian: build-essential；RHEL 系: Development Tools；Alpine: build-base）",
		DetectCommand: detectGCC,
		Plan:          planToolchain,
	},
}, systemRecipes()...)

// Catalog 返回软件目录。
func Catalog() []*Recipe { return catalog }

// GetRecipe 按 slug 查找目录项。
func GetRecipe(slug string) *Recipe {
	for _, r := range catalog {
		if r.Slug == slug {
			return r
		}
	}
	return nil
}

// Supports 判断该 Recipe 是否支持目标家族。
func (r *Recipe) Supports(f Family) bool {
	if len(r.Families) == 0 {
		return true
	}
	return inList(string(f), toStrings(r.Families))
}

func toStrings(in []Family) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		out = append(out, string(f))
	}
	return out
}
