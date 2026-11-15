// Package env 实现系统环境管理：跨主机（远程 SSH 或本机）的软件包查询、
// 安装/升级/卸载的模拟预检、安装源管理、操作快照与补偿式回滚。
//
// 设计要点：
//
//   - 目标机差异收敛在 PackageManager（apt/dnf/yum/apk）与 Profile 两处，
//     编排逻辑不感知发行版；
//   - 所有变更先经包管理器自带的求解器模拟（apt-get -s / dnf --assumeno /
//     apk -s），面板不自研依赖求解：冲突在执行前暴露给用户确认；
//   - 远程包管理不存在原子事务，因此采用"变更前快照 → 执行 → 失败补偿 →
//     版本复核 → 事后按快照回退"的尽力而为模型，回滚的可达性在界面上如实标注。
package env

// Family 是包管理器家族。
type Family string

const (
	FamilyApt Family = "apt"
	FamilyDNF Family = "dnf"
	FamilyYUM Family = "yum"
	FamilyAPK Family = "apk"
)

// Profile 是目标主机的系统画像。
type Profile struct {
	Family     Family `json:"family"`
	OS         string `json:"os"`         // os-release 的 ID：ubuntu/debian/rocky/alpine...
	PrettyName string `json:"prettyName"` // PRETTY_NAME
	Version    string `json:"version"`    // VERSION_ID
	Codename   string `json:"codename"`   // VERSION_CODENAME（apk 下为 v3.xx）
	// Arch 是归一化后的架构（amd64/arm64/...），与 Go tarball 命名一致；
	// PkgArch 是 RPM 与预编译二进制使用的包架构（x86_64/aarch64/...）。
	// 注意 apt 源要写 Debian 架构名，必须用 Arch —— 写成 x86_64 时 apt 不会报错，
	// 只会在求解阶段说"没有可安装候选"。
	Arch    string `json:"arch"`
	PkgArch string `json:"pkgArch"`
	Root    bool   `json:"root"`
	// HasSu 为真表示目标机上有 su、且主机上单独配了「提权口令」（root 口令）：
	// su 认的就是这份口令，两者缺一不可。
	HasSu bool `json:"hasSu"`
	// HasSudoNoPass 为真表示这个账号在这台机器上能免密 sudo（由探针实测得出）。
	// 它与口令无关，因此在"有没有口令"之外单独成立。
	HasSudoNoPass bool `json:"hasSudoNoPass"`
	// HasSudo 为真表示目标机上有 sudo、且执行通道拿得出登录凭据：
	// sudo 要的是执行者自己的口令，正好是登录口令 —— 而它只有密码认证下才有。
	HasSudo bool   `json:"hasSudo"`
	Kernel  string `json:"kernel"`
	// Tools 记录探测到的常用可执行文件，Recipe 据此决定是否需要先装 curl/tar。
	Tools map[string]bool `json:"tools,omitempty"`
}

// CanPrivileged 判断能否以 root 身份执行变更命令。
//
// 三条路径：已以 root 登录；用 su 换一个 root 会话（需要主机上另配的提权口令）；
// 用 sudo 提权（免密，或投喂密码认证的登录凭据）。口令的来源见
// Server.hostEscalationSecret。
//
// 代价是后两条都只是候选判断：口令对不对要等真跑一次才知道。因此探测阶段只确认
// 路径存在，口令不对的情况由执行步骤如实报错，而不是在探测阶段假装确定。
func (p *Profile) CanPrivileged() bool { return p.PrivilegeMode() != PrivNone }

// PrivilegeMode 是面板在目标机上提权所用的方式。
type PrivilegeMode string

const (
	// PrivNone 表示没有任何可用路径，只能只读。
	PrivNone PrivilegeMode = "none"
	// PrivRoot 表示已以 root 身份登录。
	PrivRoot PrivilegeMode = "root"
	// PrivSu 表示用 su 换一个 root 会话，口令由主机上另配的提权口令回答。
	PrivSu PrivilegeMode = "su"
	// PrivSudoNoPass 表示这个账号在这台机器上能免密 sudo，不需要任何口令。
	PrivSudoNoPass PrivilegeMode = "sudo-nopasswd"
	// PrivSudo 表示用 sudo 提权，口令投喂密码认证的登录凭据。
	PrivSudo PrivilegeMode = "sudo-password"
)

// PrivilegeMode 返回提权会走哪条路径，供 Wrap 与界面共用同一份判断。
//
// 顺序即优先级：已经是 root 就不必提权；有专用提权口令时走 su —— 那本来就是
// root 的口令，只有 su 认它；否则用 sudo，免密优先，其次投喂登录口令。
//
// 判据全部是"路径存在"这类可确证的事实：免密与否由 `sudo -n true` 的退出码
// 回答，而不是去读 sudo 的报错文案 —— 文案随版本与语言变化，退出码不会。
func (p *Profile) PrivilegeMode() PrivilegeMode {
	switch {
	case p.Root:
		return PrivRoot
	case p.HasSu:
		return PrivSu
	case p.HasSudoNoPass:
		return PrivSudoNoPass
	case p.HasSudo:
		return PrivSudo
	}
	return PrivNone
}

// Package 是一个已安装包。
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
}

// Upgrade 是一个可升级包。
type Upgrade struct {
	Name      string `json:"name"`
	Arch      string `json:"arch"`
	Current   string `json:"current"`
	Candidate string `json:"candidate"`
	Repo      string `json:"repo"`
}

// Candidate 是某个包的一个可安装版本。
type Candidate struct {
	Version string `json:"version"`
	Repo    string `json:"repo"`
}

// Spec 是一次包操作的目标。Version 为空表示使用仓库候选版本。
type Spec struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ChangeItem 是预检得出的一个变更项。
type ChangeItem struct {
	Name string `json:"name"`
	Arch string `json:"arch,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// SimReport 是包管理器求解器给出的模拟结果。
type SimReport struct {
	OK        bool         `json:"ok"`
	Install   []ChangeItem `json:"install"`
	Upgrade   []ChangeItem `json:"upgrade"`
	Downgrade []ChangeItem `json:"downgrade"`
	Remove    []ChangeItem `json:"remove"`
	// Conflicts 是求解器输出的冲突/缺包等阻断性信息原文（逐行）。
	Conflicts []string `json:"conflicts"`
	// DownloadKB 是预计下载量（解析得到时非 0）。
	DownloadKB int64  `json:"downloadKB"`
	Note       string `json:"note,omitempty"`
}

// ChangeCount 返回变更包总数，供前端折叠摘要用。
func (r *SimReport) ChangeCount() int {
	return len(r.Install) + len(r.Upgrade) + len(r.Downgrade) + len(r.Remove)
}

// Step 是执行计划中的一个 shell 步骤。
//
// Critical 为假时该步骤失败不触发补偿与中止（典型：无 systemd 环境下
// 尝试 enable 服务），只记录警告。
type Step struct {
	Name     string `json:"name"`
	Command  string `json:"-"`
	Critical bool   `json:"critical"`
}

// 动作名，存储层与 API 共用。
const (
	ActionInstall    = "install"
	ActionUpgrade    = "upgrade"
	ActionRemove     = "remove"
	ActionUpgradeAll = "upgrade-all"
	ActionMirror     = "mirror"
	ActionRollback   = "rollback"
)
