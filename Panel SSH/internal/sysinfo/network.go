package sysinfo

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 网络信息的分节标记，需与 networkCommand 保持一致。
const (
	secTools     = "###TOOLS"
	secNMRunning = "###NMRUNNING"
	secNMDev     = "###NMDEV"
	secLink      = "###LINK"
	secAddr      = "###ADDR"
	secRoute4    = "###ROUTE4"
	secRoute6    = "###ROUTE6"
	secResolv    = "###RESOLV"
	secResolved  = "###RESOLVED"
	secBridges   = "###BRIDGES"
	secWireless  = "###WIRELESS"
	secPhysical  = "###PHYSICAL"
	secSpeed     = "###SPEED"
	// secTCP / secUDP 直接读内核套接字表：`ss -s` 的汇总格式随 iproute2 版本变，
	// 而 /proc/net 的列序是内核 ABI，不会漂。连接数按"当前存在的套接字"算是
	// 这个概览指标唯一可靠的来源，不依赖任何外部工具。
	secTCP    = "###TCP"
	secUDP    = "###UDP"
	secListen = "###LISTEN"
	// secListenRoot 是以 root 重采的那一份监听表（见 ListenCommand）。两份分开存，
	// 是为了让"读到的是哪一份"在解析层显式可见，而不是靠拼接顺序去猜。
	secListenRoot = "###LISTENROOT"
	// secHosts 是 /etc/hosts 的原文。它跟着 networkCommand 一起采（普通用户可读，
	// 零额外 SSH），但用途不限于只读展示：界面在这上面做增删改，写回时提交的
	// 正是这份原文的摘要（见 HostsDigest），用来判断这中间有没有别人动过。
	secHosts      = "###HOSTS"
	secConfDirs   = "###CONFDIRS"
	secNetplan    = "###NETPLAN"
	secOvs        = "###OVS"
	secOvsUnread  = "###OVSUNREAD"
	secOvsReason  = "###OVSREASON"
	secOvsCreated = "###OVSCREATED"
	secOvsPersist = "###OVSPERSIST"
)

// networkCommand 一次采集全部网络信息。
//
// 与指标采集同样的取舍：分节输出、只走一次 SSH，节内命令统一吞掉 stderr ——
// 目标机上缺某个工具（例如没装 ethtool）不应该让整次采集失败，
// 缺什么由解析阶段判定，且只影响对应字段。
//
// 地址与链路信息刻意取 `ip -j`（JSON）而不是解析 `ip addr` 的文本：
// 后者的多行缩进格式、`inet` 与 `inet6` 混排、地址别名都会随版本变化，
// 解析很容易在某个发行版上悄悄失效，而 JSON 是 iproute2 明确承诺的稳定接口。
//
// 链路那一节还必须带 -d：不给 details 时 iproute2 不输出 linkinfo，
// 接口类型就只剩 link_type 可看，而它对手工建的网桥、Docker 的 br-*、OVS 网桥、
// veth 一律报 ether —— 于是全部接口都被当成以太网，类型标签与归属判定同时失准
// （归属判定里的"网桥成员全是 veth 就是容器网桥"那条证据链也跟着断掉）。
//
// OVS 那几节不在这里，见 ovsCommand：它们需要读 OVS 自己的数据库，而那个
// 套接字只允许 root 与 openvswitch 组访问，因此必须单独以 root 采集，
// 不能混进这条按普通用户执行的命令里。唯一的例外是 ###OVSPERSIST ——
// 那只是看一眼目标机上有没有一个文件，与 OVS 数据库无关，也不需要任何权限。
const networkCommand = `echo '###TOOLS'; for t in nmcli ip ss resolvectl ovs-vsctl; do command -v "$t" >/dev/null 2>&1 && echo "$t"; done
echo '###NMRUNNING'; nmcli -t -f RUNNING general 2>/dev/null
echo '###NMDEV'; nmcli -t -f DEVICE,TYPE,STATE,CONNECTION device status 2>/dev/null
echo '###LINK'; ip -d -j link show 2>/dev/null
echo '###ADDR'; ip -j addr show 2>/dev/null
echo '###ROUTE4'; ip -j route show 2>/dev/null
echo '###ROUTE6'; ip -j -6 route show 2>/dev/null
echo '###NETDEV'; cat /proc/net/dev 2>/dev/null
echo '###TCP'; cat /proc/net/tcp /proc/net/tcp6 2>/dev/null
echo '###UDP'; cat /proc/net/udp /proc/net/udp6 2>/dev/null
echo '###HOSTS'; sed 's/^/|/' /etc/hosts 2>/dev/null
echo '###RESOLV'; cat /etc/resolv.conf 2>/dev/null
echo '###RESOLVED'; resolvectl dns 2>/dev/null
echo '###BRIDGES'; for b in /sys/class/net/*/bridge; do [ -d "$b" ] || continue; n=${b%/bridge}; printf '%s %s\n' "${n##*/}" "$(ls "$b/brif" 2>/dev/null | tr '\n' ' ')"; done
echo '###WIRELESS'; for w in /sys/class/net/*/wireless; do [ -d "$w" ] || continue; n=${w%/wireless}; printf '%s\n' "${n##*/}"; done
echo '###PHYSICAL'; for d in /sys/class/net/*/device; do [ -e "$d" ] || continue; n=${d%/device}; printf '%s\n' "${n##*/}"; done
echo '###SPEED'; for s in /sys/class/net/*/speed; do [ -r "$s" ] || continue; n=${s%/speed}; v=$(cat "$s" 2>/dev/null) || continue; printf '%s %s\n' "${n##*/}" "$v"; done
echo '###LISTEN'; ss -H -tulnp 2>/dev/null || ss -tulnp 2>/dev/null
echo '###CONFDIRS'; for p in /etc/netplan /etc/systemd/network /etc/network/interfaces /etc/sysconfig/network-scripts; do [ -e "$p" ] && printf '%s\n' "$p"; done
echo '###NETPLAN'; ls /etc/netplan/*.yaml 2>/dev/null
echo '###OVSPERSIST'; [ -e /etc/systemd/system/multi-user.target.wants/panelssh-ovs.service ] && echo yes
true`

