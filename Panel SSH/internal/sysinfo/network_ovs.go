package sysinfo

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------- 现状 ----------

// OvsPort 是 OVS 网桥上的一个端口。
type OvsPort struct {
	Name string `json:"name"`
	// Physical 表示这个端口背后是一块真实的物理网卡。
	//
	// 判定方式是交叉引用 /sys/class/net/*/device，而不是去解析 ovs-vsctl
	// 报出的端口类型：物理口加进网桥后 OVS 会用同名建立普通端口，而
	// internal / patch 这些虚拟端口在 /sys 下没有 device，一眼可辨。
	Physical bool `json:"physical"`
	// CarriesDefaultRoute 表示这台机器当前的默认路由正好从这个网口出去 ——
	// 也就是面板这条 SSH 最可能经过的地方。它是"动这个口会不会把自己关在
	// 门外"最直接的一个信号。
	CarriesDefaultRoute bool `json:"carriesDefaultRoute"`
}

// OvsBridge 是一个 OVS 网桥及其端口。
type OvsBridge struct {
	Name  string    `json:"name"`
	Ports []OvsPort `json:"ports"`
	// CarriesDefaultRoute 表示默认路由的出口已经变成这个网桥本身。
	// 网口一旦被桥接，它的名字就不再出现在路由表里，只看端口上的标记会漏判。
	CarriesDefaultRoute bool `json:"carriesDefaultRoute"`
	// Created 表示这座网桥是面板建的（OVS 数据库里带着我们的 external-id）。
	//
	// 它决定了这座桥能不能从面板删掉：目标机自己的网络服务建的桥（飞牛的
	// enp1s0-ovs 就是）删掉会连带毁掉那台机器的网络配置，面板不该碰。
	Created bool `json:"created"`
}

// OvsState 是目标机的 Open vSwitch 现状。
type OvsState struct {
	// Available 为 false 表示这台机器上没有 ovs-vsctl。此时 Bridges 一定为空，
	// 前端据此决定要不要展示 OVS 相关内容 —— 这是"按能力探测"而不是
	// "按发行版识别"的落点：谁装了 OVS 谁就能用这套功能。
	Available bool `json:"available"`
	// Unreadable 表示找得到 ovs-vsctl，但读不到 OVS 的状态。
	//
	// 必须和"没有网桥"分开，否则界面只能拿"这台机器还没有网桥"去描述一件
	// 完全不同的事：ovs-vsctl 读的是 OVS 自己的数据库，套接字默认只允许
	// root 与 openvswitch 组访问，普通的登录账号跑 list-br 会直接失败。
	// 把失败报成"没有网桥"，用户就会对着自己明明建好的桥反复琢磨是哪里
	// 没打开 —— 而真相是面板没权限看。
	Unreadable bool `json:"unreadable"`
	// UnreadableReason 是"读不到"的原因：ovs-vsctl 自己的那句报错，或面板
	// 提权失败时 su 的那句话。空串表示这次没拿到原因。
	//
	// 摆到界面上，是因为同一句"读不到"背后的修法完全不同 —— 套接字权限不足
	// 要去加 openvswitch 组，口令不对要去核对主机配置，服务没起要去看 systemd。
	// 让用户自己去目标机上复现一遍好把原因问出来，正是这个功能本该省掉的事。
	UnreadableReason string      `json:"unreadableReason"`
	Bridges          []OvsBridge `json:"bridges"`
	// Persistent 表示这台机器上装着我们下发的开机自恢复服务。
	//
	// 必须让用户看得见：面板在他的机器上留了一个常驻服务，这件事不该是
	// 只有翻 systemd 才发现得了的。
	Persistent bool `json:"persistent"`
}

// defaultRouteDevs 给出默认路由的出口设备（IPv4 与 IPv6 各取一条）。
//
// 只取 IPv4 的默认路由是不够的：一台只用 IPv6 出网的机器上，v4 路由表是空的，
// 那样算出来的"管理口"会是空集，危险提示会全部失效。
func defaultRouteDevs(lists ...[]ipRoute) map[string]bool {
	devs := make(map[string]bool)
	for _, routes := range lists {
		for _, route := range routes {
			// iproute2 把 v4 与 v6 的默认路由都归一成 "default"，
			// 但补一个 "::/0" 兜底，免得某个版本改了写法就静默失效。
			if route.Dst != "default" && route.Dst != "::/0" {
				continue
			}
			if route.Dev != "" {
				devs[route.Dev] = true
			}
		}
	}
	return devs
}

