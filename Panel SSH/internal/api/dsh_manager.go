package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"panelssh/internal/dsh"
)

// 管理器接口覆盖 dsh 的完整生命周期、工作区、插件与运行日志。
//
// 为什么不把这些塞进 handleDshEntry 的状态页里：状态页是给"工作台打不开"
// 这条异常路径用的自包含 HTML，改造成管理界面会让它失去"再坏也能显示"的性质。
// 管理器是正常路径，由前端窗口承担，后端只提供数据与动作。

const (
	// dshProbeTimeout 是读取版本/数据目录这类只读探测的上限。
	dshProbeTimeout = 30 * time.Second
	// dshPackageTimeout 是 npm 全局安装/升级的上限：拉包与解包都可能很慢。
	dshPackageTimeout = 15 * time.Minute
	// dshPluginTimeout 是 pnpm 插件操作的上限。
	dshPluginTimeout = 10 * time.Minute
)

// dshProbeScript 一次性问清 dsh 的安装形态。
//
// 只读、不提权：npm 的全局包列表普通用户就能读，而探测本身不该因为
// 拿不到 root 就失败 —— 拿不到 root 时用户至少还能看到现状。
const dshProbeScript = `H=${DSH_HOME:-$HOME/.dsh}
U="$HOME/.panelssh/dsh-runtime"
UN=$(ls -d "$HOME/.panelssh/node"/node-v24.*/bin/node 2>/dev/null | sort -V | tail -n1)
echo "data=$H"
echo "node=$(node -v 2>/dev/null)"
if [ -n "$UN" ] && [ -x "$UN" ]; then echo "unode=$("$UN" -v 2>/dev/null)"; fi
echo "version=$(npm ls -g --depth=0 @deepseek-ai/dsh 2>/dev/null | sed -n 's/.*@deepseek-ai\/dsh@\(.*\)/\1/p' | head -n1)"
echo "path=$(command -v dsh 2>/dev/null)"
if [ -f "$U/lib/node_modules/@deepseek-ai/dsh/package.json" ]; then
  echo "uversion=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$U/lib/node_modules/@deepseek-ai/dsh/package.json" | head -n1)"
fi
[ -x "$U/bin/dsh" ] && echo "upath=$U/bin/dsh"
pid=$(cat "$HOME/.panelssh/dsh/dsh.pid" 2>/dev/null)
echo "pid=$pid"
exit 0`

// dshInstallScript 是 npm 全局安装/升级的统一脚本。
//
// 固定 LC_ALL=C：npm 的解析结果不该被目标机的语言环境改变。
const dshInstallScript = `set -e
export LC_ALL=C
npm install -g @deepseek-ai/dsh@latest`

// dshReinstallScript 先卸再装，用于"强制重建"。
//
// 卸载失败不阻断安装：残留的全局包目录未必可写，但 install -g 会覆盖它，
// 因此这里只把卸载当成尽力而为的清理。
const dshReinstallScript = `export LC_ALL=C
npm uninstall -g @deepseek-ai/dsh >/dev/null 2>&1 || true
npm install -g @deepseek-ai/dsh@latest`

