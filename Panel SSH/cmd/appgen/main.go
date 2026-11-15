// Command appgen 生成 apps/ 下的应用包与索引。
//
// 用法（在 Panel SSH 模块目录下）：
//
//	go run ./cmd/appgen -out ../apps
//	go run ./cmd/appgen -check -out ../apps
//
// 元数据来自 internal/env 的软件目录，安全应用的动作来自 internal/api 的
// 编译期动作清单；两者都是运行时真正使用的那一份，避免"文档一套、执行一套"。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"panelssh/internal/api"
	"panelssh/internal/appstore"
	"panelssh/internal/env"
)

const packageVersion = "1.0.0"

// excludedSlugs 是已经有专用窗口、不需要通用应用包的条目。
// 继续打包会在桌面上多出一个同名的通用图标，与专用窗口抢入口。
var excludedSlugs = map[string]bool{
	"docker": true, // DockerWindow
	"dsh":    true, // DSH 工作台（内置插件）
}

func main() {
	out := flag.String("out", "../apps", "应用包输出目录")
	check := flag.Bool("check", false, "只校验已有的 apps/，不重新生成")
	flag.Parse()

	if *check {
		if err := checkPackages(*out); err != nil {
			fmt.Fprintln(os.Stderr, "校验失败："+err.Error())
			os.Exit(1)
		}
		return
	}
	if err := generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, "生成失败："+err.Error())
		os.Exit(1)
	}
}

func generate(out string) error {
	recipes := env.Catalog()
	sort.Slice(recipes, func(i, j int) bool { return recipes[i].Slug < recipes[j].Slug })

	index := appstore.Index{
		SchemaVersion: appstore.SchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Apps:          make([]appstore.Entry, 0, len(recipes)),
	}

	for _, recipe := range recipes {
		if excludedSlugs[recipe.Slug] {
			continue
		}
		dir := filepath.Join(out, recipe.Slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		actions := buildActions(recipe)
		defaultAct := defaultAction(actions)
		if builtinDefault := api.PackageDefaultAction(recipe.Slug); builtinDefault != "" {
			defaultAct = builtinDefault
		}
		manifest := appstore.Manifest{
			SchemaVersion: appstore.SchemaVersion,
			Slug:          recipe.Slug,
			Name:          recipe.Name,
			Version:       packageVersion,
			Category:      recipe.Category,
			Description:   recipe.Description,
			UseCase:       recipe.UseCase,
			Note:          recipe.Note,
			Icon:          "icon.svg",
			DefaultAction: defaultAct,
			Config:        configPaths(recipe),
			Actions:       actions,
		}
		ui := buildUI(recipe, actions, manifest.DefaultAction)

		if err := appstore.ValidateManifest(&manifest); err != nil {
			return fmt.Errorf("%s：%w", recipe.Slug, err)
		}
		if err := writeJSON(filepath.Join(dir, "app.json"), manifest); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(dir, "ui.json"), ui); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "icon.svg"), []byte(iconFor(recipe)), 0o644); err != nil {
			return err
		}

		files, err := hashDir(dir)
		if err != nil {
			return err
		}
		index.Apps = append(index.Apps, appstore.Entry{
			Slug: recipe.Slug, Name: recipe.Name, Version: packageVersion,
			Category: recipe.Category, Description: recipe.Description,
			Icon: "icon.svg", Manifest: "app.json", UI: "ui.json",
			Files: files,
		})
	}

	if err := appstore.ValidateIndex(&index); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "index.json"), index); err != nil {
		return err
	}
	fmt.Printf("已生成 %d 个应用包 → %s\n", len(index.Apps), out)
	return nil
}

// checkPackages 校验索引、清单、文件摘要与图标规格。
func checkPackages(out string) error {
	raw, err := os.ReadFile(filepath.Join(out, "index.json"))
	if err != nil {
		return err
	}
	var index appstore.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		return err
	}
	if err := appstore.ValidateIndex(&index); err != nil {
		return err
	}
	for _, entry := range index.Apps {
		dir := filepath.Join(out, entry.Slug)
		manifestRaw, err := os.ReadFile(filepath.Join(dir, entry.Manifest))
		if err != nil {
			return fmt.Errorf("%s：%w", entry.Slug, err)
		}
		var manifest appstore.Manifest
		if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
			return fmt.Errorf("%s/app.json：%w", entry.Slug, err)
		}
		if err := appstore.ValidateManifest(&manifest); err != nil {
			return fmt.Errorf("%s：%w", entry.Slug, err)
		}
		if manifest.Slug != entry.Slug || manifest.Version != entry.Version {
			return fmt.Errorf("%s：索引与清单不一致", entry.Slug)
		}
		for _, file := range entry.Files {
			content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file.Path)))
			if err != nil {
				return fmt.Errorf("%s/%s：%w", entry.Slug, file.Path, err)
			}
			sum := sha256.Sum256(content)
			if hex.EncodeToString(sum[:]) != strings.ToLower(file.SHA256) {
				return fmt.Errorf("%s/%s：摘要不一致", entry.Slug, file.Path)
			}
		}
		icon, err := os.ReadFile(filepath.Join(dir, entry.Icon))
		if err != nil {
			return fmt.Errorf("%s：%w", entry.Slug, err)
		}
		if !strings.Contains(string(icon), `viewBox="0 0 24 24"`) {
			return fmt.Errorf("%s：图标必须是 24x24 viewBox 的 SVG", entry.Slug)
		}
	}
	fmt.Printf("校验通过：%d 个应用包\n", len(index.Apps))
	return nil
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