// ovsCreatedExternalID 是面板在 OVS 数据库里给自己建的网桥打的标记。
//
// 用 OVS 自己的 external-ids 而不是面板另存一份清单：标记跟着网桥走，
// 面板重装、数据目录丢失、换一台面板来管都不影响它 —— 而"这座桥是谁建的"
// 恰恰是删桥时唯一需要回答的问题。它同时被拼进几条远端脚本，因此改这里
// 就等于改所有地方。
const ovsCreatedExternalID = "panelssh-created"

// ovsReasonLine 把采集到的原因压成一行。
//
// ovs-vsctl 与 su 都可能一次吐好几行，而界面上那条警告只放得下一句话。
// 取第一个非空行：那是错误本身，后面跟着的通常是"怎么重试"之类的提示。
func ovsReasonLine(section string) string {
	for _, line := range strings.Split(section, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// buildOvsState 把采集输出里与 OVS 有关的几节解析成现状。
//
// ###OVS 一节的格式与 Linux 网桥那一节完全相同（「网桥 端口…」），因此复用
// parseBridges：ovs-vsctl list-ports 的输出本来就是一行一个端口，
// 而 OVS 的名字不允许出现空格，按空白切分不会有歧义。
//
// 收整个 sections 而不是几个字符串参数：要读的节已经有三处（现状、是否
// 面板建的、有没有装开机自恢复），一串同类型的字符串参数太容易传错顺序。
func buildOvsState(sections map[string]string, physical map[string]bool, tools []string, defaultDevs map[string]bool) *OvsState {
	state := &OvsState{Bridges: []OvsBridge{}}
	for _, tool := range tools {
		if tool == "ovs-vsctl" {
			state.Available = true
		}
	}
	state.Persistent = strings.TrimSpace(sections[secOvsPersist]) == "yes"
	if !state.Available {
		return state
	}
	// "读不到"只有在找得到 ovs-vsctl 时才有意义 —— 连工具都没有，谈不上读它。
	// 放在这道判断之后而不是之前，是为了让"Unreadable 蕴含 Available"这个
	// 不变式由代码保证，而不是靠"采集脚本不会那样输出"。
	state.Unreadable = strings.TrimSpace(sections[secOvsUnread]) == "yes"
	if state.Unreadable {
		state.UnreadableReason = ovsReasonLine(sections[secOvsReason])
	}

	created := parseNameSet(sections[secOvsCreated])
	parsed := parseBridges(sections[secOvs])
	names := make([]string, 0, len(parsed))
	for name := range parsed {
		if ifaceNamePattern.MatchString(name) {
			names = append(names, name)
		}
	}
	// 名字直接来自 ovs-vsctl 的输出，随后会被拼进恢复脚本，因此按网口名的
	// 白名单过滤一遍。排序是为了让同一台机器每次采集的顺序一致，
	// 否则前端每次轮询都会看到卡片位置跳动。
	sort.Strings(names)

	for _, name := range names {
		ports := make([]OvsPort, 0, len(parsed[name]))
		for _, port := range parsed[name] {
			if !ifaceNamePattern.MatchString(port) {
				continue
			}
			ports = append(ports, OvsPort{
				Name:                port,
				Physical:            physical[port],
				CarriesDefaultRoute: defaultDevs[port],
			})
		}
		sort.SliceStable(ports, func(i, j int) bool {
			// 物理口排在虚拟口前面：用户要操作的是网线，不是网桥自己的内部口。
			if ports[i].Physical != ports[j].Physical {
				return ports[i].Physical
			}
			return naturalLess(ports[i].Name, ports[j].Name)
		})
		state.Bridges = append(state.Bridges, OvsBridge{
			Name:                name,
			Ports:               ports,
			CarriesDefaultRoute: defaultDevs[name],
			Created:             created[name],
		})
	}
	return state
}

// ovsBridgeOf 反查某个网口挂在哪个 OVS 网桥上，没挂返回空串。
//
// 不能复用 Interface.Master：那个字段来自 iproute2 的 master 与
// /sys/class/net/*/bridge/brif，两者描述的都是 Linux 网桥，
// OVS 的端口关系不在它们表示范围内。
func (s *NetworkSnapshot) ovsBridgeOf(iface string) string {
	if s == nil || s.Ovs == nil {
		return ""
	}
	for _, bridge := range s.Ovs.Bridges {
		for _, port := range bridge.Ports {
			if port.Name == iface {
				return bridge.Name
			}
		}
	}
	return ""
}

// ovsBridge 按名字取一座 OVS 网桥，没有返回 nil。
func (s *NetworkSnapshot) ovsBridge(name string) *OvsBridge {
	if s == nil || s.Ovs == nil {
		return nil
	}
	for i := range s.Ovs.Bridges {
		if s.Ovs.Bridges[i].Name == name {
			return &s.Ovs.Bridges[i]
		}
	}
	return nil
}

// carriesDefaultRoute 判断某个设备是不是这台机器当前默认路由的出口。
//
// 后端把空的目的地址归一成 default，IPv6 的默认路由则可能原样写成 ::/0，
// 两种都算 —— 漏掉一种就会在一台只用 IPv6 出网的机器上把所有断连提示
// 变成空话。
func (s *NetworkSnapshot) carriesDefaultRoute(dev string) bool {
	if s == nil || dev == "" {
		return false
	}
	for _, route := range s.Routes {
		if route.Dev == dev && (route.Dst == "default" || route.Dst == "::/0") {
			return true
		}
	}
	return false
}

// ---------- 自建保护装置 ----------
//
// NetworkManager 类的变更由 NM 自己的检查点兜底：面板断连、甚至面板进程
// 整个消失，目标机上的 NM 仍会在窗口结束时把配置回滚回去。
//
// OVS 没有这样的东西。ovs-vsctl 的改动完全不经过 NM，检查点看不见它，
// 于是"改了之后 SSH 再也回不来"就没有任何兜底。这一节补的就是这个缺口，
// 目标是与 NM 等价的三条性质：
//
//  1. 回滚凭据放在目标机上，不依赖面板进程存活；
//  2. 到点自动回滚，不需要面板再发任何指令；
//  3. 拿不到保护就中止变更，绝不"没有保护也照改"。
//
// 做法是把"变更前是什么样"落成一个恢复脚本，再起一个脱离会话的看门狗
// 进程，到点没看到确认标记就执行恢复脚本。

// ovsGuardFileBase 是保护文件路径的前缀。
//
// 名字里带变更 ID：一次变更的全套文件必须同生共死。两次变更共用文件名
// 意味着后一次会把前一次的恢复脚本覆盖掉，前一次就再也回不去了。
func ovsGuardFileBase(id string) string { return "/tmp/panelssh-ovs-" + id }

// OvsGuardFiles 是一次 OVS 变更在目标机上的三个文件。
type OvsGuardFiles struct {
	// Restore 是恢复脚本：把它执行一遍，OVS 就回到变更前的样子。
	Restore string
	// Confirm 是确认标记：看门狗醒来看到它就什么都不做。
	Confirm string
	// Watchdog 记录了看门狗进程的 pid，面板用它确认看门狗真的起来了。
	Watchdog string
}

// OvsGuard 给出一次变更的保护文件路径。id 由面板生成，是十六进制串。
func OvsGuard(id string) OvsGuardFiles {
	base := ovsGuardFileBase(id)
	return OvsGuardFiles{
		Restore:  base + ".restore",
		Confirm:  base + ".confirmed",
		Watchdog: base + ".watchdog",
	}
}

// OvsRestoreScript 生成"把 Open vSwitch 恢复到变更前"的脚本。
//
// 它刻意不记录这次变更做了什么，只记录变更前长什么样。理由是变更执行到
// 一半就断连的情形：反向命令只知道"要撤销 add-port eth1"，若这条命令
// 根本没跑到，反向命令就会去删一个本来不存在的端口；而全量对齐只问
// "现在和变更前差在哪"，无论变更跑到了哪一步，结果都收敛到同一个状态。
//
// 所有命令都带 --if-exists / --may-exist，因此重复执行安全 —— 看门狗到点
// 触发与用户手动点回滚可能同时发生，两者撞上不该把网桥搞坏。
func OvsRestoreScript(before *NetworkSnapshot) string {
	var body strings.Builder
	body.WriteString("#!/bin/sh\n")
	body.WriteString("# 由面板生成：把 Open vSwitch 恢复到本次变更之前的状态。\n")
	// 变更期间 OVS 被卸载掉这类情况不必处理，但脚本可能被留到很晚才由
	// 看门狗执行，多一行守卫比执行到一半报"命令找不到"要干净。
	body.WriteString("command -v ovs-vsctl >/dev/null 2>&1 || exit 0\n")

	if before == nil || before.Ovs == nil {
		body.WriteString("exit 0\n")
		return body.String()
	}

	// 变更前存在的网桥。先收集起来，因为下面两处都要用：哪些桥要建回来，
	// 以及哪些桥是"本来就有的"因而绝不能删。
	existing := make([]string, 0, len(before.Ovs.Bridges))
	for _, bridge := range before.Ovs.Bridges {
		if ifaceNamePattern.MatchString(bridge.Name) {
			existing = append(existing, bridge.Name)
		}
	}

	// 1) 变更中被删掉的网桥要建回来。--may-exist 让这一步在桥还在时无害 ——
	//    恢复脚本可能被执行两遍（看门狗到点与用户手动点回滚撞在一起）。
	for _, name := range existing {
		fmt.Fprintf(&body, "ovs-vsctl --may-exist add-br %s\n", shellQuote(name))
	}

	// 2) 逐座把端口对齐回变更前的样子。
	for _, bridge := range before.Ovs.Bridges {
		if !ifaceNamePattern.MatchString(bridge.Name) {
			continue
		}
		quotedBridge := shellQuote(bridge.Name)

		keep := make([]string, 0, len(bridge.Ports))
		for _, port := range bridge.Ports {
			if ifaceNamePattern.MatchString(port.Name) {
				keep = append(keep, port.Name)
			}
		}

		fmt.Fprintf(&body, "\n# %s\n", bridge.Name)
		if len(keep) == 0 {
			// 变更前这个网桥是空的，现场所有端口都是变更带来的。
			fmt.Fprintf(&body,
				"for p in $(ovs-vsctl --timeout=5 list-ports %s 2>/dev/null); do ovs-vsctl --if-exists del-port %s \"$p\"; done\n",
				quotedBridge, quotedBridge)
			continue
		}

		fmt.Fprintf(&body, "for p in $(ovs-vsctl --timeout=5 list-ports %s 2>/dev/null); do\n", quotedBridge)
		// keep 里的名字已经过白名单校验，不含引号与反引号，可以安全地
		// 作为 case 的模式列表；$p 用引号包住，其中的字符不会被当成通配符。
		fmt.Fprintf(&body, "  case \"$p\" in %s) ;; *) ovs-vsctl --if-exists del-port %s \"$p\" ;; esac\n",
			strings.Join(keep, "|"), quotedBridge)
		body.WriteString("done\n")
		for _, port := range keep {
			fmt.Fprintf(&body, "ovs-vsctl --may-exist add-port %s %s\n", quotedBridge, shellQuote(port))
		}
	}

	// 3) 变更中新建的网桥要删掉。
	//
	//    只认带面板标记的：目标机自己的网络服务建的桥（飞牛的 enp1s0-ovs 就是）
	//    删掉会连带毁掉那台机器的网络配置，而它们本来就出现在变更前的快照里，
	//    会被下面的 case 挡掉。两道保险缺一不可 —— 只看"不在变更前"会误删
	//    并存的系统桥，只看"带标记"则在面板标记被误加时毫无退路。
	//
	//    existing 为空时模式是单个空格，匹配不上任何 " 名字 "，于是现场每座桥
	//    都要过一遍标记检查 —— 正是"变更前没有网桥"时该有的行为。
	body.WriteString("\n# 清理本次变更新建的网桥\n")
	body.WriteString("for b in $(ovs-vsctl --timeout=5 list-br 2>/dev/null); do\n")
	fmt.Fprintf(&body, "  case %s in\n", shellQuote(" "+strings.Join(existing, " ")+" "))
	body.WriteString("    *\" $b \"*) continue ;;\n")
	body.WriteString("  esac\n")
	fmt.Fprintf(&body, "  v=$(ovs-vsctl --timeout=5 get bridge \"$b\" external-ids:%s 2>/dev/null)\n", ovsCreatedExternalID)
	body.WriteString("  case \"$v\" in *1*) ovs-vsctl --if-exists del-br \"$b\" ;; esac\n")
	body.WriteString("done\n")

	body.WriteString("\nexit 0\n")
	return body.String()
}

// OvsGuardCommand 生成建立 OVS 保护的命令。
//
// 三步，缺一不可：
//
//  1. 把"变更前是什么样"落成恢复脚本 —— 它是回滚的唯一凭据，
//     必须落在目标机上，因为面板随时可能连不上；
//  2. 起一个脱离会话的看门狗 —— 它要在面板的 SSH 消失之后仍然活着，
//     这是 NM 检查点"由目标机自己维持"的等价物；
//  3. 确认看门狗真的起来了 —— 没起来就没有回滚能力，此时中止变更
//     与拿不到 NM 检查点时中止是同一个道理。
func OvsGuardCommand(id string, before *NetworkSnapshot, timeoutSec int) string {
	files := OvsGuard(id)
	var body strings.Builder

	// 恢复脚本整体走引号定界符的 heredoc：脚本里含引号、换行与 $，
	// 塞进单行 shell 字符串要处理层层转义，而 heredoc 里除了结束标记
	// 什么都不需要转义。结束标记不可能出现在内容里 —— 里面所有名字
	// 都过了 ifaceNamePattern，写不出下划线以外的标记字符。
	body.WriteString("cat > " + files.Restore + " <<'PANELSSH_OVS_RESTORE_EOF'\n")
	body.WriteString(OvsRestoreScript(before))
	body.WriteString("PANELSSH_OVS_RESTORE_EOF\n")
	body.WriteString("chmod 0700 " + files.Restore + "\n")

	// 先清掉可能残留的同名文件再起看门狗：上一次变更如果正好在收尾时
	// 被打断，留下的确认标记会让这次的看门狗一醒来就以为已经确认过，
	// 那样保护就是形同虚设。
	body.WriteString("rm -f " + files.Confirm + " " + files.Watchdog + "\n")

	// setsid 让看门狗进入独立会话，从面板的 SSH 会话里彻底脱离。
	// 它由提权后的 shell 派生，因此继承 root 权限，到点能自己跑 ovs-vsctl。
	//
	// 收尾也交给它：不管结果是"被确认过"还是"到点自己恢复"，三个文件都由
	// 它删掉。确认与回滚两条路都发生在主机可能正断连的时刻，指望面板收尾
	// 必然会留下一堆永远没人清理的 /tmp 文件。
	body.WriteString("setsid sh -c 'echo $$ > " + files.Watchdog +
		"; sleep " + strconv.Itoa(timeoutSec) +
		"; if [ ! -e " + files.Confirm + " ]; then sh " + files.Restore + "; fi" +
		"; rm -f " + files.Confirm + " " + files.Restore + " " + files.Watchdog +
		"' </dev/null >/dev/null 2>&1 &\n")

	// 看门狗起没起来只能靠它自己写下的 pid 文件判断（setsid 之后父 shell
	// 拿不到子进程的 pid）。留 5 秒余量：正常是毫秒级的事，这里等的是
	// "setsid 不存在"或"进程被策略拦下"这类硬失败。
	body.WriteString("i=0\n")
	body.WriteString("while [ ! -s " + files.Watchdog + " ]; do\n")
	body.WriteString("  i=$((i+1))\n")
	body.WriteString("  if [ \"$i\" -ge 5 ]; then\n")
	body.WriteString("    echo '看门狗进程未能启动，无法建立回滚保护，变更已取消' >&2\n")
	body.WriteString("    exit 1\n")
	body.WriteString("  fi\n")
	body.WriteString("  sleep 1\n")
	body.WriteString("done\n")
	body.WriteString("echo ok\n")
	return body.String()
}

// OvsConfirmCommand 生成确认变更的命令：写下确认标记，看门狗到点后自行退场。
//
// 不直接 kill 看门狗而是留一个标记，是因为确认的这一刻面板可能正在重启、
// 或者刚刚丢包重连。标记文件是两者之间唯一不受进程生死影响的信使：
// 无论谁先谁后，只要标记在，看门狗就不会动配置。
//
// 这里不删任何文件：确认标记正是让看门狗停手的凭据，删了它等于反过来
// 命令看门狗去恢复配置。三个文件统一由看门狗醒来后清理。
func OvsConfirmCommand(id string) string {
	return "touch " + OvsGuard(id).Confirm
}

// OvsRollbackCommand 生成立即回滚的命令。
//
// 顺序不能反过来：先跑恢复脚本的话，看门狗可能在恢复进行到一半时也醒来
// 跑一遍，两份恢复脚本并发操作同一个网桥会互相踩踏。先写确认标记把它
// 劝退，再从容恢复。
func OvsRollbackCommand(id string) string {
	files := OvsGuard(id)
	return "touch " + files.Confirm + "\n" +
		"sh " + files.Restore
}

// ---------- 开机自恢复 ----------
//
// 看门狗只管一次变更的确认窗口。它管不了的是另一件事：许多系统的 OVS 网桥
// 是它自己的网络服务建的（飞牛就是），重启后服务会把网桥重新建出来，而
// 手动挂上去的网口不会跟着回来。用户于是每隔一次重启就要重做一遍。
//
// 这一节负责把面板管过的端口固化在目标机上，并让目标机自己在开机时补回去。
// 三个文件分工明确：
//
//	ovs-desired.conf  纯数据：期望哪个网桥上有哪些端口
//	panelssh-ovs-apply 应用逻辑：读数据、把端口补齐
//	panelssh-ovs.service systemd oneshot：开机跑一次
//
// 数据与逻辑分开，是因为每次变更只需要重写数据文件 —— 那里面只有网桥名和
// 网口名，不需要任何转义处理，也就不容易在拼接时出错。

const (
	// ovsConfigDir 与 ovsDesiredPath 落在 /etc 而不是 /tmp：它必须跨重启存活，
	// 这正是这个功能存在的意义。
	ovsConfigDir   = "/etc/panelssh"
	ovsDesiredPath = ovsConfigDir + "/ovs-desired.conf"
	ovsApplyPath   = "/usr/local/sbin/panelssh-ovs-apply"
	ovsUnitName    = "panelssh-ovs.service"
	ovsUnitPath    = "/etc/systemd/system/" + ovsUnitName
)

// ovsApplyScript 是开机时执行的对齐脚本。
//
// 只补不删，这是它与变更保护用的恢复脚本最关键的区别。网桥上还挂着目标机
// 自己的网络服务建的上行口，删除动作一旦混进来就会把上行口一起摘掉，那是
// 直接断网。收回端口是"变更"该干的事，它有自己的确认窗口和回滚保护。
//
// 等网桥和网口都出现再动手：它们都是开机过程中陆续就绪的，固定 sleep 要么
// 白等要么来不及。
//
// 两个位置参数都有默认值，systemd 调用时不传任何参数：
//
//	$1  期望状态文件路径
//	$2  等待轮数（每轮 2 秒），用来给测试一个不用真等的档位
//
// 等待是有上限的，而且用尽之后照样把已经就绪的部分补上。这一点很重要：
// 期望里如果留了一个再也回不来的网口，上限决定了这台机器每次开机要多等
// 多久，而"补上能补的、剩下的下次开机再说"是它唯一的体面退路。
const ovsApplyScript = `#!/bin/sh
# 由面板生成，请勿手动修改。期望状态见 /etc/panelssh/ovs-desired.conf。
command -v ovs-vsctl >/dev/null 2>&1 || exit 0

DESIRED="${1:-/etc/panelssh/ovs-desired.conf}"
ROUNDS="${2:-30}"
[ -r "$DESIRED" ] || exit 0

i=0
while [ "$i" -lt "$ROUNDS" ]; do
    ready=1
    while read -r bridge ports; do
        [ -n "$bridge" ] || continue
        ovs-vsctl --timeout=5 br-exists "$bridge" 2>/dev/null || ready=0
        for p in $ports; do
            [ -d "/sys/class/net/$p" ] || ready=0
        done
    done < "$DESIRED"
    [ "$ready" = "1" ] && break
    i=$((i + 1))
    sleep 2
done

while read -r bridge ports; do
    [ -n "$bridge" ] || continue
    ovs-vsctl --timeout=5 br-exists "$bridge" 2>/dev/null || continue
    for p in $ports; do
        [ -d "/sys/class/net/$p" ] || continue
        if ovs-vsctl --timeout=5 --may-exist add-port "$bridge" "$p" 2>/dev/null; then
            logger -t panelssh-ovs "网口 $p 已加入 OVS 网桥 $bridge" 2>/dev/null
        else
            logger -t panelssh-ovs "网口 $p 加入 OVS 网桥 $bridge 失败" 2>/dev/null
        fi
    done
done < "$DESIRED"

exit 0
`

// ovsUnit 是开机自恢复的 systemd 单元。
//
// TimeoutStartSec 必须大于脚本内部的等待上限（30 × 2 = 60 秒），
// 否则 systemd 会在脚本还在等网口就绪时把它杀掉，现象是"服务失败、
// 端口没补上"，而真正的原因藏在超时里。
const ovsUnit = `[Unit]
Description=面板下发过的 OVS 网口桥接配置
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
TimeoutStartSec=120
ExecStart=/usr/local/sbin/panelssh-ovs-apply

[Install]
WantedBy=multi-user.target
`

// OvsDesiredBridge 是开机自恢复要维持的一座网桥及其端口。
type OvsDesiredBridge struct {
	Bridge string   `json:"bridge"`
	Ports  []string `json:"ports"`
}

// DesiredOvsState 从一份 OVS 现状里算出该固化哪些端口。
//
// 只取物理口：internal / patch 这些虚拟口由 OVS 或上层系统随网桥一起重建，
// 固化它们只会在开机顺序不对时带来一串失败；而面板操作的对象本来就是网线。
//
// 没有物理口的网桥直接不出现 —— 写进去也是空行，只会让那个文件更难读。
// 返回值按网桥名排序，保证同一份状态每次生成的内容完全一致。
func DesiredOvsState(ovs *OvsState) []OvsDesiredBridge {
	if ovs == nil {
		return nil
	}
	desired := make([]OvsDesiredBridge, 0, len(ovs.Bridges))
	for _, bridge := range ovs.Bridges {
		ports := make([]string, 0, len(bridge.Ports))
		for _, port := range bridge.Ports {
			if port.Physical && ifaceNamePattern.MatchString(port.Name) {
				ports = append(ports, port.Name)
			}
		}
		if len(ports) == 0 {
			continue
		}
		sort.Strings(ports)
		desired = append(desired, OvsDesiredBridge{Bridge: bridge.Name, Ports: ports})
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Bridge < desired[j].Bridge })
	return desired
}

// OvsPersistCommand 生成把开机自恢复对齐到 desired 的命令。
//
// desired 为空表示面板不再需要维持任何端口，此时连同服务一起卸掉 ——
// 不在用户机器上留下一个什么都不做的常驻服务。
//
// 整个命令是幂等的：每次都重写三个文件再 enable，因此不需要"要不要更新"
// 这类判断，也就不会出现判断错了导致目标机上跑着旧逻辑的情况。
func OvsPersistCommand(desired []OvsDesiredBridge) string {
	if len(desired) == 0 {
		return OvsUnpersistCommand()
	}

	var conf strings.Builder
	for _, item := range desired {
		fmt.Fprintf(&conf, "%s %s\n", item.Bridge, strings.Join(item.Ports, " "))
	}

	var body strings.Builder
	// 面板可能是这台机器上第一个用 /etc/panelssh 的东西。
	body.WriteString("mkdir -p " + ovsConfigDir + "\n")
	body.WriteString("cat > " + ovsDesiredPath + " <<'PANELSSH_OVS_DESIRED_EOF'\n")
	body.WriteString(conf.String())
	body.WriteString("PANELSSH_OVS_DESIRED_EOF\n")
	body.WriteString("chmod 0644 " + ovsDesiredPath + "\n")

	body.WriteString("cat > " + ovsApplyPath + " <<'PANELSSH_OVS_APPLY_EOF'\n")
	body.WriteString(ovsApplyScript)
	body.WriteString("PANELSSH_OVS_APPLY_EOF\n")
	body.WriteString("chmod 0755 " + ovsApplyPath + "\n")

	body.WriteString("cat > " + ovsUnitPath + " <<'PANELSSH_OVS_UNIT_EOF'\n")
	body.WriteString(ovsUnit)
	body.WriteString("PANELSSH_OVS_UNIT_EOF\n")

	body.WriteString("systemctl daemon-reload\n")
	body.WriteString("systemctl enable " + ovsUnitName)
	return body.String()
}

// OvsUnpersistCommand 生成卸掉开机自恢复的命令。
//
// 先 disable 再删文件：反过来的话 systemd 会留下一个指向已消失单元的软链，
// 每次开机都报一次 failed。
func OvsUnpersistCommand() string {
	return "systemctl disable " + ovsUnitName + " >/dev/null 2>&1\n" +
		"rm -f " + ovsUnitPath + " " + ovsApplyPath + " " + ovsDesiredPath + "\n" +
		// 只在目录空时删掉：里面还留着别的东西就说明不是我们建的，不该动。
		"rmdir " + ovsConfigDir + " 2>/dev/null\n" +
		"systemctl daemon-reload"
}