// dshProvisionScript 在目标机上做一次完全用户级的安装。
//
// 为什么需要它：Node.js 24 与系统包安装都要 root，而面板经常以普通用户运行
// （宿主机尤其如此，没有主机记录也就没地方存 sudo 口令）。dsh 的依赖要求
// Node >= 22.19，系统自带的 Node 20 不够，因此这里把 Node 24 官方二进制包
// 解到用户家目录，再用它自带的 npm 把 dsh 装到用户前缀，全程不需要提权。
//
// 只输出两个绝对路径：启动命令由后端拼成 "<node> <bin.js>"，因为用户前缀
// 里的 dsh 首行是 `#!/usr/bin/env node`，而 Node 24 不在登录 shell 的 PATH 上。
const dshProvisionScript = `set -e
export LC_ALL=C
R="$HOME/.panelssh"
ND="$R/node"
PFX="$R/dsh-runtime"
mkdir -p "$ND" "$PFX"

case "$(uname -m)" in
  x86_64|amd64) A=x64 ;;
  aarch64|arm64) A=arm64 ;;
  *) echo "不支持的 CPU 架构：$(uname -m)" >&2; exit 4 ;;
esac

NB=$(ls -d "$ND"/node-v24.*/bin/node 2>/dev/null | sort -V | tail -n1)
if [ -z "$NB" ] || [ ! -x "$NB" ]; then
  V=""
  BASE=""
  for B in https://nodejs.org/dist https://npmmirror.com/mirrors/node; do
    V=$(wget -qO- "$B/index.tab" 2>/dev/null | awk '$1 ~ /^v24\./ {print $1; exit}')
    if [ -n "$V" ]; then BASE="$B"; break; fi
  done
  [ -n "$V" ] || { echo "无法获取 Node 24 的下载地址" >&2; exit 5; }

  T="node-$V-linux-$A.tar.xz"
  TMP=$(mktemp -d)
  trap 'rm -rf "$TMP"' EXIT
  wget -q -O "$TMP/$T" "$BASE/$V/$T"
  tar -xJf "$TMP/$T" -C "$ND"
  NB="$ND/node-$V-linux-$A/bin/node"
fi

NDIR=$(dirname "$(dirname "$NB")")
export PATH="$NDIR/bin:$PATH"
"$NB" -v

npm install -g --prefix "$PFX" @deepseek-ai/dsh@latest --loglevel=error --no-audit --no-fund --fetch-timeout=120000 --fetch-retries=3

BIN="$PFX/lib/node_modules/@deepseek-ai/dsh/lib/bin.js"
[ -f "$BIN" ] || { echo "安装完成但找不到 dsh 入口文件" >&2; exit 6; }

echo "NODE=$NB"
echo "DSH=$BIN"`

