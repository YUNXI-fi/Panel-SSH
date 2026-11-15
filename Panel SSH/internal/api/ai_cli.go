package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// aiCLIApp 是「应用中心 · AI CLI」里的一项。
//
// 这些工具都是 npm 上的命令行程序，安装方式统一为"用户级 npm 前缀 + 软链到
// ~/.local/bin"：面板不持有 sudo 口令，系统级 npm -g 在多数目标机上会因权限
// 失败；用户级安装对宿主机和远端主机都成立。
type aiCLIApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Pkg  string `json:"pkg"`
	// Bins 是该包提供的可执行文件名，用于探测与建软链。
	Bins []string `json:"bins"`
	// NodeMajor 是运行所需的最低 Node 大版本；0 表示不限制。
	NodeMajor int `json:"nodeMajor"`
	// OS 与 Arch 是工具自身支持的平台，取 uname 的写法（linux/darwin、x64/arm64）。
	// 为空表示不限制：纯 JS 包由 npm 自己挑产物，面板不替它设门槛。
	//
	// 区分这两项的原因：Codex、OpenCode 这类包的可执行文件是随平台分发的原生二进制，
	// 装到不支持的机器上不会"功能缺失"，而是装完直接跑不起来。
	OS   []string `json:"os"`
	Arch []string `json:"arch"`
	// Group 是目录分类：ai 是 AI 编码代理，dev 是通用开发工具。
	// 空值按 ai 处理，因为分类是为了新增页签才引入的，老条目不必逐条回填。
	Group string `json:"group"`
	Note  string `json:"note,omitempty"`
}

// aiCLIStatus 是目录项在目标机上的实际状态。
type aiCLIStatus struct {
	aiCLIApp
	Installed bool `json:"installed"`
	// Supported 表示目标机是否落在工具声明的平台范围内（同时受面板安装链路的限制）。
	Supported bool   `json:"supported"`
	Version   string `json:"version"`
	Path      string `json:"path"`
}

// 面板这套用户级安装链路能覆盖的平台：Node 官方产物与 POSIX sh 探测脚本都只覆盖这两类。
// 它和目录项自身的 OS/Arch 是两件事：前者限制"能不能装"，后者限制"装上后跑不跑得动"。
var (
	aiCLIInstallerOS   = []string{"linux", "darwin"}
	aiCLIInstallerArch = []string{"x64", "arm64"}
)