// ListenCommand 单独采一次监听表，由调用方以 root 执行。
//
// 为什么这一节要换个身份重采：ss 的 -p（users 段）只对"执行者有权看到的进程"
// 显示 —— 登录用户看 root 起的服务（sshd、nginx、docker-proxy）时那一栏是空的，
// 而界面上"结束这个监听进程"需要一个 pid。于是以 root 再采一次，节名用独立的
// ###LISTENROOT，解析时优先取它（见 pickListenSection）：提不了权的机器上它不存在，
// 回落到登录用户那份，界面照旧可用，只是没有 pid。
//
// 与 OVS 那几节的差别：那些要读 root 独占的套接字，属于"非 root 根本读不到"；
// 这一节非 root 读得到、只是缺 pid。所以这里不设前置判断，每次采集都走一次。
func ListenCommand() string {
	return `echo '` + secListenRoot + `'; ss -H -tulnp 2>/dev/null || ss -tulnp 2>/dev/null
true`
}

// ovsCommand 采集 Open vSwitch 的现状，由调用方以 root 执行。
//
// 它和 networkCommand 分开，是因为 OVS 的状态不在内核里而在它自己的数据库里：
// 套接字默认只允许 root 与 openvswitch 组访问，普通登录账号跑 list-br 会直接
// 失败。而 su 这条提权路径要把 stdin 让给口令（见 env.Wrap 的
// 说明），没法在脚本内部临时提一次权，只能整段脚本换个身份跑。
//
// 因此这里不做任何提权尝试：调用方怎么执行这段脚本，就是什么身份。以 root
// 跑就正常读，以普通用户跑就如实报 ###OVSUNREAD —— 那个标记必须留着，
// 它让"读不到"和"没有网桥"在界面上是两句话，而不是一句。
//
// OVSREAD 的初值是 unreadable，只有 list-br 真的跑成功才改成 ok。反过来
// 写（初值 ok、失败时才改）会漏掉一条路：提权会把 PATH 换成 root 的那一份
// （su 的登录环境），而 ovs-vsctl 未必在里面。命令找
// 不到时脚本什么都没有说，而"什么都没说"在识别层与"读到了，只是网桥列表
// 是空的"完全一样 —— 界面于是宣称这台机器还没有网桥，排查方向就此跑偏。
//
// 依旧带 --timeout：vswitchd 没在跑时 ovs-vsctl 会一直等数据库连接。
//
// list-br 的 stderr 被收进 ###OVSREASON 而不是丢掉：读不到时那句话是排查的
// 唯一线索，而"套接字权限不足"和"数据库连不上"在界面上长得一模一样。
// stdout 与 stderr 合在一起收、再按退出码分流，是为了不把这条 5 秒超时的
// 命令跑两遍 —— list-br 成功时本来也不会往 stderr 写东西。
const ovsCommand = `OVSREAD=unreadable; BR=; OVSREASON=
if command -v ovs-vsctl >/dev/null 2>&1; then
  OUT=$(ovs-vsctl --timeout=5 list-br 2>&1)
  if [ $? -eq 0 ]; then OVSREAD=ok; BR=$OUT; else OVSREASON=$OUT; fi
else
  OVSREASON='PATH 里找不到 ovs-vsctl（提权会把 PATH 换成 root 的那一份）'
fi
echo '###OVS'; for b in $BR; do printf '%s %s\n' "$b" "$(ovs-vsctl --timeout=5 list-ports "$b" 2>/dev/null | tr '\n' ' ')"; done
echo '###OVSUNREAD'; [ "$OVSREAD" = unreadable ] && echo yes
echo '###OVSREASON'; printf '%s\n' "$OVSREASON"
echo '###OVSCREATED'; for b in $BR; do v=$(ovs-vsctl --timeout=5 get bridge "$b" external-ids:panelssh-created 2>/dev/null); case "$v" in *1*) printf '%s\n' "$b" ;; esac; done
true`

// NetworkCommand 返回网络信息采集命令。
func NetworkCommand() string { return networkCommand }

// OvsCommand 返回 OVS 现状的采集命令。
//
// 调用方要以 root 执行它（走 execEscalated + escalatePreferred）。以普通
// 用户跑也能跑完，只是读不到 OVS 数据库 —— 那种情况下结果会被如实标成
// "读不到"，不会伪装成"这台机器没有网桥"。
func OvsCommand() string { return ovsCommand }

// OvsUnreadableOutput 是采集彻底失败时，用来替代真实输出的合成结果。
//
// 为什么需要它：提权被拒、口令不对、会话抖动都会让采集根本跑不完，而空输出
// 与"这台机器上确实一座网桥都没有"在解析层看起来一模一样 —— 界面于是只能拿
// 后者去描述前者。让失败也走这条合成的"读不到"，解析与界面就只需要处理
// 一种情况，不必再追问这个空是从哪来的。
//
// reason 是失败的原因（提权时 su 的那句报错），它会一路走到界面上。
// 同一句"读不到"背后的原因各不相同，修法也不相同 —— 把原因摆出来，用户就
// 不必再去目标机上手工复现一遍，而那正是这个功能本该替他省掉的事。
//
// 只给这两节：网桥列表与面板标记本来就无从得知，缺了它们正好表示"没读到"，
// 而不是"读到了空的"。
func OvsUnreadableOutput(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return secOvsUnread + "\nyes\n"
	}
	return secOvsUnread + "\nyes\n" + secOvsReason + "\n" + reason + "\n"
}

// HasOvsTool 判断一份 networkCommand 的输出里，目标机是否装了 ovs-vsctl。
//
// 调用方据此决定要不要再去采一次 OVS。那一步要换一个身份跑（见 OvsCommand），
// 而绝大多数目标机没有 OVS —— 不该为看一眼根本不存在的东西去申请一次提权。
func HasOvsTool(raw string) bool {
	for _, line := range strings.Split(splitSections(raw)[secTools], "\n") {
		if strings.TrimSpace(line) == "ovs-vsctl" {
			return true
		}
	}
	return false
}