// dshOverviewView 是管理器首屏需要的全部事实。
type dshOverviewView struct {
	Available   bool      `json:"available"`
	Installed   bool      `json:"installed"`
	State       string    `json:"state"`
	Message     string    `json:"message"`
	URL         string    `json:"url"`
	Command     string    `json:"command"`
	Version     string    `json:"version"`
	NodeVersion string    `json:"nodeVersion"`
	DataDir     string    `json:"dataDir"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	Logs        []string  `json:"logs"`
}

func (s *Server) dshOverviewValue(ctx context.Context, hostID int64) dshOverviewView {
	sup := s.dsh.Supervisor(hostID)
	status := sup.Status()

	view := dshOverviewView{
		Available: sup.Available(),
		State:     string(status.State),
		Message:   status.Message,
		Command:   sup.Command(),
		PID:       status.PID,
		StartedAt: status.StartedAt,
		Logs:      status.Logs,
	}
	if status.State == dsh.StateRunning {
		view.URL = dshPrefixFor(hostID) + "/"
	}

	if res, err := s.execOnHost(ctx, hostID, dshProbeScript, dshProbeTimeout); err == nil {
		for _, line := range strings.Split(res.Stdout, "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			switch key {
			case "data":
				view.DataDir = value
			case "node":
				view.NodeVersion = value
			case "unode":
				// 用户级安装有自己的 Node 24；系统 Node 版本不代表 dsh 实际用的那个。
				if value != "" {
					view.NodeVersion = value
				}
			case "version":
				view.Version = value
			case "uversion":
				// 用户级安装优先：它才是 dshProvisionScript 装出来的那一个。
				if value != "" {
					view.Version = value
					view.Installed = true
				}
			case "path", "upath":
				if value != "" {
					view.Installed = true
				}
			case "pid":
				if value != "" && view.PID == 0 {
					if pid, err := strconv.Atoi(value); err == nil {
						view.PID = pid
					}
				}
			}
		}
	} else if view.Message == "" {
		// 探测失败通常是连不上目标机；把它说出来，否则界面会表现为"没安装"。
		view.Message = "读取目标机上的安装信息失败：" + err.Error()
	}
	if view.Version != "" {
		view.Installed = true
	}
	return view
}

func (s *Server) handleDshOverview(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.dshOverviewValue(r.Context(), hostID))
}

// startDshAsync 在后台拉起 dsh，并立刻把当前状态交给前端。
//
// 同步等待会占住一次 HTTP 请求最长 120 秒（首次启动要装载数百个插件），
// 前端在这期间什么也做不了。改为后台启动 + 前端轮询，是这套状态机本来的用法。
func (s *Server) startDshAsync(hostID int64) {
	sup := s.dsh.Supervisor(hostID)
	if !sup.Available() {
		return
	}
	if sup.Status().State == dsh.StateStarting {
		return
	}
	go func() {
		if err := sup.EnsureRunning(); err != nil {
			return
		}
		// 远端主机：趁用户还在看加载动画，把首屏资源顺序抓到面板本地。
		// 本机直连不需要，免得白占磁盘和 CPU。
		if hostID != LocalHostID {
			s.dshCache.startWarm(s, hostID, true)
		}
	}()
}

func (s *Server) handleDshStart(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	sup := s.dsh.Supervisor(hostID)
	if !sup.Available() {
		writeErr(w, http.StatusBadRequest, "尚未安装 dsh 或未回填启动命令")
		return
	}
	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "收到启动请求")
	s.startDshAsync(hostID)
	writeJSON(w, http.StatusOK, s.dshStatusViewValue(hostID))
}

func (s *Server) handleDshStop(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "收到停止请求")
	s.dsh.Stop(hostID)
	s.dshNative.close(hostID)
	s.dshClients.close(hostID)
	writeJSON(w, http.StatusOK, s.dshStatusViewValue(hostID))
}

func (s *Server) handleDshRestart(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	sup := s.dsh.Supervisor(hostID)
	if !sup.Available() {
		writeErr(w, http.StatusBadRequest, "尚未安装 dsh 或未回填启动命令")
		return
	}
	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "收到重启请求")
	s.dsh.Stop(hostID)
	s.dshNative.close(hostID)
	s.startDshAsync(hostID)
	writeJSON(w, http.StatusOK, s.dshStatusViewValue(hostID))
}

// handleDshUpdate 与 handleDshReinstall 共用一次全局包操作。
//
// 安装本身走环境配方（Node.js 要先就位），这里只负责"已经能调用 npm 之后"的
// 升级与重建 —— 两者都是 `npm install -g @deepseek-ai/dsh@latest`，npm 对
// 已安装的包就是覆盖安装，不需要为"没装"和"已装"写两条路径。
func (s *Server) handleDshUpdate(w http.ResponseWriter, r *http.Request) {
	s.runDshPackageOp(w, r, "更新", dshInstallScript, false)
}

func (s *Server) handleDshReinstall(w http.ResponseWriter, r *http.Request) {
	s.runDshPackageOp(w, r, "重装", dshReinstallScript, true)
}

// handleDshProvision 做一次不需要 root 的用户级安装。
//
// 安装完成后直接回填启动命令并拉起实例：这三步对用户是一个动作，
// 中间任何一步单独暴露出来都只会让人问"接下来呢"。
func (s *Server) handleDshProvision(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "开始用户级安装（Node 24 + dsh）")
	res, err := s.execOnHost(r.Context(), hostID, dshProvisionScript, dshPackageTimeout)
	if err != nil {
		dsh.AppendLog(s.dshLogPath(hostID), "ERROR", "用户级安装失败："+err.Error())
		s.writeHostError(w, err, "用户级安装失败")
		return
	}
	if res.ExitCode != 0 {
		detail := lastLine(res.Stderr)
		if detail == "" {
			detail = lastLine(res.Stdout)
		}
		if detail == "" {
			detail = fmt.Sprintf("命令退出码 %d", res.ExitCode)
		}
		dsh.AppendLog(s.dshLogPath(hostID), "ERROR", "用户级安装失败："+detail)
		writeErr(w, http.StatusBadGateway, "用户级安装失败："+detail)
		return
	}

	node, bin := "", ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "NODE":
			node = value
		case "DSH":
			bin = value
		}
	}
	if !strings.HasPrefix(node, "/") || !strings.HasPrefix(bin, "/") {
		writeErr(w, http.StatusBadGateway, "安装完成但没有拿到可用的启动路径")
		return
	}

	command := node + " " + bin
	if err := s.dsh.SetCommand(hostID, command); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存启动命令失败")
		return
	}
	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "用户级安装完成，启动 dsh")
	s.startDshAsync(hostID)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "用户级安装完成",
		"command": command,
	})
}

// dshUserRuntime 从启动命令认出"用户级安装"并拆出 node 与 npm 前缀。
//
// 判据是命令形如 "<node> .../.panelssh/dsh-runtime/lib/node_modules/.../bin.js"：
// 这是 dshProvisionScript 生成的唯一形态，系统安装的 dsh 是一条单命令，
// 不会命中。
func dshUserRuntime(command string) (node, prefix string, ok bool) {
	fields := strings.Fields(command)
	if len(fields) < 2 {
		return "", "", false
	}
	bin := fields[1]
	marker := "/.panelssh/dsh-runtime/"
	idx := strings.Index(bin, marker)
	if idx <= 0 {
		return "", "", false
	}
	prefix = bin[:idx] + "/.panelssh/dsh-runtime"
	return fields[0], prefix, true
}

// dshUserPackageScript 生成用户级安装的升级/重装脚本。
//
// 必须先把自己的 node 目录放进 PATH：用户前缀没有环境变量注入，
// npm 本身的 shebang 也是 `#!/usr/bin/env node`。
// --no-audit/-no-fund 与显式 fetch 超时是防挂用的：在线安装实测会在
// 收尾阶段卡在一个不返回的审计/资助请求上，包其实已经装好了。
func dshUserPackageScript(node, prefix string, reinstall bool) string {
	script := "set -e\nexport LC_ALL=C\nexport PATH=" + shellArg(pathDir(node)) + ":$PATH\n"
	if reinstall {
		script += "npm uninstall -g --prefix " + shellArg(prefix) + " @deepseek-ai/dsh >/dev/null 2>&1 || true\n"
	}
	return script + "npm install -g --prefix " + shellArg(prefix) +
		" @deepseek-ai/dsh@latest --loglevel=error --no-audit --no-fund" +
		" --fetch-timeout=120000 --fetch-retries=3"
}

// pathDir 取一个目标机路径的父目录。
//
// 用 path 而不是 filepath：这是目标机上的 Unix 路径，与面板所在平台无关。
func pathDir(value string) string {
	idx := strings.LastIndex(value, "/")
	if idx <= 0 {
		return "/"
	}
	return value[:idx]
}

// runDshPackageOp 执行一次全局包操作，成功后回填启动命令并重启。
//
// 安装形态决定用哪套 npm：用户级安装（~/.panelssh/dsh-runtime）不需要提权，
// 系统安装才走 root。判据是启动命令本身，因此面板重启后仍然成立。
func (s *Server) runDshPackageOp(w http.ResponseWriter, r *http.Request, label, systemScript string, reinstall bool) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "开始"+label+" dsh")

	sup := s.dsh.Supervisor(hostID)
	var res *execResult
	var err error
	if node, prefix, userLevel := dshUserRuntime(sup.Command()); userLevel {
		res, err = s.execOnHost(r.Context(), hostID, dshUserPackageScript(node, prefix, reinstall), dshPackageTimeout)
	} else {
		res, err = s.execEscalated(r.Context(), hostID, systemScript, dshPackageTimeout, escalatePreferred)
	}
	if err != nil {
		dsh.AppendLog(s.dshLogPath(hostID), "ERROR", label+"失败："+err.Error())
		s.writeHostError(w, err, label+" dsh 失败")
		return
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		if detail == "" {
			detail = fmt.Sprintf("命令退出码 %d", res.ExitCode)
		}
		dsh.AppendLog(s.dshLogPath(hostID), "ERROR", label+"失败："+firstLine(detail))
		writeErr(w, http.StatusBadGateway, label+"失败："+firstLine(detail))
		return
	}

	// 回填命令是"装完能不能用"的唯一判据：装在哪儿由目标机的 npm 前缀决定。
	detect, detectErr := s.execEscalated(r.Context(), hostID, dshDetectScript, defaultExecTimeout, escalatePreferred)
	if detectErr != nil || detect.ExitCode != 0 {
		dsh.AppendLog(s.dshLogPath(hostID), "WARN", label+"完成，但没能定位 dsh 可执行文件")
		writeErr(w, http.StatusBadGateway, label+"完成，但在目标机上找不到 dsh 可执行文件")
		return
	}
	command := firstLine(detect.Stdout)
	if err := s.dsh.SetCommand(hostID, command); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存启动命令失败")
		return
	}

	dsh.AppendLog(s.dshLogPath(hostID), "INFO", label+"完成，重启 dsh")
	s.dsh.Stop(hostID)
	s.dshNative.close(hostID)
	s.startDshAsync(hostID)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": label + "完成",
		"output":  strings.TrimSpace(res.Stdout),
	})
}

