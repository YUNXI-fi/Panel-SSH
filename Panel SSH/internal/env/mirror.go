package env

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// MirrorInfo 是可选安装镜像站。
type MirrorInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// 镜像站标识。
const (
	MirrorTuna   = "tuna"
	MirrorAliyun = "aliyun"
)

// ListMirrors 返回支持的镜像站。
func ListMirrors() []MirrorInfo {
	return []MirrorInfo{
		{ID: MirrorTuna, Name: "清华大学 TUNA"},
		{ID: MirrorAliyun, Name: "阿里云"},
	}
}

// mirrorHosts 各镜像站在不同发行版上的 URL 前缀。
// Rocky/Alma 的目录命名两家不一致，必须分别列。
func mirrorHosts(id string) map[string]string {
	switch id {
	case MirrorTuna:
		return map[string]string{
			"debian":        "https://mirrors.tuna.tsinghua.edu.cn",
			"ubuntu":        "https://mirrors.tuna.tsinghua.edu.cn",
			"rocky":         "https://mirrors.tuna.tsinghua.edu.cn/rocky",
			"alma":          "https://mirrors.tuna.tsinghua.edu.cn/almalinux",
			"centos-stream": "https://mirrors.tuna.tsinghua.edu.cn/centos-stream",
			"centos-vault":  "https://mirrors.tuna.tsinghua.edu.cn/centos-vault",
			"fedora":        "https://mirrors.tuna.tsinghua.edu.cn/fedora",
			"alpine":        "https://mirrors.tuna.tsinghua.edu.cn/alpine",
		}
	case MirrorAliyun:
		return map[string]string{
			"debian":        "https://mirrors.aliyun.com",
			"ubuntu":        "https://mirrors.aliyun.com",
			"rocky":         "https://mirrors.aliyun.com/rockylinux",
			"alma":          "https://mirrors.aliyun.com/almalinux",
			"centos-stream": "https://mirrors.aliyun.com/centos-stream",
			"centos-vault":  "https://mirrors.aliyun.com/centos-vault",
			"fedora":        "https://mirrors.aliyun.com/fedora",
			"alpine":        "https://mirrors.aliyun.com/alpine",
		}
	}
	return nil
}

// MirrorPlan 是换源计划。
type MirrorPlan struct {
	Steps   []Step `json:"steps"`
	Preview string `json:"preview"`
}