// Address 是接口上的一个 IP 地址。
type Address struct {
	Family string `json:"family"`
	Addr   string `json:"addr"`
	Prefix int    `json:"prefix"`
	Scope  string `json:"scope"`
	// Dynamic 表示地址来自 DHCP 或 IPv6 自动配置 —— 它决定了改静态地址时
	// 会不会被租约续期覆盖回去，是用户判断"这个地址为什么变"的关键线索。
	Dynamic bool `json:"dynamic"`
	// Forever 表示地址没有租期（静态配置），false 时 Remaining 才是有效值。
	Forever   bool   `json:"forever"`
	Remaining string `json:"remaining"`
}

// 接口归属。技术类型回答"它是什么"，归属回答"它归谁"。
//
// 两者必须分开：docker0 与管理员建的 br0 在技术上是同一种东西（网桥），
// 但对用户的意义完全不同。一台跑着十几个容器的机器上，光靠类型根本
// 分不清哪张是真实网卡，哪张是容器网络的零件。
const (
	// OwnerPhysical 有内核硬件设备支撑的真实网卡。
	OwnerPhysical = "physical"
	// OwnerContainer 容器运行时创建的虚拟网络：veth 网卡对，以及成员全是
	// veth 的网桥（Docker 默认的 docker0、compose 建出来的 br-xxxxxxxxxxxx）。
	OwnerContainer = "container"
	// OwnerVirtual 管理员或系统创建的其它虚拟设备：网桥、绑定、VLAN、隧道。
	OwnerVirtual = "virtual"
	// OwnerKernel 内核自身的接口，目前只有回环。
	OwnerKernel = "kernel"
)

// Interface 是一个网络接口的当前状态。
type Interface struct {
	Name string `json:"name"`
	// Type 是接口类型：ethernet / bridge / bond / vlan / wireless / veth / loopback / ...
	Type string `json:"type"`
	// Owner 是接口归属：physical / container / virtual / kernel。
	Owner string `json:"owner"`
	MAC   string `json:"mac"`
	MTU   int    `json:"mtu"`
	// AdminUp 是管理态（ip link set up/down），Up 是运行态（对端链路真的起来了）。
	// 两个都要给：网线没插时管理态为 up 而运行态为 down，只看一个会误判。
	AdminUp   bool   `json:"adminUp"`
	Up        bool   `json:"up"`
	OperState string `json:"operState"`
	// Master 是接口所属的网桥或绑定；Members 只在接口本身是网桥/绑定时才有值。
	Master  string   `json:"master"`
	Members []string `json:"members"`
	// SpeedMbps 为 0 表示读不到（虚拟设备没有物理速率），不是"零速率"。
	SpeedMbps int       `json:"speedMbps"`
	Addrs     []Address `json:"addrs"`
	RxBytes   uint64    `json:"rxBytes"`
	TxBytes   uint64    `json:"txBytes"`
	RxRate    float64   `json:"rxRate"`
	TxRate    float64   `json:"txRate"`
	// ConnName / ConnState 来自 NetworkManager：有连接名说明该设备由 NM 托管，
	// 后续的改地址、加网桥都必须走 nmcli，直接改配置文件会被 NM 覆盖。
	ConnName  string `json:"connName"`
	ConnState string `json:"connState"`
}

// Gateway 与主地址，前端用来在列表行里直接显示"这个口通不通外网"。
type Route struct {
	Family  string `json:"family"`
	Dst     string `json:"dst"`
	Gateway string `json:"gateway"`
	Dev     string `json:"dev"`
	PrefSrc string `json:"prefsrc"`
	Proto   string `json:"protocol"`
	Metric  int    `json:"metric"`
}

// LinkDNS 是 systemd-resolved 里按链路配置的 DNS。
type LinkDNS struct {
	Link    string   `json:"link"`
	Servers []string `json:"servers"`
}

// DNSConfig 是目标机的域名解析配置。
type DNSConfig struct {
	Servers []string  `json:"servers"`
	Search  []string  `json:"search"`
	ByLink  []LinkDNS `json:"byLink"`
	// Stub 表示 /etc/resolv.conf 指向本机 stub resolver（127.0.0.53 之类），
	// 此时 Servers 只是转发地址，真实上游 DNS 要看 ByLink。
	Stub bool `json:"stub"`
}

// Listener 是目标机上处于监听状态的端口。
type Listener struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Process  string `json:"process"`
	// PID 是占用这个端口的进程号，界面据它结束进程。为 0 表示读不到：
	// 非 root 看不到别人的进程，而这一栏来自以 root 重采的那一份（见 ListenCommand），
	// 目标机提不了权时它整份都不存在，此时进程名可能仍能显示、pid 只能是 0。
	PID int `json:"pid"`
}

// SocketCounts 是目标机当前的 TCP / UDP 套接字数量。
//
// TCP 只算活动连接：LISTEN 是服务端点、TIME_WAIT / CLOSE 是已结束的残影，
// 把它们算进"连接数"会让一台刚压测完的机器长期显示成千上万条连接。
// UDP 没有连接状态，按内核里存在的套接字计。
type SocketCounts struct {
	TCP int `json:"tcp"`
	UDP int `json:"udp"`
}

// PacketLoss 是整机报文的累计丢弃量。
//
// 口径只能是"自开机以来"：/proc/net/dev 给的是累计计数，要算"最近 15 分钟"
// 得有历史快照，而面板重启后那段历史就没了 —— 与其编一个看起来很短的窗口，
// 不如把真实口径写在界面上。
type PacketLoss struct {
	// Packets 是成功收发的报文总数，Drops 是被内核丢弃的数量。
	Packets uint64 `json:"packets"`
	Drops   uint64 `json:"drops"`
	// Percent 是 Drops / (Packets + Drops)，已经排除回环。
	Percent float64 `json:"percent"`
}