// handleDshLogs 返回面板侧留存的运行日志。
func (s *Server) handleDshLogs(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	limit := 500
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	lines, err := dsh.ReadLogTail(s.dshLogPath(hostID), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取日志失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) handleDshClearLogs(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	if err := dsh.TruncateLog(s.dshLogPath(hostID)); err != nil {
		writeErr(w, http.StatusInternalServerError, "清空日志失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// dshWorkspaceView 是工作区页的一行。
type dshWorkspaceView struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Path         string `json:"path"`
	SessionCount int    `json:"sessionCount"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

// dshWorkspaceFile 是 dsh 的 workspace 存储单元（single 布局的整单元文档）。
//
// 只取用得到的字段：这份文件由 dsh 自己维护，面板是只读消费方，
// 多声明一个字段就多一份跟随上游变化的风险。
type dshWorkspaceFile struct {
	Global struct {
		WorkspaceIDs       []string `json:"workspaceIds"`
		ArchivedSessionIDs []string `json:"archivedSessionIds"`
	} `json:"global"`
	Tables struct {
		Workspaces map[string]struct {
			Path       string   `json:"path"`
			Title      string   `json:"title"`
			SessionIDs []string `json:"sessionIds"`
			CreatedAt  string   `json:"createdAt"`
			UpdatedAt  string   `json:"updatedAt"`
		} `json:"workspaces"`
	} `json:"tables"`
}

const dshWorkspaceScript = `H=${DSH_HOME:-$HOME/.dsh}
cat "$H/storages/workspace.json" 2>/dev/null
exit 0`

func (s *Server) handleDshWorkspaces(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}

	res, err := s.execOnHost(r.Context(), hostID, dshWorkspaceScript, dshProbeTimeout)
	if err != nil {
		s.writeHostError(w, err, "读取工作区失败")
		return
	}

	views := make([]dshWorkspaceView, 0)
	if strings.TrimSpace(res.Stdout) != "" {
		parsed, err := parseDshWorkspaces([]byte(res.Stdout))
		if err != nil {
			writeErr(w, http.StatusBadGateway, "dsh 的工作区 registry 解析失败："+err.Error())
			return
		}
		views = parsed
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": views})
}

// parseDshWorkspaces 把 dsh 的 workspace 存储单元转成界面要的行。
//
// 抽成纯函数是为了能直接验证顺序、归档过滤与"记录存在但不在顺序里"这三条规则 ——
// 它们决定了用户在界面上看到的数量与次序，靠手工点页面验证太慢。
func parseDshWorkspaces(raw []byte) ([]dshWorkspaceView, error) {
	var file dshWorkspaceFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}

	archived := make(map[string]struct{}, len(file.Global.ArchivedSessionIDs))
	for _, id := range file.Global.ArchivedSessionIDs {
		archived[id] = struct{}{}
	}

	ordered := make([]string, 0, len(file.Tables.Workspaces))
	for _, id := range file.Global.WorkspaceIDs {
		if _, ok := file.Tables.Workspaces[id]; ok {
			ordered = append(ordered, id)
		}
	}
	// 记录存在但不在顺序里（例如刚创建完就被中断）也要显示出来，
	// 否则用户会在界面上丢失一个磁盘上确实存在的工作区。
	// 这些孤儿没有权威顺序，按最近更新时间排，避免每次刷新位置乱跳。
	orphans := make([]string, 0)
	for id := range file.Tables.Workspaces {
		found := false
		for _, known := range ordered {
			if known == id {
				found = true
				break
			}
		}
		if !found {
			orphans = append(orphans, id)
		}
	}
	sort.Slice(orphans, func(i, j int) bool {
		return file.Tables.Workspaces[orphans[i]].UpdatedAt >
			file.Tables.Workspaces[orphans[j]].UpdatedAt
	})
	ordered = append(ordered, orphans...)

	views := make([]dshWorkspaceView, 0, len(ordered))
	for _, id := range ordered {
		record := file.Tables.Workspaces[id]
		count := 0
		for _, sessionID := range record.SessionIDs {
			if _, hidden := archived[sessionID]; !hidden {
				count++
			}
		}
		views = append(views, dshWorkspaceView{
			ID:           id,
			Title:        record.Title,
			Path:         record.Path,
			SessionCount: count,
			CreatedAt:    record.CreatedAt,
			UpdatedAt:    record.UpdatedAt,
		})
	}
	return views, nil
}

// dshPluginView 是插件页的一行。
type dshPluginView struct {
	Name        string `json:"name"`
	Spec        string `json:"spec"`
	Enabled     bool   `json:"enabled"`
	Bundle      bool   `json:"bundle"`
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
}

// dshPluginListScript 读取 web profile 的依赖清单。
//
// 用 node 而不是在 Go 里解析 shell 输出：目标机上装了 dsh 就一定有 node，
// 而 package.json 的转义、作用域包名这些细节交给 JSON 解析器处理最稳妥。
// 只把声明了 dsh.bundle 的依赖当成"插件"——普通依赖是库，不该出现在插件页。
const dshPluginListScript = `H=${DSH_HOME:-$HOME/.dsh}
P="$H/profiles/web"
[ -f "$P/package.json" ] || { echo "[]"; exit 0; }
node - "$P" <<'NODE'
const fs = require("fs"), path = require("path");
const dir = process.argv[2];
const m = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
const bundles = (m.dsh && m.dsh.profile && m.dsh.profile.bundles) || [];
const out = [];
for (const name of Object.keys(m.dependencies || {})) {
  let bundle = false, description = "", homepage = "";
  try {
    const q = JSON.parse(fs.readFileSync(path.join(dir, "node_modules", name, "package.json"), "utf8"));
    bundle = !!(q.dsh && q.dsh.bundle);
    description = q.description || "";
    homepage = typeof q.homepage === "string" ? q.homepage : "";
  } catch (e) {}
  if (!bundle) continue;
  out.push({
    name: name,
    spec: m.dependencies[name],
    enabled: bundles.indexOf(name) >= 0,
    bundle: true,
    description: description,
    homepage: homepage,
  });
}
process.stdout.write(JSON.stringify(out));
NODE`

func (s *Server) handleDshPlugins(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	res, err := s.execOnHost(r.Context(), hostID, dshPluginListScript, dshProbeTimeout)
	if err != nil {
		s.writeHostError(w, err, "读取插件清单失败")
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadGateway, "读取插件清单失败："+firstLine(res.Stderr))
		return
	}

	plugins := make([]dshPluginView, 0)
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &plugins); err != nil {
		writeErr(w, http.StatusBadGateway, "插件清单解析失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": plugins})
}

// dshPluginSpecPattern 约束安装 spec，避免把任意 shell 拼进脚本。
//
// 它允许 npm 常见的包名、版本范围、标签、git/文件路径，但拒绝空格、引号、
// 分号与反引号 —— 这些是"能不能拼进命令"的边界，不是功能限制。
var dshPluginSpecPattern = regexp.MustCompile(`^[A-Za-z0-9@._/:~^+*=-]{1,200}$`)

var dshPluginNamePattern = regexp.MustCompile(`^(@[A-Za-z0-9._-]+/)?[A-Za-z0-9._-]{1,120}$`)

// dshPluginProfilePrelude 是每个插件操作共用的前置：定位 profile 并确认它已初始化。
const dshPluginProfilePrelude = `set -e
export LC_ALL=C
H=${DSH_HOME:-$HOME/.dsh}
P="$H/profiles/web"
[ -f "$P/package.json" ] || { echo "web profile 尚未初始化，请先启动一次 dsh" >&2; exit 3; }
cd "$P"
`

// dshPluginPrelude 在公共前置之后补上 PATH。
//
// 面板执行命令是非登录 shell，~/.local/bin（pnpm 常见安装位置）未必在 PATH 上；
// 用户级安装还必须用它自己的 Node 24，否则 pnpm 的 shebang 会落到系统 Node 20。
func (s *Server) dshPluginPrelude(hostID int64) string {
	prelude := dshPluginProfilePrelude
	if node, _, ok := dshUserRuntime(s.dsh.Supervisor(hostID).Command()); ok {
		prelude += "export PATH=" + shellArg(pathDir(node)) + ":$HOME/.local/bin:$PATH\n"
	} else {
		prelude += "export PATH=\"$HOME/.local/bin:$PATH\"\n"
	}
	return prelude
}

// handleDshPluginInstall 安装一个插件包（pnpm add），并把新出现的组合包加入启用列表。
func (s *Server) handleDshPluginInstall(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		Spec string `json:"spec"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	spec := strings.TrimSpace(req.Spec)
	if !dshPluginSpecPattern.MatchString(spec) {
		writeErr(w, http.StatusBadRequest, "插件 spec 不合法")
		return
	}

	script := s.dshPluginPrelude(hostID) +
		`BEFORE=$(node - "$P" <<'NODE'
const fs = require("fs"), path = require("path");
const m = JSON.parse(fs.readFileSync(path.join(process.argv[2], "package.json"), "utf8"));
process.stdout.write(Object.keys(m.dependencies || {}).join("\n"));
NODE
)
pnpm add ` + shellArg(spec) + `
BEFORE="$BEFORE" node - "$P" <<'NODE'
const fs = require("fs"), path = require("path");
const dir = process.argv[2];
const m = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
m.dsh = m.dsh || {};
m.dsh.profile = m.dsh.profile || {};
const bundles = Array.isArray(m.dsh.profile.bundles) ? m.dsh.profile.bundles : [];
const before = new Set((process.env.BEFORE || "").split("\n").filter(Boolean));
for (const name of Object.keys(m.dependencies || {})) {
  if (before.has(name)) continue;
  try {
    const q = JSON.parse(fs.readFileSync(path.join(dir, "node_modules", name, "package.json"), "utf8"));
    if (q.dsh && q.dsh.bundle && bundles.indexOf(name) < 0) bundles.push(name);
  } catch (e) {}
}
m.dsh.profile.bundles = bundles;
fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify(m, null, 2) + "\n");
NODE`

	s.runDshPluginOp(w, r, hostID, "安装插件", script)
}

func (s *Server) handleDshPluginUpdate(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !dshPluginNamePattern.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "插件名不合法")
		return
	}
	script := s.dshPluginPrelude(hostID) + "pnpm add " + shellArg(name+"@latest")
	s.runDshPluginOp(w, r, hostID, "更新插件 "+name, script)
}

func (s *Server) handleDshPluginRemove(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !dshPluginNamePattern.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "插件名不合法")
		return
	}

	// 顺序与 dsh 自己的卸载一致：先取消选择，再移除依赖。
	script := s.dshPluginPrelude(hostID) + dshBundleScript(name, false) +
		"\npnpm remove " + shellArg(name)
	s.runDshPluginOp(w, r, hostID, "卸载插件 "+name, script)
}