// MirrorSteps 生成切换镜像站的步骤。
//
// 系统源文件的原始内容在执行前由统一快照流程备份，回滚即恢复快照，
// 因此这里可以直接重写官方源；第三方源（sources.list.d 里的 docker、
// nodesource 等）刻意不动。
func MirrorSteps(p *Profile, mirrorID string) (*MirrorPlan, error) {
	hosts := mirrorHosts(mirrorID)
	if hosts == nil {
		return nil, fmt.Errorf("%w：%s", ErrUnknownMirror, mirrorID)
	}
	mirrorName := map[string]string{MirrorTuna: "清华 TUNA", MirrorAliyun: "阿里云"}[mirrorID]

	var steps []Step
	var preview string
	refresh := sourceRefreshStep(p)

	switch p.Family {
	case FamilyApt:
		if p.OS != "ubuntu" && p.OS != "debian" {
			return nil, fmt.Errorf("一键换源暂仅支持 Ubuntu/Debian（当前发行版：%s），可手工编辑源文件", p.OS)
		}
		content := aptSourceContent(p.OS, p.Codename, hosts[p.OS])
		steps = []Step{
			{Name: "停用新版 deb822 格式系统源（如有）", Command: aptDisableDeb822, Critical: false},
			{Name: "重写 /etc/apt/sources.list 为" + mirrorName + "镜像", Command: "set -e\ncat > /etc/apt/sources.list <<'PANELSSH_EOF'\n" + content + "PANELSSH_EOF\n", Critical: true},
			refresh,
		}
		preview = fmt.Sprintf("将把 %s（%s）的官方软件源替换为%s镜像，共 4 个组件；第三方源不受影响，可随时回滚。",
			p.PrettyName, p.Codename, mirrorName)
	case FamilyDNF, FamilyYUM:
		switch {
		case p.OS == "rocky":
			steps = sedRepoSteps("rocky*.repo",
				`s|^mirrorlist=|#mirrorlist=|g`,
				`s|^#baseurl=http://dl.rockylinux.org/\$contentdir|baseurl=`+hosts["rocky"]+`|g`)
		case p.OS == "alma":
			steps = sedRepoSteps("almalinux*.repo",
				`s|^mirrorlist=|#mirrorlist=|g`,
				`s|^# *baseurl=https://repo.almalinux.org|baseurl=`+hosts["alma"]+`|g`,
				`s|^baseurl=https://repo.almalinux.org|baseurl=`+hosts["alma"]+`|g`)
		case p.OS == "centos" && majorVersion(p.Version) != "7":
			steps = sedRepoSteps("centos-*.repo",
				`s|^mirrorlist=|#mirrorlist=|g`,
				`s|^#baseurl=http://mirror.centos.org|baseurl=`+hosts["centos-stream"]+`|g`)
		case p.OS == "centos" && majorVersion(p.Version) == "7":
			steps = []Step{{
				Name:     "重写 CentOS 7 源为 vault 镜像（官方源已下线）",
				Command:  "set -e\ncat > /etc/yum.repos.d/CentOS-Base.repo <<'PANELSSH_EOF'\n" + centos7VaultContent(hosts["centos-vault"]) + "PANELSSH_EOF\n",
				Critical: true,
			}}
		case p.OS == "fedora":
			steps = sedRepoSteps("fedora*.repo",
				`s|^metalink=|#metalink=|g`,
				`s|^#baseurl=http://download.example/pub/fedora/linux|baseurl=`+hosts["fedora"]+`|g`)
		default:
			return nil, fmt.Errorf("一键换源暂不支持该发行版：%s，可手工编辑 /etc/yum.repos.d", p.OS)
		}
		steps = append(steps, refresh)
		preview = fmt.Sprintf("将把 %s 的官方仓库地址替换为%s镜像并刷新缓存；第三方仓库不受影响，可随时回滚。",
			p.PrettyName, mirrorName)
	case FamilyAPK:
		// 同时替换 http/https 两种写法，不依赖 sed 对问号量词的支持。
		steps = []Step{{
			Name:     "替换 /etc/apk/repositories 为" + mirrorName + "镜像",
			Command:  "set -e\nsed -i -e 's|http://dl-cdn.alpinelinux.org/alpine|" + hosts["alpine"] + "|g' -e 's|https://dl-cdn.alpinelinux.org/alpine|" + hosts["alpine"] + "|g' /etc/apk/repositories",
			Critical: true,
		}, refresh}
		preview = fmt.Sprintf("将把 Alpine %s 的仓库地址替换为%s镜像并刷新索引，可随时回滚。", p.Codename, mirrorName)
	}
	return &MirrorPlan{Steps: steps, Preview: preview}, nil
}

// aptDisableDeb822 把 Ubuntu 24/Debian 12 的新版 .sources 源改名停用，
// 避免与重写后的 sources.list 双份生效（apt 对重复源虽去重，但调试时
// 两套配置并存极易误判哪个生效）。改名而非删除，快照也另存了一份。
const aptDisableDeb822 = `
for f in /etc/apt/sources.list.d/ubuntu.sources /etc/apt/sources.list.d/debian.sources; do
  [ -f "$f" ] && mv "$f" "$f.panelssh-bak" || true
done
`

func aptSourceContent(osID, codename, host string) string {
	switch osID {
	case "ubuntu":
		components := "main restricted universe multiverse"
		return fmt.Sprintf(`deb %s/ubuntu/ %s %s
deb %s/ubuntu/ %s-updates %s
deb %s/ubuntu/ %s-backports %s
deb %s/ubuntu/ %s-security %s
`, host, codename, components,
			host, codename, components,
			host, codename, components,
			host, codename, components)
	default: // debian
		components := "main contrib non-free non-free-firmware"
		return fmt.Sprintf(`deb %s/debian/ %s %s
deb %s/debian/ %s-updates %s
deb %s/debian/ %s-backports %s
deb %s/debian-security/ %s-security %s
`, host, codename, components,
			host, codename, components,
			host, codename, components,
			host, codename, components)
	}
}

