package sysinfo

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// 支持的变更类型。
const (
	// NetChangeAddress 改接口的 IPv4 配置（静态地址或切回 DHCP）。
	NetChangeAddress = "address"
	// NetChangeMTU 改接口 MTU。
	NetChangeMTU = "mtu"
	// NetChangeLinkState 启用或停用一条连接。
	NetChangeLinkState = "link-state"
	// NetChangeBridgeCreate 新建网桥，可同时把若干网口挂上去。
	NetChangeBridgeCreate = "bridge-create"
	// NetChangeBridgeDelete 删除网桥（先释放成员）。
	NetChangeBridgeDelete = "bridge-delete"
	// NetChangeBridgeAttach 把网口挂到已有网桥上。
	NetChangeBridgeAttach = "bridge-attach"
	// NetChangeBridgeDetach 把网口从网桥上摘下来。
	NetChangeBridgeDetach = "bridge-detach"
	// NetChangeOvsAttach 把网口挂到已有的 OVS 网桥上。
	//
	// 与 NetChangeBridgeAttach 面向的是两套完全不同的网桥：前者走 nmcli 管
	// Linux 网桥，后者走 ovs-vsctl 管 Open vSwitch。它们的保护方式也不同
	// （NM 检查点 vs 自建的状态存档，见 network_ovs.go），因此必须是
	// 两个动作而不是一个"自动判断该用哪套"的动作 —— 用户点下去之前
	// 就该知道自己在改哪一套。
	NetChangeOvsAttach = "ovs-attach"
	// NetChangeOvsDetach 把网口从 OVS 网桥上摘下来。
	NetChangeOvsDetach = "ovs-detach"
	// NetChangeOvsBridgeCreate 新建一座 OVS 网桥，可同时挂入若干网口。
	NetChangeOvsBridgeCreate = "ovs-bridge-create"
	// NetChangeOvsBridgeDelete 删除一座 OVS 网桥。
	//
	// 只允许删面板自己建的（见 OvsBridge.Created）。目标机自己的网络服务
	// 建的桥面板不碰 —— 它无从知道那座桥是干什么用的。
	NetChangeOvsBridgeDelete = "ovs-bridge-delete"
)

// NetChangeRequest 是一次网络变更的请求参数。
//
// 所有字段都会经过严格校验：它们最终会被拼进远端 shell 命令，
// 而这里没有任何"相信调用方"的余地 —— 面板的输入边界就是这个函数。
type NetChangeRequest struct {
	Action string `json:"action"`
	// Interface 是目标网口，例如 eth0。改地址、MTU、上下线时必填。
	Interface string `json:"interface"`
	// Connection 是 NetworkManager 连接名。留空时由 Interface 反查。
	Connection string `json:"connection"`
	// Bridge 是网桥名，创建/删除/挂接时使用。
	Bridge string `json:"bridge"`
	// Members 是要挂到网桥上的网口列表。
	Members []string `json:"members"`
	// Method 为 manual 或 auto：静态地址用前者，DHCP 用后者。
	Method string `json:"method"`
	// Addresses 是静态地址（CIDR 形式），仅 manual 时有意义。
	Addresses []string `json:"addresses"`
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
	MTU       int      `json:"mtu"`
	// Up 是 link-state 的目标状态：true 启用，false 停用。
	Up bool `json:"up"`
}

// NetChangePlan 是一次变更的执行计划。
//
// 命令被刻意分成两段，这是整个机制的核心：
//
//   - Safe 段只改配置文件（nmcli con add / con mod），不碰运行态。
//     它可以同步执行并把报错原样返回给用户 —— 名字冲突、权限不足、
//     属性名写错这类问题会在这里立刻暴露，不会等断连之后才发现。
//
//   - Risky 段才真正让配置生效（con up / con down / con delete），
//     它会切断包括面板自己那条 SSH 在内的既有连接。这一段必须延迟、
//     脱离终端执行，并由 NetworkManager 检查点兜底。
type NetChangePlan struct {
	// Summary 是一句人话描述，直接展示在确认框里。
	Summary string `json:"summary"`
	// Safe 是同步执行段，任一命令失败即整体中止并回滚检查点。
	Safe []string `json:"safe"`
	// Risky 是延迟执行段，会断开现有连接。
	Risky  []string         `json:"risky"`
	Expect *NetChangeExpect `json:"-"`
}

// NetChangeExpect 描述"变更成功后应该看到什么"，用于验证变更是否真的生效。
//
// 没有这一步就无法区分两种结局：变更生效后 SSH 恢复，与变更失败被检查点
// 自动回滚后 SSH 恢复 —— 两者的现象完全一样（主机又能连上了），
// 只看连通性会把回滚误报成成功。
type NetChangeExpect struct {
	Action    string
	Interface string
	Bridge    string
	Members   []string
	Addresses []string
	Method    string
	MTU       int
	Up        bool
}

// 名字类参数的白名单。连接名可以是中文（NM 的默认连接名就是"有线连接 1"），
// 因此不能只放行 ASCII；但也不能任其包含引号、反引号、分号这些会改变
// 命令结构的字符。放行的这些字符在单引号包裹下都无害。
var (
	connectionNamePattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ():._@+-]{0,63}$`)
	ifaceNamePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,14}$`)
)

