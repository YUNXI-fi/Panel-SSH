package env

import (
	"context"
	"encoding/base64"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 本文件回答"这台机器上有哪些环境变量、它们从哪来、现在是不是真的生效"。
//
// 口径：以**登录 shell 里实际生效的值**为准，再回头去配置文件里找它是在哪定义的。
// 反过来做（只解析配置文件）会在同一变量被多处定义时给出错误答案 —— 而"我明明改了
// 却没生效"恰恰是最常见的那类问题，只列文件解决不了它。
//
// 因此采集分两步：先让目标机的登录 shell 把自己的环境吐出来（env -0），
// 再把候选配置文件原文取回来，在面板侧解析出赋值行，最后把两边合起来 ——
// 每个变量都能回答"值是多少"（生效值）与"谁定的"（定义文件与行号），
// 以及第三种情况：文件里定义了、但当前会话里并没有它（条件没走到，或被后来的覆盖）。

// 变量作用域。
const (
	// ScopeSystem 系统级：定义在 /etc 下，对这台机器上所有用户生效。
	ScopeSystem = "system"
	// ScopeUser 用户级：定义在登录用户的家目录下，只对该用户生效。
	ScopeUser = "user"
	// ScopeSession 会话注入：ssh 守护进程、systemd、shell 自身在会话建立时放进去的，
	// 没有任何配置文件写它（SSH_CONNECTION、PWD、SHLVL 这些）。
	ScopeSession = "session"
)

// EnvVarFile 是一个候选配置文件。
type EnvVarFile struct {
	Path   string `json:"path"`
	Scope  string `json:"scope"`
	Exists bool   `json:"exists"`
	// Read 为假表示文件存在但内容没取回来（体积超限或超出本轮配额），
	// 此时 Reason 说明原因 —— 来源推断会在这些文件上留缺口，必须让用户看得见。
	Read    bool   `json:"read"`
	Reason  string `json:"reason,omitempty"`
	Defines int    `json:"defines"`
}

// EnvVar 是一个环境变量。
type EnvVar struct {
	Name string `json:"name"`
	// Value 是生效值（登录 shell 里的实际值）；Effective 为假时是文件里写的字面值。
	Value     string `json:"value"`
	Effective bool   `json:"effective"`
	Scope     string `json:"scope"`
	// File/Line/Raw 是定义来源：文件里最后一条对它的赋值。
	// 会话注入的变量这三项都为空 —— 没有任何文件定义它，如实留空而不是硬凑一个来源。
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	Raw  string `json:"raw,omitempty"`
	// Note 解释"为什么看到的和期望的不一样"：被更晚的定义覆盖、含变量引用未展开、
	// 文件里定义了却没生效，等等。
	Note string `json:"note,omitempty"`
}

// VarsView 是一次环境变量采集的结果。
type VarsView struct {
	Shell string       `json:"shell"`
	Home  string       `json:"home"`
	Files []EnvVarFile `json:"files"`
	Vars  []EnvVar     `json:"vars"`
	// Truncated 为真表示有文件没读到，来源推断可能不完整。
	Truncated bool `json:"truncated"`
}

// varsTimeout 覆盖登录 shell 的启动（rc 文件里可能有网络请求之类）与全部文件读取。
const varsTimeout = 25 * time.Second

// 单个配置文件与整轮采集的体积上限。
//
// 必须有上限：执行通道单侧只保留 64KB（见 MaxStepOutputBytes），而 base64 还要
// 额外膨胀三分之一，一次把几十个 profile.d 全塞回来会直接撞上截断 —— 被截断的是
// 采集结果本身，解析出来就会莫名其妙少一半变量。
const (
	maxVarFileBytes  = 8 * 1024
	maxVarsTotalByte = 24 * 1024
)

// varsCommand 一次采集环境变量所需的全部材料。
//
// 登录 shell 用 -lic 而不是 -lc：用户期望看到的"我的环境变量"，是他在终端里
// 登录之后的那一份 —— 交互式登录 shell 才会读 ~/.bashrc，而 Debian/Ubuntu 的
// ~/.bashrc 开头就有"非交互直接 return"的保护，用 -lc 取到的环境会缺掉用户
// 自己写在 ~/.bashrc 里的那些变量，界面上看着就像"我设的变量丢了"。
//
// 取不到时逐级退化（-lic 的 env -0 → -lic 的 env → -lc 的 env）：某个 rc 文件
// 写坏了不该让整个页签变空，退化后至少还有一份可读的会话环境。
//
// env -0 优先，因为值里允许有换行（证书、多行配置），按行切会把它们切碎；
// busybox 的 env 不支持 -0，所以退化路径必须留着。
//
// 这里是 var 而不是 const：脚本里的体积上限由上面两个常量拼出来，而常量声明
// 里不能调用 strconv —— 写成字面量又会让上限散成两份，改一处忘一处。
var varsCommand = `export LC_ALL=C
echo '===SHELL==='
printf '%s\n' "${SHELL:-}"
echo '===HOME==='
printf '%s\n' "${HOME:-}"
EFF=''; MODE=lines
if command -v bash >/dev/null 2>&1; then
  EFF=$(bash -lic 'env -0' 2>/dev/null | base64 | tr -d '\n'); MODE=nul
  if [ -z "$EFF" ]; then MODE=lines; EFF=$(bash -lic 'env' 2>/dev/null | base64 | tr -d '\n'); fi
  if [ -z "$EFF" ]; then EFF=$(bash -lc 'env' 2>/dev/null | base64 | tr -d '\n'); fi
else
  EFF=$(sh -lc 'env' 2>/dev/null | base64 | tr -d '\n')
fi
echo '===EFFECTIVE_MODE==='
printf '%s\n' "$MODE"
echo '===EFFECTIVE_B64==='
printf '%s\n' "$EFF"
echo '===FILES==='
TOTAL=0
read_one() {
  P=$1
  if [ ! -f "$P" ]; then printf 'MISS\t%s\n' "$P"; return; fi
  SZ=$(wc -c < "$P" 2>/dev/null | tr -d ' ')
  [ -n "$SZ" ] || SZ=0
  if [ "$SZ" -gt ` + strconv.Itoa(maxVarFileBytes) + ` ]; then printf 'BIG\t%s\t%s\n' "$P" "$SZ"; return; fi
  if [ $((TOTAL + SZ)) -gt ` + strconv.Itoa(maxVarsTotalByte) + ` ]; then printf 'SKIP\t%s\t%s\n' "$P" "$SZ"; return; fi
  TOTAL=$((TOTAL + SZ))
  printf 'FILE\t%s\t%s\t%s\n' "$P" "$SZ" "$(base64 < "$P" | tr -d '\n')"
}
read_one /etc/environment
read_one /etc/profile
for f in /etc/profile.d/*.sh; do [ -f "$f" ] && read_one "$f"; done
read_one /etc/bash.bashrc
read_one "$HOME/.bash_profile"
read_one "$HOME/.bash_login"
read_one "$HOME/.profile"
read_one "$HOME/.bashrc"
read_one "$HOME/.zprofile"
read_one "$HOME/.zshrc"
true`

// CollectVars 采集目标机的环境变量。只读，不需要提权：
// 读的都是登录用户本来就能读的文件，取环境也是以自己的身份取。
func CollectVars(ctx context.Context, ex Executor) (*VarsView, error) {
	out, err := runQuery(ctx, ex, varsCommand, varsTimeout)
	if err != nil {
		return nil, err
	}
	return parseVars(out), nil
}

// varAssignRe 匹配一条环境变量赋值行。
//
// 只认最直白的形式（可选 export 前缀 + NAME=值）：条件赋值、循环里拼出来的、
// 函数里 export 的，一律不猜 —— 猜错会给出一个看似确定、实则错误的"来源"，
// 那比留空更糟。这类变量仍然会以生效值的身份出现在列表里，只是来源推断不到。
var varAssignRe = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// varNameRe 是允许写入的变量名形态。
var varNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidVarName 判断变量名是否可写。写接口与采集共用这一条规则：
// 两处各写一份，迟早会出现"采集能列出来、却写不进去"的变量。
func ValidVarName(name string) bool { return varNameRe.MatchString(name) }

// VarScopeOfPath 按路径判断作用域：/etc 下是系统级，其余（家目录里的）是用户级。
func VarScopeOfPath(path, home string) string {
	if strings.HasPrefix(path, "/etc/") {
		return ScopeSystem
	}
	if home != "" && strings.HasPrefix(path, home+"/") {
		return ScopeUser
	}
	return ScopeUser
}

// shellSingleQuote 把值包成 shell 单引号字面量。
//
// 单引号是唯一不需要转义反斜杠、$、反引号的壳：值里出现单引号时按 POSIX 的
// '\'' 断开再接回去。用双引号的话，"$PATH" 这类值写进文件后会被再次展开 ——
// 用户填的是字面值，面板必须原样写下去。
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// varDefinition 是一个变量在某文件里的定义。
type varDefinition struct {
	value string
	file  string
	line  int
	raw   string
	// hasRef 表示定义行的值里含变量引用或命令替换（PATH="$PATH:/opt" 这种），
	// 也就是"文件里写的"与"会话里生效的"必然不同。它只是一个标记，不是成品文案：
	// 有生效值和没有生效值两种情形下，同一句话的含义正好相反（见 parseVars）。
	hasRef bool
}

// parseVars 把采集输出解析成变量视图。
func parseVars(raw string) *VarsView {
	sec := splitSections(raw)
	view := &VarsView{Shell: strings.TrimSpace(sec["SHELL"]), Home: strings.TrimSpace(sec["HOME"])}
	effective := parseEffective(sec)

	// 按采集顺序（也就是 shell 读取这些文件的顺序）逐行覆盖：最后一条赋值
	// 就是"谁定的"。顺序来自脚本里 read_one 的调用次序，两边必须一起改。
	defs := map[string]varDefinition{}
	counts := map[string]int{}

	for _, line := range strings.Split(sec["FILES"], "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 2 || fields[1] == "" {
			continue
		}
		kind, path := fields[0], fields[1]
		file := EnvVarFile{Path: path, Scope: VarScopeOfPath(path, view.Home)}
		switch kind {
		case "FILE":
			file.Exists, file.Read = true, true
		case "BIG":
			file.Exists = true
			file.Reason = "文件超过 " + strconv.Itoa(maxVarFileBytes/1024) + " KB，未读取内容"
		case "SKIP":
			file.Exists = true
			file.Reason = "本轮读取配额已用尽，未读取内容"
		default: // MISS
			view.Files = append(view.Files, file)
			continue
		}
		if file.Read && len(fields) >= 4 {
			data, err := base64.StdEncoding.DecodeString(fields[3])
			if err != nil {
				file.Read = false
				file.Reason = "内容解码失败"
			} else {
				for name, d := range parseAssignments(string(data), path) {
					defs[name] = d
					counts[path]++
				}
			}
		}
		if !file.Read {
			view.Truncated = true
		}
		file.Defines = counts[path]
		view.Files = append(view.Files, file)
	}

	for name, value := range effective {
		item := EnvVar{Name: name, Value: value, Effective: true, Scope: ScopeSession}
		if d, ok := defs[name]; ok {
			item.Scope, item.File, item.Line, item.Raw = VarScopeOfPath(d.file, view.Home), d.file, d.line, d.raw
			switch {
			// 值列显示的是会话里的实际值（引用已被 shell 展开过），所以这里只是
			// 提醒"别拿来源列那行原文跟它对账"。
			case d.hasRef:
				item.Note = "定义行里含变量引用或命令替换，当前值是它展开后的结果"
			case d.value != value:
				// 文件里写的和会话里生效的不一样，说明它被更晚的定义盖住了 ——
				// 这正是"改了没生效"的答案，必须点出来而不是让用户自己比对。
				item.Note = "该文件写的值与会话中生效的值不同，说明它被更晚的定义覆盖"
			}
		}
		view.Vars = append(view.Vars, item)
	}

	// 文件里定义了、会话里却没有：条件分支没走到，或者被后来的定义整个抹掉了。
	// 这类变量最容易被误判成"已经配好了"，单独列出来。
	for name, d := range defs {
		if _, ok := effective[name]; ok {
			continue
		}
		// 这一条只取到了文件里的原文，没有生效值可对照 —— 含引用的行显示的就是
		// 未展开的字面量，不能当实际值看。两件事都要说，不能互相顶掉。
		note := "在配置文件里定义了，但当前登录会话中并没有这个变量"
		if d.hasRef {
			note = "定义行里含变量引用或命令替换，这里显示的是未展开的原文；" + note
		}
		view.Vars = append(view.Vars, EnvVar{
			Name: name, Value: d.value, Effective: false,
			Scope: VarScopeOfPath(d.file, view.Home), File: d.file, Line: d.line, Raw: d.raw, Note: note,
		})
	}

	sort.Slice(view.Vars, func(i, j int) bool {
		a, b := strings.ToLower(view.Vars[i].Name), strings.ToLower(view.Vars[j].Name)
		if a != b {
			return a < b
		}
		return view.Vars[i].Name < view.Vars[j].Name
	})
	return view
}