func (s *Server) handleDshPluginEnabled(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !dshPluginNamePattern.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "插件名不合法")
		return
	}

	script := s.dshPluginPrelude(hostID) + dshBundleScript(name, req.Enabled)
	label := "停用插件 " + name
	if req.Enabled {
		label = "启用插件 " + name
	}
	s.runDshPluginOp(w, r, hostID, label, script)
}

// dshBundleScript 生成"增删 dsh.profile.bundles"的 node 片段。
//
// 组合包选择是 dsh 自己持久化的启用状态（见上游 plugin-manager）：
// 关闭只取消选择、保留依赖，开启追加到列表末尾。面板直接改这份清单，
// 与 Web 端插件页写的是同一个字段。
func dshBundleScript(name string, enabled bool) string {
	action := "remove"
	if enabled {
		action = "add"
	}
	return `
node - "$P" ` + shellArg(name) + ` ` + action + ` <<'NODE'
const fs = require("fs"), path = require("path");
const dir = process.argv[2], name = process.argv[3], action = process.argv[4];
const m = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
m.dsh = m.dsh || {};
m.dsh.profile = m.dsh.profile || {};
let bundles = Array.isArray(m.dsh.profile.bundles) ? m.dsh.profile.bundles : [];
if (action === "add") {
  if (bundles.indexOf(name) < 0) bundles.push(name);
} else {
  bundles = bundles.filter(function (item) { return item !== name; });
}
m.dsh.profile.bundles = bundles;
fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify(m, null, 2) + "\n");
NODE`
}