// shellQuote 用单引号包裹一个参数。
//
// 单引号内的内容不会被 shell 解释，唯一需要处理的是参数自身含单引号的情况：
// 结束当前单引号、插入一个转义的单引号、再重新开始单引号。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PlanNetworkChange 校验请求并生成执行计划。
//
// snapshot 是变更前的现状，用于确认目标接口/连接确实存在，
// 以及反查网口对应的 NetworkManager 连接名。
func PlanNetworkChange(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	if snapshot == nil {
		return nil, errors.New("没有该主机的网络快照，无法校验变更目标")
	}

	// OVS 类的变更优先分派：它们不需要 NetworkManager，也不该被下面那道
	// "必须是 NetworkManager" 的门槛挡住 —— ovs-vsctl 的改动本来就在 NM
	// 的管辖之外，只是改错了没有检查点兜底，需要另一套保护（network_ovs.go）。
	switch req.Action {
	case NetChangeOvsAttach:
		return planOvsAttach(req, snapshot)
	case NetChangeOvsDetach:
		return planOvsDetach(req, snapshot)
	case NetChangeOvsBridgeCreate:
		return planOvsBridgeCreate(req, snapshot)
	case NetChangeOvsBridgeDelete:
		return planOvsBridgeDelete(req, snapshot)
	}

	if snapshot.Manager != "NetworkManager" {
		// 其它后端（netplan / ifcfg / ifupdown）没有等价的检查点机制，
		// 改错了就只能靠带外手段救回来，因此这里拒绝执行而不是"尽力而为"。
		return nil, fmt.Errorf("目标主机的网络由 %s 管理，面板目前只支持在 NetworkManager 上执行变更", describeManager(snapshot.Manager))
	}

	switch req.Action {
	case NetChangeAddress:
		return planAddress(req, snapshot)
	case NetChangeMTU:
		return planMTU(req, snapshot)
	case NetChangeLinkState:
		return planLinkState(req, snapshot)
	case NetChangeBridgeCreate:
		return planBridgeCreate(req, snapshot)
	case NetChangeBridgeDelete:
		return planBridgeDelete(req, snapshot)
	case NetChangeBridgeAttach:
		return planBridgeAttach(req, snapshot)
	case NetChangeBridgeDetach:
		return planBridgeDetach(req, snapshot)
	default:
		return nil, fmt.Errorf("不支持的变更类型：%s", req.Action)
	}
}

func describeManager(manager string) string {
	if manager == "" || manager == "unknown" {
		return "未知的配置后端"
	}
	return manager
}

// ---------- 各类变更 ----------

func planAddress(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	iface, err := requireInterface(snapshot, req.Interface)
	if err != nil {
		return nil, err
	}
	conn, err := resolveConnection(iface, req.Connection)
	if err != nil {
		return nil, err
	}

	switch req.Method {
	case "auto":
		// 切回 DHCP 时必须显式清空静态项：只把 method 改成 auto 的话，
		// 原来那几条 static 地址仍留在配置里，NM 会一起用上。
		return &NetChangePlan{
			Summary: fmt.Sprintf("把 %s（连接「%s」）切回 DHCP", iface.Name, conn),
			Safe: []string{
				fmt.Sprintf("nmcli con mod %s ipv4.method auto ipv4.addresses '' ipv4.gateway '' ipv4.dns ''", shellQuote(conn)),
			},
			Risky: []string{fmt.Sprintf("nmcli con up %s", shellQuote(conn))},
			Expect: &NetChangeExpect{
				Action:    NetChangeAddress,
				Interface: iface.Name,
				Method:    "auto",
			},
		}, nil

	case "manual":
		addresses, err := validateAddresses(req.Addresses)
		if err != nil {
			return nil, err
		}
		gateway, err := validateOptionalAddr(req.Gateway)
		if err != nil {
			return nil, fmt.Errorf("网关地址无效：%w", err)
		}
		dns, err := validateDNS(req.DNS)
		if err != nil {
			return nil, err
		}

		command := fmt.Sprintf("nmcli con mod %s ipv4.method manual ipv4.addresses %s ipv4.gateway %s ipv4.dns %s",
			shellQuote(conn), shellQuote(strings.Join(addresses, ",")), shellQuote(gateway), shellQuote(strings.Join(dns, ",")))

		summary := fmt.Sprintf("把 %s（连接「%s」）设为静态 %s", iface.Name, conn, strings.Join(addresses, "、"))
		if gateway != "" {
			summary += "，网关 " + gateway
		}

		return &NetChangePlan{
			Summary: summary,
			Safe:    []string{command},
			Risky:   []string{fmt.Sprintf("nmcli con up %s", shellQuote(conn))},
			Expect: &NetChangeExpect{
				Action:    NetChangeAddress,
				Interface: iface.Name,
				Method:    "manual",
				Addresses: addresses,
			},
		}, nil

	default:
		return nil, errors.New("地址方式只支持 manual（静态）或 auto（DHCP）")
	}
}

