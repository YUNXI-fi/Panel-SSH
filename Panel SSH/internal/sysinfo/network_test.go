package sysinfo

import (
	"math"
	"strings"
	"testing"
	"time"
)

// sampleNetworkOutput 是仿照一台运行着 Docker 的 Debian 主机的真实输出。
//
// 场景刻意做全：eth0 通过 DHCP 拿到地址并桥接到管理员建的 br0，eth1 未插网线，
// eth2 是 br0 的第二个成员（故意不带 master 字段，用来覆盖"用 brif 反查所属
// 网桥"的分支），docker0 是 Docker 的默认网桥、成员全是 veth。
// 关键点是 br0 与 docker0 技术类型相同（都是网桥），但归属必须被区分开。
const sampleNetworkOutput = `###TOOLS
ip
nmcli
ss
###NMRUNNING
running
###NMDEV
eth0:ethernet:connected:Wired connection 1
eth1:ethernet:disconnected:--
eth2:ethernet:connected:Wired connection 2
br0:bridge:connected:br0
docker0:bridge:connected:docker0
###LINK
[{"ifindex":1,"ifname":"lo","flags":["LOOPBACK","UP","LOWER_UP"],"mtu":65536,"operstate":"UNKNOWN","link_type":"loopback","address":"00:00:00:00:00:00"},
{"ifindex":2,"ifname":"eth0","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"mtu":1500,"operstate":"UP","link_type":"ether","address":"aa:bb:cc:dd:ee:01","master":"br0"},
{"ifindex":3,"ifname":"eth1","flags":["BROADCAST","MULTICAST"],"mtu":9000,"operstate":"DOWN","link_type":"ether","address":"aa:bb:cc:dd:ee:02"},
{"ifindex":4,"ifname":"eth2","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"mtu":1500,"operstate":"UP","link_type":"ether","address":"aa:bb:cc:dd:ee:03"},
{"ifindex":5,"ifname":"br0","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"mtu":1500,"operstate":"UP","link_type":"ether","address":"aa:bb:cc:dd:ee:01","linkinfo":{"info_kind":"bridge"}},
{"ifindex":6,"ifname":"docker0","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"mtu":1500,"operstate":"UP","link_type":"ether","address":"02:42:aa:bb:cc:dd","linkinfo":{"info_kind":"bridge"}},
{"ifindex":7,"ifname":"veth1a2b3c","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"mtu":1500,"operstate":"UP","link_type":"ether","address":"6e:11:22:33:44:55","master":"docker0","linkinfo":{"info_kind":"veth"}},
{"ifindex":8,"ifname":"veth4d5e6f","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"],"mtu":1500,"operstate":"UP","link_type":"ether","address":"7a:66:77:88:99:aa","master":"docker0","linkinfo":{"info_kind":"veth"}}]
###ADDR
[{"ifindex":1,"ifname":"lo","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8,"scope":"host","valid_life_time":4294967295}]},
{"ifindex":2,"ifname":"eth0","addr_info":[]},
{"ifindex":3,"ifname":"eth1","addr_info":[]},
{"ifindex":5,"ifname":"br0","addr_info":[{"family":"inet","local":"192.168.1.10","prefixlen":24,"scope":"global","flags":["dynamic"],"valid_life_time":86390,"preferred_life_time":86390},{"family":"inet6","local":"2408:8207:1234::5","prefixlen":64,"scope":"global","valid_life_time":4294967295},{"family":"inet6","local":"fe80::aabb:ccff:fedd:ee01","prefixlen":64,"scope":"link","valid_life_time":4294967295}]},
{"ifindex":6,"ifname":"docker0","addr_info":[{"family":"inet","local":"172.17.0.1","prefixlen":16,"scope":"global","valid_life_time":4294967295}]}]
###ROUTE4
[{"dst":"default","gateway":"192.168.1.1","dev":"br0","protocol":"dhcp","prefsrc":"192.168.1.10","metric":100},
{"dst":"172.17.0.0/16","dev":"docker0","protocol":"kernel","scope":"link","prefsrc":"172.17.0.1","metric":0}]
###ROUTE6
[{"dst":"default","gateway":"fe80::1","dev":"br0","protocol":"ra","metric":1024}]
###NETDEV
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:   12000     120    0    0    0     0          0         0    12000     120    0    0    0     0       0          0
  eth0: 1000000    1000    0    4    0     0          0         0   500000     800    0    0    0     0       0          0
  eth1:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
  eth2:  200000     200    0    0    0     0          0         0   100000     100    0    0    0     0       0          0
   br0: 1200000    1200    0    0    0     0          0         0   600000     900    0    0    0     0       0          0
docker0:   30000      30    0    0    0     0          0         0    20000      20    0    0    0     0       0          0
###TCP
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 20001 1
   1: 0100007F:D431 0100007F:0016 01 00000000:00000000 00:00000000 00000000  1000        0 20002 1
   2: 0100007F:D432 0100007F:0016 06 00000000:00000000 00:00000000 00000000  1000        0 20003 1
   3: 0100007F:D433 0100007F:0016 01 00000000:00000000 00:00000000 00000000  1000        0 20004 1
   4: 00000000000000000000000001000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 20005 1
   5: 00000000000000000000000001000000:D434 00000000000000000000000001000000:0016 01 00000000:00000000 00:00000000 00000000  1000        0 20006 1
###UDP
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0044 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 20101 1
   1: 0100007F:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000   102        0 20102 1
###RESOLV
# Generated by NetworkManager
nameserver 127.0.0.53
search lan
###RESOLVED
Global: 8.8.8.8 1.1.1.1
Link 2 (eth0): 192.168.1.1
###BRIDGES
br0 eth0 eth2 
docker0 veth1a2b3c veth4d5e6f 
###WIRELESS
###PHYSICAL
eth0
eth1
eth2
###SPEED
eth0 1000
eth1 Invalid argument
eth2 2500
###LISTEN
tcp   LISTEN 0      4096   127.0.0.53%lo:53         0.0.0.0:*    users:(("systemd-resolve",pid=605,fd=13))
tcp   LISTEN 0      128    0.0.0.0:22               0.0.0.0:*    users:(("sshd",pid=812,fd=3))
tcp   LISTEN 0      128    [::]:22                  [::]:*       users:(("sshd",pid=812,fd=4))
udp   UNCONN 0      0      0.0.0.0:68               0.0.0.0:*    users:(("dhclient",pid=736,fd=6))
###CONFDIRS
/etc/network/interfaces
###NETPLAN
true`