// aiCLIApps 是应用中心内置的 AI CLI 目录。
//
// 版本号不写死：安装时统一取 @latest，探测时读目标机上的实际版本。
// OS/Arch 只写在"产物随平台分发"的条目上（原生二进制、pty、sharp 之类），
// 纯 JS 的包留空，交给 npm 自己挑依赖。
var aiCLIApps = []aiCLIApp{
	{ID: "claude", Name: "Claude Code", Pkg: "@anthropic-ai/claude-code", Bins: []string{"claude"}, NodeMajor: 22,
		Note: "postinstall 会下载原生二进制"},
	{ID: "gemini", Name: "Gemini CLI", Pkg: "@google/gemini-cli", Bins: []string{"gemini"}, NodeMajor: 20},
	{ID: "codex", Name: "OpenAI Codex", Pkg: "@openai/codex", Bins: []string{"codex"}, NodeMajor: 18,
		OS: []string{"linux", "darwin"}, Arch: []string{"x64", "arm64"},
		Note: "使用前需要 OpenAI 账号登录"},
	{ID: "qwen", Name: "Qwen Code", Pkg: "@qwen-code/qwen-code", Bins: []string{"qwen"}, NodeMajor: 22,
		OS: []string{"linux", "darwin"}, Arch: []string{"x64", "arm64"},
		Note: "使用前需要 Qwen 账号登录"},
	{ID: "iflow", Name: "iFlow CLI", Pkg: "@iflow-ai/iflow-cli", Bins: []string{"iflow"}, NodeMajor: 20,
		OS: []string{"linux", "darwin"}, Arch: []string{"x64", "arm64"},
		Note: "使用前需要心流账号登录"},
	{ID: "continue", Name: "Continue CLI", Pkg: "@continuedev/cli", Bins: []string{"cn"}, NodeMajor: 20,
		OS: []string{"linux", "darwin"}, Arch: []string{"x64", "arm64"},
		Note: "运行前需要配置模型提供方"},
	{ID: "amp", Name: "Amp", Pkg: "@ampcode/cli", Bins: []string{"amp"}, NodeMajor: 0,
		Note: "使用前需要 Amp 账号登录"},
	{ID: "grok", Name: "Grok CLI", Pkg: "@vibe-kit/grok-cli", Bins: []string{"grok"}, NodeMajor: 18,
		Note: "使用前需要 xAI API Key"},
	{ID: "opencode", Name: "OpenCode", Pkg: "@opencode/cli", Bins: []string{"opencode"}, NodeMajor: 0,
		OS: []string{"linux", "darwin"}, Arch: []string{"x64", "arm64"}},
	{ID: "auggie", Name: "Auggie CLI", Pkg: "@augmentcode/auggie", Bins: []string{"auggie"}, NodeMajor: 20,
		Note: "使用前需要 Augment 账号登录"},
	{ID: "kilo", Name: "Kilo Code CLI", Pkg: "@kilocode/cli", Bins: []string{"kilo", "kilocode"}, NodeMajor: 0,
		OS: []string{"linux", "darwin"}, Arch: []string{"x64", "arm64"},
		Note: "使用前需要 Kilo Code 账号登录"},
	{ID: "letta", Name: "Letta Code", Pkg: "@letta-ai/letta-code", Bins: []string{"letta"}, NodeMajor: 22,
		Note: "使用前需要 Letta 账号登录"},
	{ID: "pi", Name: "Pi", Pkg: "@earendil-works/pi-coding-agent", Bins: []string{"pi"}, NodeMajor: 22},
	{ID: "mothx", Name: "MothX", Pkg: "mothx-installer", Bins: []string{"mothx"}, NodeMajor: 14,
		Note: "上游 latest 是预发布版"},
	{ID: "penguin", Name: "PenguinHarness", Pkg: "@prismshadow/penguin-cli", Bins: []string{"penguin"}, NodeMajor: 24},
	{ID: "omo", Name: "OmO", Pkg: "omo-ai", Bins: []string{"omo"}, NodeMajor: 24},
	{ID: "copilot", Name: "Copilot CLI", Pkg: "@github/copilot", Bins: []string{"copilot"}, NodeMajor: 0,
		Note: "使用前需要 GitHub 登录"},
	{ID: "crush", Name: "Crush", Pkg: "@charmland/crush", Bins: []string{"crush"}, NodeMajor: 0,
		Note: "postinstall 会下载原生二进制"},
	{ID: "dsh", Name: "DeepSeek Harness", Pkg: "@deepseek-ai/dsh", Bins: []string{"dsh"}, NodeMajor: 24},
	{ID: "omp", Name: "omp", Pkg: "@oh-my-pi/pi-coding-agent", Bins: []string{"omp"}, NodeMajor: 0},
	{ID: "cline", Name: "Cline", Pkg: "cline", Bins: []string{"cline"}, NodeMajor: 0},
	{ID: "codebuddy", Name: "CodeBuddy", Pkg: "@tencent-ai/codebuddy-code", Bins: []string{"codebuddy", "cbc"}, NodeMajor: 0},
	{ID: "atomcode", Name: "AtomCode", Pkg: "@atomgit.com/atomcode", Bins: []string{"atomcode"}, NodeMajor: 18},
	{ID: "snow", Name: "Snow CLI", Pkg: "snow-ai", Bins: []string{"snow"}, NodeMajor: 22},

	// 开发工具：和上面的 AI 代理同一条安装链路（用户级 npm 前缀 + 软链），
	// 只是分类不同。放在同一张表里，探测仍然一次 SSH 跑完。
	{ID: "pm2", Name: "PM2", Pkg: "pm2", Bins: []string{"pm2", "pm2-runtime"}, NodeMajor: 18, Group: "dev",
		Note: "守护进程需要另行配置开机自启"},
	{ID: "nodemon", Name: "nodemon", Pkg: "nodemon", Bins: []string{"nodemon"}, NodeMajor: 10, Group: "dev"},
	{ID: "wrangler", Name: "Wrangler", Pkg: "wrangler", Bins: []string{"wrangler"}, NodeMajor: 22, Group: "dev",
		Note: "首次使用需要 Cloudflare 登录"},
	{ID: "firebase", Name: "Firebase CLI", Pkg: "firebase-tools", Bins: []string{"firebase"}, NodeMajor: 20, Group: "dev",
		Note: "首次使用需要 firebase login"},
	{ID: "vercel", Name: "Vercel CLI", Pkg: "vercel", Bins: []string{"vercel"}, NodeMajor: 20, Group: "dev",
		Note: "首次使用需要 vercel login"},
	{ID: "netlify", Name: "Netlify CLI", Pkg: "netlify-cli", Bins: []string{"netlify", "ntl"}, NodeMajor: 22, Group: "dev",
		Note: "首次使用需要 netlify login"},
	{ID: "railway", Name: "Railway CLI", Pkg: "@railway/cli", Bins: []string{"railway"}, NodeMajor: 16, Group: "dev",
		Note: "首次使用需要 railway login"},
	{ID: "serve", Name: "serve", Pkg: "serve", Bins: []string{"serve"}, NodeMajor: 14, Group: "dev"},
	{ID: "http-server", Name: "http-server", Pkg: "http-server", Bins: []string{"http-server"}, NodeMajor: 12, Group: "dev"},
	{ID: "pnpm", Name: "pnpm", Pkg: "pnpm", Bins: []string{"pnpm"}, NodeMajor: 18, Group: "dev"},
	{ID: "tsx", Name: "tsx", Pkg: "tsx", Bins: []string{"tsx"}, NodeMajor: 18, Group: "dev"},
	{ID: "prettier", Name: "Prettier", Pkg: "prettier", Bins: []string{"prettier"}, NodeMajor: 14, Group: "dev"},
	{ID: "eslint", Name: "ESLint", Pkg: "eslint", Bins: []string{"eslint"}, NodeMajor: 20, Group: "dev"},
	{ID: "nest", Name: "Nest CLI", Pkg: "@nestjs/cli", Bins: []string{"nest"}, NodeMajor: 20, Group: "dev"},
	{ID: "mongosh", Name: "MongoDB Shell", Pkg: "mongosh", Bins: []string{"mongosh"}, NodeMajor: 16, Group: "dev",
		Note: "连接串形如 mongodb://用户:口令@主机"},
	{ID: "degit", Name: "degit", Pkg: "degit", Bins: []string{"degit"}, NodeMajor: 20, Group: "dev"},
	{ID: "mcp-inspector", Name: "MCP Inspector", Pkg: "@modelcontextprotocol/inspector", Bins: []string{"mcp-inspector"}, NodeMajor: 22, Group: "dev",
		Note: "用于调试 MCP 服务器"},
}

