package env

import (
	"context"
	"testing"
)

func TestParseProbeUbuntu(t *testing.T) {
	output := `
===OSRELEASE_BEGIN===
PRETTY_NAME="Ubuntu 22.04.4 LTS"
NAME="Ubuntu"
VERSION_ID="22.04"
VERSION_CODENAME=jammy
ID=ubuntu
ID_LIKE=debian
===UNAME_M===
x86_64
===UNAME_R===
5.15.0-119-generic
===UID===
0
===SUDONPASS===
no
===TOOLS===
apt-get
apt-cache
base64
curl
dpkg
gpg
tar
systemctl
sudo
===END===
`
	p, err := parseProbe(output)
	if err != nil {
		t.Fatalf("parseProbe 返回错误: %v", err)
	}
	if p.Family != FamilyApt {
		t.Fatalf("Family = %q, want apt", p.Family)
	}
	if p.OS != "ubuntu" || p.Codename != "jammy" || p.Version != "22.04" {
		t.Fatalf("发行版解析错误: %+v", p)
	}
	if p.Arch != "amd64" || p.PkgArch != "x86_64" {
		t.Fatalf("架构解析错误: %+v", p)
	}
	if !p.Root {
		t.Fatalf("权限解析错误: root=%v", p.Root)
	}
	if !p.Tools["curl"] || p.Tools["apk"] {
		t.Fatalf("工具集解析错误: %+v", p.Tools)
	}
	// 装了 sudo 但免密不可用：解析阶段如实记 false，不靠猜。
	if p.HasSudoNoPass {
		t.Fatalf("SUDONPASS=no 时不该判定为免密：%+v", p)
	}
}

func TestParseProbeAlpineArm(t *testing.T) {
	output := `
===OSRELEASE_BEGIN===
PRETTY_NAME="Alpine Linux v3.20"
ID=alpine
VERSION_ID=3.20.3
===UNAME_M===
aarch64
===UNAME_R===
6.6.34-0-rpi
===UID===
1000
===SUDONPASS===
no
===TOOLS===
apk
base64
tar
===END===
`
	p, err := parseProbe(output)
	if err != nil {
		t.Fatalf("parseProbe 返回错误: %v", err)
	}
	if p.Family != FamilyAPK {
		t.Fatalf("Family = %q, want apk", p.Family)
	}
	if p.Codename != "v3.20" {
		t.Fatalf("Alpine 分支 = %q, want v3.20", p.Codename)
	}
	if p.Arch != "arm64" || p.PkgArch != "aarch64" {
		t.Fatalf("架构解析错误: %+v", p)
	}
	// 解析阶段只认 UID、免密探测与工具集：那两条要口令的路径成不成立取决于
	// 通道手里有没有口令，由 markEscalation 补上，因此这里非 root 就该是 PrivNone。
	if p.Root || p.HasSu || p.HasSudo || p.PrivilegeMode() != PrivNone {
		t.Fatalf("权限解析错误: %+v", p)
	}
}

func TestParseProbeUnsupported(t *testing.T) {
	output := `
===OSRELEASE_BEGIN===
ID=freebsd
===UNAME_M===
amd64
===UID===
0
===TOOLS===
base64
tar
===END===
`
	if _, err := parseProbe(output); err == nil {
		t.Fatal("不支持的系统应返回错误")
	}
}

// 免密 sudo 是探针唯一能独立确证的提权路径：`sudo -n true` 的退出码说了算，
// 与面板手里有没有口令无关，因此它在解析阶段就定下来。
func TestParseProbeSudoNoPass(t *testing.T) {
	output := `
===OSRELEASE_BEGIN===
PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
ID=debian
VERSION_ID="12"
VERSION_CODENAME=bookworm
===UNAME_M===
x86_64
===UNAME_R===
6.1.0-13-amd64
===UID===
1000
===SUDONPASS===
yes
===TOOLS===
apt-get
apt-cache
dpkg
sudo
su
===END===
`
	p, err := parseProbe(output)
	if err != nil {
		t.Fatalf("parseProbe 返回错误: %v", err)
	}
	if !p.HasSudoNoPass {
		t.Fatalf("SUDONPASS=yes 且装了 sudo 时应判定为免密：%+v", p)
	}
	// 免密优先于"要口令的 sudo"：能不问就不问。
	if p.PrivilegeMode() != PrivSudoNoPass {
		t.Fatalf("免密机器应走 sudo-nopasswd，实际 %s：%+v", p.PrivilegeMode(), p)
	}
}

