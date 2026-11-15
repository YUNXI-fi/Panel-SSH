package api

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"panelssh/internal/appstore"
	"panelssh/internal/env"
)

// 本文件是"安全应用的原生 UI 操作层"。
//
// 前端不拼命令、不碰终端：它只提交 action id 与表单参数，后端按白名单
// 把参数校验后拼成固定命令，再走环境模块那套提权执行链路。
// 这样界面可以做成表单/按钮，同时执行面仍然只有这里列出的固定命令。

type securityField struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // text / number / select
	Placeholder string   `json:"placeholder,omitempty"`
	Default     string   `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Help        string   `json:"help,omitempty"`

	// pattern 只用于校验，不对外暴露。
	pattern string
	maxLen  int
}

type securityAction struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	// Group 只用于界面分组：inspect 是读取/诊断，manage 会改变系统状态。
	Group   string           `json:"group,omitempty"`
	Fields  []*securityField `json:"fields,omitempty"`
	Confirm string           `json:"confirm,omitempty"`
	Danger  bool             `json:"danger,omitempty"`

	build    func(params map[string]string) (string, error)
	needRoot bool
	timeout  time.Duration
}

type securityConfigEntry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

type securityApp struct {
	Slug          string                `json:"slug"`
	Name          string                `json:"name"`
	Description   string                `json:"description,omitempty"`
	DefaultAction string                `json:"defaultAction,omitempty"`
	Actions       []*securityAction     `json:"actions"`
	Config        []securityConfigEntry `json:"config"`

	// 以下字段只对"从应用仓库安装的应用包"有值；内置安全应用保持空。
	Version  string       `json:"version,omitempty"`
	Category string       `json:"category,omitempty"`
	IconURL  string       `json:"iconUrl,omitempty"`
	UI       *appstore.UI `json:"ui,omitempty"`
}

func textField(key, label, def, pattern, help string) *securityField {
	return &securityField{
		Key: key, Label: label, Type: "text", Default: def,
		pattern: pattern, Help: help, Required: true, maxLen: 200,
	}
}

func numField(key, label, def string) *securityField {
	return &securityField{Key: key, Label: label, Type: "number", Default: def, Required: true, maxLen: 8}
}

func selectField(key, label, def string, options ...string) *securityField {
	return &securityField{Key: key, Label: label, Type: "select", Default: def, Options: options, Required: true}
}

// passwordField 走文本框校验，但界面用密码框渲染：用于 GnuPG 密钥口令这类
// 既要提交给命令、又不该在屏幕上明文显示的值。
func passwordField(key, label, help string) *securityField {
	return &securityField{Key: key, Label: label, Type: "password", Help: help, maxLen: 128}
}

// optionalTextField 是"可以留空、留空不校验格式"的文本字段，用于邮箱这类选填项。
func optionalTextField(key, label, pattern, help string) *securityField {
	return &securityField{Key: key, Label: label, Type: "text", pattern: pattern, Help: help, maxLen: 190}
}

func textareaField(key, label string, maxLen int, help string) *securityField {
	return &securityField{Key: key, Label: label, Type: "textarea", Required: true, Help: help, maxLen: maxLen}
}

// confirm 给需要二次确认的动作补上说明文字。
func confirm(action *securityAction, text string) *securityAction {
	action.Confirm = text
	return action
}

func act(id, label string, root bool, build func(map[string]string) (string, error), fields ...*securityField) *securityAction {
	return &securityAction{
		ID: id, Label: label, Group: "inspect",
		Fields: fields, build: build, needRoot: root, timeout: 90 * time.Second,
	}
}

// manage 把动作标成"会改变系统状态"，界面据此与只读诊断分组。
func manage(action *securityAction) *securityAction {
	action.Group = "manage"
	return action
}

func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// 目标地址：IP、域名或 CIDR，不允许空格与 shell 元字符。
var (
	hostPattern    = `^[A-Za-z0-9._:/-]{1,128}$`
	portPattern    = `^[0-9,-]{1,32}$`
	pathPattern    = `^/[A-Za-z0-9._/ -]{0,200}$`
	ipPattern      = `^[0-9A-Fa-f:.]{2,45}$`
	namePattern    = `^[A-Za-z0-9._@-]{1,64}$`
	servicePattern = `^[A-Za-z0-9._-]{1,64}$`
	entryPattern   = `^[A-Za-z0-9._/@-]{1,128}$`
	// gpgNamePattern 排除尖括号与控制字符：姓名会被拼进 OpenPGP 参数文件，
	// 换行会变成一条新指令，尖括号会破坏 "姓名 <邮箱>" 的 UID 结构。
	gpgNamePattern     = `^[^[:cntrl:]<>]{1,64}$`
	gpgEmailPattern    = `^[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}$`
	fingerprintPattern = `^[A-Fa-f0-9]{8,64}$`
)

// securityApps 是安全应用的操作目录，在 init 里按软件目录顺序组装。
var securityApps = map[string]*securityApp{}

func init() {
	securityApps["nmap"] = &securityApp{
		Slug: "nmap", Name: "nmap", DefaultAction: "version",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "nmap --version", nil
			}),
			manage(act("scan", "端口扫描", false, func(p map[string]string) (string, error) {
				return "nmap " + shq(p["mode"]) + " -p " + shq(p["ports"]) + " " + shq(p["target"]), nil
			},
				textField("target", "目标", "127.0.0.1", hostPattern, "IP、域名或网段，如 192.168.1.0/24"),
				textField("ports", "端口范围", "1-1000", portPattern, "如 22、80,443 或 1-1000"),
				selectField("mode", "扫描方式", "-sT", "-sT", "-sV"),
			)),
		},
	}
	securityApps["tcpdump"] = &securityApp{
		Slug: "tcpdump", Name: "tcpdump", DefaultAction: "version",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "tcpdump --version", nil
			}),
			act("interfaces", "列出网卡", false, func(map[string]string) (string, error) {
				return "tcpdump -D", nil
			}),
			{
				ID: "capture", Label: "抓取数据包", Group: "manage",
				Confirm: "将抓取指定数量的数据包，输出可能较多。",
				Fields: []*securityField{
					textField("iface", "网卡", "any", hostPattern, "如 eth0；any 表示所有网卡"),
					numField("count", "抓包数量", "10"),
				},
				needRoot: true, timeout: 90 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "tcpdump -i " + shq(p["iface"]) + " -nn -c " + shq(p["count"]), nil
				},
			},
		},
	}
	securityApps["ufw"] = &securityApp{
		Slug: "ufw", Name: "UFW", DefaultAction: "status",
		Actions: []*securityAction{
			act("status", "查看状态", true, func(map[string]string) (string, error) {
				return "ufw status verbose", nil
			}),
			act("numbered", "查看编号规则", true, func(map[string]string) (string, error) {
				return "ufw status numbered", nil
			}),
			{
				ID: "allow", Label: "放行端口", Group: "manage", Fields: []*securityField{
					numField("port", "端口", "22"),
					selectField("proto", "协议", "tcp", "tcp", "udp"),
					textField("from", "来源", "any", hostPattern, "any 表示任意来源，也可填 IP 或网段"),
				},
				needRoot: true, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "ufw allow from " + shq(p["from"]) + " to any port " + shq(p["port"]) +
						" proto " + shq(p["proto"]) + " && ufw status numbered", nil
				},
			},
			{
				ID: "delete", Label: "删除规则", Group: "manage", Danger: true, Confirm: "删除后该规则立即失效，请确认编号无误。",
				Fields:   []*securityField{numField("number", "规则编号", "1")},
				needRoot: true, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "ufw --force delete " + shq(p["number"]) + " && ufw status numbered", nil
				},
			},
			{
				ID: "enable", Label: "启用防火墙", Group: "manage", Danger: true,
				Confirm:  "启用前请确认已放行 SSH 端口，否则当前远程连接可能中断。",
				needRoot: true, timeout: 60 * time.Second,
				build: func(map[string]string) (string, error) { return "ufw --force enable", nil },
			},
			{
				ID: "disable", Label: "停用防火墙", Group: "manage", Danger: true,
				Confirm:  "停用后现有防火墙规则将不再生效。",
				needRoot: true, timeout: 60 * time.Second,
				build: func(map[string]string) (string, error) { return "ufw disable", nil },
			},
			manage(act("reload", "重新加载规则", true, func(map[string]string) (string, error) {
				return "ufw reload", nil
			})),
		},
	}
	securityApps["fail2ban"] = &securityApp{
		Slug: "fail2ban", Name: "Fail2Ban", DefaultAction: "status",
		Actions: []*securityAction{
			act("status", "查看封禁状态", true, func(map[string]string) (string, error) {
				return "fail2ban-client status", nil
			}),
			{
				ID: "jail", Label: "查看单个 Jail", Fields: []*securityField{
					textField("jail", "Jail 名称", "sshd", namePattern, "如 sshd、nginx-http-auth"),
				},
				needRoot: true, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "fail2ban-client status " + shq(p["jail"]), nil
				},
			},
			{
				ID: "unban", Label: "解封 IP", Group: "manage", Fields: []*securityField{
					textField("jail", "Jail 名称", "sshd", namePattern, "要操作的 Jail"),
					textField("ip", "IP 地址", "", ipPattern, "要解封的 IP"),
				},
				needRoot: true, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "fail2ban-client set " + shq(p["jail"]) + " unbanip " + shq(p["ip"]), nil
				},
			},
			manage(act("reload", "重载配置", true, func(map[string]string) (string, error) {
				return "fail2ban-client reload", nil
			})),
			act("test", "校验配置", true, func(map[string]string) (string, error) {
				return "fail2ban-client -t", nil
			}),
			// 服务没起时界面只显示"未运行"没有出路，因此给一个明确的启动动作。
			// 启动后立即回读状态，让调用方拿到的是"真的起来了"而不是 systemctl 的退出码。
			confirm(manage(act("start", "启动服务", true, func(map[string]string) (string, error) {
				return "systemctl start fail2ban && sleep 1 && fail2ban-client status", nil
			})), "启动后 Fail2Ban 会立即按现有规则封禁匹配的地址。"),
		},
	}
	securityApps["clamav"] = &securityApp{
		Slug: "clamav", Name: "ClamAV", DefaultAction: "version",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "clamscan --version", nil
			}),
			{
				ID: "update", Label: "更新病毒库", Group: "manage", Confirm: "首次更新病毒库需要下载较多数据，耗时取决于网络。",
				needRoot: true, timeout: 300 * time.Second,
				build: func(map[string]string) (string, error) { return "freshclam", nil },
			},
			{
				ID: "scan", Label: "扫描文件", Group: "manage", Fields: []*securityField{
					textField("path", "扫描路径", "/tmp", pathPattern, "目标机上的绝对路径"),
				},
				needRoot: false, timeout: 600 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "clamscan " + shq(p["path"]), nil
				},
			},
			{
				ID: "scan-recursive", Label: "递归扫描目录", Group: "manage", Fields: []*securityField{
					textField("path", "扫描路径", "/tmp", pathPattern, "目标机上的绝对路径"),
				},
				needRoot: false, timeout: 600 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "clamscan -r " + shq(p["path"]), nil
				},
			},
		},
	}
	securityApps["wireguard-tools"] = &securityApp{
		Slug: "wireguard-tools", Name: "WireGuard", DefaultAction: "status",
		Actions: []*securityAction{
			act("interfaces", "隧道接口", true, func(map[string]string) (string, error) {
				return "wg show interfaces", nil
			}),
			act("status", "隧道状态", true, func(map[string]string) (string, error) {
				return "wg show", nil
			}),
			act("configs", "配置目录", true, func(map[string]string) (string, error) {
				return "ls -l /etc/wireguard", nil
			}),
		},
	}
	securityApps["openvpn"] = &securityApp{
		Slug: "openvpn", Name: "OpenVPN", DefaultAction: "units",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "openvpn --version", nil
			}),
			act("units", "服务实例", false, func(map[string]string) (string, error) {
				return "systemctl list-units 'openvpn*' --no-pager", nil
			}),
			act("configs", "配置目录", false, func(map[string]string) (string, error) {
				return "ls -l /etc/openvpn/server /etc/openvpn/client", nil
			}),
			{
				ID: "restart", Label: "重启服务", Group: "manage", Danger: true,
				Confirm: "重启 VPN 服务会短暂中断已建立的连接。",
				Fields: []*securityField{
					textField("unit", "服务单元", "openvpn-server@server", servicePattern, "如 openvpn-server@server"),
				},
				needRoot: true, timeout: 90 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "systemctl restart " + shq(p["unit"]) + " && systemctl status " + shq(p["unit"]) + " --no-pager", nil
				},
			},
		},
	}
	securityApps["nethogs"] = &securityApp{
		Slug: "nethogs", Name: "NetHogs", DefaultAction: "version",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "nethogs -V", nil
			}),
			{
				ID: "sample", Label: "采样 5 秒流量", Group: "manage", Confirm: "将采样 5 秒，期间会阻塞等待结果。",
				needRoot: true, timeout: 30 * time.Second,
				build: func(map[string]string) (string, error) {
					return "nethogs -t -d 1 -c 5", nil
				},
			},
		},
	}
	securityApps["tor"] = &securityApp{
		Slug: "tor", Name: "Tor", DefaultAction: "status",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "tor --version", nil
			}),
			act("status", "服务状态", false, func(map[string]string) (string, error) {
				return "systemctl status tor --no-pager", nil
			}),
			act("verify", "校验配置", false, func(map[string]string) (string, error) {
				return "tor --verify-config", nil
			}),
			act("logs", "最近日志", true, func(map[string]string) (string, error) {
				return "journalctl -u tor -n 80 --no-pager", nil
			}),
			{
				ID: "restart", Label: "重启服务", Group: "manage", Danger: true, Confirm: "重启 Tor 会短暂中断现有连接。",
				needRoot: true, timeout: 90 * time.Second,
				build: func(map[string]string) (string, error) {
					return "systemctl restart tor && systemctl status tor --no-pager", nil
				},
			},
		},
	}
	securityApps["gnupg"] = &securityApp{
		Slug: "gnupg", Name: "GnuPG", DefaultAction: "public",
		Actions: []*securityAction{
			act("public", "公钥列表", false, func(map[string]string) (string, error) {
				return "gpg --list-keys", nil
			}),
			act("secret", "私钥列表", false, func(map[string]string) (string, error) {
				return "gpg --list-secret-keys", nil
			}),
			act("fingerprint", "查看指纹", false, func(map[string]string) (string, error) {
				return "gpg --fingerprint", nil
			}),
			{
				ID: "export", Label: "导出公钥", Fields: []*securityField{
					textField("key", "密钥 ID", "", entryPattern, "公钥 ID、邮箱或指纹"),
				},
				needRoot: false, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "gpg --armor --export " + shq(p["key"]), nil
				},
			},
			{
				ID: "generate", Label: "生成密钥", Group: "manage",
				Confirm: "将在登录用户的家目录生成一对新密钥；生成过程可能持续数十秒。",
				Fields: []*securityField{
					textField("name", "姓名", "", gpgNamePattern, "密钥用户标识里的真实姓名"),
					optionalTextField("email", "邮箱", gpgEmailPattern, "选填；会拼成「姓名 <邮箱>」"),
					selectField("algo", "算法", "rsa2048", "rsa2048", "rsa4096", "ed25519"),
					selectField("expire", "有效期", "2y", "1y", "2y", "5y", "0"),
					passwordField("passphrase", "口令", "选填；留空表示不设口令，建议设置以保护私钥"),
				},
				needRoot: false, timeout: 300 * time.Second,
				build: buildGnuPGGenerate,
			},
			{
				ID: "import", Label: "导入公钥", Group: "manage",
				Confirm: "将把提交的 ASCII 公钥导入登录用户的钥匙环。",
				Fields: []*securityField{
					textareaField("armored", "公钥文本", 64<<10, "粘贴以 -----BEGIN PGP PUBLIC KEY BLOCK----- 开头的文本"),
				},
				needRoot: false, timeout: 60 * time.Second,
				build: buildGnuPGImport,
			},
			{
				ID: "delete", Label: "删除密钥", Group: "manage", Danger: true,
				Confirm: "删除后无法恢复，确认指纹无误再执行。",
				Fields: []*securityField{
					selectField("scope", "删除范围", "public", "public", "secret"),
					textField("fingerprint", "指纹", "", fingerprintPattern, "要删除的密钥指纹"),
				},
				needRoot: false, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					fingerprint := strings.ToUpper(p["fingerprint"])
					if p["scope"] == "secret" {
						return "gpg --batch --yes --delete-secret-and-public-key " + shq(fingerprint), nil
					}
					return "gpg --batch --yes --delete-key " + shq(fingerprint), nil
				},
			},
		},
	}
	securityApps["pass"] = &securityApp{
		Slug: "pass", Name: "pass", DefaultAction: "list",
		Actions: []*securityAction{
			act("list", "密码条目", false, func(map[string]string) (string, error) {
				return "pass ls", nil
			}),
			{
				ID: "show", Label: "查看密码", Confirm: "将在界面中以明文显示该条目的密码。",
				Fields: []*securityField{
					textField("entry", "条目名称", "", entryPattern, "如 web/admin"),
				},
				needRoot: false, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "pass show " + shq(p["entry"]), nil
				},
			},
			act("git-status", "版本状态", false, func(map[string]string) (string, error) {
				return "pass git status", nil
			}),
		},
	}
	securityApps["lynis"] = &securityApp{
		Slug: "lynis", Name: "Lynis", DefaultAction: "version",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "lynis --version", nil
			}),
			{
				ID: "audit", Label: "快速安全审计", Group: "manage", Confirm: "快速审计会运行一段时间，并在完成后返回结果。",
				needRoot: true, timeout: 300 * time.Second,
				build: func(map[string]string) (string, error) { return "lynis audit system --quick", nil },
			},
			act("report", "查看审计报告", true, func(map[string]string) (string, error) {
				return "tail -n 100 /var/log/lynis.log", nil
			}),
		},
	}
	securityApps["rkhunter"] = &securityApp{
		Slug: "rkhunter", Name: "Rootkit Hunter", DefaultAction: "version",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "rkhunter --version", nil
			}),
			{
				ID: "check", Label: "开始检查", Group: "manage", Confirm: "完整检查耗时较长，且需要已经建立过文件基线。",
				needRoot: true, timeout: 600 * time.Second,
				build: func(map[string]string) (string, error) { return "rkhunter --check --sk", nil },
			},
			{
				ID: "baseline", Label: "更新文件基线", Group: "manage", Danger: true,
				Confirm:  "这会更新文件完整性基线，仅在确认当前系统干净时执行。",
				needRoot: true, timeout: 300 * time.Second,
				build: func(map[string]string) (string, error) { return "rkhunter --propupd", nil },
			},
		},
	}
	securityApps["chkrootkit"] = &securityApp{
		Slug: "chkrootkit", Name: "chkrootkit", DefaultAction: "version",
		Actions: []*securityAction{
			act("version", "版本信息", false, func(map[string]string) (string, error) {
				return "chkrootkit -V", nil
			}),
			{
				ID: "check", Label: "开始检查", Group: "manage", Confirm: "扫描需要一段时间。",
				needRoot: true, timeout: 600 * time.Second,
				build: func(map[string]string) (string, error) { return "chkrootkit", nil },
			},
			{
				ID: "quiet", Label: "只输出可疑项", Group: "manage", Confirm: "扫描仍需要一段时间，只在发现可疑项时输出。",
				needRoot: true, timeout: 600 * time.Second,
				build: func(map[string]string) (string, error) { return "chkrootkit -q", nil },
			},
		},
	}
	securityApps["firewalld"] = &securityApp{
		Slug: "firewalld", Name: "firewalld", DefaultAction: "list",
		Actions: []*securityAction{
			act("state", "运行状态", true, func(map[string]string) (string, error) {
				return "firewall-cmd --state", nil
			}),
			act("list", "当前规则", true, func(map[string]string) (string, error) {
				return "firewall-cmd --list-all", nil
			}),
			manage(act("reload", "重新加载规则", true, func(map[string]string) (string, error) {
				return "firewall-cmd --reload", nil
			})),
			{
				ID: "add-service", Label: "放行服务", Group: "manage", Fields: []*securityField{
					textField("service", "服务名", "http", servicePattern, "如 http、https、ssh"),
				},
				needRoot: true, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "firewall-cmd --permanent --add-service=" + shq(p["service"]) + " && firewall-cmd --reload", nil
				},
			},
			{
				ID: "remove-service", Label: "移除服务", Group: "manage", Danger: true,
				Confirm: "移除后该服务端口将立即不再放行。",
				Fields: []*securityField{
					textField("service", "服务名", "http", servicePattern, "如 http、https、ssh"),
				},
				needRoot: true, timeout: 60 * time.Second,
				build: func(p map[string]string) (string, error) {
					return "firewall-cmd --permanent --remove-service=" + shq(p["service"]) + " && firewall-cmd --reload", nil
				},
			},
			act("service", "服务状态", false, func(map[string]string) (string, error) {
				return "systemctl status firewalld --no-pager", nil
			}),
		},
	}
	// 配置路径来自软件目录；安全应用只补操作，不重复声明配置文件。
	directoryPaths := map[string]bool{
		"/etc/wireguard": true, "/etc/openvpn/server": true, "/etc/openvpn/client": true,
		"/etc/fail2ban/jail.d": true, "/etc/firewalld/zones": true, "~/.password-store": true,
	}
	for slug, app := range securityApps {
		recipe := env.GetRecipe(slug)
		if recipe == nil || recipe.Launch == nil {
			continue
		}
		app.Name = recipe.Name
		app.Description = recipe.Description
		for _, p := range recipe.Launch.ConfigPaths {
			app.Config = append(app.Config, securityConfigEntry{Path: p, IsDir: directoryPaths[p]})
		}
	}
}

// buildGnuPGGenerate 用 batch 参数文件生成一对完整密钥。
//
// 为什么不用 --quick-generate-key：它对 RSA 只生成签名主钥，还得再补一条
// --quick-add-key，并且要靠 UID 反查指纹才能补。参数文件一次就能写清主钥、
// 加密子钥、有效期与口令，生成结果与交互式向导一致。
//
// 参数文件通过 base64 落到目标机的临时文件，整段脚本再由 env.Wrap 编码投递，
// 口令不会出现在任何一层的命令行参数里。
func buildGnuPGGenerate(p map[string]string) (string, error) {
	name := strings.TrimSpace(p["name"])
	email := strings.TrimSpace(p["email"])
	if name == "" {
		return "", errors.New("姓名不能为空")
	}
	// 参数文件是逐行指令：任何换行都会变成新的字段，必须拒绝。
	if strings.ContainsAny(name+email+p["passphrase"], "\r\n") {
		return "", errors.New("参数里不能包含换行")
	}

	var params strings.Builder
	switch p["algo"] {
	case "rsa4096":
		params.WriteString("Key-Type: RSA\nKey-Length: 4096\nKey-Usage: sign,cert\n")
		params.WriteString("Subkey-Type: RSA\nSubkey-Length: 4096\nSubkey-Usage: encrypt\n")
	case "ed25519":
		params.WriteString("Key-Type: EDDSA\nKey-Curve: Ed25519\nKey-Usage: sign,cert\n")
		params.WriteString("Subkey-Type: ECDH\nSubkey-Curve: Curve25519\nSubkey-Usage: encrypt\n")
	default:
		params.WriteString("Key-Type: RSA\nKey-Length: 2048\nKey-Usage: sign,cert\n")
		params.WriteString("Subkey-Type: RSA\nSubkey-Length: 2048\nSubkey-Usage: encrypt\n")
	}
	params.WriteString("Name-Real: " + name + "\n")
	if email != "" {
		params.WriteString("Name-Email: " + email + "\n")
	}
	expire := p["expire"]
	if expire == "" {
		expire = "2y"
	}
	params.WriteString("Expire-Date: " + expire + "\n")
	if p["passphrase"] == "" {
		params.WriteString("%no-protection\n")
	} else {
		params.WriteString("Passphrase: " + p["passphrase"] + "\n")
	}
	params.WriteString("%commit\n")

	encoded := base64.StdEncoding.EncodeToString([]byte(params.String()))
	return strings.Join([]string{
		"set -e",
		"umask 077",
		"PARAM=$(mktemp)",
		`trap 'rm -f "$PARAM"' EXIT`,
		"printf '%s' " + shq(encoded) + ` | base64 -d > "$PARAM"`,
		`gpg --batch --yes --pinentry-mode loopback --generate-key "$PARAM"`,
	}, "\n"), nil
}

// buildGnuPGImport 只接受 ASCII 公钥块。
//
// 私钥块会以明文私钥的形式进入 HTTP 请求与目标机临时文件，这不是这个入口的
// 职责；明确拒绝比"能导入但没人知道传了什么"更安全。
func buildGnuPGImport(p map[string]string) (string, error) {
	armored := strings.TrimSpace(p["armored"])
	if !strings.HasPrefix(armored, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		return "", errors.New("只接受 ASCII 公钥文本（以 -----BEGIN PGP PUBLIC KEY BLOCK----- 开头）")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(armored))
	return strings.Join([]string{
		"set -e",
		"umask 077",
		"KEYFILE=$(mktemp)",
		`trap 'rm -f "$KEYFILE"' EXIT`,
		"printf '%s' " + shq(encoded) + ` | base64 -d > "$KEYFILE"`,
		`gpg --batch --yes --import "$KEYFILE"`,
	}, "\n"), nil
}