func centos7VaultContent(host string) string {
	return fmt.Sprintf(`[base]
name=CentOS-7.9.2009 - Base
baseurl=%s/7.9.2009/os/$basearch/
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-7

[updates]
name=CentOS-7.9.2009 - Updates
baseurl=%s/7.9.2009/updates/$basearch/
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-7

[extras]
name=CentOS-7.9.2009 - Extras
baseurl=%s/7.9.2009/extras/$basearch/
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-7
`, host, host, host)
}

// sedRepoSteps 生成对 /etc/yum.repos.d 下文件批量 sed 的步骤。
func sedRepoSteps(glob string, expressions ...string) []Step {
	args := make([]string, 0, len(expressions))
	for _, e := range expressions {
		args = append(args, "-e '"+e+"'")
	}
	cmd := "set -e\nfor f in /etc/yum.repos.d/" + glob + "; do\n  [ -f \"$f\" ] && sed -i " +
		strings.Join(args, " ") + " \"$f\"\ndone"
	return []Step{{Name: "改写官方仓库地址为镜像站", Command: cmd, Critical: true}}
}

// sourceRefreshStep 生成换源后的索引刷新步骤；刷新失败即整步失败，
// 因为"换了一个用不了的源"不能算成功（编排器会据此回滚快照）。
func sourceRefreshStep(p *Profile) Step {
	switch p.Family {
	case FamilyApt:
		return Step{Name: "刷新软件包索引（apt-get update）", Command: aptEnv + "\napt-get update -qq", Critical: true}
	case FamilyDNF:
		return Step{Name: "刷新软件包缓存（dnf makecache）", Command: "export LC_ALL=C\ndnf makecache -y", Critical: true}
	case FamilyYUM:
		return Step{Name: "刷新软件包缓存（yum makecache）", Command: "export LC_ALL=C\nyum makecache fast -y || yum makecache -y", Critical: true}
	case FamilyAPK:
		return Step{Name: "刷新软件包索引（apk update）", Command: "export LC_ALL=C\napk update", Critical: true}
	}
	return Step{}
}

// SourceFile 是目标机上的一个源配置文件。
type SourceFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// maxSourceBytes 限制源文件回读总量。源配置都是小文本，256KiB 足够；
// 防止异常机器上 sources.list.d 被塞大文件把面板内存吃掉。
const maxSourceBytes = 256 * 1024

// readSourcesScript 回读包管理器的源配置，用 ===FILE:path=== 标记分块。
func readSourcesScript(p *Profile) string {
	paths := []string{}
	for _, sp := range NewManager(p).SourcePaths() {
		paths = append(paths, "'/"+sp+"'")
	}
	// 目录则展开一层普通文件；符号链接与不存在的条目跳过。
	return `
export LC_ALL=C
for d in ` + strings.Join(paths, " ") + `; do
  if [ -d "$d" ]; then
    find "$d" -maxdepth 1 -type f 2>/dev/null | sort
  elif [ -f "$d" ]; then
    echo "$d"
  fi
done | while IFS= read -r f; do
  echo "===FILE:$f==="
  cat "$f" 2>/dev/null
  echo
done
echo "===END==="
`
}

// ReadSources 读取目标机当前的安装源配置。
func ReadSources(ctx context.Context, ex Executor, p *Profile) ([]SourceFile, error) {
	out, err := runQuery(ctx, ex, readSourcesScript(p), queryTimeout)
	if err != nil {
		return nil, err
	}
	files := make([]SourceFile, 0)
	var cur *SourceFile
	used := 0
	flush := func() {
		if cur != nil {
			files = append(files, *cur)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "===FILE:") {
			flush()
			cur = &SourceFile{Path: strings.TrimSuffix(strings.TrimPrefix(line, "===FILE:"), "===")}
			continue
		}
		if line == "===END===" {
			break
		}
		if cur != nil {
			used += len(line) + 1
			if used <= maxSourceBytes {
				cur.Content += line + "\n"
			}
		}
	}
	flush()
	for i := range files {
		files[i].Path = path.Clean(files[i].Path)
	}
	return files, nil
}
