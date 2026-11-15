package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 本文件是安全应用的可视化视图数据源。
//
// 通用安全窗口只把命令输出原样贴进 <pre>，用户看到的仍然是命令行。
// Fail2Ban 与 GnuPG 有稳定的机器可读口径（gpg --with-colons / fail2ban-client
// 的分行状态），因此在这里解析成结构化数据，前端专注画卡片与表格。
// 解析只负责"读"，所有变更仍然回到 security_apps.go 的白名单动作。

// jailNamePattern 与操作目录里的 namePattern 保持一致：
// Jail 名来自 fail2ban 自己的输出，但拼进 shell 前仍要过一遍白名单。
var jailNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type fail2banJail struct {
	Name            string   `json:"name"`
	CurrentlyFailed int      `json:"currentlyFailed"`
	TotalFailed     int      `json:"totalFailed"`
	CurrentlyBanned int      `json:"currentlyBanned"`
	TotalBanned     int      `json:"totalBanned"`
	BannedIPs       []string `json:"bannedIps"`
	// Error 只表示这一个 Jail 读取失败：其余 Jail 照常展示，不至于整页作废。
	Error string `json:"error,omitempty"`
}

type fail2banOverview struct {
	// Available 表示这台机器上能连到 fail2ban 服务；Running 才表示服务已启动。
	Available bool           `json:"available"`
	Running   bool           `json:"running"`
	Error     string         `json:"error,omitempty"`
	Jails     []fail2banJail `json:"jails"`
	CheckedAt string         `json:"checkedAt"`
}

// handleFail2BanOverview 读取所有 Jail 的状态与封禁名单。
//
// 先跑一次 fail2ban-client status 拿到 Jail 列表，再逐个 Jail 读明细。
// 单个 Jail 失败不影响其它 Jail；整条命令失败（服务没起、没有提权）时返回
// available=false 并带上原始错误，界面据此显示"未运行"而不是空列表。
func (s *Server) handleFail2BanOverview(w http.ResponseWriter, r *http.Request) {
	app, hostID, ok := s.securityAppFor(w, r)
	if !ok {
		return
	}
	if app.Slug != "fail2ban" {
		writeErr(w, http.StatusNotFound, "该应用没有可视化视图")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	result := fail2banOverview{
		Jails:     []fail2banJail{},
		CheckedAt: time.Now().Format(time.RFC3339),
	}
	res, err := s.runSecurityScript(ctx, hostID, "fail2ban-client status", true, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取 Fail2Ban 状态失败："+err.Error())
		return
	}
	if res.ExitCode != 0 {
		// 127 是 shell 的"命令不存在"：这时是没装而不是没启动，
		// 界面据此区分"去安装"与"点启动"两种引导。
		result.Available = res.ExitCode != 127
		result.Error = strings.TrimSpace(res.Stdout + res.Stderr)
		if result.Error == "" {
			result.Error = "fail2ban-client 返回了非零退出码"
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	result.Available = true
	result.Running = true
	for _, name := range parseFail2BanJailList(res.Stdout) {
		if !jailNamePattern.MatchString(name) {
			continue
		}
		result.Jails = append(result.Jails, s.readFail2BanJail(ctx, hostID, name))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) readFail2BanJail(ctx context.Context, hostID int64, name string) fail2banJail {
	jail := fail2banJail{Name: name, BannedIPs: []string{}}
	res, err := s.runSecurityScript(ctx, hostID, "fail2ban-client status "+shq(name), true, 30*time.Second)
	if err != nil {
		jail.Error = err.Error()
		return jail
	}
	if res.ExitCode != 0 {
		jail.Error = strings.TrimSpace(res.Stdout + res.Stderr)
		if jail.Error == "" {
			jail.Error = "读取这个 Jail 失败"
		}
		return jail
	}
	parseFail2BanJailStatus(res.Stdout, &jail)
	return jail
}

// parseFail2BanJailList 从概览输出里取出 Jail 名。
//
// 形如 "`- Jail list:	sshd, nginx-http-auth"；没有启用任何 Jail 时该行为空。
func parseFail2BanJailList(out string) []string {
	for _, line := range strings.Split(out, "\n") {
		idx := strings.Index(line, "Jail list:")
		if idx < 0 {
			continue
		}
		value := strings.TrimSpace(line[idx+len("Jail list:"):])
		if value == "" {
			return nil
		}
		names := make([]string, 0, 4)
		for _, item := range strings.Split(value, ",") {
			if name := strings.TrimSpace(item); name != "" {
				names = append(names, name)
			}
		}
		return names
	}
	return nil
}

// parseFail2BanJailStatus 解析单个 Jail 的指标与封禁名单。
//
// 行格式在 0.10 与 1.x 之间保持稳定：
//
//	|- Currently failed:	1
//	|- Total failed:	8
//	`- Banned IP list:	1.2.3.4 5.6.7.8
//
// 这里按"去掉前导装饰后找第一个冒号"来读，不依赖具体缩进字符。
func parseFail2BanJailStatus(out string, jail *fail2banJail) {
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimLeft(strings.TrimSpace(raw), "|`- ")
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:idx]))
		value := strings.TrimSpace(line[idx+1:])
		switch key {
		case "currently failed":
			jail.CurrentlyFailed = atoiOrZero(value)
		case "total failed":
			jail.TotalFailed = atoiOrZero(value)
		case "currently banned":
			jail.CurrentlyBanned = atoiOrZero(value)
		case "total banned":
			jail.TotalBanned = atoiOrZero(value)
		case "banned ip list":
			jail.BannedIPs = splitIPList(value)
		}
	}
	if jail.BannedIPs == nil {
		jail.BannedIPs = []string{}
	}
}