// parseEffective 解出登录 shell 里生效的环境变量。mode 为 nul 时按 NUL 切分，
// 否则按行 —— 值里含换行的少数变量只在 NUL 模式下能完整还原。
func parseEffective(sec map[string]string) map[string]string {
	out := map[string]string{}
	b64 := strings.TrimSpace(sec["EFFECTIVE_B64"])
	if b64 == "" {
		return out
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return out
	}
	sep := "\n"
	if strings.TrimSpace(sec["EFFECTIVE_MODE"]) == "nul" {
		sep = "\x00"
	}
	for _, item := range strings.Split(string(data), sep) {
		name, value, ok := strings.Cut(item, "=")
		if !ok || !ValidVarName(name) {
			continue
		}
		out[name] = value
	}
	return out
}

// parseAssignments 解析一个配置文件里的赋值行。
func parseAssignments(content, path string) map[string]varDefinition {
	out := map[string]varDefinition{}
	for index, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		m := varAssignRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		name, rawValue := m[1], strings.TrimSpace(m[2])
		value, quoted := unquote(rawValue)
		// 未加引号的值几乎一定是变量引用或命令替换（PATH=$PATH:/opt 这种）。
		// 展开它是另一回事（面板不替目标机的 shell 求值），只标记下来，
		// 由 parseVars 按"这条有没有生效值"给出各自准确的说法。
		hasRef := !quoted && strings.ContainsAny(value, "$`\\")
		out[name] = varDefinition{value: value, file: path, line: index + 1, raw: trimmed, hasRef: hasRef}
	}
	return out
}

