package env

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// 本文件回答"这台机器上还有哪些装好了、但当前 shell 用不到的命令行环境"。
//
// 场景：飞牛这类 NAS 把应用各自装在独立目录里（/var/apps/<app>/target/bin），
// 而面板与包管理器装的东西在 /usr/bin。用户在终端里敲 node 说"命令找不到"，
// 可机器上其实装着 —— 缺的只是 PATH 里的一条路径。
//
// 口径：**只读，不改任何东西**。扫描只回答三件事：目录在哪、里面有哪些可执行文件、
// 这个目录当前是否已经在登录 shell 的 PATH 里；以及一个必须提前给出来的风险 ——
// 把它加进 PATH 会遮住哪些现有命令。改 PATH 是另一个动作（见 vars_path.go），
// 必须让用户看着这些信息自己决定。
//
// 为什么不用"把每个 bin 目录里的可执行文件全软链到 /usr/local/bin"那种做法
// （参考脚本 path_tool.sh 就是如此）：那是拿应用目录的 node/python 去抢系统同名
// 命令，系统包一升级、应用一卸载就留下满地的坏软链，而且从 PATH 里看不出来
// 什么被谁顶掉了。面板只加 PATH 条目 —— 它可逆、可审计、看得见。

// 采集上限。必须有：执行通道单侧只保留 64KB（见 MaxStepOutputBytes），
// 而这里每个命令要输出一行（名字 + 它当前解析到哪）。
const (
	maxScanCommandsPerDir = 30
	maxScanCommandsTotal  = 240
	maxScanDirs           = 80
)

// scanRoot 是一个内置的扫描位置。
type scanRoot struct {
	// pattern 由目标机的 shell 展开（家目录写成 $HOME，由目标机自己回答）。
	pattern string
	// source 是机器可读的来源标识；中文标签由面板侧映射（scanSourceText），
	// 脚本里不出现非 ASCII 文案。
	source string
	// limit 限制一个模式最多展开出几个环境，避免 /opt/*/bin 这类位置
	// 在某台机器上展开出几百个条目把输出撑爆。
	limit int
}

// scanRoots 是内置的约定位置，按"用户最可能想加进 PATH"的顺序排列。
//
// 都是同一类东西：装好了、但不在默认 PATH 里。单个目录（家目录下那几个）
// limit 为 1 —— 它们不需要展开。
var scanRoots = []scanRoot{
	{pattern: "/var/apps/*/target/bin", source: "fnos", limit: 60},
	{pattern: "/opt/*/bin", source: "opt", limit: 40},
	{pattern: "/usr/local/*/bin", source: "usrlocal", limit: 40},
	{pattern: "$HOME/.local/bin", source: "user", limit: 1},
	{pattern: "$HOME/go/bin", source: "go", limit: 1},
	{pattern: "$HOME/.cargo/bin", source: "cargo", limit: 1},
	{pattern: "$HOME/.local/go/bin", source: "golang", limit: 1},
}

// scanSourceText 把来源标识翻成界面上的说法。
var scanSourceText = map[string]string{
	"fnos":     "飞牛应用",
	"opt":      "可选软件包",
	"usrlocal": "本地编译安装",
	"user":     "用户本地",
	"go":       "Go 工具",
	"cargo":    "Rust 工具",
	"golang":   "Go 官方安装",
}

// scanTimeout 要覆盖一次登录 shell 的启动（取登录 PATH）与全部目录遍历。
const scanTimeout = 20 * time.Second

// ScanCommand 是扫描到的一个可执行文件。
type ScanCommand struct {
	Name string `json:"name"`
	// Resolved 是这个名字在当前登录 PATH 下解析到的绝对路径；
	// 空串表示现在根本用不到它（这正是"为什么敲了没反应"的答案）。
	Resolved string `json:"resolved,omitempty"`
}

// ScanDir 是一个扫描到的环境目录。
type ScanDir struct {
	Path string `json:"path"`
	// Source/Label 是来源标识与展示名。
	Source string `json:"source"`
	Label  string `json:"label"`
	// Commands 是目录里的可执行文件名（截断到上限），Total 是实际总数。
	Commands []ScanCommand `json:"commands"`
	Total    int           `json:"total"`
	// Denied 为真表示目录存在但没有读权限，列不出内容 —— 与"目录不存在"
	// 是两件事，不能让用户以为是没装。
	Denied bool `json:"denied,omitempty"`
	// InPath 为真表示这个目录已经在登录 shell 的 PATH 里了。
	InPath bool `json:"inPath"`
	// Shadows 是"把它加进 PATH 会被遮住的现有命令"：当前解析到别处的同名命令。
	// 空表示加进去只增加、不改变任何现有命令。这一条必须在动手之前给出来 ——
	// 用户想加的是某个应用的 node，但如果它也会遮住系统的 python，
	// 等发现时已经是"某个脚本突然不工作了"。
	Shadows []string `json:"shadows,omitempty"`
}

// ScanView 是一次扫描的结果。
type ScanView struct {
	Home string `json:"home"`
	// PathDirs 是当前登录 shell 的 PATH 目录列表（界面用来展示"现在有什么"）。
	PathDirs []string  `json:"pathDirs"`
	Dirs     []ScanDir `json:"dirs"`
}

// ScanEnv 扫描目标机上内置约定位置里的命令行环境。只读，不需要提权。
func ScanEnv(ctx context.Context, ex Executor) (*ScanView, error) {
	out, err := runQuery(ctx, ex, scanScript(), scanTimeout)
	if err != nil {
		return nil, err
	}
	return parseScan(out), nil
}