func TestWrapEncoding(t *testing.T) {
	// 脚本含单双引号与美元符号，包装后不得被外层 shell 改写。
	script := `echo 'a"b$c'; rm -rf /tmp/x`

	plain := Wrap(script, nil)
	if !contains(plain.Line, "| sh") || plain.Secret || plain.TTY {
		t.Fatalf("无画像时应直接管道给 sh：%+v", plain)
	}

	root := Wrap(script, &Profile{Family: FamilyApt, Root: true})
	if !contains(root.Line, "| sh") || contains(root.Line, "su ") || root.Secret || root.TTY {
		t.Fatalf("已是 root 时不该再套一层提权：%+v", root)
	}

	// 没有可用提权路径时也只是普通执行，调用方自行判断该不该往下跑。
	none := Wrap(script, &Profile{Family: FamilyApt})
	if !contains(none.Line, "| sh") || none.Secret || none.TTY {
		t.Fatalf("无可提权路径时应退化为普通执行：%+v", none)
	}

	// su：必须申请伪终端（su 从 /dev/tty 读口令），脚本由 su -c 吃进去。
	suCmd := Wrap(script, &Profile{Family: FamilyApt, HasSu: true})
	if !contains(suCmd.Line, "su -c '") || !suCmd.Secret || !suCmd.TTY {
		t.Fatalf("su 场景的命令行不正确：%+v", suCmd)
	}

	// 免密 sudo：脚本照旧走管道，但 -n 必须在，让"免密其实已失效"当场报错。
	noPass := Wrap(script, &Profile{Family: FamilyApt, HasSudoNoPass: true})
	if !contains(noPass.Line, "sudo -n sh") || noPass.Secret || noPass.TTY {
		t.Fatalf("免密 sudo 场景的命令行不正确：%+v", noPass)
	}

	// 要口令的 sudo：-S 独占 stdin（因此不能申请伪终端），脚本只能从
	// sh -c 的参数进去，否则会被口令那行输入顶掉。
	sudoCmd := Wrap(script, &Profile{Family: FamilyApt, HasSudo: true})
	if !contains(sudoCmd.Line, "sudo -S -p '' sh -c '") || !sudoCmd.Secret || sudoCmd.TTY {
		t.Fatalf("要口令的 sudo 场景的命令行不正确：%+v", sudoCmd)
	}
}

// 端到端串一遍：探针原文 → 解析 → 选路。通道手里是主机上另配的「提权口令」
// （root 的口令），目标机装了 su；这份口令过不了 sudo，就必须退回 su。
func TestProbeSelectsSuWhenDedicatedSecret(t *testing.T) {
	raw := `
===OSRELEASE_BEGIN===
PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
ID=debian
VERSION_ID="12"
VERSION_CODENAME=bookworm
===UNAME_M===
x86_64
===UNAME_R===
6.1.0-13-amd64
===UID===
1000
===SUDONPASS===
no
===TOOLS===
apt-get
apt-cache
dpkg
sudo
su
===END===
`
	p, err := Probe(context.Background(), &stubExecutor{stdout: raw, secret: "rootpw", dedicated: true, sudoDenied: true})
	if err != nil {
		t.Fatalf("Probe 返回错误: %v", err)
	}
	if p.PrivilegeMode() != PrivSu {
		t.Fatalf("口令过不了 sudo 且装了 su 时应走 su，实际 %s：%+v", p.PrivilegeMode(), p)
	}
	cmd := Wrap("echo hi", p)
	if !contains(cmd.Line, "su -c '") || !cmd.TTY || !cmd.Secret {
		t.Fatalf("su 路径必须带伪终端并投喂口令：%+v", cmd)
	}
}

// 通道手里只有登录凭据时，唯一走得通的是 sudo —— sudo 认的正是执行者
// 自己的口令。这正是 su 那条路在真机上失败的原因（su 认 root 的口令）。
func TestProbeSelectsSudoWhenLoginSecret(t *testing.T) {
	raw := `
===OSRELEASE_BEGIN===
PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
ID=debian
VERSION_ID="12"
VERSION_CODENAME=bookworm
===UNAME_M===
x86_64
===UNAME_R===
6.1.0-13-amd64
===UID===
1000
===SUDONPASS===
no
===TOOLS===
apt-get
apt-cache
dpkg
sudo
su
===END===
`
	p, err := Probe(context.Background(), &stubExecutor{stdout: raw, secret: "loginpw"})
	if err != nil {
		t.Fatalf("Probe 返回错误: %v", err)
	}
	if p.PrivilegeMode() != PrivSudo {
		t.Fatalf("只有登录凭据时应走 sudo，实际 %s：%+v", p.PrivilegeMode(), p)
	}
	cmd := Wrap("echo hi", p)
	if !contains(cmd.Line, "sudo -S -p '' sh -c '") || !cmd.Secret || cmd.TTY {
		t.Fatalf("要口令的 sudo 必须独占 stdin 投喂口令：%+v", cmd)
	}
}