func collectOnce(t *testing.T, collector *NetworkCollector, raw string, now time.Time) *NetworkSnapshot {
	t.Helper()
	snapshot, err := collector.Collect(1, raw, now)
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	return snapshot
}

func findInterface(t *testing.T, snapshot *NetworkSnapshot, name string) Interface {
	t.Helper()
	for _, iface := range snapshot.Interfaces {
		if iface.Name == name {
			return iface
		}
	}
	t.Fatalf("采集结果里没有接口 %s", name)
	return Interface{}
}

func TestNetworkCollectSnapshot(t *testing.T) {
	collector := NewNetworkCollector()
	snapshot := collectOnce(t, collector, sampleNetworkOutput, time.Now())

	// 排序：物理网口 → 管理员建的虚拟设备 → 容器网络 → 回环。
	// 容器网络沉到后面是关键：一台跑满容器的机器上它能有几十项。
	order := make([]string, 0, len(snapshot.Interfaces))
	for _, iface := range snapshot.Interfaces {
		order = append(order, iface.Name)
	}
	want := []string{"eth0", "eth1", "eth2", "br0", "docker0", "veth1a2b3c", "veth4d5e6f", "lo"}
	if len(order) != len(want) {
		t.Fatalf("接口数量为 %v，期望 %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("接口顺序为 %v，期望 %v", order, want)
		}
	}

	// 归属：br0 与 docker0 技术类型相同（都是网桥），归属必须被区分开 ——
	// 这正是"docker 和物理网卡分不开"的症结所在。
	owners := map[string]string{
		"eth0":       OwnerPhysical,
		"eth1":       OwnerPhysical,
		"eth2":       OwnerPhysical,
		"br0":        OwnerVirtual,
		"docker0":    OwnerContainer,
		"veth1a2b3c": OwnerContainer,
		"veth4d5e6f": OwnerContainer,
		"lo":         OwnerKernel,
	}
	for name, wantOwner := range owners {
		if got := findInterface(t, snapshot, name).Owner; got != wantOwner {
			t.Fatalf("%s 的归属为 %q，期望 %q", name, got, wantOwner)
		}
	}
	if findInterface(t, snapshot, "br0").Type != findInterface(t, snapshot, "docker0").Type {
		t.Fatal("样例里的 br0 与 docker0 应当是同一种技术类型，否则这组断言失去意义")
	}

	eth0 := findInterface(t, snapshot, "eth0")
	if eth0.Type != "ethernet" || eth0.Master != "br0" {
		t.Fatalf("eth0 类型/所属网桥错误：%+v", eth0)
	}
	if !eth0.AdminUp || !eth0.Up || eth0.SpeedMbps != 1000 {
		t.Fatalf("eth0 状态或速率错误：%+v", eth0)
	}
	if eth0.ConnName != "Wired connection 1" || eth0.ConnState != "connected" {
		t.Fatalf("eth0 的 NetworkManager 信息错误：%+v", eth0)
	}

	// eth1 管理态未 up、运行态 down，且 speed 读出来是 "Invalid argument"。
	eth1 := findInterface(t, snapshot, "eth1")
	if eth1.AdminUp || eth1.Up || eth1.SpeedMbps != 0 || eth1.MTU != 9000 {
		t.Fatalf("eth1 状态解析错误：%+v", eth1)
	}
	if eth1.ConnName != "" {
		t.Fatalf("未托管的设备不应有连接名，实际为 %q", eth1.ConnName)
	}

	// eth2 不在 ip link 的 master 字段里，只能靠 /sys/class/net/br0/brif 反查。
	eth2 := findInterface(t, snapshot, "eth2")
	if eth2.Master != "br0" {
		t.Fatalf("eth2 的所属网桥未能通过 brif 反查出来：%+v", eth2)
	}

	br0 := findInterface(t, snapshot, "br0")
	if br0.Type != "bridge" {
		t.Fatalf("br0 类型错误：%s", br0.Type)
	}
	if len(br0.Members) != 2 || br0.Members[0] != "eth0" || br0.Members[1] != "eth2" {
		t.Fatalf("br0 成员错误：%v", br0.Members)
	}

	// 地址：链路本地 IPv6 与回环地址都应当被过滤掉。
	if len(br0.Addrs) != 2 {
		t.Fatalf("br0 地址数量错误：%+v", br0.Addrs)
	}
	v4 := br0.Addrs[0]
	if v4.Family != "ipv4" || v4.Addr != "192.168.1.10" || v4.Prefix != 24 {
		t.Fatalf("br0 IPv4 地址错误：%+v", v4)
	}
	if !v4.Dynamic || v4.Forever || v4.Remaining != "23 小时" {
		t.Fatalf("DHCP 租期判定错误：%+v", v4)
	}
	v6 := br0.Addrs[1]
	if v6.Family != "ipv6" || !v6.Forever || v6.Dynamic {
		t.Fatalf("静态 IPv6 地址判定错误：%+v", v6)
	}

	// 路由：默认路由要给出网关，v6 默认路由单独成组。
	if len(snapshot.Routes) != 3 {
		t.Fatalf("路由数量错误：%+v", snapshot.Routes)
	}
	if snapshot.Routes[0].Dst != "default" || snapshot.Routes[0].Gateway != "192.168.1.1" {
		t.Fatalf("默认路由错误：%+v", snapshot.Routes[0])
	}

	// DNS：resolv.conf 指向 stub，真实上游在 resolvectl 的按链路输出里。
	if !snapshot.DNS.Stub {
		t.Fatal("resolv.conf 指向 127.0.0.53，应判定为 stub")
	}
	if len(snapshot.DNS.ByLink) != 2 || snapshot.DNS.ByLink[1].Link != "Link 2 (eth0)" {
		t.Fatalf("resolvectl 输出解析错误：%+v", snapshot.DNS.ByLink)
	}
	if len(snapshot.DNS.Search) != 1 || snapshot.DNS.Search[0] != "lan" {
		t.Fatalf("search 域解析错误：%v", snapshot.DNS.Search)
	}

	// 监听端口：/proc 表头两行必须被跳过；按端口排序。
	if len(snapshot.Listeners) != 4 {
		t.Fatalf("监听端口数量错误：%+v", snapshot.Listeners)
	}
	if snapshot.Listeners[0].Port != 22 || snapshot.Listeners[0].Process != "sshd" {
		t.Fatalf("监听端口解析错误：%+v", snapshot.Listeners[0])
	}
	// pid 是界面上「结束」按钮的唯一目标，取不到它那个按钮就只能置灰。
	if snapshot.Listeners[0].PID != 812 {
		t.Fatalf("监听端口的 pid 解析错误：%+v", snapshot.Listeners[0])
	}
	if snapshot.Listeners[2].Address != "127.0.0.53" || snapshot.Listeners[2].Port != 53 {
		t.Fatalf("带作用域后缀的地址解析错误：%+v", snapshot.Listeners[2])
	}

	// 连接数：LISTEN 与 TIME_WAIT 不算活动连接；tcp6 的长地址行同样要能解析。
	// 样例里是 3 条活动连接（2 条 v4 + 1 条 v6），UDP 两条绑定。
	if snapshot.Connections.TCP != 3 {
		t.Fatalf("TCP 连接数错误：%+v", snapshot.Connections)
	}
	if snapshot.Connections.UDP != 2 {
		t.Fatalf("UDP 套接字数量错误：%+v", snapshot.Connections)
	}

	// 丢包率排除回环：非回环报文 4250，其中 eth0 接收丢弃 4。
	// 用近似比较：这是一个派生比值，不该被浮点尾数卡住。
	if snapshot.PacketLoss.Drops != 4 || snapshot.PacketLoss.Packets != 4250 {
		t.Fatalf("累加口径错误：%+v", snapshot.PacketLoss)
	}
	if math.Abs(snapshot.PacketLoss.Percent-4.0/4254.0*100) > 1e-9 {
		t.Fatalf("丢包率计算错误：%+v", snapshot.PacketLoss)
	}

	// eth0 本身没有地址（地址配在网桥 br0 上），但流量计数按接口如实给出。
	if len(eth0.Addrs) != 0 {
		t.Fatalf("eth0 未配置地址，实际解析出 %+v", eth0.Addrs)
	}
	if eth0.RxBytes != 1000000 || eth0.TxBytes != 500000 {
		t.Fatalf("eth0 流量计数错误：%+v", eth0)
	}

	if snapshot.Manager != "NetworkManager" {
		t.Fatalf("网络后端判定错误：%s", snapshot.Manager)
	}

	// 这些列表前端会直接读 .length，nil 切片会序列化成 JSON 的 null 并让页面抛错。
	for _, iface := range snapshot.Interfaces {
		if iface.Addrs == nil || iface.Members == nil {
			t.Fatalf("接口 %s 的 addrs/members 不应为 nil", iface.Name)
		}
	}
	if snapshot.DNS.Servers == nil || snapshot.DNS.Search == nil || snapshot.DNS.ByLink == nil {
		t.Fatal("DNS 的各列表不应为 nil")
	}
	if snapshot.Listeners == nil || snapshot.Routes == nil || snapshot.Tools == nil {
		t.Fatal("监听端口 / 路由 / 工具列表不应为 nil")
	}
}