// scanScript 生成采集脚本。
//
// 由 Go 拼出来而不是写成常量：扫描位置的清单只有一份定义（scanRoots），
// 写成常量就得在脚本里再抄一遍，加一个位置忘一处。
func scanScript() string {
	var b strings.Builder
	b.WriteString(`export LC_ALL=C
LOGIN_PATH=''
if command -v bash >/dev/null 2>&1; then
  LOGIN_PATH=$(bash -lic 'printf %s "$PATH"' 2>/dev/null)
fi
[ -n "$LOGIN_PATH" ] || LOGIN_PATH="$PATH"
# 后面的 command -v 必须按"登录 shell 的 PATH"解析：判据是"用户在那个终端里
# 用不用得到这个命令"，而面板自己的 SSH 会话 PATH 会答错 —— 它缺 ~/.bashrc
# 里加的那些目录，会把用户明明能用的命令报成"用不到"。
PATH="$LOGIN_PATH"
export PATH
echo '===HOME==='
printf '%s\n' "${HOME:-}"
echo '===PATH==='
printf '%s\n' "$LOGIN_PATH"
echo '===DIRS==='
DIRS=0
TOTALCMDS=0
emit_dir() {
  D=$1
  S=$2
  [ -d "$D" ] || return 0
  if [ ! -r "$D" ]; then
    printf 'DENIED\t%s\t%s\n' "$D" "$S"
    return 0
  fi
  N=0
  T=0
  for f in "$D"/*; do
    # -f 跟随软链：指向文件的软链算（命令通常就是软链），指向目录的软链不算。
    [ -f "$f" ] || continue
    [ -x "$f" ] || continue
    T=$((T+1))
    if [ "$N" -ge ` + strconv.Itoa(maxScanCommandsPerDir) + ` ]; then continue; fi
    if [ "$TOTALCMDS" -ge ` + strconv.Itoa(maxScanCommandsTotal) + ` ]; then continue; fi
    n=${f##*/}
    r=$(command -v "$n" 2>/dev/null || true)
    printf 'CMD\t%s\t%s\t%s\n' "$D" "$n" "$r"
    N=$((N+1))
    TOTALCMDS=$((TOTALCMDS+1))
  done
  # 一个命令都没有的 bin 目录不算"环境"：它只会让列表变长。
  [ "$T" -gt 0 ] || return 0
  DIRS=$((DIRS+1))
  printf 'DIR\t%s\t%s\t%s\t%s\n' "$D" "$S" "$N" "$T"
}
`)

	for _, root := range scanRoots {
		b.WriteString("SEEN=0\n")
		b.WriteString("for d in " + root.pattern + "; do\n")
		b.WriteString("  [ \"$SEEN\" -lt " + strconv.Itoa(root.limit) + " ] || break\n")
		b.WriteString("  if [ \"$DIRS\" -ge " + strconv.Itoa(maxScanDirs) + " ]; then break; fi\n")
		b.WriteString("  SEEN=$((SEEN+1))\n")
		b.WriteString("  emit_dir \"$d\" " + root.source + "\n")
		b.WriteString("done\n")
	}
	b.WriteString("echo '===END==='\ntrue")
	return b.String()
}

// parseScan 解析采集输出。
//
// CMD 行与 DIR 行的顺序无关（每行都自带目录路径），面板侧按路径归并：
// 脚本是先出 CMD 再出 DIR 的（要遍历完才知道总数），解析不依赖这个顺序。
func parseScan(raw string) *ScanView {
	sec := splitSections(raw)
	view := &ScanView{Home: strings.TrimSpace(sec["HOME"])}
	for _, dir := range strings.Split(strings.TrimSpace(sec["PATH"]), ":") {
		if dir != "" {
			view.PathDirs = append(view.PathDirs, dir)
		}
	}
	inPath := map[string]bool{}
	for _, dir := range view.PathDirs {
		inPath[strings.TrimSuffix(dir, "/")] = true
	}

	byPath := map[string]*ScanDir{}
	var order []string
	ensure := func(path, source string) *ScanDir {
		if d, ok := byPath[path]; ok {
			return d
		}
		d := &ScanDir{
			Path:   path,
			Source: source,
			Label:  scanSourceText[source],
			InPath: inPath[strings.TrimSuffix(path, "/")],
		}
		if d.Label == "" {
			d.Label = source
		}
		byPath[path] = d
		order = append(order, path)
		return d
	}

	for _, line := range strings.Split(sec["DIRS"], "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 2 || fields[1] == "" {
			continue
		}
		switch fields[0] {
		case "CMD":
			if len(fields) < 4 {
				continue
			}
			dir := ensure(fields[1], "")
			name := fields[2]
			resolved := fields[3]
			dir.Commands = append(dir.Commands, ScanCommand{Name: name, Resolved: resolved})
			// 名字解析到别处 → 这个目录一旦排到前面就会遮住它。
			// （面板把新目录放在 PATH 最前，见 vars_path.go。）
			if resolved != "" && resolved != strings.TrimSuffix(dir.Path, "/")+"/"+name {
				dir.Shadows = append(dir.Shadows, name)
			}
		case "DIR":
			if len(fields) < 5 {
				continue
			}
			dir := ensure(fields[1], fields[2])
			if dir.Source == "" {
				dir.Source = fields[2]
				dir.Label = scanSourceText[fields[2]]
				if dir.Label == "" {
					dir.Label = fields[2]
				}
			}
			dir.Total = atoiSafe(fields[4])
		case "DENIED":
			dir := ensure(fields[1], fields[2])
			dir.Denied = true
		}
	}

	for _, path := range order {
		view.Dirs = append(view.Dirs, *byPath[path])
	}
	return view
}

// atoiSafe 解析一个数，失败时给 0（目标机的输出不该因为一个字段异常整段丢掉）。
func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}