// unquote 去掉值外面的成对引号，返回是否本来就是带引号的字面量。
func unquote(raw string) (string, bool) {
	if len(raw) >= 2 {
		first, last := raw[0], raw[len(raw)-1]
		if first == last && (first == '"' || first == '\'') {
			inner := raw[1 : len(raw)-1]
			if first == '\'' {
				return inner, true
			}
			return unescapeDoubleQuoted(inner), true
		}
	}
	// 未加引号：行尾的 " # 注释" 不属于值。只在空格加井号处截断，
	// 值本身带井号（LESS='#ff0000' 之类）不会被误伤。
	if cut, _, found := strings.Cut(raw, " #"); found {
		return strings.TrimSpace(cut), false
	}
	return raw, false
}

// unescapeDoubleQuoted 处理双引号里的转义。只认 POSIX 规定的四个，
// 其余反斜杠在双引号内是字面量 —— 全部吃掉会把 Windows 路径这类值改坏。
func unescapeDoubleQuoted(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\\', '$', '`':
				b.WriteByte(s[i+1])
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// varHomeScript 只取登录用户的家目录。
//
// 用户级变量的落地文件是"家目录下某个文件"，这个前缀必须由目标机给出：
// 面板不能让前端把路径报上来（那等于让用户决定往哪写），也不能用面板侧缓存的
// 家目录（同一份缓存对应多台机器，换台机器就写错地方）。
var varHomeScript = `export LC_ALL=C
echo '===HOME==='
printf '%s\n' "${HOME:-}"
echo '===END==='
true`

// homeTimeout 是一次家目录探测的时限（纯读环境变量，不启动登录 shell）。
const homeTimeout = 10 * time.Second

// CollectHome 读取目标机登录用户的家目录。
func CollectHome(ctx context.Context, ex Executor) (string, error) {
	out, err := runQuery(ctx, ex, varHomeScript, homeTimeout)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(splitSections(out)["HOME"]), nil
}

// varProbeScript 让目标机新起一个登录 shell，读某个变量此刻的生效值。
//
// 这是"改完到底生效没有"唯一可信的答案。写进 profile 文件只意味着"下次登录会读到"，
// 而 ~/.bashrc 开头的非交互保护、被更晚的文件覆盖、条件分支没走到，都只有真起一个
// 登录 shell 才看得见 —— 面板若只核对文件内容，就会把"写了但读不到"报成成功。
//
// 用 S:/U 前缀区分"有值（可能为空串）"与"根本没定义"：直接打值的话两者都是空输出。
// shell 用 bash -lic 与采集保持一致（见 varsCommand 的说明），没有 bash 时退化到 sh -lc。
func varProbeScript(name string) string {
	probe := `if [ "${` + name + `+x}" = x ]; then printf 'S:%s' "${` + name + `}"; else printf 'U'; fi`
	q := shellSingleQuote(probe)
	return `export LC_ALL=C
OUT=''
if command -v bash >/dev/null 2>&1; then
  OUT=$(bash -lic ` + q + ` 2>/dev/null)
fi
if [ -z "$OUT" ]; then
  OUT=$(sh -lc ` + q + ` 2>/dev/null)
fi
echo '===VALUE==='
printf '%s\n' "$OUT"
echo '===END==='
true`
}

// VarProbeResult 是某个变量在新登录会话里的状态。
type VarProbeResult struct {
	Set   bool
	Value string
}

// ParseVarProbe 解析 varProbeScript 的输出。
func ParseVarProbe(out string) VarProbeResult {
	raw := strings.TrimRight(splitSections(out)["VALUE"], "\n")
	if line, _, found := strings.Cut(raw, "\n"); found {
		// 值里不该有换行（写入侧拒绝换行），真出现多行时取第一行并保持其余原样丢弃：
		// 这种情况只会是目标机 rc 文件自己往 stdout 打了东西。
		raw = line
	}
	if rest, ok := strings.CutPrefix(raw, "S:"); ok {
		return VarProbeResult{Set: true, Value: rest}
	}
	return VarProbeResult{}
}