// aiCLIUserPrefix 是用户级 npm 前缀，与 dsh 的运行时目录并列。
const aiCLIUserPrefix = "$HOME/.panelssh/ai-cli"

// aiCLIDetectScript 生成一次探测全部目录项的脚本。
//
// 一次 SSH 执行回答所有条目，避免 20 多次往返；npm 全局列表也只取一次。
// 输出两段：先一行 platform<TAB>os<TAB>arch 说明目标机是什么，
// 再每个条目一行 id<TAB>installed|missing<TAB>version<TAB>path。
// 平台随探测一起带回来，是为了让"这个工具在这台机器上装不了"能在点安装之前就知道。
func aiCLIDetectScript() string {
	var b strings.Builder
	b.WriteString("export LC_ALL=C\n")
	b.WriteString("printf 'platform\\t%s\\t%s\\n' \"$(uname -s 2>/dev/null | tr 'A-Z' 'a-z')\" \"$(uname -m 2>/dev/null | tr 'A-Z' 'a-z')\"\n")
	b.WriteString("GL=$(npm ls -g --depth=0 2>/dev/null || true)\n")
	for _, app := range aiCLIApps {
		fmt.Fprintf(&b, "found=''\npath=''\n")
		for _, bin := range app.Bins {
			fmt.Fprintf(&b, "if [ -z \"$found\" ]; then\n")
			fmt.Fprintf(&b, "  p=$(command -v %s 2>/dev/null || true)\n", bin)
			fmt.Fprintf(&b, "  [ -z \"$p\" ] && [ -x \"$HOME/.local/bin/%s\" ] && p=\"$HOME/.local/bin/%s\"\n", bin, bin)
			fmt.Fprintf(&b, "  [ -z \"$p\" ] && [ -x \"%s/bin/%s\" ] && p=\"%s/bin/%s\"\n", aiCLIUserPrefix, bin, aiCLIUserPrefix, bin)
			fmt.Fprintf(&b, "  if [ -n \"$p\" ] && [ -x \"$p\" ]; then found=1; path=\"$p\"; fi\n")
			fmt.Fprintf(&b, "fi\n")
		}
		fmt.Fprintf(&b, "ver=''\n")
		fmt.Fprintf(&b, "if [ -n \"$path\" ]; then ver=$(timeout 5 \"$path\" --version 2>/dev/null | head -1); fi\n")
		fmt.Fprintf(&b, "if [ -z \"$found\" ] && echo \"$GL\" | grep -q '%s@'; then found=1; path='npm 全局'; fi\n", app.Pkg)
		fmt.Fprintf(&b, "if [ -z \"$found\" ] && [ -f \"%s/lib/node_modules/%s/package.json\" ]; then\n", aiCLIUserPrefix, app.Pkg)
		fmt.Fprintf(&b, "  found=1; path='%s'\n", aiCLIUserPrefix)
		fmt.Fprintf(&b, "  ver=$(sed -n 's/.*\"version\": *\"\\([^\"]*\\)\".*/\\1/p' \"%s/lib/node_modules/%s/package.json\" | head -1)\n", aiCLIUserPrefix, app.Pkg)
		fmt.Fprintf(&b, "fi\n")
		fmt.Fprintf(&b, "if [ -n \"$found\" ]; then printf '%%s\\tinstalled\\t%%s\\t%%s\\n' '%s' \"$ver\" \"$path\"; else printf '%%s\\tmissing\\t\\t\\n' '%s'; fi\n", app.ID, app.ID)
	}
	return b.String()
}

