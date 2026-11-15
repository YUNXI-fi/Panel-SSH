package env

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// probeScript 在目标机上一次性采集系统画像。
//
// 只读、不需要特权。输出用段标记切分，避免多次 SSH 往返；
// LC_ALL=C 让后续命令的输出格式固定，不随登录 locale 变化。
//
// sudo 只探一件事：免密能不能用（`sudo -n true` 的退出码）。不去读它的报错
// 文案 —— "a password is required" 与 "not in the sudoers file" 这类句子
// 随版本、随语言变化，靠匹配它们判断"能不能提权"会把结论变成一句要看运气
// 的话；退出码不会骗人。要口令的那两条路成不成立取决于面板手里有没有口令，
// 由 markEscalation 补上。
const probeScript = `
export LC_ALL=C
echo "===OSRELEASE_BEGIN==="
if [ -r /etc/os-release ]; then cat /etc/os-release; fi
echo "===UNAME_M==="
uname -m
echo "===UNAME_R==="
uname -r
echo "===UID==="
id -u
echo "===SUDONPASS==="
if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then echo yes; else echo no; fi
echo "===TOOLS==="
for t in apt-get apt-cache dpkg dnf yum rpm apk tar base64 gpg curl wget systemctl docker sudo su; do
  if command -v "$t" >/dev/null 2>&1; then echo "$t"; fi
done
echo "===END==="
`

// ErrUnsupportedFamily 表示目标机上没有可识别的包管理器。
var ErrUnsupportedFamily = errors.New("无法识别目标系统的包管理器（仅支持 apt/dnf/yum/apk）")

// Probe 探测目标机画像。探测本身是只读操作，不需要 root 权限。
func Probe(ctx context.Context, ex Executor) (*Profile, error) {
	cmd := Wrap(probeScript, nil)
	res, err := ex.Run(ctx, cmd, ProbeTimeout)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("系统探测命令失败：%s", TailLines(res.Stderr, 5))
	}
	p, err := parseProbe(res.Stdout)
	if err != nil {
		return nil, err
	}
	markEscalation(ctx, p, ex)
	return p, nil
}

// markEscalation 标记"需要口令的提权路径"是否成立。
//
// 有口令时先用它实测一次 sudo：sudo 只对单条命令提权，比 su 换一个 root 会话
// 更可控；而且"配了口令、其实是 sudo 用户"的机器比"真的只有 root 口令"的
// 机器常见得多。sudo 通了就优先走 sudo，不通才退回 su（专用提权口令是 root
// 的口令，只有 su 认它）。
//
// 与旧版"只看路径存不存在"的区别：口令错会在探测阶段就暴露成 PrivNone，
// 而不是等到执行变更时抛一句 Authentication failure。
func markEscalation(ctx context.Context, p *Profile, ex Executor) {
	if p.Root || p.HasSudoNoPass {
		return
	}
	feeder, ok := ex.(SecretFeeder)
	if !ok {
		return
	}
	secret, dedicated, ok := feeder.EscalationSecret()
	if !ok || secret == "" {
		return
	}

	if p.Tools["sudo"] {
		works, ran := sudoAcceptsSecret(ctx, ex)
		if works {
			p.HasSudo = true
			return
		}
		if !ran && !dedicated {
			// 探测命令本身没跑完（会话抖动），沿用"路径存在即候选"的旧语义。
			p.HasSudo = true
			return
		}
	}
	if dedicated {
		p.HasSu = p.Tools["su"]
	}
}

// sudoAcceptsSecret 用执行通道里的口令实测一次 sudo。
//
// works 为真表示口令与 sudo 权限都成立；ran 为假表示命令没能执行完，
// 与"执行了但被拒绝"区分开 —— 后者是确定性结论，前者只是这次没测成。
func sudoAcceptsSecret(ctx context.Context, ex Executor) (works, ran bool) {
	res, err := ex.Run(ctx, Command{Line: "LC_ALL=C sudo -S -p '' true", Secret: true}, ProbeTimeout)
	if err != nil || res == nil {
		return false, false
	}
	return res.ExitCode == 0, true
}

// ProbeTimeout 是探测的时长上限：读 os-release 与 which 都是毫秒级操作，
// 卡住只可能是会话本身有问题。
const ProbeTimeout = 15 * time.Second

