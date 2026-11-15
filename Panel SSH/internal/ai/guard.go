package ai

import (
	"regexp"
	"strings"
)

// DangerRule 是一条危险命令规则。
type DangerRule struct {
	Pattern *regexp.Regexp
	Reason  string
}

// dangerRules 是危险命令护栏。
//
// 定位必须说清楚：它不试图判断"一条命令是否安全"（这在通用 shell 上不可能做到），
// 而是拦住一小撮一旦执行就不可挽回、且几乎不可能是正常运维意图的命令。
// 命中后由用户在界面上确认，而不是直接拒绝 —— 模型提出、人做决定。
//
// 规则刻意保守：宁可漏掉一些危险命令，也不要频繁误伤正常操作，
// 否则用户会习惯性地无脑点确认，护栏就形同虚设。
var dangerRules = []DangerRule{
	{
		Pattern: regexp.MustCompile(`(?i)\brm\b[^\n]*\s-[a-zA-Z]*[rR][a-zA-Z]*\b[^\n]*\s(\/|\/\*|~\/?|\$HOME\/?)(\s|$)`),
		Reason:  "递归删除根目录或家目录",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\bmkfs(\.[a-z0-9]+)?\b`),
		Reason:  "格式化文件系统",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\bdd\b[^\n]*\bof=/dev/(sd|nvme|vd|hd|mmcblk)`),
		Reason:  "直接覆写块设备",
	},
	{
		Pattern: regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|vd|hd|mmcblk)`),
		Reason:  "重定向覆写块设备",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\b(shutdown|reboot|halt|poweroff)\b`),
		Reason:  "关机或重启主机",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\binit\s+0\b`),
		Reason:  "切换到关机运行级别",
	},
	{
		Pattern: regexp.MustCompile(`:\s*\(\s*\)\s*\{.*\}\s*;?\s*:`),
		Reason:  "疑似 fork 炸弹",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\bchmod\s+(-[a-zA-Z]+\s+)*777\s+/(\s|$)`),
		Reason:  "把根目录权限放宽为 777",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\bkill\s+(-9\s+|-s\s+9\s+)?1(\s|$)`),
		Reason:  "终止 init/systemd，会导致主机不可用",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\b(systemctl|service)\s+(stop|disable|mask)\s+(sshd|ssh|systemd-networkd|NetworkManager|firewalld|ufw)\b`),
		Reason:  "停用 SSH 或网络服务，可能造成失联",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\biptables\s+-F\b`),
		Reason:  "清空防火墙规则",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\bpasswd\s+(-d|--delete)\b`),
		Reason:  "清除账户口令",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\b(userdel|groupdel)\s+[^\n]*\broot\b`),
		Reason:  "删除 root 用户或组",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n|]*\|\s*(sudo\s+)?(ba|z|d)?sh\b`),
		Reason:  "把远端脚本直接管道给 shell 执行",
	},
	{
		Pattern: regexp.MustCompile(`(?i)>\s*/etc/(passwd|shadow|fstab|hosts)\b`),
		Reason:  "覆盖关键系统文件",
	},
	{
		Pattern: regexp.MustCompile(`(?i)\btruncate\s+(-s\s*0\s+)?/etc/(passwd|shadow|fstab|hosts)\b`),
		Reason:  "清空关键系统文件",
	},
}

// CheckCommand 判断命令是否触碰护栏，命中时返回原因。
func CheckCommand(command string) (reason string, dangerous bool) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "", false
	}

	for _, rule := range dangerRules {
		if rule.Pattern.MatchString(trimmed) {
			return rule.Reason, true
		}
	}
	return "", false
}