func TestMarkEscalation(t *testing.T) {
	ctx := context.Background()
	both := map[string]bool{"su": true, "sudo": true}

	// 口令能过 sudo 时优先走 sudo：即使配的是专用提权口令（root 口令）也一样。
	preferSudo := &Profile{Tools: both}
	markEscalation(ctx, preferSudo, &stubExecutor{secret: "rootpw", dedicated: true})
	if preferSudo.HasSu || !preferSudo.HasSudo || preferSudo.PrivilegeMode() != PrivSudo {
		t.Fatalf("口令能通过 sudo 时应优先走 sudo：%+v", preferSudo)
	}

	// sudo 拒绝 + 专用提权口令：那是 root 的口令，只有 su 认它。
	su := &Profile{Tools: both}
	markEscalation(ctx, su, &stubExecutor{secret: "rootpw", dedicated: true, sudoDenied: true})
	if !su.HasSu || su.HasSudo || su.PrivilegeMode() != PrivSu {
		t.Fatalf("sudo 不通且是专用口令时应退回 su：%+v", su)
	}

	// 退回的登录凭据是执行者自己的口令，sudo 实测通过就走 sudo。
	sudo := &Profile{Tools: both}
	markEscalation(ctx, sudo, &stubExecutor{secret: "loginpw"})
	if sudo.HasSu || !sudo.HasSudo || sudo.PrivilegeMode() != PrivSudo {
		t.Fatalf("登录凭据能过 sudo 时应判定为走 sudo：%+v", sudo)
	}

	// 登录凭据过不了 sudo：口令是错的，别再谎称能提权。
	badSecret := &Profile{Tools: both}
	markEscalation(ctx, badSecret, &stubExecutor{secret: "loginpw", sudoDenied: true})
	if badSecret.CanPrivileged() {
		t.Fatalf("口令被 sudo 拒绝时不该判定为可提权：%+v", badSecret)
	}

	// 通道拿不到任何口令 → 两条要口令的路径都不成立，界面要按只读处理。
	noSecret := &Profile{Tools: both}
	markEscalation(ctx, noSecret, &stubExecutor{})
	if noSecret.CanPrivileged() {
		t.Fatalf("没有可用口令时不应判定为可提权：%+v", noSecret)
	}

	// 目标机上没有对应的提权命令，有口令也没用。
	noTools := &Profile{Tools: map[string]bool{"apt-get": true}}
	markEscalation(ctx, noTools, &stubExecutor{secret: "pw", dedicated: true})
	if noTools.CanPrivileged() {
		t.Fatalf("没有 su/sudo 时不该判定为可提权：%+v", noTools)
	}

	// 有 su 没 sudo 且只有登录凭据：su 不认这份口令，sudo 又不存在。
	suOnly := &Profile{Tools: map[string]bool{"su": true}}
	markEscalation(ctx, suOnly, &stubExecutor{secret: "loginpw"})
	if suOnly.CanPrivileged() {
		t.Fatalf("只有 su 而口令是登录凭据时不该判定为可提权：%+v", suOnly)
	}

	// 本机执行器压根不实现 SecretFeeder：面板没有宿主机的提权口令可用。
	local := &Profile{Tools: both}
	markEscalation(ctx, local, NewLocalExecutor())
	if local.CanPrivileged() {
		t.Fatalf("本机通道不应判定为可用口令提权：%+v", local)
	}

	// 已经是 root：提权路径没有意义，不该被标记。
	root := &Profile{Root: true, Tools: both}
	markEscalation(ctx, root, &stubExecutor{secret: "rootpw", dedicated: true})
	if root.HasSu || root.HasSudo {
		t.Fatalf("root 登录时不该再标提权路径：%+v", root)
	}

	// 免密 sudo 已经可用时不必再测口令。
	noPass := &Profile{Tools: both, HasSudoNoPass: true}
	markEscalation(ctx, noPass, &stubExecutor{secret: "pw", dedicated: true, sudoDenied: true})
	if noPass.HasSu || noPass.HasSudo || noPass.PrivilegeMode() != PrivSudoNoPass {
		t.Fatalf("免密 sudo 可用时不该被 su 覆盖：%+v", noPass)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}