func TestNetworkCollectRates(t *testing.T) {
	collector := NewNetworkCollector()
	start := time.Now()

	first := collectOnce(t, collector, sampleNetworkOutput, start)
	// 首轮没有基准，速率必须是 0，而不是把累计量误当成速率。
	if iface := findInterface(t, first, "eth0"); iface.RxRate != 0 || iface.TxRate != 0 {
		t.Fatalf("首轮采样不应给出速率：%+v", iface)
	}
	if first.DeltaReady {
		t.Fatal("首轮采样没有基准，deltaReady 应为 false")
	}

	// 2 秒后 eth0 多收 2000 字节、多发 1000 字节 => 1000 B/s 与 500 B/s。
	grown := strings.Replace(sampleNetworkOutput, "  eth0: 1000000    1000", "  eth0: 1002000    1000", 1)
	grown = strings.Replace(grown, "   500000     800", "   501000     801", 1)
	second := collectOnce(t, collector, grown, start.Add(2*time.Second))
	if !second.DeltaReady {
		t.Fatal("第二轮采样已有基准，deltaReady 应为 true")
	}
	iface := findInterface(t, second, "eth0")
	if iface.RxRate != 1000 || iface.TxRate != 500 {
		t.Fatalf("速率计算错误：rx=%v tx=%v", iface.RxRate, iface.TxRate)
	}
}