// NetworkSnapshot 是一次网络信息采集的结果。
type NetworkSnapshot struct {
	Interfaces []Interface `json:"interfaces"`
	Routes     []Route     `json:"routes"`
	DNS        DNSConfig   `json:"dns"`
	Listeners  []Listener  `json:"listeners"`
	// Connections 与 PacketLoss 是顶部概览用的两个整机读数。
	Connections SocketCounts `json:"connections"`
	PacketLoss  PacketLoss   `json:"packetLoss"`
	// Manager 是目标机实际的网络配置后端：NetworkManager / netplan /
	// systemd-networkd / ifupdown / ifcfg / unknown。面板据它决定用哪套命令改配置。
	Manager string `json:"manager"`
	// Tools 是目标机上存在的相关命令，供前端在操作前给出"缺工具"的提示。
	Tools []string `json:"tools"`
	// Hosts 是 /etc/hosts 里的静态域名映射，HostsMd5 是那份原文的摘要。
	// 摘要要一路交到前端再带回来：界面上改一条记录不可能把整份文件发上来，
	// 于是用"你看到的是哪一份"来兜住并发修改（见 HostsDigest）。
	Hosts    []HostEntry `json:"hosts"`
	HostsMd5 string      `json:"hostsMd5"`
	// Ovs 是目标机的 Open vSwitch 现状。Available 为 false 表示这台机器
	// 没有 ovs-vsctl，此时 Bridges 为空，前端不展示 OVS 相关内容。
	Ovs *OvsState `json:"ovs,omitempty"`
	// DeltaReady 表示本次是否已能算出收发速率。首次采样没有基准，
	// 前端据此显示"首次采样"而不是把 0 当成真的没有流量。
	DeltaReady bool      `json:"deltaReady"`
	SampledAt  time.Time `json:"sampledAt"`
}

// ---------- 采集器 ----------

// NetworkCollector 保存每台主机上一次的接口计数，用于算收发速率。
//
// /proc/net/dev 给的是自开机以来的累计字节，和 CPU 时间一样必须两次采样求差；
// 这是该功能无法省略的状态，与指标采集器分开存放，避免两处生命周期互相牵动。
type NetworkCollector struct {
	mu   sync.Mutex
	prev map[int64]*netSample
}

// netDevStats 是 /proc/net/dev 里一行的累计计数。
//
// 字节用来算速率，报文数与丢弃数用来算整机丢包率 —— 两者同源同一行，
// 分成两个解析函数只会让"这份输出到底读没读到"变成两处各自判断。
type netDevStats struct {
	rxBytes   uint64
	txBytes   uint64
	rxPackets uint64
	txPackets uint64
	rxDrops   uint64
	txDrops   uint64
}

type netSample struct {
	at       time.Time
	counters map[string]netDevStats
}

// NewNetworkCollector 创建网络采集器。
func NewNetworkCollector() *NetworkCollector {
	return &NetworkCollector{prev: make(map[int64]*netSample)}
}

// Forget 丢弃某主机的历史采样。
func (c *NetworkCollector) Forget(hostID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.prev, hostID)
}

// Collect 解析远端输出并给出网络快照。
func (c *NetworkCollector) Collect(hostID int64, raw string, now time.Time) (*NetworkSnapshot, error) {
	sections := splitSections(raw)

	links, err := parseJSON[ipLink](sections[secLink])
	if err != nil || len(links) == 0 {
		// 这里必须明确报错而不是返回空列表：iproute2 过旧时 `ip -j` 会直接报错，
		// 静默给出"没有网卡"会让用户去查一台其实好好的机器。
		return nil, errors.New("目标系统的 iproute2 不支持 JSON 输出（ip -j），请升级 iproute2 后重试")
	}
	addrs, _ := parseJSON[ipAddr](sections[secAddr])

	routes4, _ := parseJSON[ipRoute](sections[secRoute4])
	routes6, _ := parseJSON[ipRoute](sections[secRoute6])

	tools := parseTools(sections[secTools])
	bridges := parseBridges(sections[secBridges])
	wireless := parseNameSet(sections[secWireless])
	physical := parseNameSet(sections[secPhysical])
	speeds := parseKeyInt(sections[secSpeed])
	nmDevices := parseNMDevices(sections[secNMDev])
	counters := parseNetDevCounters(sections[secNetdev])
	addrByIface := indexAddrs(addrs)

	rx, tx, deltaReady := c.delta(hostID, counters, now)

	// 技术类型要先全部算完：归属的判定需要读"这个网桥的成员都是什么类型"，
	// 而成员类型正是这一遍的结果，两遍没法合并。
	kinds := make(map[string]string, len(links))
	for _, link := range links {
		kinds[link.IfName] = linkKind(link, wireless[link.IfName])
	}

	interfaces := make([]Interface, 0, len(links))
	for _, link := range links {
		// 这两项在 map 里没有对应条目时会是 nil 切片，序列化成 JSON 的 null，
		// 而前端要直接读 .length —— 因此统一给空切片，保持"永远是数组"的契约。
		addrs := addrByIface[link.IfName]
		if addrs == nil {
			addrs = []Address{}
		}
		members := bridges[link.IfName]
		if members == nil {
			members = []string{}
		}

		iface := Interface{
			Name:      link.IfName,
			Type:      kinds[link.IfName],
			Owner:     interfaceOwner(link, kinds, bridges, physical),
			MAC:       link.Address,
			MTU:       link.MTU,
			AdminUp:   hasFlag(link.Flags, "UP"),
			Up:        hasFlag(link.Flags, "LOWER_UP") || link.OperState == "up",
			OperState: link.OperState,
			Master:    link.Master,
			Members:   members,
			SpeedMbps: speeds[link.IfName],
			Addrs:     addrs,
		}

		// 网桥成员关系有两个来源：`ip -j link` 的 master 字段（iproute2 ≥ 5.x）
		// 与 /sys/class/net/*/brif。旧版 iproute2 没有前者，用后者补齐，
		// 这样不同版本的目标机都能正确显示"这个网口挂在哪个网桥上"。
		if iface.Master == "" {
			iface.Master = findBridgeOf(bridges, link.IfName)
		}

		if counter, ok := counters[link.IfName]; ok {
			iface.RxBytes, iface.TxBytes = counter.rxBytes, counter.txBytes
			iface.RxRate, iface.TxRate = rx[link.IfName], tx[link.IfName]
		}
		if nm, ok := nmDevices[link.IfName]; ok {
			iface.ConnName, iface.ConnState = nm.name, nm.state
		}

		interfaces = append(interfaces, iface)
	}

	// 分组顺序见 interfaceRank。同组内按名字里的数字自然排序（eth2 在 eth10 之前）。
	sort.SliceStable(interfaces, func(i, j int) bool {
		ri, rj := interfaceRank(interfaces[i]), interfaceRank(interfaces[j])
		if ri != rj {
			return ri < rj
		}
		return naturalLess(interfaces[i].Name, interfaces[j].Name)
	})

	hosts, hostsMd5 := parseHosts(sections[secHosts])

	return &NetworkSnapshot{
		Interfaces: interfaces,
		Routes:     append(convertRoutes(routes4, "ipv4"), convertRoutes(routes6, "ipv6")...),
		DNS:        parseDNS(sections[secResolv], sections[secResolved]),
		Listeners:  parseListeners(pickListenSection(sections)),
		Connections: SocketCounts{
			TCP: countTCPSockets(sections[secTCP]),
			UDP: countUDPSockets(sections[secUDP]),
		},
		PacketLoss: aggregatePacketLoss(counters),
		Hosts:      hosts,
		HostsMd5:   hostsMd5,
		Manager:    detectManager(sections),
		Tools:      tools,
		Ovs:        buildOvsState(sections, physical, tools, defaultRouteDevs(routes4, routes6)),
		DeltaReady: deltaReady,
		SampledAt:  now,
	}, nil
}

