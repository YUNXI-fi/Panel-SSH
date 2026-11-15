package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"panelssh/internal/env"
)

// maxSecurityConfigBytes 是配置文件读写的上限。
//
// 配置不是数据文件，超过这个量基本意味着选错了路径；限制住也能避免
// 把超大内容塞进一次 SSH 命令或 HTTP 响应。
const maxSecurityConfigBytes = 64 << 10

type securityRunResult struct {
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	DurationMS int64  `json:"durationMs"`
}

func (s *Server) securityAppFor(w http.ResponseWriter, r *http.Request) (*securityApp, int64, bool) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return nil, 0, false
	}
	if hostID != LocalHostID {
		if _, err := s.store.GetHost(hostID); err != nil {
			writeErr(w, http.StatusNotFound, "主机不存在或已被删除")
			return nil, 0, false
		}
	}
	// 应用仓库安装的同名包优先：它带来最新的元数据、界面配置与命令模板；
	// 没装包或包损坏时回退到编译期内置目录。
	app := s.packageApp(r.PathValue("slug"))
	if app == nil {
		app = securityApps[r.PathValue("slug")]
	}
	if app == nil {
		writeErr(w, http.StatusNotFound, "没有这个安全应用")
		return nil, 0, false
	}
	return app, hostID, true
}

// handleSecurityApp 返回一款应用的界面元数据：操作、表单字段与配置文件清单。
func (s *Server) handleSecurityApp(w http.ResponseWriter, r *http.Request) {
	app, _, ok := s.securityAppFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func normalizeSecurityParams(action *securityAction, raw map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(action.Fields))
	for _, f := range action.Fields {
		value := strings.TrimSpace(raw[f.Key])
		if value == "" {
			value = f.Default
		}
		if value == "" {
			if f.Required {
				return nil, fmt.Errorf("%s不能为空", f.Label)
			}
			out[f.Key] = ""
			continue
		}
		if f.maxLen > 0 && len(value) > f.maxLen {
			return nil, fmt.Errorf("%s过长", f.Label)
		}
		switch f.Type {
		case "number":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%s必须是正整数", f.Label)
			}
		case "select":
			valid := false
			for _, option := range f.Options {
				if option == value {
					valid = true
					break
				}
			}
			if !valid {
				return nil, fmt.Errorf("%s不是可选值", f.Label)
			}
		}
		if f.pattern != "" {
			// 模式都是本文件里的常量；编译失败属于代码错误，按格式不正确处理。
			if matched, err := regexp.MatchString(f.pattern, value); err != nil || !matched {
				return nil, fmt.Errorf("%s格式不正确", f.Label)
			}
		}
		out[f.Key] = value
	}
	return out, nil
}

// handleSecurityAction 执行一个白名单动作。
//
// 前端只能决定"做哪件事"和表单参数，不能提供命令本身；参数按字段声明
// 校验后再拼进固定命令，因此即使前端被篡改，执行面也仍然只有这份目录。
func (s *Server) handleSecurityAction(w http.ResponseWriter, r *http.Request) {
	app, hostID, ok := s.securityAppFor(w, r)
	if !ok {
		return
	}
	actionID := r.PathValue("action")
	var action *securityAction
	for _, item := range app.Actions {
		if item.ID == actionID {
			action = item
			break
		}
	}
	if action == nil || action.build == nil {
		writeErr(w, http.StatusNotFound, "没有这个操作")
		return
	}

	var body struct {
		Params map[string]string `json:"params"`
	}
	// 1 MiB：GnuPG 导入的 ASCII 公钥本身可能到 64 KiB，加上 JSON 转义后
	// 仍要留足余量；其余动作的参数都远小于这个量级。
	if err := decodeJSONBody(w, r, &body, 1<<20); err != nil {
		return
	}
	params, err := normalizeSecurityParams(action, body.Params)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	script, err := action.build(params)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	started := time.Now()
	res, err := s.runSecurityScript(r.Context(), hostID, script, action.needRoot, action.timeout)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "执行失败："+err.Error())
		return
	}
	output := strings.TrimSpace(res.Stdout + res.Stderr)
	if output == "" {
		output = "执行完成，没有输出。"
	}
	writeJSON(w, http.StatusOK, securityRunResult{
		OK: res.ExitCode == 0, ExitCode: res.ExitCode,
		Output: output, Stdout: res.Stdout, Stderr: res.Stderr,
		DurationMS: time.Since(started).Milliseconds(),
	})
}

// runSecurityScript 在目标主机上执行固定脚本。
//
// 需要 root 时走环境模块的提权链路（sudo -n / su / sudo -S），复用面板
// 已经配置好的提权凭据；拿不到提权路径时直接报错，而不是悄悄降权执行
// 然后让用户看到一个"命令失败"的假象。
func (s *Server) runSecurityScript(ctx context.Context, hostID int64, script string, needRoot bool, timeout time.Duration) (*env.ExecResult, error) {
	ex, _, err := s.envTarget(hostID)
	if err != nil {
		return nil, err
	}
	cmd := env.Command{Line: script}
	if needRoot {
		profile, err := s.hostProfile(ctx, hostID, false)
		if err != nil {
			return nil, err
		}
		if profile.PrivilegeMode() == env.PrivNone {
			return nil, errors.New("目标机没有可用的提权方式，请先在主机设置里配置 sudo/su 凭据")
		}
		cmd = env.Wrap(script, profile)
	}
	return ex.Run(ctx, cmd, timeout)
}