func planMTU(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	iface, err := requireInterface(snapshot, req.Interface)
	if err != nil {
		return nil, err
	}
	if iface.Type != "ethernet" {
		// 属性名随连接类型变化（ethernet.mtu / bridge.mtu / bond.mtu），
		// 在没有逐一验证之前只放行最常见的以太网口，避免写出无效属性。
		return nil, fmt.Errorf("MTU 变更目前只支持以太网口，%s 的类型是 %s", iface.Name, iface.Type)
	}
	conn, err := resolveConnection(iface, req.Connection)
	if err != nil {
		return nil, err
	}
	if req.MTU < 68 || req.MTU > 65535 {
		return nil, errors.New("MTU 需要在 68 到 65535 之间")
	}

	return &NetChangePlan{
		Summary: fmt.Sprintf("把 %s 的 MTU 改为 %d", iface.Name, req.MTU),
		Safe: []string{
			fmt.Sprintf("nmcli con mod %s ethernet.mtu %d", shellQuote(conn), req.MTU),
		},
		Risky: []string{fmt.Sprintf("nmcli con up %s", shellQuote(conn))},
		Expect: &NetChangeExpect{
			Action:    NetChangeMTU,
			Interface: iface.Name,
			MTU:       req.MTU,
		},
	}, nil
}

func planLinkState(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	iface, err := requireInterface(snapshot, req.Interface)
	if err != nil {
		return nil, err
	}
	conn, err := resolveConnection(iface, req.Connection)
	if err != nil {
		return nil, err
	}
	if iface.AdminUp == req.Up {
		// 状态已经是要达成的样子，再下发一次只会白白承担一次断连风险。
		state := "停用"
		if req.Up {
			state = "启用"
		}
		return nil, fmt.Errorf("%s 当前已经是%s状态，无需变更", iface.Name, state)
	}

	verb := "down"
	summary := fmt.Sprintf("停用 %s（连接「%s」）", iface.Name, conn)
	if req.Up {
		verb = "up"
		summary = fmt.Sprintf("启用 %s（连接「%s」）", iface.Name, conn)
	}

	return &NetChangePlan{
		Summary: summary,
		Risky:   []string{fmt.Sprintf("nmcli con %s %s", verb, shellQuote(conn))},
		Expect: &NetChangeExpect{
			Action:    NetChangeLinkState,
			Interface: iface.Name,
			Up:        req.Up,
		},
	}, nil
}

func planBridgeCreate(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	bridge, err := validateBridgeName(req.Bridge)
	if err != nil {
		return nil, err
	}
	if snapshot.interfaceByName(bridge) != nil {
		return nil, fmt.Errorf("接口 %s 已存在，请换一个网桥名", bridge)
	}
	if len(req.Members) == 0 {
		return nil, errors.New("请至少选择一个要加入网桥的网口")
	}
	if len(req.Members) > 16 {
		return nil, errors.New("单个网桥最多挂接 16 个网口")
	}

	memberConns := make([]string, 0, len(req.Members))
	for _, member := range req.Members {
		iface, err := requireInterface(snapshot, member)
		if err != nil {
			return nil, err
		}
		if iface.Master != "" {
			return nil, fmt.Errorf("%s 已经挂在 %s 上，请先从原网桥移出", iface.Name, iface.Master)
		}
		if iface.Type == "loopback" {
			return nil, errors.New("回环接口不能加入网桥")
		}
		conn, err := resolveConnection(iface, "")
		if err != nil {
			return nil, err
		}
		memberConns = append(memberConns, conn)
	}

	// 网桥自身的地址配置：沿用传入的静态参数或 DHCP。
	addArgs := fmt.Sprintf("nmcli con add type bridge ifname %s con-name %s", shellQuote(bridge), shellQuote(bridge))
	switch req.Method {
	case "auto":
		addArgs += " ipv4.method auto"
	case "manual", "":
		addresses, err := validateAddresses(req.Addresses)
		if err != nil {
			return nil, err
		}
		gateway, err := validateOptionalAddr(req.Gateway)
		if err != nil {
			return nil, fmt.Errorf("网关地址无效：%w", err)
		}
		dns, err := validateDNS(req.DNS)
		if err != nil {
			return nil, err
		}
		addArgs += fmt.Sprintf(" ipv4.method manual ipv4.addresses %s ipv4.gateway %s ipv4.dns %s",
			shellQuote(strings.Join(addresses, ",")), shellQuote(gateway), shellQuote(strings.Join(dns, ",")))
	default:
		return nil, errors.New("网桥地址方式只支持 manual（静态）或 auto（DHCP）")
	}

	safe := []string{addArgs}
	// 把成员连接改为网桥的从设备。这一步只改配置文件里的 master 属性，
	// 设备真正切换过去要等网桥被激活（Risky 段）。
	risky := make([]string, 0, len(memberConns)+1)
	for _, conn := range memberConns {
		risky = append(risky, fmt.Sprintf("nmcli con mod %s master %s", shellQuote(conn), shellQuote(bridge)))
	}
	risky = append(risky, fmt.Sprintf("nmcli con up %s", shellQuote(bridge)))

	return &NetChangePlan{
		Summary: fmt.Sprintf("新建网桥 %s，成员 %s", bridge, strings.Join(req.Members, "、")),
		Safe:    safe,
		Risky:   risky,
		Expect: &NetChangeExpect{
			Action:    NetChangeBridgeCreate,
			Interface: bridge,
			Bridge:    bridge,
			Members:   req.Members,
			Method:    req.Method,
		},
	}, nil
}