// delta 用相邻两次计数算出各接口的收发速率，并保存本次计数。
// 第二个返回值表示本次是否真的算出了速率 —— 没有基准时它与全 0 无法区分，必须显式给出。
func (c *NetworkCollector) delta(hostID int64, counters map[string]netDevStats, now time.Time) (rx, tx map[string]float64, ready bool) {
	rx = make(map[string]float64, len(counters))
	tx = make(map[string]float64, len(counters))

	c.mu.Lock()
	defer c.mu.Unlock()

	c.reapStale(now)
	prev := c.prev[hostID]
	c.prev[hostID] = &netSample{at: now, counters: counters}

	if prev == nil {
		return rx, tx, false
	}
	elapsed := now.Sub(prev.at).Seconds()
	// 间隔过小时差值被噪声淹没，宁可这一轮不给速率。
	if elapsed < 0.5 {
		return rx, tx, false
	}

	for name, current := range counters {
		before, ok := prev.counters[name]
		if !ok {
			// 接口是本轮新建的（例如刚插上 USB 网卡），没有基准值可比。
			continue
		}
		// 计数器回绕或接口重建会让当前值小于上次，此时差值无意义。
		if current.rxBytes >= before.rxBytes {
			rx[name] = float64(current.rxBytes-before.rxBytes) / elapsed
		}
		if current.txBytes >= before.txBytes {
			tx[name] = float64(current.txBytes-before.txBytes) / elapsed
		}
	}
	return rx, tx, true
}

// netPrevTTL 是历史采样的最长保留时间，与指标采集器同理：
// 速率只看相邻两次采样，更老的条目只会随主机反复增删慢慢累积。
const netPrevTTL = 30 * time.Minute

func (c *NetworkCollector) reapStale(now time.Time) {
	for id, sample := range c.prev {
		if now.Sub(sample.at) > netPrevTTL {
			delete(c.prev, id)
		}
	}
}

// ---------- iproute2 JSON 结构 ----------

type ipLink struct {
	IfName    string   `json:"ifname"`
	Flags     []string `json:"flags"`
	MTU       int      `json:"mtu"`
	OperState string   `json:"operstate"`
	LinkType  string   `json:"link_type"`
	Address   string   `json:"address"`
	Master    string   `json:"master"`
	LinkInfo  *struct {
		Kind string `json:"info_kind"`
	} `json:"linkinfo"`
}

type ipAddr struct {
	IfName   string `json:"ifname"`
	AddrInfo []struct {
		Family    string   `json:"family"`
		Local     string   `json:"local"`
		PrefixLen int      `json:"prefixlen"`
		Scope     string   `json:"scope"`
		Flags     []string `json:"flags"`
		// 有效/首选租期，单位秒；4294967295（0xFFFFFFFF）是内核表示"永久"的哨兵值。
		ValidLifeTime     int64 `json:"valid_life_time"`
		PreferredLifeTime int64 `json:"preferred_life_time"`
	} `json:"addr_info"`
}

type ipRoute struct {
	Dst      string `json:"dst"`
	Gateway  string `json:"gateway"`
	Dev      string `json:"dev"`
	PrefSrc  string `json:"prefsrc"`
	Protocol string `json:"protocol"`
	Metric   int    `json:"metric"`
}

// lifetimeForever 是内核表示"无租期"的哨兵值。
const lifetimeForever = 4294967295

// ---------- 解析 ----------

func parseJSON[T any](section string) ([]T, error) {
	trimmed := strings.TrimSpace(section)
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func hasFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}

// linkKind 给出接口的技术类型。
func linkKind(link ipLink, wireless bool) string {
	if link.IfName == "lo" || hasFlag(link.Flags, "LOOPBACK") {
		return "loopback"
	}
	if link.LinkInfo != nil && link.LinkInfo.Kind != "" {
		switch link.LinkInfo.Kind {
		case "bridge", "bond", "vlan", "veth", "dummy", "macvlan", "wireguard", "tun":
			return link.LinkInfo.Kind
		case "tap":
			return "tun"
		}
		// 未知类型直接透出内核给的名字，比统一归到 other 更有诊断价值。
		return link.LinkInfo.Kind
	}
	if wireless {
		return "wireless"
	}
	if link.LinkType == "ether" {
		return "ethernet"
	}
	return "other"
}