// aiCLIInstallScript 生成单个工具的一键安装脚本。
//
// 步骤：按需准备 Node 24（用户级，不碰系统 Node）→ npm 装到用户前缀 →
// 把可执行文件软链到 ~/.local/bin。全程不需要 root。
func aiCLIInstallScript(app aiCLIApp) string {
	var b strings.Builder
	b.WriteString(`set -e
export LC_ALL=C
R="$HOME/.panelssh"
ND="$R/node"
PFX="$R/ai-cli"
mkdir -p "$ND" "$PFX" "$HOME/.local/bin"

case "$(uname -s)" in
  Linux) O=linux ;;
  Darwin) O=darwin ;;
  *) echo "不支持的操作系统：$(uname -s)" >&2; exit 4 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) A=x64 ;;
  aarch64|arm64) A=arm64 ;;
  *) echo "不支持的 CPU 架构：$(uname -m)" >&2; exit 4 ;;
esac

if command -v curl >/dev/null 2>&1; then
  GET="curl -fsSL --max-time 30"
  DL="curl -fsSL --max-time 600 -o"
else
  GET="wget -qO-"
  DL="wget -q -O"
fi

NB=$(ls -d "$ND"/node-v24.*/bin/node 2>/dev/null | sort -V | tail -n1)
CUR=""
if command -v node >/dev/null 2>&1; then CUR=$(node -v 2>/dev/null | sed 's/^v//' | cut -d. -f1); fi
USE24=0
`)
	fmt.Fprintf(&b, "NEED=%d\n", app.NodeMajor)
	b.WriteString(`if [ -z "$CUR" ]; then USE24=1; fi
if [ "$NEED" -gt 0 ] && { [ -z "$CUR" ] || [ "$CUR" -lt "$NEED" ]; }; then USE24=1; fi
if ! command -v npm >/dev/null 2>&1; then USE24=1; fi

if [ "$USE24" = 1 ]; then
  if [ -z "$NB" ] || [ ! -x "$NB" ]; then
    V=$($GET https://npmmirror.com/mirrors/node/index.tab 2>/dev/null | awk '$1 ~ /^v24\./ {print $1; exit}')
    [ -n "$V" ] || V=$($GET https://nodejs.org/dist/index.tab 2>/dev/null | awk '$1 ~ /^v24\./ {print $1; exit}')
    [ -n "$V" ] || { echo "无法获取 Node 24 的下载地址" >&2; exit 5; }
    T="node-$V-$O-$A.tar.xz"
    TMP=$(mktemp -d)
    trap 'rm -rf "$TMP"' EXIT
    $DL "$TMP/$T" "https://npmmirror.com/mirrors/node/$V/$T" || $DL "$TMP/$T" "https://nodejs.org/dist/$V/$T"
    tar -xJf "$TMP/$T" -C "$ND"
    NB="$ND/node-$V-$O-$A/bin/node"
  fi
  export PATH="$(dirname "$NB"):$PATH"
fi

`)
	fmt.Fprintf(&b, "npm install -g --prefix \"$PFX\" --allow-scripts=%s %s@latest --no-audit --no-fund --loglevel=error\n", app.Pkg, app.Pkg)
	for _, bin := range app.Bins {
		fmt.Fprintf(&b, "if [ -x \"$PFX/bin/%s\" ]; then ln -sf \"$PFX/bin/%s\" \"$HOME/.local/bin/%s\"; fi\n", bin, bin, bin)
	}
	b.WriteString(`hash -r 2>/dev/null || true
echo "安装完成：`)
	b.WriteString(app.Name)
	b.WriteString(`"
`)
	return b.String()
}