func planBridgeDelete(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	bridge := strings.TrimSpace(req.Bridge)
	iface, err := requireInterface(snapshot, bridge)
	if err != nil {
		return nil, err
	}
	if iface.Type != "bridge" {
		return nil, fmt.Errorf("%s 不是网桥（类型为 %s）", bridge, iface.Type)
	}
	conn, err := resolveConnection(iface, req.Connection)
	if err != nil {
		return nil, err
	}

	// 先摘掉所有成员再删网桥：成员连接在 master 仍指向一个不存在的网桥时
	// 会变成"等待主机"状态，之后还得手工收拾。
	risky := make([]string, 0, len(iface.Members)+1)
	for _, member := range iface.Members {
		memberIface := snapshot.interfaceByName(member)
		if memberIface == nil {
			continue
		}
		memberConn, err := resolveConnection(memberIface, "")
		if err != nil {
			return nil, err
		}
		risky = append(risky, fmt.Sprintf("nmcli con mod %s master ''", shellQuote(memberConn)))
	}
	risky = append(risky, fmt.Sprintf("nmcli con delete %s", shellQuote(conn)))

	summary := fmt.Sprintf("删除网桥 %s", bridge)
	if len(iface.Members) > 0 {
		summary += fmt.Sprintf("，并释放成员 %s", strings.Join(iface.Members, "、"))
	}

	return &NetChangePlan{
		Summary: summary,
		Risky:   risky,
		Expect: &NetChangeExpect{
			Action:    NetChangeBridgeDelete,
			Interface: bridge,
			Bridge:    bridge,
		},
	}, nil
}

func planBridgeAttach(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	iface, err := requireInterface(snapshot, req.Interface)
	if err != nil {
		return nil, err
	}
	bridge, err := requireBridge(snapshot, req.Bridge)
	if err != nil {
		return nil, err
	}
	if iface.Master == bridge.Name {
		return nil, fmt.Errorf("%s 已经挂在 %s 上", iface.Name, bridge.Name)
	}
	if iface.Master != "" {
		return nil, fmt.Errorf("%s 已挂在 %s 上，请先移出", iface.Name, iface.Master)
	}
	if iface.Type == "loopback" {
		return nil, errors.New("回环接口不能加入网桥")
	}
	conn, err := resolveConnection(iface, req.Connection)
	if err != nil {
		return nil, err
	}
	bridgeConn, err := resolveConnection(bridge, "")
	if err != nil {
		return nil, err
	}

	return &NetChangePlan{
		Summary: fmt.Sprintf("把 %s 加入网桥 %s", iface.Name, bridge.Name),
		Risky: []string{
			fmt.Sprintf("nmcli con mod %s master %s", shellQuote(conn), shellQuote(bridgeConn)),
			fmt.Sprintf("nmcli con up %s", shellQuote(bridgeConn)),
		},
		Expect: &NetChangeExpect{
			Action:    NetChangeBridgeAttach,
			Interface: iface.Name,
			Bridge:    bridge.Name,
		},
	}, nil
}

func planBridgeDetach(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	iface, err := requireInterface(snapshot, req.Interface)
	if err != nil {
		return nil, err
	}
	if iface.Master == "" {
		return nil, fmt.Errorf("%s 没有挂在任何网桥上", iface.Name)
	}
	conn, err := resolveConnection(iface, req.Connection)
	if err != nil {
		return nil, err
	}

	return &NetChangePlan{
		Summary: fmt.Sprintf("把 %s 从网桥 %s 移出", iface.Name, iface.Master),
		// 摘下来的网口会重新按自己的连接配置取地址，这一步同样会瞬断。
		Risky: []string{
			fmt.Sprintf("nmcli con mod %s master ''", shellQuote(conn)),
			fmt.Sprintf("nmcli con up %s", shellQuote(conn)),
		},
		Expect: &NetChangeExpect{
			Action:    NetChangeBridgeDetach,
			Interface: iface.Name,
		},
	}, nil
}

// ---------- OVS 桥接 ----------
//
// 这些动作都只改运行态，没有"先改配置再让配置生效"的两段结构 ——
// OVS 的拓扑就是它的运行态本身，执行即生效。因此 Safe 段一律为空，
// 保护装置由 api 层在下发 Risky 之前先建立（见 network_ovs.go）。

func planOvsAttach(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	bridge, err := requireOvsBridge(snapshot, req.Bridge)
	if err != nil {
		return nil, err
	}
	if len(req.Members) == 0 {
		return nil, errors.New("请至少选择一个要加入网桥的网口")
	}

	members, err := validateOvsMembers(snapshot, req.Members, bridge.Name)
	if err != nil {
		return nil, err
	}

	risky := make([]string, 0, len(members))
	for _, member := range members {
		// --may-exist 而不是裸 add-port：变更脚本可能因为重试或看门狗回滚
		// 之后的再执行而跑第二遍，此时端口已经在桥上，裸 add-port 会报错。
		risky = append(risky, fmt.Sprintf("ovs-vsctl --may-exist add-port %s %s",
			shellQuote(bridge.Name), shellQuote(member)))
	}

	return &NetChangePlan{
		Summary: fmt.Sprintf("把 %s 加入 OVS 网桥 %s", strings.Join(members, "、"), bridge.Name),
		Risky:   risky,
		Expect: &NetChangeExpect{
			Action:  NetChangeOvsAttach,
			Bridge:  bridge.Name,
			Members: members,
		},
	}, nil
}