func (s *Server) securityConfigEntryFor(app *securityApp, path string) (*securityConfigEntry, bool) {
	for i := range app.Config {
		if app.Config[i].Path == path {
			return &app.Config[i], true
		}
	}
	return nil, false
}

// securityConfigNeedsRoot：/etc 下的配置通常只有 root 可读写；家目录里的
// 用户配置必须按登录用户执行，否则 sudo 会把 ~ 展开成 /root。
func securityConfigNeedsRoot(path string) bool {
	return !strings.HasPrefix(path, "~") && !strings.HasPrefix(path, "/home/")
}

// shellPath 把配置路径转成 shell 里的安全写法。
//
// 不能用单引号包 ~：单引号会阻止波浪号展开，cat '~/.gnupg/gpg.conf' 会去找
// 一个字面名为 ~ 的目录。家目录路径改用 "$HOME/..."，其余路径继续单引号。
func shellPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		rest := strings.ReplaceAll(path[2:], `"`, `\"`)
		return `"$HOME/` + rest + `"`
	}
	return shq(path)
}

// missingConfigMarker 用于区分"文件不存在"与"文件存在但内容为空"。
const missingConfigMarker = "__PANELSSH_CONFIG_MISSING__"

// handleSecurityConfigRead 读取一份配置文件。
func (s *Server) handleSecurityConfigRead(w http.ResponseWriter, r *http.Request) {
	app, hostID, ok := s.securityAppFor(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	entry, found := s.securityConfigEntryFor(app, path)
	if !found {
		writeErr(w, http.StatusBadRequest, "该路径不在这个应用的配置文件清单里")
		return
	}
	if entry.IsDir {
		writeErr(w, http.StatusBadRequest, "这是目录，请使用目录浏览")
		return
	}

	// 文件可能还不存在（比如 fail2ban 推荐新建的 jail.local）：
	// 不存在时返回空内容，让界面直接进入可编辑、可新建的状态。
	script := "if [ -e " + shellPath(path) + " ]; then cat " + shellPath(path) +
		"; else printf '" + missingConfigMarker + "'; fi"
	res, err := s.runSecurityScript(r.Context(), hostID, script, securityConfigNeedsRoot(path), 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取失败："+err.Error())
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, strings.TrimSpace(res.Stderr+res.Stdout))
		return
	}
	exists := res.Stdout != missingConfigMarker
	content := res.Stdout
	if !exists {
		content = ""
	}
	if len(content) > maxSecurityConfigBytes {
		writeErr(w, http.StatusBadRequest, "配置文件过大，无法在界面中编辑")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "content": content, "exists": exists, "backup": path + ".panelssh.bak",
	})
}

// handleSecurityConfigSave 保存配置文件。
//
// 先 cp -p 备份，再写临时文件替换：cp -p 保住原文件的权限与所有者，
// mv 做原子替换；中途失败时原文件不动，用户还能从 .panelssh.bak 恢复。
func (s *Server) handleSecurityConfigSave(w http.ResponseWriter, r *http.Request) {
	app, hostID, ok := s.securityAppFor(w, r)
	if !ok {
		return
	}
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeJSONBody(w, r, &body, 2<<20); err != nil {
		return
	}
	entry, found := s.securityConfigEntryFor(app, body.Path)
	if !found {
		writeErr(w, http.StatusBadRequest, "该路径不在这个应用的配置文件清单里")
		return
	}
	if entry.IsDir {
		writeErr(w, http.StatusBadRequest, "目录不能直接保存")
		return
	}
	if len(body.Content) > maxSecurityConfigBytes {
		writeErr(w, http.StatusBadRequest, "配置文件过大，无法在界面中保存")
		return
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(body.Content))
	path := body.Path
	backup := path + ".panelssh.bak"
	temp := path + ".panelssh.tmp"
	script := strings.Join([]string{
		"set -e",
		// 目标文件可能还不存在（例如 fail2ban 的 jail.local）。
		// 存在就保留权限做备份，不存在就按 0644 新建。
		"if [ -e " + shellPath(path) + " ]; then",
		"  cp -p " + shellPath(path) + " " + shellPath(backup),
		"  cp -p " + shellPath(path) + " " + shellPath(temp),
		"else",
		"  : > " + shellPath(temp),
		"  chmod 0644 " + shellPath(temp),
		"fi",
		"printf '%s' " + shq(encoded) + " | base64 -d > " + shellPath(temp),
		"mv " + shellPath(temp) + " " + shellPath(path),
	}, "\n")

	res, err := s.runSecurityScript(r.Context(), hostID, script, securityConfigNeedsRoot(path), 60*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "保存失败："+err.Error())
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, strings.TrimSpace(res.Stderr+res.Stdout))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "backup": backup})
}

// handleSecurityBrowse 列出目录型配置的内容（只读）。
func (s *Server) handleSecurityBrowse(w http.ResponseWriter, r *http.Request) {
	app, hostID, ok := s.securityAppFor(w, r)
	if !ok {
		return
	}
	path := r.URL.Query().Get("path")
	entry, found := s.securityConfigEntryFor(app, path)
	if !found {
		writeErr(w, http.StatusBadRequest, "该路径不在这个应用的配置文件清单里")
		return
	}
	if !entry.IsDir {
		writeErr(w, http.StatusBadRequest, "这不是目录")
		return
	}
	res, err := s.runSecurityScript(r.Context(), hostID, "ls -la "+shellPath(path), securityConfigNeedsRoot(path), 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取目录失败："+err.Error())
		return
	}
	if res.ExitCode != 0 {
		writeErr(w, http.StatusBadRequest, strings.TrimSpace(res.Stderr+res.Stdout))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "output": res.Stdout})
}