// splitIPList 兼容空格分隔（1.x）与逗号分隔（部分旧版本）两种写法。
func splitIPList(value string) []string {
	value = strings.ReplaceAll(value, ",", " ")
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return []string{}
	}
	return fields
}

type gpgSubkey struct {
	KeyID        string   `json:"keyId"`
	Fingerprint  string   `json:"fingerprint"`
	Algorithm    string   `json:"algorithm"`
	Bits         int      `json:"bits"`
	Created      int64    `json:"created"`
	Expires      int64    `json:"expires"`
	Capabilities []string `json:"capabilities"`
}

type gpgKey struct {
	Fingerprint  string      `json:"fingerprint"`
	KeyID        string      `json:"keyId"`
	UID          string      `json:"uid"`
	Name         string      `json:"name"`
	Email        string      `json:"email,omitempty"`
	Algorithm    string      `json:"algorithm"`
	Bits         int         `json:"bits"`
	Created      int64       `json:"created"`
	Expires      int64       `json:"expires"`
	Validity     string      `json:"validity"`
	Capabilities []string    `json:"capabilities"`
	Subkeys      []gpgSubkey `json:"subkeys"`
}

type gpgKeysView struct {
	Available bool     `json:"available"`
	Error     string   `json:"error,omitempty"`
	Keys      []gpgKey `json:"keys"`
	CheckedAt string   `json:"checkedAt"`
}