func planOvsBridgeCreate(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	if err := requireOvsAvailable(snapshot); err != nil {
		return nil, err
	}
	bridge, err := validateBridgeName(req.Bridge)
	if err != nil {
		return nil, err
	}
	// 名字不能撞上任何已有接口：OVS 建桥时会额外创建一个同名的内部端口，
	// 名字被占用时建出来的东西和用户以为的完全不是一回事。
	if snapshot.interfaceByName(bridge) != nil {
		return nil, fmt.Errorf("接口 %s 已存在，请换一个网桥名", bridge)
	}

	members, err := validateOvsMembers(snapshot, req.Members, bridge)
	if err != nil {
		return nil, err
	}

	// 顺序有讲究：建桥之后立刻打标记，然后再挂端口。
	//
	// 标记必须紧跟着建桥，因为回滚要*删掉*这座新建的桥，而恢复脚本只有
	// 认得出"这是我们建的"才敢动手。要是把它排在挂端口之后，那么在挂端口
	// 那一步断连时，恢复脚本面对的是一座没有标记、来历不明的桥 —— 它只能
	// 放着不管，把半成品永久留在目标机上。
	risky := []string{
		fmt.Sprintf("ovs-vsctl add-br %s", shellQuote(bridge)),
		fmt.Sprintf("ovs-vsctl set bridge %s external-ids:%s=1", shellQuote(bridge), ovsCreatedExternalID),
	}
	for _, member := range members {
		risky = append(risky, fmt.Sprintf("ovs-vsctl --may-exist add-port %s %s",
			shellQuote(bridge), shellQuote(member)))
	}

	summary := fmt.Sprintf("新建 OVS 网桥 %s", bridge)
	if len(members) > 0 {
		summary += "，成员 " + strings.Join(members, "、")
	}
	// 成员里若有用着默认路由的网口，地址会转移到新桥上，这一步就断连了。
	// 这是这个动作最需要提前说清楚的地方。
	summary += ovsRiskNote(snapshot, members, nil)

	return &NetChangePlan{
		Summary: summary,
		Risky:   risky,
		Expect: &NetChangeExpect{
			Action:  NetChangeOvsBridgeCreate,
			Bridge:  bridge,
			Members: members,
		},
	}, nil
}

func planOvsBridgeDelete(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	bridge, err := requireOvsBridge(snapshot, req.Bridge)
	if err != nil {
		return nil, err
	}
	// 只删面板自己建的。目标机自己的网络服务建的桥（飞牛的 enp1s0-ovs 就是）
	// 删掉会连带毁掉那台机器的网络配置，而面板无从知道那座桥承担着什么，
	// 也就无从判断该不该"帮"用户删掉它。
	if !bridge.Created {
		return nil, fmt.Errorf(
			"%s 不是面板建的，面板不删它：这类网桥通常由目标机自己的网络服务维护，删掉会连带毁掉那台机器的网络配置",
			bridge.Name)
	}

	// del-br 会连同端口一起释放，被桥接的物理网口会变回独立网卡。
	summary := fmt.Sprintf("删除 OVS 网桥 %s，并释放它的成员", bridge.Name)
	if bridge.CarriesDefaultRoute {
		summary += "。注意：这台机器的默认路由出口就是这个网桥，删除它会立刻切断面板的连接"
	}

	return &NetChangePlan{
		Summary: summary,
		Risky:   []string{fmt.Sprintf("ovs-vsctl --if-exists del-br %s", shellQuote(bridge.Name))},
		Expect: &NetChangeExpect{
			Action: NetChangeOvsBridgeDelete,
			Bridge: bridge.Name,
		},
	}, nil
}

// ovsRiskNote 给摘要是补上"这一步会不会切断面板自己那条连接"。
//
// 判据是默认路由的出口：网口进桥之后它的地址会转移到桥上，管理流量跟着走，
// 面板就断了。不能因此拒绝执行（有人恰恰就要这么改），但必须在按下按钮之前
// 说清楚，并且让他知道断了也会在窗口结束后自动恢复。
func ovsRiskNote(snapshot *NetworkSnapshot, members []string, bridge *OvsBridge) string {
	risky := make([]string, 0, len(members)+1)
	for _, member := range members {
		if snapshot.carriesDefaultRoute(member) {
			risky = append(risky, member)
		}
	}
	if bridge != nil && bridge.CarriesDefaultRoute {
		risky = append(risky, bridge.Name)
	}
	if len(risky) == 0 {
		return ""
	}
	return "。注意：" + strings.Join(risky, "、") +
		" 承载着这台机器当前的默认路由，这一步会切断面板与目标机的连接；" +
		"若一直连不回来，确认窗口结束后目标机会自动恢复到变更前的状态"
}