// interfaceOwner 判断接口归谁所有。
//
// 三类证据，按可靠程度依次使用：
//
//  1. /sys/class/net/<if>/device 存在 —— 内核硬件设备，这是物理网卡的铁证，
//     与命名无关（enxc84d442992d2 这种 USB 网卡名字毫无规律）。
//  2. 设备类型 —— veth 是成对的虚拟网卡，一端留在这里、另一端在另一个
//     网络命名空间里，这种形态的用途几乎只有容器与虚拟机。
//  3. 网桥的成员构成 —— 网桥自己看不出用途，但成员能：Docker 把每个容器的
//     veth 挂上去，而管理员建物理网桥时挂的是真实网口。
func interfaceOwner(link ipLink, kinds map[string]string, bridges map[string][]string, physical map[string]bool) string {
	if link.IfName == "lo" || hasFlag(link.Flags, "LOOPBACK") {
		return OwnerKernel
	}
	if physical[link.IfName] {
		return OwnerPhysical
	}
	if kinds[link.IfName] == "veth" {
		return OwnerContainer
	}
	if kinds[link.IfName] == "bridge" {
		members := bridges[link.IfName]
		if len(members) > 0 && allKindIs(members, kinds, "veth") {
			return OwnerContainer
		}
	}
	return OwnerVirtual
}

func allKindIs(names []string, kinds map[string]string, want string) bool {
	for _, name := range names {
		if kinds[name] != want {
			return false
		}
	}
	return true
}

// interfaceRank 决定排序分组：物理网口最前，管理员建的虚拟设备次之，
// 容器网络再次，回环最后。
//
// 一台跑着十几个容器的机器上，容器网桥加 veth 能有几十个；把它们排在物理
// 网口前面，会让"这台机器到底有几张真网卡"这种最基本的问题都难以回答。
func interfaceRank(iface Interface) int {
	switch iface.Owner {
	case OwnerPhysical:
		return 0
	case OwnerVirtual:
		return 1
	case OwnerContainer:
		return 2
	default:
		return 3
	}
}

// naturalLess 按名字里的数字段比较，让 eth2 排在 eth10 前面。
// 纯字典序会让 eth10 插在 eth1 与 eth2 之间，网口一多就很难找。
func naturalLess(a, b string) bool {
	ia, ib := 0, 0
	for ia < len(a) && ib < len(b) {
		ca, cb := a[ia], b[ib]
		if isDigit(ca) && isDigit(cb) {
			na, wa := readNumber(a[ia:])
			nb, wb := readNumber(b[ib:])
			if na != nb {
				return na < nb
			}
			ia += wa
			ib += wb
			continue
		}
		if ca != cb {
			return ca < cb
		}
		ia++
		ib++
	}
	return len(a) < len(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func readNumber(s string) (value, width int) {
	for width < len(s) && isDigit(s[width]) {
		value = value*10 + int(s[width]-'0')
		width++
	}
	return value, width
}

// indexAddrs 把 ip -j addr 的输出按接口名归拢，并丢弃内核自带的链路本地地址。
//
// fe80:: 开头的地址每块网卡都有，列出来只会把真正配置的地址挤下去；
// 回环接口的 127.0.0.1 与 ::1 同理（回环本身在列表里一眼可辨）。
func indexAddrs(addrs []ipAddr) map[string][]Address {
	indexed := make(map[string][]Address, len(addrs))
	for _, entry := range addrs {
		for _, info := range entry.AddrInfo {
			if info.Scope == "link" && info.Family == "inet6" {
				continue
			}
			family := "ipv4"
			if info.Family == "inet6" {
				family = "ipv6"
			}
			if entry.IfName == "lo" {
				continue
			}

			addr := Address{
				Family: family,
				Addr:   strings.TrimSuffix(info.Local, "/"+strconv.Itoa(info.PrefixLen)),
				Prefix: info.PrefixLen,
				Scope:  info.Scope,
				Forever: info.ValidLifeTime == lifetimeForever ||
					info.ValidLifeTime == 0,
				Dynamic: hasFlag(info.Flags, "dynamic") ||
					(info.ValidLifeTime != lifetimeForever && info.ValidLifeTime > 0),
			}
			if !addr.Forever {
				addr.Remaining = formatLifetime(info.ValidLifeTime)
			}
			indexed[entry.IfName] = append(indexed[entry.IfName], addr)
		}
	}
	return indexed
}

// formatLifetime 把秒数租期转成便于阅读的剩余时间。
func formatLifetime(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds >= 86400:
		return strconv.FormatInt(seconds/86400, 10) + " 天"
	case seconds >= 3600:
		return strconv.FormatInt(seconds/3600, 10) + " 小时"
	case seconds >= 60:
		return strconv.FormatInt(seconds/60, 10) + " 分钟"
	default:
		return strconv.FormatInt(seconds, 10) + " 秒"
	}
}

func convertRoutes(routes []ipRoute, family string) []Route {
	out := make([]Route, 0, len(routes))
	for _, route := range routes {
		dst := route.Dst
		if dst == "" {
			dst = "default"
		}
		out = append(out, Route{
			Family:  family,
			Dst:     dst,
			Gateway: route.Gateway,
			Dev:     route.Dev,
			PrefSrc: route.PrefSrc,
			Proto:   route.Protocol,
			Metric:  route.Metric,
		})
	}
	return out
}

// findBridgeOf 从网桥成员表反查某接口所属的网桥。
func findBridgeOf(bridges map[string][]string, iface string) string {
	for bridge, members := range bridges {
		for _, member := range members {
			if member == iface {
				return bridge
			}
		}
	}
	return ""
}

// parseBridges 解析形如「br0 eth0 eth1」的行，给出网桥及其成员。
func parseBridges(section string) map[string][]string {
	bridges := make(map[string][]string)
	for _, line := range strings.Split(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		members := make([]string, 0, len(fields)-1)
		members = append(members, fields[1:]...)
		bridges[fields[0]] = members
	}
	return bridges
}

func parseNameSet(section string) map[string]bool {
	set := make(map[string]bool)
	for _, line := range strings.Split(section, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			set[name] = true
		}
	}
	return set
}

// parseKeyInt 解析「名字 数值」两列，非数字的值直接跳过 ——
// /sys/class/net/*/speed 对虚拟设备会返回 "Invalid argument" 而不是报错。
func parseKeyInt(section string) map[string]int {
	out := make(map[string]int)
	for _, line := range strings.Split(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.Atoi(fields[1])
		if err != nil || value <= 0 {
			continue
		}
		out[fields[0]] = value
	}
	return out
}