func parseProbe(output string) (*Profile, error) {
	sections := splitSections(output)
	osRelease := parseOSRelease(sections["OSRELEASE"])

	tools := map[string]bool{}
	for _, line := range strings.Split(sections["TOOLS"], "\n") {
		if t := strings.TrimSpace(line); t != "" {
			tools[t] = true
		}
	}

	family := detectFamily(osRelease["ID"], osRelease["ID_LIKE"], tools)
	if family == "" {
		return nil, ErrUnsupportedFamily
	}

	unameM := strings.TrimSpace(sections["UNAME_M"])
	arch, pkgArch := normalizeArch(unameM)

	codename := strings.TrimSpace(osRelease["VERSION_CODENAME"])
	if family == FamilyAPK && codename == "" {
		// Alpine 不提供 VERSION_CODENAME，仓库路径按 v3.20 这种主次版本组织。
		codename = alpineBranch(strings.TrimSpace(osRelease["VERSION_ID"]))
	}

	p := &Profile{
		Family:     family,
		OS:         strings.TrimSpace(osRelease["ID"]),
		PrettyName: strings.TrimSpace(osRelease["PRETTY_NAME"]),
		Version:    strings.TrimSpace(osRelease["VERSION_ID"]),
		Codename:   codename,
		Arch:       arch,
		PkgArch:    pkgArch,
		Kernel:     strings.TrimSpace(sections["UNAME_R"]),
		Tools:      tools,
	}

	p.Root = strings.TrimSpace(sections["UID"]) == "0"
	// 免密 sudo 与口令无关：探针的 `sudo -n true` 已经替我们把答案问出来了。
	p.HasSudoNoPass = p.Tools["sudo"] && strings.TrimSpace(sections["SUDONPASS"]) == "yes"
	return p, nil
}

// detectFamily 依据已安装的可执行文件判定包管理器家族，os-release 只作辅助。
//
// 以工具存在性为准：ID 可以被裁剪镜像随意改，而 apt-get/dpkg 同时存在
// 才是真的能装 deb 的系统。dnf 优先于 yum —— CentOS 8+ 上 yum 只是
// 指向 dnf 的兼容软链，命令行为并不完全相同。
func detectFamily(osID, idLike string, tools map[string]bool) Family {
	switch {
	case tools["apt-get"] && tools["dpkg"]:
		return FamilyApt
	case tools["dnf"]:
		return FamilyDNF
	case tools["yum"] && tools["rpm"]:
		return FamilyYUM
	case tools["apk"]:
		return FamilyAPK
	}

	// 工具探测异常（PATH 被裁剪）时退回发行版标识。
	// 注意必须用 Contains（子串匹配）而不是 ContainsAny（后者是字符集合匹配，
	// 会把 freebsd 误判成 debian —— 它恰好含 d、e、b、s 几个字母）。
	all := osID + " " + idLike
	switch {
	case strings.Contains(all, "alpine"):
		return FamilyAPK
	case strings.Contains(all, "debian") || strings.Contains(all, "ubuntu"):
		return FamilyApt
	case strings.Contains(all, "fedora") || strings.Contains(all, "centos") ||
		strings.Contains(all, "rhel") || strings.Contains(all, "rocky") ||
		strings.Contains(all, "alma"):
		return FamilyDNF
	}
	return ""
}

// normalizeArch 把 uname -m 的架构名归一化为 Go 风格（amd64/arm64），
// 同时保留发行版包架构名（x86_64/aarch64，写源配置时必须用这个）。
func normalizeArch(uname string) (arch, pkgArch string) {
	switch uname {
	case "x86_64":
		return "amd64", "x86_64"
	case "aarch64":
		return "arm64", "aarch64"
	case "armv7l":
		return "armv7", "armv7"
	case "riscv64":
		return "riscv64", "riscv64"
	default:
		return uname, uname
	}
}

// alpineBranch 把 "3.20.3" 归并为仓库分支 "v3.20"。
func alpineBranch(version string) string {
	parts := strings.Split(version, ".")
	if len(parts) >= 2 {
		return "v" + parts[0] + "." + parts[1]
	}
	return ""
}

// splitSections 按 ===NAME=== 标记切分探针输出。
func splitSections(output string) map[string]string {
	sections := map[string]string{}
	var cur strings.Builder
	name := ""
	flush := func() {
		if name != "" {
			sections[name] = cur.String()
		}
		cur.Reset()
	}
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "===") && strings.HasSuffix(trimmed, "===") {
			flush()
			name = strings.TrimSuffix(strings.TrimPrefix(trimmed, "==="), "===")
			// OSRELEASE_BEGIN 这类段名只是左标记，去掉 _BEGIN 后缀方便取用。
			name = strings.TrimSuffix(name, "_BEGIN")
			continue
		}
		if name != "" {
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	flush()
	return sections
}

// parseOSRelease 解析 KEY="VALUE" 形式的 os-release。
func parseOSRelease(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		out[strings.TrimSpace(key)] = val
	}
	return out
}