// validateOvsMembers 校验一组要挂到 OVS 网桥上的网口，返回规范化后的名字。
//
// 挂到已有网桥和新建网桥共用这一段：两者对"哪些网口能挂上去"的要求完全一样，
// 各写一遍迟早会分叉。
func validateOvsMembers(snapshot *NetworkSnapshot, raw []string, bridge string) ([]string, error) {
	if len(raw) > 16 {
		return nil, errors.New("单个网桥最多挂接 16 个网口")
	}

	members := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, member := range raw {
		iface, err := requireInterface(snapshot, member)
		if err != nil {
			return nil, err
		}
		if seen[iface.Name] {
			return nil, fmt.Errorf("%s 被选了两次", iface.Name)
		}
		seen[iface.Name] = true

		if iface.Name == bridge {
			return nil, errors.New("网桥本身不能作为端口加入自己")
		}
		if iface.Type == "loopback" {
			return nil, errors.New("回环接口不能加入网桥")
		}
		// OVS 的端口关系不在 Interface.Master 里，必须单独反查；但 Linux
		// 网桥那一侧也要看一眼 —— 一个网口不能同时属于两套桥接体系，
		// 那样地址归谁、由谁拉起都会变得说不清。
		switch current := snapshot.ovsBridgeOf(iface.Name); {
		case current == bridge:
			return nil, fmt.Errorf("%s 已经挂在 OVS 网桥 %s 上", iface.Name, bridge)
		case current != "":
			return nil, fmt.Errorf("%s 已挂在 OVS 网桥 %s 上，请先移出", iface.Name, current)
		case iface.Master != "":
			return nil, fmt.Errorf("%s 已挂在 Linux 网桥 %s 上，请先移出", iface.Name, iface.Master)
		}
		members = append(members, iface.Name)
	}
	return members, nil
}

func planOvsDetach(req NetChangeRequest, snapshot *NetworkSnapshot) (*NetChangePlan, error) {
	iface, err := requireInterface(snapshot, req.Interface)
	if err != nil {
		return nil, err
	}
	current := snapshot.ovsBridgeOf(iface.Name)
	if current == "" {
		return nil, fmt.Errorf("%s 没有挂在任何 OVS 网桥上", iface.Name)
	}
	// 调用方给了网桥名就核对一下：请求与实际不一致说明界面上的数据是旧的，
	// 此时按请求执行会摘错网桥。宁可报错让用户刷新，也不猜。
	if requested := strings.TrimSpace(req.Bridge); requested != "" && requested != current {
		return nil, fmt.Errorf("%s 实际挂在 OVS 网桥 %s 上，与请求的 %s 不一致，请刷新后重试",
			iface.Name, current, requested)
	}

	return &NetChangePlan{
		Summary: fmt.Sprintf("把 %s 从 OVS 网桥 %s 移出", iface.Name, current),
		Risky: []string{
			fmt.Sprintf("ovs-vsctl --if-exists del-port %s %s", shellQuote(current), shellQuote(iface.Name)),
		},
		Expect: &NetChangeExpect{
			Action:    NetChangeOvsDetach,
			Interface: iface.Name,
			Bridge:    current,
		},
	}, nil
}

// requireOvsAvailable 确认目标机上装了 Open vSwitch，并且它的现状是读得到的。
//
// 读不到时必须拒绝，理由与界面上禁用按钮的那条一样：变更计划建立在现状之上，
// 而一份"什么都没看到"的现状会被当成"这台机器上还没有网桥" —— 据此规划出来
// 的动作可能去删、去改建一座其实已经存在的桥。
func requireOvsAvailable(snapshot *NetworkSnapshot) error {
	if snapshot.Ovs == nil || !snapshot.Ovs.Available {
		return errors.New("目标主机上没有 Open vSwitch（ovs-vsctl 不存在），无法执行 OVS 桥接变更")
	}
	if snapshot.Ovs.Unreadable {
		return errors.New("读不到目标主机的 OVS 状态，无法据此规划变更。" +
			"请确认面板在这台机器上能不能提权（su 提权口令），以及 OVS 服务是否在运行")
	}
	return nil
}

// requireOvsBridge 确认目标机上确实有 Open vSwitch，且请求的网桥是其中之一。
func requireOvsBridge(snapshot *NetworkSnapshot, name string) (*OvsBridge, error) {
	if err := requireOvsAvailable(snapshot); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("请先选择 OVS 网桥")
	}
	for i := range snapshot.Ovs.Bridges {
		if snapshot.Ovs.Bridges[i].Name == name {
			return &snapshot.Ovs.Bridges[i], nil
		}
	}
	return nil, fmt.Errorf("主机上没有名为 %s 的 OVS 网桥", name)
}

// ---------- 校验辅助 ----------

func requireInterface(snapshot *NetworkSnapshot, name string) (*Interface, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("请先选择网口")
	}
	if !ifaceNamePattern.MatchString(name) {
		return nil, fmt.Errorf("网口名 %q 不合法", name)
	}
	iface := snapshot.interfaceByName(name)
	if iface == nil {
		return nil, fmt.Errorf("主机上不存在网口 %s", name)
	}
	return iface, nil
}