func parseTools(section string) []string {
	tools := make([]string, 0, 4)
	for _, line := range strings.Split(section, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			tools = append(tools, name)
		}
	}
	sort.Strings(tools)
	return tools
}

type nmDevice struct {
	name  string
	state string
}

// parseNMDevices 解析 nmcli 的设备状态，形如「eth0:ethernet:connected:Wired connection 1」。
//
// 用 SplitN 而不是 Split：连接名里可能含冒号（用户自己起的名字），
// 只有连接名会含分隔符，切分前四段即可。
func parseNMDevices(section string) map[string]nmDevice {
	devices := make(map[string]nmDevice)
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fields := strings.SplitN(trimmed, ":", 4)
		if len(fields) < 4 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		conn := strings.TrimSpace(fields[3])
		// 未托管的设备在 nmcli 里显示为 "--"，那是占位符而不是连接名。
		if conn == "--" {
			conn = ""
		}
		devices[name] = nmDevice{name: conn, state: strings.TrimSpace(fields[2])}
	}
	return devices
}

// parseNetDevCounters 按接口提取累计计数。
//
// 列序来自 /proc/net/dev 的固定字段：0–7 是接收（bytes packets errs drop ...），
// 8–15 是发送（bytes packets errs drop ...）。
func parseNetDevCounters(section string) map[string]netDevStats {
	counters := make(map[string]netDevStats)
	for _, line := range strings.Split(section, "\n") {
		name, stats, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		iface := strings.TrimSpace(name)
		if iface == "" {
			continue
		}
		fields := strings.Fields(stats)
		if len(fields) < 12 {
			continue
		}
		values := make([]uint64, 12)
		valid := true
		for i := 0; i < 12; i++ {
			value, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				valid = false
				break
			}
			values[i] = value
		}
		if !valid {
			continue
		}
		// 包含 lo：回环流量在总量统计里要被排除，但按接口展示时用户看的是自己那张网卡，
		// 这里如实给出每个接口的数字，过滤留给展示层。
		counters[iface] = netDevStats{
			rxBytes:   values[0],
			rxPackets: values[1],
			rxDrops:   values[3],
			txBytes:   values[8],
			txPackets: values[9],
			txDrops:   values[11],
		}
	}
	return counters
}

// aggregatePacketLoss 汇总整机丢包率，排除回环。
//
// 回环上的丢弃与真实链路无关，算进去只会稀释读数。这里用累计口径：
// /proc/net/dev 只有累计计数，想要"最近 N 分钟"必须有历史窗口，
// 而面板重启后那段历史不存在，所以界面上如实标注"自开机累计"。
func aggregatePacketLoss(counters map[string]netDevStats) PacketLoss {
	var loss PacketLoss
	for name, stats := range counters {
		if name == "lo" {
			continue
		}
		loss.Packets += stats.rxPackets + stats.txPackets
		loss.Drops += stats.rxDrops + stats.txDrops
	}
	if total := loss.Packets + loss.Drops; total > 0 {
		loss.Percent = float64(loss.Drops) * 100 / float64(total)
	}
	return loss
}

// countTCPSockets 统计 /proc/net/tcp{,6} 里的活动连接。
//
// 状态 0A（LISTEN）是服务端点，06（TIME_WAIT）与 07（CLOSE）是已结束的残影：
// 把它们算进"连接数"，一台刚压测完的机器会长期显示几万条，概览指标就失去了意义。
func countTCPSockets(section string) int {
	count := 0
	for _, line := range strings.Split(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !socketRowIndex(fields[0]) {
			continue
		}
		switch strings.ToUpper(fields[3]) {
		case "0A", "06", "07":
			continue
		}
		count++
	}
	return count
}

// countUDPSockets 统计 /proc/net/udp{,6} 里的套接字。
//
// UDP 是无连接协议，内核表里的每一行就是一个绑定中的套接字，
// 没有"已建立 / 未建立"可区分 —— 这里全部计入，与 ss -u 的口径一致。
func countUDPSockets(section string) int {
	count := 0
	for _, line := range strings.Split(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !socketRowIndex(fields[0]) {
			continue
		}
		count++
	}
	return count
}

// socketRowIndex 判断一行的首列是不是 /proc/net 套接字表的序号。
//
// 序号写成 "0:"（带冒号），表头是 "sl" —— 直接按十进制判断会把所有数据行
// 一起丢掉，最后得到的永远是 0 条连接。
func socketRowIndex(field string) bool {
	return isDecimal(strings.TrimSuffix(field, ":"))
}