// handleAICLIList 返回目录项在目标机上的安装状态。
func (s *Server) handleAICLIList(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	res, err := s.execOnHost(r.Context(), hostID, aiCLIDetectScript(), defaultExecTimeout)
	if err != nil {
		s.writeHostError(w, err, "读取 AI CLI 状态失败")
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadGateway, "读取 AI CLI 状态失败："+firstLine(res.Stderr))
		return
	}

	statuses := make([]aiCLIStatus, 0, len(aiCLIApps))
	byID := map[string]aiCLIStatus{}
	var targetOS, targetArch string
	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 3 {
			continue
		}
		if fields[0] == "platform" {
			targetOS, targetArch = fields[1], aiCLIArch(fields[2])
			continue
		}
		if len(fields) < 4 {
			continue
		}
		byID[fields[0]] = aiCLIStatus{
			Installed: fields[1] == "installed",
			Version:   strings.TrimSpace(fields[2]),
			Path:      strings.TrimSpace(fields[3]),
		}
	}
	for _, item := range aiCLIApps {
		// OS/Arch 必须序列化成 []，不能是 null：Go 的 nil 切片会编码成 null，
		// 而前端按数组取 length，null 会直接抛错、把整个页签渲染弄白。
		app := item
		if app.OS == nil {
			app.OS = []string{}
		}
		if app.Arch == nil {
			app.Arch = []string{}
		}
		if app.Group == "" {
			app.Group = "ai"
		}
		st := byID[app.ID]
		st.aiCLIApp = app
		st.Supported = aiCLIPlatformSupported(app, targetOS, targetArch)
		statuses = append(statuses, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apps":     statuses,
		"platform": map[string]string{"os": targetOS, "arch": targetArch},
	})
}

// aiCLIArch 把 uname -m 的写法统一成 npm 的架构名。
func aiCLIArch(raw string) string {
	switch raw {
	case "x86_64", "amd64":
		return "x64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l", "armv7":
		return "arm"
	case "i386", "i486", "i586", "i686", "x86":
		return "x86"
	}
	return raw
}

// aiCLIPlatformSupported 判断目标机能不能装、装了能不能跑。
//
// 两道门槛：先是面板这套安装链路（Node 官方产物 + POSIX sh 脚本）能覆盖的平台，
// 再是工具自己声明的 OS/Arch。目标平台探测失败时不设限 —— 让 npm 去报错，
// 总好过在这里因为读不到 uname 就把安装按钮关掉。
func aiCLIPlatformSupported(app aiCLIApp, targetOS, targetArch string) bool {
	if targetOS == "" || targetArch == "" {
		return true
	}
	if !slices.Contains(aiCLIInstallerOS, targetOS) || !slices.Contains(aiCLIInstallerArch, targetArch) {
		return false
	}
	if len(app.OS) > 0 && !slices.Contains(app.OS, targetOS) {
		return false
	}
	if len(app.Arch) > 0 && !slices.Contains(app.Arch, targetArch) {
		return false
	}
	return true
}