func requireBridge(snapshot *NetworkSnapshot, name string) (*Interface, error) {
	bridge, err := validateBridgeName(name)
	if err != nil {
		return nil, err
	}
	iface := snapshot.interfaceByName(bridge)
	if iface == nil {
		return nil, fmt.Errorf("主机上不存在网桥 %s", bridge)
	}
	if iface.Type != "bridge" {
		return nil, fmt.Errorf("%s 不是网桥（类型为 %s）", bridge, iface.Type)
	}
	return iface, nil
}

func validateBridgeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("网桥名不能为空")
	}
	if !ifaceNamePattern.MatchString(name) {
		return "", fmt.Errorf("网桥名 %q 不合法：只能用字母、数字与 . _ : @ - ，且不超过 15 个字符", name)
	}
	return name, nil
}

// resolveConnection 找出接口对应的 NetworkManager 连接名。
//
// 连接名是后续所有 nmcli 命令的操作对象，取错就会改到另一台设备的配置上，
// 所以这里宁可报错也不猜：设备未被 NM 托管、或调用方指定的连接名与设备对不上，
// 都直接拒绝。
func resolveConnection(iface *Interface, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" && !connectionNamePattern.MatchString(requested) {
		return "", fmt.Errorf("连接名 %q 不合法", requested)
	}
	if iface.ConnName == "" {
		return "", fmt.Errorf("网口 %s 不由 NetworkManager 管理，无法通过面板修改", iface.Name)
	}
	if requested != "" && requested != iface.ConnName {
		return "", fmt.Errorf("连接名 %q 与网口 %s 的实际连接「%s」不一致", requested, iface.Name, iface.ConnName)
	}
	return iface.ConnName, nil
}

// validateAddresses 校验地址列表并返回规范化后的 CIDR 形式。
func validateAddresses(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("请至少填写一个静态地址")
	}
	if len(raw) > 8 {
		return nil, errors.New("单个接口最多配置 8 个地址")
	}

	out := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, fmt.Errorf("地址 %q 不是合法的 CIDR 形式（例如 192.168.1.10/24）", item)
		}
		// 用 Addr 而不是 Masked().Addr()：后者会把主机位抹掉，
		// 192.168.1.10/24 会变成网络地址 192.168.1.0/24，配上去就错了一位。
		out = append(out, prefix.Addr().String()+"/"+strconv.Itoa(prefix.Bits()))
	}
	if len(out) == 0 {
		return nil, errors.New("请至少填写一个静态地址")
	}
	return out, nil
}

func validateOptionalAddr(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return "", fmt.Errorf("%q 不是合法的 IP 地址", raw)
	}
	return addr.String(), nil
}

func validateDNS(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > 4 {
		return nil, errors.New("最多填写 4 个 DNS 服务器")
	}

	out := make([]string, 0, len(raw))
	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("DNS %q 不是合法的 IP 地址", item)
		}
		out = append(out, addr.String())
	}
	return out, nil
}

func (s *NetworkSnapshot) interfaceByName(name string) *Interface {
	if s == nil {
		return nil
	}
	for i := range s.Interfaces {
		if s.Interfaces[i].Name == name {
			return &s.Interfaces[i]
		}
	}
	return nil
}

// ---------- 变更生效校验 ----------