// runDshPluginOp 执行一次插件变更；成功后重启 dsh 让新组合生效。
func (s *Server) runDshPluginOp(w http.ResponseWriter, r *http.Request, hostID int64, label, script string) {
	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "开始"+label)
	res, err := s.execOnHost(r.Context(), hostID, script, dshPluginTimeout)
	if err != nil {
		dsh.AppendLog(s.dshLogPath(hostID), "ERROR", label+"失败："+err.Error())
		s.writeHostError(w, err, label+"失败")
		return
	}
	if res.ExitCode != 0 {
		detail := firstLine(res.Stderr)
		if detail == "" {
			detail = firstLine(res.Stdout)
		}
		if detail == "" {
			detail = fmt.Sprintf("命令退出码 %d", res.ExitCode)
		}
		dsh.AppendLog(s.dshLogPath(hostID), "ERROR", label+"失败："+detail)
		writeErr(w, http.StatusBadGateway, label+"失败："+detail)
		return
	}

	dsh.AppendLog(s.dshLogPath(hostID), "INFO", label+"完成，重启 dsh")
	if s.dsh.Supervisor(hostID).Available() {
		s.dsh.Stop(hostID)
		s.dshNative.close(hostID)
		s.startDshAsync(hostID)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": label + "完成"})
}

// shellArg 把参数包成 shell 单引号字面量。
//
// spec 与插件名已经过白名单校验，这里仍然加引号：校验是"不该出现"的第一道闸，
// 引号是"即使出现了也不会被当成命令"的第二道。
func shellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// lastLine 取输出的最后一行非空内容。
//
// 安装失败时真正的原因通常在 stderr 末尾（npm 的报错会先打上下文），
// 用第一行只会拿到一句无关紧要的警告。
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