func isDecimal(text string) bool {
	if text == "" {
		return false
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// stubResolvers 是本机 stub resolver 的地址前缀。
// 它们出现在 /etc/resolv.conf 里意味着真实上游 DNS 由 systemd-resolved 保管。
var stubResolvers = []string{"127.0.0.53", "127.0.0.54", "127.0.0.1", "::1"}

func parseDNS(resolv, resolved string) DNSConfig {
	// 同接口的 addrs：这两个字段前端直接读 .length，必须给空切片而不是 nil。
	config := DNSConfig{
		Servers: make([]string, 0, 2),
		Search:  make([]string, 0, 1),
		ByLink:  make([]LinkDNS, 0, 2),
	}

	for _, line := range strings.Split(resolv, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "nameserver":
			server := strings.TrimSpace(value)
			config.Servers = append(config.Servers, server)
			for _, stub := range stubResolvers {
				if server == stub {
					config.Stub = true
				}
			}
		case "search", "domain":
			config.Search = append(config.Search, strings.Fields(value)...)
		}
	}

	// resolvectl dns 的输出形如「Global: 8.8.8.8 1.1.1.1」或「Link 2 (eth0): 192.168.1.1」。
	for _, line := range strings.Split(resolved, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		link, servers, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		servers = strings.TrimSpace(servers)
		if servers == "" {
			continue
		}
		config.ByLink = append(config.ByLink, LinkDNS{
			Link:    strings.TrimSpace(link),
			Servers: strings.Fields(servers),
		})
	}
	return config
}

// maxListeners 限制返回条数。一台跑满容器的机器可能有上千个监听端口，
// 面板要回答的是"这台机器对外开了什么"，而不是给出完整套接字表。
const maxListeners = 500

// pickListenSection 取解析用的那份监听表：以 root 重采的那份优先，没有就回落。
//
// 判据是"整段是否为空"而不是"有没有含 pid"：提不了权的机器上 root 那份整份都不存在，
// 而登录用户那份哪怕一个 pid 都没有，它依然是这台机器完整的监听列表，照旧要显示，
// 只是结束不了进程。若按 pid 判，那台机器就会变成"一个端口都读不到"。
func pickListenSection(sections map[string]string) string {
	if root := strings.TrimSpace(sections[secListenRoot]); root != "" {
		return root
	}
	return sections[secListen]
}

func parseListeners(section string) []Listener {
	listeners := make([]Listener, 0, 64)
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		// 兼容不支持 -H 的旧版 ss：它的第一行是表头。
		if trimmed == "" || strings.HasPrefix(trimmed, "Netid") || strings.HasPrefix(trimmed, "State") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 5 {
			continue
		}

		protocol := fields[0]
		if protocol != "tcp" && protocol != "udp" {
			continue
		}

		address, port := splitSocketAddr(fields[4])
		if port == 0 {
			continue
		}

		listener := Listener{Protocol: protocol, Address: address, Port: port}
		if len(fields) > 5 {
			listener.Process, listener.PID = parseSSUsers(strings.Join(fields[5:], " "))
		}
		listeners = append(listeners, listener)
	}

	if len(listeners) > maxListeners {
		listeners = listeners[:maxListeners]
	}
	// 按端口排序，让同一项服务的新旧进程、以及 v4/v6 双栈监听挨在一起。
	sort.SliceStable(listeners, func(i, j int) bool {
		if listeners[i].Port != listeners[j].Port {
			return listeners[i].Port < listeners[j].Port
		}
		return listeners[i].Protocol < listeners[j].Protocol
	})
	return listeners
}

// splitSocketAddr 拆出地址与端口。地址可能形如 `127.0.0.53%lo:53`、
// `[::]:22`、`*:8080`、`[fe80::1]%eth0:546`，端口始终在最后一个冒号之后。
func splitSocketAddr(raw string) (string, int) {
	idx := strings.LastIndex(raw, ":")
	if idx < 0 {
		return raw, 0
	}
	port, err := strconv.Atoi(raw[idx+1:])
	if err != nil {
		return raw, 0
	}

	address := raw[:idx]
	// 必须先剥掉 %eth0 这类作用域后缀，再去方括号。ss 对带作用域的 IPv6 写的是
	// `[fe80::1]%eth0`，顺序反过来时 Trim 看到的首字符是 "["、尾字符却是 "0"，
	// 方括号会留在结果里，变成 `fe80::1]` 这种既不像地址也不像别的东西的字符串。
	if percent := strings.Index(address, "%"); percent >= 0 {
		address = address[:percent]
	}
	address = strings.Trim(address, "[]")

	if address == "*" || address == "" {
		address = "0.0.0.0"
	}
	return address, port
}

// parseSSUsers 从 `users:(("sshd",pid=812,fd=3))` 里取出进程名与进程号。
// ss 不带 -p、或执行者无权看到该进程时整段为空，名字与 pid 都取不到，这是正常结果。
//
// 只取第一组：同一行可能挂着多个进程（SO_REUSEPORT 共享同一端口），
// 名字与 pid 必须来自同一组，否则界面会显示成"nginx 跑在另一个 pid 上"，
// 而那个 pid 结束掉的其实是别的进程。
func parseSSUsers(text string) (string, int) {
	start := strings.Index(text, "((")
	if start < 0 {
		return "", 0
	}
	rest := text[start+2:]
	open := strings.Index(rest, "\"")
	if open < 0 {
		return "", 0
	}
	rest = rest[open+1:]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return "", 0
	}
	name := rest[:end]

	// pid=N 紧跟在本组的进程名之后，因此把查找范围截到本组的 "))" 为止，
	// 免得读到下一个进程的 pid。
	group := rest[end:]
	if stop := strings.Index(group, "))"); stop >= 0 {
		group = group[:stop]
	}
	pid := 0
	if idx := strings.Index(group, "pid="); idx >= 0 {
		digits := group[idx+len("pid="):]
		width := 0
		for width < len(digits) && digits[width] >= '0' && digits[width] <= '9' {
			width++
		}
		if width > 0 {
			if value, err := strconv.Atoi(digits[:width]); err == nil {
				pid = value
			}
		}
	}
	return name, pid
}

// detectManager 判定目标机实际使用的网络配置后端。
//
// 顺序有讲究：NetworkManager 在跑时它才是真正的权威 —— 装了 netplan 的
// Ubuntu 桌面、装了 ifupdown 的 Debian 服务器都可能同时存在别的配置文件，
// 但那些文件在 NM 接管后不再生效，照着它们改配置会被 NM 覆盖回去。
func detectManager(sections map[string]string) string {
	// nmcli 的输出只有 running / not running 两种，必须整串比较：
	// 用 Contains 判断的话 "not running" 里也含 "running"，会把没接管的机器
	// 也判成 NetworkManager，后续所有改配置的命令都会打错地方。
	running := strings.ToLower(strings.TrimSpace(sections[secNMRunning]))
	hasNmcli := strings.Contains(sections[secTools], "nmcli")
	if hasNmcli && running == "running" {
		return "NetworkManager"
	}

	confDirs := parseNameSet(sections[secConfDirs])
	netplanFiles := strings.TrimSpace(sections[secNetplan])
	if netplanFiles != "" && confDirs["/etc/netplan"] {
		return "netplan"
	}
	if confDirs["/etc/systemd/network"] {
		return "systemd-networkd"
	}
	if confDirs["/etc/sysconfig/network-scripts"] {
		return "ifcfg"
	}
	if confDirs["/etc/network/interfaces"] {
		return "ifupdown"
	}
	return "unknown"
}