// Verify 检查变更是否真的达效。
func (e *NetChangeExpect) Verify(after *NetworkSnapshot) error {
	if after == nil {
		return errors.New("变更后无法读取网络状态")
	}

	switch e.Action {
	case NetChangeOvsAttach, NetChangeOvsDetach, NetChangeOvsBridgeCreate, NetChangeOvsBridgeDelete:
		// OVS 类的核对在这里统一拦住：读不到 OVS 状态时，"没看到那座网桥"
		// 与"那座网桥真的没了"完全分不开 —— 照后面各分支走下去，会把一次
		// 其实已经被看门狗回滚掉的变更报成生效。
		if after.Ovs == nil || !after.Ovs.Available || after.Ovs.Unreadable {
			return errors.New("变更后无法读取 OVS 状态")
		}
	}

	switch e.Action {
	case NetChangeAddress:
		iface := after.interfaceByName(e.Interface)
		if iface == nil {
			return fmt.Errorf("变更后主机上找不到网口 %s", e.Interface)
		}
		if e.Method == "auto" {
			for _, addr := range iface.Addrs {
				if addr.Family == "ipv4" && addr.Dynamic {
					return nil
				}
			}
			// 地址也可能被 DHCP 分配到了别的接口（例如已桥接），放宽到全表再找一次。
			if snapshotHasDynamicIPv4(after) {
				return nil
			}
			return fmt.Errorf("%s 尚未通过 DHCP 取得地址", e.Interface)
		}
		for _, want := range e.Addresses {
			if !snapshotHasAddress(after, e.Interface, want) {
				return fmt.Errorf("变更后未在 %s 上看到地址 %s", e.Interface, want)
			}
		}
		return nil

	case NetChangeMTU:
		iface := after.interfaceByName(e.Interface)
		if iface == nil {
			return fmt.Errorf("变更后主机上找不到网口 %s", e.Interface)
		}
		if iface.MTU != e.MTU {
			return fmt.Errorf("MTU 仍是 %d，未变成 %d", iface.MTU, e.MTU)
		}
		return nil

	case NetChangeLinkState:
		iface := after.interfaceByName(e.Interface)
		if iface == nil {
			return fmt.Errorf("变更后主机上找不到网口 %s", e.Interface)
		}
		if iface.AdminUp != e.Up {
			state := "停用"
			if e.Up {
				state = "启用"
			}
			return fmt.Errorf("%s 未能%s", e.Interface, state)
		}
		return nil

	case NetChangeBridgeCreate:
		iface := after.interfaceByName(e.Bridge)
		if iface == nil {
			return fmt.Errorf("变更后仍未看到网桥 %s", e.Bridge)
		}
		if iface.Type != "bridge" {
			return fmt.Errorf("%s 的类型是 %s，不是网桥", e.Bridge, iface.Type)
		}
		return verifyMembers(after, e.Bridge, e.Members)

	case NetChangeBridgeDelete:
		if iface := after.interfaceByName(e.Bridge); iface != nil {
			return fmt.Errorf("网桥 %s 仍然存在", e.Bridge)
		}
		return nil

	case NetChangeBridgeAttach:
		iface := after.interfaceByName(e.Interface)
		if iface == nil {
			return fmt.Errorf("变更后主机上找不到网口 %s", e.Interface)
		}
		if iface.Master != e.Bridge {
			return fmt.Errorf("%s 当前挂在 %q 上，期望是 %s", e.Interface, iface.Master, e.Bridge)
		}
		return nil

	case NetChangeBridgeDetach:
		iface := after.interfaceByName(e.Interface)
		if iface == nil {
			return fmt.Errorf("变更后主机上找不到网口 %s", e.Interface)
		}
		if iface.Master != "" {
			return fmt.Errorf("%s 仍挂在 %s 上", e.Interface, iface.Master)
		}
		return nil

	case NetChangeOvsAttach:
		for _, member := range e.Members {
			switch current := after.ovsBridgeOf(member); current {
			case e.Bridge:
			case "":
				return fmt.Errorf("%s 还没有挂到 OVS 网桥 %s 上", member, e.Bridge)
			default:
				return fmt.Errorf("%s 挂到了 OVS 网桥 %s 上，期望是 %s", member, current, e.Bridge)
			}
		}
		return nil

	case NetChangeOvsDetach:
		if current := after.ovsBridgeOf(e.Interface); current != "" {
			return fmt.Errorf("%s 仍挂在 OVS 网桥 %s 上", e.Interface, current)
		}
		return nil

	case NetChangeOvsBridgeCreate:
		if after.ovsBridge(e.Bridge) == nil {
			return fmt.Errorf("变更后仍未看到 OVS 网桥 %s", e.Bridge)
		}
		for _, member := range e.Members {
			switch current := after.ovsBridgeOf(member); current {
			case e.Bridge:
			case "":
				return fmt.Errorf("%s 还没有挂到 OVS 网桥 %s 上", member, e.Bridge)
			default:
				return fmt.Errorf("%s 挂到了 OVS 网桥 %s 上，期望是 %s", member, current, e.Bridge)
			}
		}
		return nil

	case NetChangeOvsBridgeDelete:
		if after.ovsBridge(e.Bridge) != nil {
			return fmt.Errorf("OVS 网桥 %s 仍然存在", e.Bridge)
		}
		return nil
	}

	return fmt.Errorf("未知的变更类型：%s", e.Action)
}

func verifyMembers(snapshot *NetworkSnapshot, bridge string, members []string) error {
	iface := snapshot.interfaceByName(bridge)
	if iface == nil {
		return fmt.Errorf("找不到网桥 %s", bridge)
	}
	attached := make(map[string]bool, len(iface.Members))
	for _, member := range iface.Members {
		attached[member] = true
	}
	for _, member := range members {
		if !attached[member] {
			return fmt.Errorf("网口 %s 还没有挂到 %s 上", member, bridge)
		}
	}
	return nil
}

// snapshotHasAddress 判断快照里是否存在某个 CIDR 地址。
//
// 优先在指定接口上找；找不到时再全表找一次 —— 接口被桥接后地址会落在网桥上，
// 而用户关心的"地址配上了没有"与它具体落在哪个接口无关。
func snapshotHasAddress(snapshot *NetworkSnapshot, iface string, want string) bool {
	prefix, err := netip.ParsePrefix(want)
	if err != nil {
		return false
	}
	addr := prefix.Addr().String()
	bits := prefix.Bits()

	has := func(item *Interface) bool {
		for _, a := range item.Addrs {
			if a.Addr == addr && a.Prefix == bits {
				return true
			}
		}
		return false
	}

	if target := snapshot.interfaceByName(iface); target != nil && has(target) {
		return true
	}
	for i := range snapshot.Interfaces {
		if has(&snapshot.Interfaces[i]) {
			return true
		}
	}
	return false
}

func snapshotHasDynamicIPv4(snapshot *NetworkSnapshot) bool {
	for _, iface := range snapshot.Interfaces {
		for _, addr := range iface.Addrs {
			if addr.Family == "ipv4" && addr.Dynamic {
				return true
			}
		}
	}
	return false
}