func TestNetworkCollectRejectsMissingIPJSON(t *testing.T) {
	// 旧版 iproute2 不支持 `ip -j`，此时输出为空。必须明确报错，
	// 而不是返回一台"没有网卡"的机器。
	raw := "###TOOLS\nip\n###LINK\n\n###ADDR\n\n"
	if _, err := NewNetworkCollector().Collect(1, raw, time.Now()); err == nil {
		t.Fatal("ip -j 不可用时应返回错误")
	}
}

func TestDetectManager(t *testing.T) {
	cases := []struct {
		name     string
		sections map[string]string
		want     string
	}{
		{
			name: "NetworkManager 运行时优先",
			sections: map[string]string{
				secTools:     "ip\nnmcli\nss",
				secNMRunning: "running",
				secConfDirs:  "/etc/netplan",
				secNetplan:   "/etc/netplan/01-netcfg.yaml",
			},
			want: "NetworkManager",
		},
		{
			name: "装了 nmcli 但服务没跑，回落到 netplan",
			sections: map[string]string{
				secTools:     "ip\nnmcli\nss",
				secNMRunning: "not running",
				secConfDirs:  "/etc/netplan\n/etc/network/interfaces",
				secNetplan:   "/etc/netplan/01-netcfg.yaml",
			},
			want: "netplan",
		},
		{
			name: "只有 /etc/network/interfaces",
			sections: map[string]string{
				secTools:    "ip\nss",
				secConfDirs: "/etc/network/interfaces",
			},
			want: "ifupdown",
		},
		{
			name: "RHEL 系 ifcfg",
			sections: map[string]string{
				secTools:    "ip\nss",
				secConfDirs: "/etc/sysconfig/network-scripts",
			},
			want: "ifcfg",
		},
		{
			name:     "都认不出来",
			sections: map[string]string{secTools: "ip\nss"},
			want:     "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectManager(tc.sections); got != tc.want {
				t.Fatalf("判定为 %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 监听表有两份：networkCommand 里按登录账号采的，与以 root 重采的（带 pid）。
// 取舍只有一条规则 —— 有 root 那份就用它。这里把三种"没有"分别固定下来：
// 提不了权（整节不存在）、提权跑通但 ss 没输出（空行）、以及两份齐全。
func TestPickListenSection(t *testing.T) {
	userOnly := "tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n"
	rootFull := "tcp LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:((\"sshd\",pid=812,fd=3))\n"

	both := map[string]string{secListen: userOnly, secListenRoot: rootFull}
	if got := pickListenSection(both); !strings.Contains(got, "pid=812") {
		t.Fatalf("两份齐全时应取 root 那份，实际取到 %q", got)
	}

	// 提不了权的机器：root 那份整节都不存在，端口列表必须照旧完整。
	fallback := map[string]string{secListen: userOnly}
	if got := pickListenSection(fallback); got != userOnly {
		t.Fatalf("没有 root 那份时应回落到登录用户那份，实际取到 %q", got)
	}

	// 提权跑通了但没有任何输出：同样要回落，否则界面会从"端口齐全、只是没 pid"
	// 变成"一个端口都没有"—— 空行不能当成一份有效结果。
	blank := map[string]string{secListen: userOnly, secListenRoot: "\n"}
	if got := pickListenSection(blank); got != userOnly {
		t.Fatalf("root 那份为空时应回落到登录用户那份，实际取到 %q", got)
	}
}

// 同一端口上可能挂着多个进程（SO_REUSEPORT），名字与 pid 必须取自同一组：
// 张冠李戴的后果是界面显示 A 的名字、结束按钮杀掉的却是 B。
func TestParseSSUsers(t *testing.T) {
	name, pid := parseSSUsers(`users:(("nginx",pid=101,fd=6),("nginx",pid=102,fd=7))`)
	if name != "nginx" || pid != 101 {
		t.Fatalf("多进程组应取第一组，实际得到 %q %d", name, pid)
	}

	// 不带 -p、或执行者无权看到该进程时整段为空：这是正常结果，不是错误。
	if name, pid := parseSSUsers(""); name != "" || pid != 0 {
		t.Fatalf("空段应给出空名字与 pid 0，实际得到 %q %d", name, pid)
	}

	// 进程名带连字符（systemd-resolve 这类）时不能被截断。
	if name, pid := parseSSUsers(`users:(("systemd-resolve",pid=605,fd=13))`); name != "systemd-resolve" || pid != 605 {
		t.Fatalf("带连字符的进程名解析错误：%q %d", name, pid)
	}
}

// ss 输出的本地地址形态很杂：v4 通配、v6 通配带方括号、带 %scope 的链路本地地址。
// 这里逐种覆盖，尤其是带作用域的 IPv6 —— 它最容易在剥后缀与剥括号的顺序上出错。
func TestSplitSocketAddr(t *testing.T) {
	cases := []struct {
		raw     string
		address string
		port    int
	}{
		{"0.0.0.0:22", "0.0.0.0", 22},
		{"127.0.0.53%lo:53", "127.0.0.53", 53},
		{"[::]:22", "::", 22},
		{"*:8080", "0.0.0.0", 8080},
		{"[fe80::4274:e0ff:fedf:9d1e]%wlo1:546", "fe80::4274:e0ff:fedf:9d1e", 546},
		{"[2408:8207:1234::5]:443", "2408:8207:1234::5", 443},
	}

	for _, tc := range cases {
		address, port := splitSocketAddr(tc.raw)
		if address != tc.address || port != tc.port {
			t.Fatalf("splitSocketAddr(%q) = (%q, %d)，期望 (%q, %d)", tc.raw, address, port, tc.address, tc.port)
		}
	}
}

// 归属判定的边界：网桥本身看不出用途，全靠成员构成来推。
func TestInterfaceOwner(t *testing.T) {
	kinds := map[string]string{
		"eth0": "ethernet", "eth1": "ethernet",
		"vethA": "veth", "vethB": "veth",
		"tap0":   "tun",
		"br0":    "bridge",
		"docker": "bridge",
		"mixed":  "bridge",
		"empty":  "bridge",
	}
	physical := map[string]bool{"eth0": true, "eth1": true}
	bridges := map[string][]string{
		"br0":    {"eth0", "eth1"}, // 管理员把两张物理网卡桥接起来
		"docker": {"vethA", "vethB"},
		"mixed":  {"eth0", "vethA"}, // 物理网卡与容器网卡混挂
		"empty":  {},
	}

	cases := []struct {
		name string
		link ipLink
		want string
	}{
		{"有内核硬件设备的网卡", ipLink{IfName: "eth0"}, OwnerPhysical},
		{"回环", ipLink{IfName: "lo"}, OwnerKernel},
		{"带 LOOPBACK 标志的接口", ipLink{IfName: "whatever", Flags: []string{"LOOPBACK"}}, OwnerKernel},
		{"veth 网卡对", ipLink{IfName: "vethA"}, OwnerContainer},
		{"成员全是 veth 的网桥", ipLink{IfName: "docker"}, OwnerContainer},
		{"成员是物理网卡的网桥", ipLink{IfName: "br0"}, OwnerVirtual},
		{"没有成员的网桥", ipLink{IfName: "empty"}, OwnerVirtual},
		// 混合成员时保守归为管理员建的：只凭一个 veth 成员就断定它是容器网络，
		// 会让"把容器网卡挂进自己网桥"的机器被误标，宁可少断言也不要错标。
		{"成员混合的网桥", ipLink{IfName: "mixed"}, OwnerVirtual},
		{"没有硬件设备的普通接口", ipLink{IfName: "unknown0"}, OwnerVirtual},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := interfaceOwner(tc.link, kinds, bridges, physical)
			if got != tc.want {
				t.Fatalf("归属判定为 %q，期望 %q", got, tc.want)
			}
		})
	}

	// 虚拟机网桥（成员是 tap）不归容器：它由 libvirt 之类的工具管理，
	// 与容器运行时是两回事。
	vm := map[string]string{"virbr0": "bridge", "tap0": "tun"}
	vmBridges := map[string][]string{"virbr0": {"tap0"}}
	if got := interfaceOwner(ipLink{IfName: "virbr0"}, vm, vmBridges, nil); got != OwnerVirtual {
		t.Fatalf("虚拟机网桥的归属为 %q，期望 %q", got, OwnerVirtual)
	}
}

func TestNaturalLess(t *testing.T) {
	// 纯字典序会把 eth10 排在 eth2 之前，网口一多就很难找。
	cases := []struct {
		a, b string
		want bool
	}{
		{"eth2", "eth10", true},
		{"eth10", "eth2", false},
		{"eth0", "eth1", true},
		{"br0", "eth0", true},
		{"eth0", "eth0", false},
	}
	for _, tc := range cases {
		if got := naturalLess(tc.a, tc.b); got != tc.want {
			t.Fatalf("naturalLess(%q, %q) = %v，期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}