// handleAICLIInstall 一键安装/升级一个工具。
func (s *Server) handleAICLIInstall(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	app, found := aiCLIByID(req.ID)
	if !found {
		writeErr(w, http.StatusBadRequest, "未知的 AI CLI："+req.ID)
		return
	}

	res, err := s.execOnHost(r.Context(), hostID, aiCLIInstallScript(app), dshPackageTimeout)
	if err != nil {
		s.writeHostError(w, err, "安装 "+app.Name+" 失败")
		return
	}
	output := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = output
		}
		writeErr(w, http.StatusBadGateway, "安装 "+app.Name+" 失败："+firstLine(detail))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"output": output,
	})
}

// aiCLIElevatedUninstallScript 以 root 卸载系统级的工具。
//
// 系统路径下的可执行文件有三种来路，删法完全不同：npm 全局包、发行版包、
// 以及手工放进去的文件。因此这里不是"直接 npm uninstall"，而是：
// 先按 npm 全局包删一次，再回头看它还在不在；还在就查 dpkg/rpm 的归属，
// 把"它归谁管、该用哪条命令"作为失败原因返回，而不是笼统报一句卸载失败。
func aiCLIElevatedUninstallScript(app aiCLIApp, target string) string {
	var b strings.Builder
	b.WriteString(`export LC_ALL=C
# 提权后的 PATH 由 sudo 的 secure_path / su 的登录环境决定，未必包含 npm 所在目录。
for d in /usr/local/sbin /usr/local/bin /usr/sbin /usr/bin /sbin /bin; do
  case ":$PATH:" in *":$d:"*) ;; *) PATH="$PATH:$d" ;; esac
done
export PATH
if ! command -v npm >/dev/null 2>&1; then
  echo "目标机上找不到 npm，无法按 npm 全局包卸载" >&2
fi
`)
	fmt.Fprintf(&b, "npm uninstall -g %s >/dev/null 2>&1 || true\n", app.Pkg)
	for _, bin := range app.Bins {
		fmt.Fprintf(&b, "for p in /usr/local/bin/%s /usr/bin/%s; do\n", bin, bin)
		b.WriteString("  if [ -e \"$p\" ]; then rm -f \"$p\"; fi\ndone\n")
	}

	if target != "" {
		fmt.Fprintf(&b, "TARGET=%s\n", shellQuotePath(target))
	} else {
		b.WriteString("TARGET=''\n")
	}
	b.WriteString(`hash -r 2>/dev/null || true
BIN=''
`)
	for _, bin := range app.Bins {
		fmt.Fprintf(&b, "[ -z \"$BIN\" ] && BIN=$(command -v %s 2>/dev/null || true)\n", bin)
	}
	b.WriteString(`[ -z "$BIN" ] && [ -n "$TARGET" ] && [ -e "$TARGET" ] && BIN="$TARGET"
if [ -n "$BIN" ] && [ -e "$BIN" ]; then
  if command -v dpkg >/dev/null 2>&1; then
    OWNER=$(dpkg -S "$BIN" 2>/dev/null | sed 's/:.*//' | head -1)
    if [ -n "$OWNER" ]; then
      echo "该工具由系统包 $OWNER 提供，不是 npm 全局安装；卸载请执行：sudo apt-get remove -y $OWNER" >&2
      exit 4
    fi
  fi
  if command -v rpm >/dev/null 2>&1; then
    OWNER=$(rpm -qf "$BIN" 2>/dev/null | head -1)
    if [ -n "$OWNER" ]; then
      echo "该工具由系统包 $OWNER 提供，不是 npm 全局安装；卸载请执行：sudo dnf remove -y $OWNER" >&2
      exit 4
    fi
  fi
  echo "卸载后仍存在：$BIN（既不是 npm 全局包，也不归发行版包管理器管，需要手动删除）" >&2
  exit 4
fi
echo 已卸载
`)
	return b.String()
}