// handleGnuPGKeys 用 --with-colons 读取钥匙环。
//
// colon 格式是 GnuPG 明确承诺稳定的机器可读口径：字段固定、不受界面语言影响。
// kind=secret 时读私钥列表（sec/ssb），其余一律读公钥列表。
func (s *Server) handleGnuPGKeys(w http.ResponseWriter, r *http.Request) {
	app, hostID, ok := s.securityAppFor(w, r)
	if !ok {
		return
	}
	if app.Slug != "gnupg" {
		writeErr(w, http.StatusNotFound, "该应用没有可视化视图")
		return
	}

	kind := r.URL.Query().Get("kind")
	if kind != "secret" {
		kind = "public"
	}
	flag := "--list-keys"
	if kind == "secret" {
		flag = "--list-secret-keys"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	result := gpgKeysView{
		Keys:      []gpgKey{},
		CheckedAt: time.Now().Format(time.RFC3339),
	}
	script := "gpg --with-colons --fixed-list-mode --fingerprint " + flag
	// 私钥列表同样按登录用户读取：钥匙环在用户的 ~/.gnupg 下，
	// 提权到 root 只会读到另一个人的钥匙环。
	res, err := s.runSecurityScript(ctx, hostID, script, false, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "读取密钥失败："+err.Error())
		return
	}
	if res.ExitCode != 0 {
		result.Error = strings.TrimSpace(res.Stdout + res.Stderr)
		if result.Error == "" {
			result.Error = "gpg 返回了非零退出码"
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	result.Available = true
	result.Keys = parseGpgColonKeys(res.Stdout)
	writeJSON(w, http.StatusOK, result)
}

// parseGpgColonKeys 解析 --with-colons 输出。
//
// 关键记录：
//
//	pub/sec  主钥（字段 3 长度、4 算法、5 KeyID、6 创建、7 到期、12 能力）
//	fpr      指纹（字段 10）；紧跟在主钥后的那条才是主钥指纹
//	uid      用户标识（字段 10，值里的 \xHH 需要还原）
//	sub/ssb  子钥
//
// 其余记录（tru/grp/rvk 等）与界面无关，直接跳过。
func parseGpgColonKeys(out string) []gpgKey {
	keys := make([]gpgKey, 0)
	var current *gpgKey
	var currentSub *gpgSubkey
	// pending 记录下一条 fpr 属于谁：主钥还是刚出现的子钥。
	pending := ""

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "pub", "sec":
			// 主钥记录至少要包含到"能力"这一列；不完整就跳过，避免越界。
			if len(fields) < 12 {
				continue
			}
			keys = append(keys, gpgKey{
				KeyID:        fields[4],
				Bits:         atoiOrZero(fields[2]),
				Algorithm:    gpgAlgorithm(fields[3], gpgField(fields, 16)),
				Created:      int64(atoiOrZero(fields[5])),
				Expires:      int64(atoiOrZero(fields[6])),
				Validity:     fields[1],
				Capabilities: gpgCapabilities(fields[11]),
				Subkeys:      []gpgSubkey{},
			})
			current = &keys[len(keys)-1]
			currentSub = nil
			pending = "primary"
		case "sub", "ssb":
			if current == nil || len(fields) < 12 {
				continue
			}
			current.Subkeys = append(current.Subkeys, gpgSubkey{
				KeyID:        fields[4],
				Bits:         atoiOrZero(fields[2]),
				Algorithm:    gpgAlgorithm(fields[3], gpgField(fields, 16)),
				Created:      int64(atoiOrZero(fields[5])),
				Expires:      int64(atoiOrZero(fields[6])),
				Capabilities: gpgCapabilities(fields[11]),
			})
			currentSub = &current.Subkeys[len(current.Subkeys)-1]
			pending = "sub"
		case "fpr":
			fingerprint := gpgField(fields, 9)
			if fingerprint == "" {
				continue
			}
			switch {
			case pending == "primary" && current != nil:
				current.Fingerprint = fingerprint
				pending = ""
			case pending == "sub" && currentSub != nil:
				currentSub.Fingerprint = fingerprint
				pending = ""
			}
		case "uid":
			if current == nil {
				continue
			}
			value := unescapeGpgField(gpgField(fields, 9))
			if current.UID == "" {
				current.UID = value
				current.Name, current.Email = splitGPGUID(value)
			}
		}
	}
	return keys
}

func gpgField(fields []string, index int) string {
	if index < 0 || index >= len(fields) {
		return ""
	}
	return fields[index]
}

// unescapeGpgField 还原 colon 格式里的 \xHH 转义（常见于 UID 中的冒号）。
func unescapeGpgField(value string) string {
	if !strings.Contains(value, `\x`) {
		return value
	}
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) && value[i+1] == 'x' {
			if code, err := strconv.ParseUint(value[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(code))
				i += 3
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

// splitGPGUID 把 "姓名 <邮箱>" 拆成两段；没有邮箱时整段都是姓名。
func splitGPGUID(uid string) (string, string) {
	open := strings.LastIndex(uid, "<")
	close := strings.LastIndex(uid, ">")
	if open >= 0 && close > open {
		name := strings.TrimSpace(uid[:open])
		email := strings.TrimSpace(uid[open+1 : close])
		return name, email
	}
	return strings.TrimSpace(uid), ""
}

var gpgAlgorithms = map[string]string{
	"1": "RSA", "2": "RSA", "3": "RSA",
	"16": "ElGamal", "17": "DSA", "18": "ECDH", "19": "ECDSA",
	"20": "ElGamal", "21": "Diffie-Hellman", "22": "EdDSA",
}

var gpgCurves = map[string]string{
	"ed25519": "Ed25519", "cv25519": "Curve25519",
	"nistp256": "NIST P-256", "nistp384": "NIST P-384", "nistp521": "NIST P-521",
	"brainpoolp256r1": "Brainpool P-256", "brainpoolp384r1": "Brainpool P-384",
	"brainpoolp512r1": "Brainpool P-512",
}

func gpgAlgorithm(code, curve string) string {
	if name, ok := gpgCurves[strings.ToLower(curve)]; ok {
		return name
	}
	if name, ok := gpgAlgorithms[code]; ok {
		return name
	}
	return "未知算法"
}

// gpgCapabilities 把能力串（如 "scESC"）收敛成界面上的四种用途。
// 大小写分别表示"具备能力"与"实际可用"，对用户来说是同一件事，这里合并。
func gpgCapabilities(raw string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, ch := range raw {
		switch ch {
		case 's', 'S':
			add("sign")
		case 'c', 'C':
			add("certify")
		case 'e', 'E':
			add("encrypt")
		case 'a', 'A':
			add("authenticate")
		}
	}
	return out
}

func atoiOrZero(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return n
}