func hashDir(dir string) ([]appstore.FileRef, error) {
	files := make([]appstore.FileRef, 0)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		files = append(files, appstore.FileRef{
			Path: filepath.ToSlash(rel), SHA256: hex.EncodeToString(sum[:]),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// ---------------- 动作 ----------------

func buildActions(recipe *env.Recipe) []appstore.Action {
	// 网络安全应用继续使用编译期动作：GnuPG 的密钥生成、Fail2Ban 的
	// 状态卡片都需要多步编排，包只带元数据与界面配置。
	if actions := api.PackageActions(recipe.Slug); len(actions) > 0 {
		return actions
	}

	actions := []appstore.Action{versionAction(recipe)}
	units := unitNames(recipe.ServiceUnits)
	if len(units) > 0 {
		actions = append(actions,
			statusAction(units),
			logsAction(units),
			serviceAction("start", "启动服务", "启动后服务会立即对外提供能力。", units, false),
			serviceAction("restart", "重启服务", "重启会短暂中断正在使用的连接。", units, false),
			serviceAction("stop", "停止服务", "停止后服务不再对外提供能力，直到再次启动。", units, true),
		)
	}
	return actions
}

func versionAction(recipe *env.Recipe) appstore.Action {
	command := ""
	if recipe.DetectCommand != nil {
		command = strings.TrimSpace(recipe.DetectCommand(&env.Profile{Family: env.FamilyApt}))
	}
	if command == "" {
		command = "command -v " + shellSingle(recipe.Slug) + " 2>/dev/null || echo 未找到 " + shellSingle(recipe.Slug)
	}
	return appstore.Action{
		ID: "version", Label: "版本信息", Group: "inspect",
		Command: command, TimeoutSec: 30,
	}
}

func statusAction(units []string) appstore.Action {
	command := unitScript(units) + `if command -v systemctl >/dev/null 2>&1 && systemctl cat "$unit" >/dev/null 2>&1; then
  systemctl status "$unit" --no-pager
elif command -v rc-service >/dev/null 2>&1; then
  rc-service "$unit" status
else
  echo ` + shellSingle("未找到服务单元："+strings.Join(units, " / ")) + `
  exit 1
fi`
	return appstore.Action{
		ID: "status", Label: "服务状态", Group: "inspect",
		Command: command, TimeoutSec: 30,
	}
}

func logsAction(units []string) appstore.Action {
	command := unitScript(units) + `if command -v journalctl >/dev/null 2>&1; then
  journalctl -u "$unit" -n 80 --no-pager
else
  echo ` + shellSingle("该主机没有 journalctl，请到系统日志窗口查看 "+strings.Join(units, " / ")) + `
fi`
	return appstore.Action{
		ID: "logs", Label: "最近日志", Group: "inspect",
		Command: command, TimeoutSec: 30,
	}
}

func serviceAction(id, label, confirm string, units []string, danger bool) appstore.Action {
	command := unitScript(units) + `if command -v systemctl >/dev/null 2>&1 && systemctl cat "$unit" >/dev/null 2>&1; then
  systemctl ` + id + ` "$unit"
elif command -v rc-service >/dev/null 2>&1; then
  rc-service "$unit" ` + id + `
else
  echo ` + shellSingle("未找到服务单元："+strings.Join(units, " / ")) + `
  exit 1
fi`
	return appstore.Action{
		ID: id, Label: label, Group: "manage", Command: command,
		Confirm: confirm, Danger: danger, NeedRoot: true, TimeoutSec: 90,
	}
}

func unitScript(units []string) string {
	quoted := make([]string, len(units))
	for i, unit := range units {
		quoted[i] = shellSingle(unit)
	}
	list := strings.Join(quoted, " ")
	return "unit=\"\"\n" +
		"if command -v systemctl >/dev/null 2>&1; then\n" +
		"  for u in " + list + "; do\n" +
		"    if systemctl cat \"$u\" >/dev/null 2>&1; then unit=\"$u\"; break; fi\n" +
		"  done\n" +
		"fi\n" +
		"if [ -z \"$unit\" ]; then\n" +
		"  for u in " + list + "; do\n" +
		"    if [ -x \"/etc/init.d/$u\" ]; then unit=\"$u\"; break; fi\n" +
		"  done\n" +
		"fi\n" +
		"[ -n \"$unit\" ] || unit=" + quoted[0] + "\n"
}

func unitNames(units map[env.Family]string) []string {
	if len(units) == 0 {
		return nil
	}
	order := []env.Family{env.FamilyApt, env.FamilyDNF, env.FamilyYUM, env.FamilyAPK}
	seen := map[string]bool{}
	out := make([]string, 0, len(units))
	for _, family := range order {
		unit := units[family]
		if unit == "" || seen[unit] {
			continue
		}
		seen[unit] = true
		out = append(out, unit)
	}
	// 未列入 order 的家族按名字补在后面，保证输出稳定。
	rest := make([]string, 0)
	for family, unit := range units {
		known := family == env.FamilyApt || family == env.FamilyDNF ||
			family == env.FamilyYUM || family == env.FamilyAPK
		if known || unit == "" || seen[unit] {
			continue
		}
		seen[unit] = true
		rest = append(rest, unit)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func shellSingle(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func defaultAction(actions []appstore.Action) string {
	ids := map[string]bool{}
	for _, action := range actions {
		ids[action.ID] = true
	}
	for _, candidate := range []string{"status", "version", "public", "list"} {
		if ids[candidate] {
			return candidate
		}
	}
	if len(actions) > 0 {
		return actions[0].ID
	}
	return ""
}

// ---------------- 界面配置 ----------------

func buildUI(recipe *env.Recipe, actions []appstore.Action, defaultAction string) appstore.UI {
	inspect := make([]string, 0)
	manage := make([]string, 0)
	for _, action := range actions {
		if action.Group == "manage" || action.Danger {
			manage = append(manage, action.ID)
		} else {
			inspect = append(inspect, action.ID)
		}
	}
	sections := make([]appstore.Section, 0, 2)
	if len(inspect) > 0 {
		sections = append(sections, appstore.Section{Title: "查看与诊断", Actions: inspect})
	}
	if len(manage) > 0 {
		sections = append(sections, appstore.Section{Title: "管理操作", Actions: manage})
	}
	overview := []string{}
	if defaultAction != "" {
		overview = append(overview, defaultAction)
	}
	return appstore.UI{
		SchemaVersion: appstore.SchemaVersion,
		Layout:        "generic",
		Overview:      overview,
		Sections:      sections,
		Hint:          hintFor(recipe),
	}
}

func hintFor(recipe *env.Recipe) string {
	switch recipe.Category {
	case "security":
		return "动作由面板编译期实现；应用包负责元数据、图标、界面配置与配置文件入口。"
	case "service", "database":
		return "服务由 systemd / OpenRC 托管；启停会改变主机状态，请先确认影响范围。"
	case "monitor":
		return "交互式终端工具；面板提供版本与运行状态，日常使用请在终端窗口中运行。"
	default:
		return "命令行工具；面板提供版本、安装状态与配置入口，日常使用请在终端窗口中运行。"
	}
}

// ---------------- 配置路径 ----------------

func configPaths(recipe *env.Recipe) []appstore.ConfigPath {
	paths := make([]string, 0)
	if recipe.Launch != nil {
		paths = append(paths, recipe.Launch.ConfigPaths...)
	}
	paths = append(paths, recipe.ConfigPaths...)
	paths = append(paths, extraConfig[recipe.Slug]...)

	seen := map[string]bool{}
	out := make([]appstore.ConfigPath, 0, len(paths))
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, appstore.ConfigPath{Path: path, IsDir: directoryPaths[path]})
	}
	return out
}

var extraConfig = map[string][]string{
	"nginx":      {"/etc/nginx/nginx.conf", "/etc/nginx/conf.d", "/etc/nginx/sites-available"},
	"postgresql": {"/etc/postgresql"},
	"mariadb":    {"/etc/mysql/mariadb.conf.d", "/etc/my.cnf"},
	"mysql":      {"/etc/mysql", "/etc/my.cnf"},
	"redis":      {"/etc/redis/redis.conf"},
	"git":        {"~/.gitconfig", "/etc/gitconfig"},
	"php":        {"/etc/php"},
	"tmux":       {"~/.tmux.conf"},
	"htop":       {"~/.config/htop/htoprc"},
	"btop":       {"~/.config/btop/btop.conf"},
}

var directoryPaths = map[string]bool{
	"/etc/nginx/conf.d": true, "/etc/nginx/sites-available": true,
	"/etc/postgresql": true, "/etc/mysql": true, "/etc/mysql/mariadb.conf.d": true,
	"/etc/php": true, "/etc/fail2ban/jail.d": true, "/etc/wireguard": true,
	"/etc/openvpn/server": true, "/etc/openvpn/client": true,
	"/etc/firewalld/zones": true, "~/.password-store": true,
}

// ---------------- 图标 ----------------

// securityGlyphs 与 AppGlyphs.vue 中的 15 个 sec-* 符号保持一致。
// 应用包里的 SVG 是它们的独立副本，便于应用中心在不加载精灵表时展示。
var securityGlyphs = map[string]string{
	"nmap": `<circle cx="12" cy="12" r="8" />
        <circle cx="12" cy="12" r="4" />
        <path d="M12 12l6-6M12 4v2M20 12h-2" />`,
	"tcpdump": `<rect x="3" y="4" width="18" height="6" rx="1.5" />
        <rect x="3" y="14" width="12" height="6" rx="1.5" />
        <path d="M17 17h4m-2-2 2 2-2 2" />`,
	"ufw": `<path d="M12 3l7 3v5.5c0 4.4-2.9 7.6-7 9.5-4.1-1.9-7-5.1-7-9.5V6z" />
        <path d="m9 12 2 2 4-4" />`,
	"fail2ban": `<circle cx="12" cy="12" r="8" />
        <path d="m6.3 17.7 11.4-11.4" />`,
	"clamav": `<rect x="7" y="8" width="10" height="12" rx="5" />
        <path d="M9 8V6m6 2V6M4 11h3m10 0h3M4 16h3m10 0h3M9 6 8 4m7 2 1-2" />`,
	"wireguard-tools": `<circle cx="9" cy="12" r="4" />
        <circle cx="15" cy="12" r="4" />
        <path d="M3 7v10M21 7v10" />`,
	"openvpn": `<rect x="7" y="10" width="10" height="10" rx="2" />
        <path d="M9 10V7.5a3 3 0 0 1 6 0V10" />
        <path d="M3 12a9 9 0 0 1 2.5-6.2M21 12a9 9 0 0 0-2.5-6.2" />`,
	"nethogs": `<path d="M8 6v12m0 0-2.5-2.5M8 18l2.5-2.5" />
        <path d="M16 18V6m0 0-2.5 2.5M16 6l2.5 2.5" />`,
	"tor": `<circle cx="12" cy="13" r="8" />
        <path d="M12 5c2.8 2.2 4.5 5 4.5 8S14.8 18.8 12 21c-2.8-2.2-4.5-5-4.5-8S9.2 7.2 12 5Z" />
        <path d="M12 3v2" />`,
	"gnupg": `<circle cx="8" cy="8" r="3.5" />
        <path d="m10.5 10.5 8 8M15 15l2-2M17.5 17.5l2-2" />`,
	"pass": `<rect x="5" y="10" width="14" height="10" rx="2" />
        <path d="M8 10V7a4 4 0 0 1 8 0v3" />
        <path d="M12 14v2" />`,
	"lynis": `<rect x="5" y="4" width="14" height="17" rx="2" />
        <path d="M9 4V3h6v1" />
        <path d="m8.5 13 2.5 2.5 5-5" />`,
	"rkhunter": `<circle cx="10.5" cy="10.5" r="6.5" />
        <path d="m15.5 15.5 5 5M8 9h5M8 12h3" />`,
	"chkrootkit": `<circle cx="12" cy="12" r="7" />
        <path d="M12 2v4M12 18v4M2 12h4M18 12h4" />
        <circle cx="12" cy="12" r="1.5" />`,
	"firewalld": `<path d="M3 7h18v12H3z" />
        <path d="M3 11h18M3 15h18M9 7v4M15 7v4M6 11v4M12 11v4M18 11v4" />`,
}

func iconFor(recipe *env.Recipe) string {
	if glyph, ok := securityGlyphs[recipe.Slug]; ok {
		return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" role="img" aria-label="%s">
  <g fill="none" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round" stroke-width="1.6">
        %s
  </g>
</svg>
`, html.EscapeString(recipe.Name), glyph)
	}
	return monogramIcon(recipe.Name)
}

func monogramIcon(name string) string {
	var letters strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			letters.WriteRune(r)
		}
	}
	text := letters.String()
	if len(text) > 2 {
		text = text[:2]
	}
	if text == "" {
		text = "APP"
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" role="img" aria-label="%s">
  <rect x="2.25" y="2.25" width="19.5" height="19.5" rx="3.5" fill="none" stroke="currentColor" stroke-width="1.5" opacity="0.55" />
  <text x="12" y="15.4" text-anchor="middle" font-family="ui-monospace, SFMono-Regular, Menlo, monospace" font-size="7.2" font-weight="600" letter-spacing="0.3" fill="currentColor">%s</text>
</svg>
`, html.EscapeString(name), text)
}