// handleAICLIUninstall 卸载一个工具并清掉软链。
func (s *Server) handleAICLIUninstall(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		ID string `json:"id"`
		// Elevated 表示这份工具装在系统 npm 全局前缀里（如 /usr/local），
		// 需要以 root 执行卸载；面板管理的用户级目录不需要它。
		Elevated bool `json:"elevated"`
		// Path 是页面探测到的可执行文件路径。有了它，卸载失败时能说清
		// "这份归系统包管"还是"不是包管理器装的"，而不是笼统报一句失败。
		Path string `json:"path"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	app, found := aiCLIByID(req.ID)
	if !found {
		writeErr(w, http.StatusBadRequest, "未知的 AI CLI："+req.ID)
		return
	}

	if req.Elevated {
		script := aiCLIElevatedUninstallScript(app, req.Path)
		res, err := s.execEscalated(r.Context(), hostID, script, dshPackageTimeout, escalateRequired)
		if err != nil {
			s.writeHostError(w, err, "卸载 "+app.Name+" 失败")
			return
		}
		if res.ExitCode != 0 {
			// 报错原文整段回给界面：卸载失败的原因常常藏在第二行（npm 的 ERR! 摘要之后）。
			writeErr(w, http.StatusBadGateway,
				"卸载 "+app.Name+" 失败：\n"+combinedOutput(res.Stdout, res.Stderr))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"output": combinedOutput(res.Stdout, res.Stderr),
		})
		return
	}

	var b strings.Builder
	// 卸载只处理面板自己装的那一份：先找回面板下载的 Node（安装时也是这么做的），
	// 再确认包确实在管理的目录里 —— 否则一律报错，不能"提示成功、实际还在"。
	b.WriteString(`export LC_ALL=C
R="$HOME/.panelssh"
ND="$R/node"
NB=$(ls -d "$ND"/node-v24.*/bin 2>/dev/null | sort -V | tail -n1)
if [ -n "$NB" ]; then PATH="$NB:$PATH"; fi
if ! command -v npm >/dev/null 2>&1; then
  echo "目标机上找不到 npm，无法卸载" >&2
  exit 3
fi
`)
	fmt.Fprintf(&b, "if [ ! -d \"%s/lib/node_modules/%s\" ]; then\n", aiCLIUserPrefix, app.Pkg)
	b.WriteString("  HIT=$(command -v " + app.Bins[0] + " 2>/dev/null || true)\n")
	fmt.Fprintf(&b, "  echo \"%s 不在面板管理的目录下（当前命中：${HIT:-未找到}），可能装在系统 npm 前缀或由发行版包提供；可改用页面上的「提权卸载」\" >&2\n", app.Name)
	b.WriteString("  exit 4\nfi\n")
	fmt.Fprintf(&b, "npm uninstall -g --prefix \"%s\" %s >/dev/null 2>&1 || true\n", aiCLIUserPrefix, app.Pkg)
	for _, bin := range app.Bins {
		fmt.Fprintf(&b, "rm -f \"$HOME/.local/bin/%s\"\n", bin)
	}
	b.WriteString("echo 已卸载\n")

	res, err := s.execOnHost(r.Context(), hostID, b.String(), defaultExecTimeout)
	if err != nil {
		s.writeHostError(w, err, "卸载 "+app.Name+" 失败")
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadGateway,
			"卸载 "+app.Name+" 失败：\n"+combinedOutput(res.Stdout, res.Stderr))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"output": combinedOutput(res.Stdout, res.Stderr),
	})
}

// combinedOutput 把 stdout / stderr 合起来作为界面展示的输出。
//
// 卸载脚本的成功提示走 stdout、失败原因走 stderr，界面上只有一个输出窗口，
// 因此这里统一拼起来，别让"有报错但窗口是空的"再发生一次。
func combinedOutput(stdout, stderr string) string {
	out := strings.TrimSpace(stdout)
	errOut := strings.TrimSpace(stderr)
	switch {
	case out == "":
		return errOut
	case errOut == "":
		return out
	}
	return out + "\n" + errOut
}

func aiCLIByID(id string) (aiCLIApp, bool) {
	for _, app := range aiCLIApps {
		if app.ID == id {
			return app, true
		}
	}
	return aiCLIApp{}, false
}
