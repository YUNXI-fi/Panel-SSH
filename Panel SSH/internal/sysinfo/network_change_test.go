package sysinfo

import (
	"strings"
	"testing"
)

// changeSnapshot 是变更校验用的现状：eth0 空闲且由 NetworkManager 托管，
// eth1 未被托管，eth2 已挂在 br0 上。四种典型情况各占一个，用例按需取用。
func changeSnapshot() *NetworkSnapshot {
	return &NetworkSnapshot{
		Manager: "NetworkManager",
		Interfaces: []Interface{
			{Name: "eth0", Type: "ethernet", AdminUp: false, MTU: 1500, ConnName: "Wired connection 1"},
			{Name: "eth1", Type: "ethernet", AdminUp: false, MTU: 1500},
			{Name: "eth2", Type: "ethernet", AdminUp: true, MTU: 1500, ConnName: "Wired connection 2", Master: "br0"},
			{Name: "br0", Type: "bridge", AdminUp: true, MTU: 1500, ConnName: "br0", Members: []string{"eth2"}},
		},
	}
}

func TestPlanAddressManual(t *testing.T) {
	plan, err := PlanNetworkChange(NetChangeRequest{
		Action:    NetChangeAddress,
		Interface: "eth0",
		Method:    "manual",
		Addresses: []string{"192.168.1.50/24"},
		Gateway:   "192.168.1.1",
		DNS:       []string{"223.5.5.5", "1.1.1.1"},
	}, changeSnapshot())
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	want := "nmcli con mod 'Wired connection 1' ipv4.method manual ipv4.addresses '192.168.1.50/24' ipv4.gateway '192.168.1.1' ipv4.dns '223.5.5.5,1.1.1.1'"
	if len(plan.Safe) != 1 || plan.Safe[0] != want {
		t.Fatalf("同步段命令错误：%v", plan.Safe)
	}
	if len(plan.Risky) != 1 || plan.Risky[0] != "nmcli con up 'Wired connection 1'" {
		t.Fatalf("延迟段命令错误：%v", plan.Risky)
	}

	// 主机位必须保留：写成 192.168.1.0/24 会让这台机器配上一个网络地址。
	if len(plan.Expect.Addresses) != 1 || plan.Expect.Addresses[0] != "192.168.1.50/24" {
		t.Fatalf("期望地址被规范化错了：%v", plan.Expect.Addresses)
	}
}

func TestPlanAddressAuto(t *testing.T) {
	plan, err := PlanNetworkChange(NetChangeRequest{
		Action:    NetChangeAddress,
		Interface: "eth0",
		Method:    "auto",
	}, changeSnapshot())
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	// 切回 DHCP 时必须显式清空静态项，否则旧的静态地址会残留在配置里。
	want := "nmcli con mod 'Wired connection 1' ipv4.method auto ipv4.addresses '' ipv4.gateway '' ipv4.dns ''"
	if plan.Safe[0] != want {
		t.Fatalf("同步段命令错误：%s", plan.Safe[0])
	}
}

func TestPlanRejectsBadInput(t *testing.T) {
	snapshot := changeSnapshot()

	cases := []struct {
		name string
		req  NetChangeRequest
	}{
		{
			name: "网口名里带命令分隔符",
			req:  NetChangeRequest{Action: NetChangeAddress, Interface: "eth0; reboot", Method: "auto"},
		},
		{
			name: "地址里夹带命令",
			req: NetChangeRequest{
				Action: NetChangeAddress, Interface: "eth0", Method: "manual",
				Addresses: []string{"192.168.1.5/24; reboot"},
			},
		},
		{
			name: "DNS 里夹带命令",
			req: NetChangeRequest{
				Action: NetChangeAddress, Interface: "eth0", Method: "manual",
				Addresses: []string{"192.168.1.5/24"}, DNS: []string{"8.8.8.8; reboot"},
			},
		},
		{
			name: "网关不是 IP",
			req: NetChangeRequest{
				Action: NetChangeAddress, Interface: "eth0", Method: "manual",
				Addresses: []string{"192.168.1.5/24"}, Gateway: "192.168.1.1/24",
			},
		},
		{
			name: "地址缺前缀长度",
			req: NetChangeRequest{
				Action: NetChangeAddress, Interface: "eth0", Method: "manual",
				Addresses: []string{"192.168.1.5"},
			},
		},
		{
			name: "地址方式未指定",
			req:  NetChangeRequest{Action: NetChangeAddress, Interface: "eth0"},
		},
		{
			name: "网口不存在",
			req:  NetChangeRequest{Action: NetChangeAddress, Interface: "eth9", Method: "auto"},
		},
		{
			name: "设备未被 NetworkManager 托管",
			req:  NetChangeRequest{Action: NetChangeAddress, Interface: "eth1", Method: "auto"},
		},
		{
			name: "MTU 超出范围",
			req:  NetChangeRequest{Action: NetChangeMTU, Interface: "eth0", MTU: 90000},
		},
		{
			name: "网桥不支持改 MTU",
			req:  NetChangeRequest{Action: NetChangeMTU, Interface: "br0", MTU: 1400},
		},
		{
			name: "未指定成员就建网桥",
			req:  NetChangeRequest{Action: NetChangeBridgeCreate, Bridge: "br1", Method: "auto"},
		},
		{
			name: "网桥名已存在",
			req:  NetChangeRequest{Action: NetChangeBridgeCreate, Bridge: "br0", Members: []string{"eth0"}, Method: "auto"},
		},
		{
			name: "成员已经挂在别的网桥上",
			req:  NetChangeRequest{Action: NetChangeBridgeCreate, Bridge: "br1", Members: []string{"eth2"}, Method: "auto"},
		},
		{
			name: "把非网桥当网桥删",
			req:  NetChangeRequest{Action: NetChangeBridgeDelete, Bridge: "eth0"},
		},
		{
			name: "把非网桥当网桥去挂接",
			req:  NetChangeRequest{Action: NetChangeBridgeAttach, Interface: "eth0", Bridge: "eth2"},
		},
		{
			name: "状态未变化时拒绝下发",
			req:  NetChangeRequest{Action: NetChangeLinkState, Interface: "br0", Up: true},
		},
		{
			name: "改一个没挂在网桥上的网口",
			req:  NetChangeRequest{Action: NetChangeBridgeDetach, Interface: "eth0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PlanNetworkChange(tc.req, snapshot); err == nil {
				t.Fatal("应当被拒绝，实际通过了校验")
			}
		})
	}
}

func TestPlanRejectsNonNetworkManager(t *testing.T) {
	snapshot := changeSnapshot()
	snapshot.Manager = "netplan"

	// 没有 NM 就没有检查点，改错配置将无法自动回滚，必须直接拒绝。
	_, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeAddress, Interface: "eth0", Method: "auto",
	}, snapshot)
	if err == nil || !strings.Contains(err.Error(), "netplan") {
		t.Fatalf("应当以配置后端为由拒绝，实际：%v", err)
	}
}

func TestPlanLinkState(t *testing.T) {
	plan, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeLinkState, Interface: "eth0", Up: true,
	}, changeSnapshot())
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	if len(plan.Safe) != 0 {
		t.Fatalf("启用连接不应有同步段：%v", plan.Safe)
	}
	if len(plan.Risky) != 1 || plan.Risky[0] != "nmcli con up 'Wired connection 1'" {
		t.Fatalf("延迟段命令错误：%v", plan.Risky)
	}
}

func TestPlanBridgeCreate(t *testing.T) {
	plan, err := PlanNetworkChange(NetChangeRequest{
		Action:    NetChangeBridgeCreate,
		Bridge:    "br1",
		Members:   []string{"eth0"},
		Method:    "manual",
		Addresses: []string{"192.168.1.60/24"},
		Gateway:   "192.168.1.1",
	}, changeSnapshot())
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	// 新建连接对象不影响运行态，可以同步执行并把报错交给用户。
	if len(plan.Safe) != 1 || !strings.HasPrefix(plan.Safe[0], "nmcli con add type bridge ifname 'br1' con-name 'br1' ipv4.method manual") {
		t.Fatalf("同步段命令错误：%v", plan.Safe)
	}
	// 挂成员与激活网桥会切断现有连接，必须放到延迟段。
	if len(plan.Risky) != 2 {
		t.Fatalf("延迟段命令数量错误：%v", plan.Risky)
	}
	if plan.Risky[0] != "nmcli con mod 'Wired connection 1' master 'br1'" {
		t.Fatalf("挂成员命令错误：%s", plan.Risky[0])
	}
	if plan.Risky[1] != "nmcli con up 'br1'" {
		t.Fatalf("激活网桥命令错误：%s", plan.Risky[1])
	}
}

func TestPlanBridgeDeleteReleasesMembersFirst(t *testing.T) {
	plan, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeBridgeDelete, Bridge: "br0",
	}, changeSnapshot())
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	// br0 的成员是 eth2；必须先释放再删除网桥，
	// 否则成员连接会停在"等待一个已不存在的网桥"的状态。
	if len(plan.Risky) != 2 {
		t.Fatalf("延迟段命令数量错误：%v", plan.Risky)
	}
	if plan.Risky[0] != "nmcli con mod 'Wired connection 2' master ''" {
		t.Fatalf("释放成员命令错误：%s", plan.Risky[0])
	}
	if plan.Risky[1] != "nmcli con delete 'br0'" {
		t.Fatalf("删除网桥命令错误：%s", plan.Risky[1])
	}
}

func TestVerifyDetectsIneffectiveChange(t *testing.T) {
	// 这是"变更失败被检查点回滚"与"变更成功"唯一的分界：
	// 两种情况下主机都能连上，只能靠状态比对区分。
	expect := &NetChangeExpect{
		Action:    NetChangeAddress,
		Interface: "eth0",
		Method:    "manual",
		Addresses: []string{"192.168.1.50/24"},
	}

	changed := &NetworkSnapshot{Interfaces: []Interface{
		{Name: "eth0", Addrs: []Address{{Family: "ipv4", Addr: "192.168.1.50", Prefix: 24}}},
	}}
	if err := expect.Verify(changed); err != nil {
		t.Fatalf("地址已变更却判定未生效：%v", err)
	}

	rolledBack := &NetworkSnapshot{Interfaces: []Interface{
		{Name: "eth0", Addrs: []Address{{Family: "ipv4", Addr: "192.168.1.10", Prefix: 24}}},
	}}
	if err := expect.Verify(rolledBack); err == nil {
		t.Fatal("地址没变时应判定为未生效")
	}

	// 前缀长度对不上同样算未生效：192.168.1.50/16 与 /24 不是同一件事。
	wrongPrefix := &NetworkSnapshot{Interfaces: []Interface{
		{Name: "eth0", Addrs: []Address{{Family: "ipv4", Addr: "192.168.1.50", Prefix: 16}}},
	}}
	if err := expect.Verify(wrongPrefix); err == nil {
		t.Fatal("前缀长度不同时应判定为未生效")
	}
}

func TestVerifyBridgeAndLinkState(t *testing.T) {
	attach := &NetChangeExpect{Action: NetChangeBridgeAttach, Interface: "eth1", Bridge: "br0"}
	attached := &NetworkSnapshot{Interfaces: []Interface{{Name: "eth1", Master: "br0"}}}
	if err := attach.Verify(attached); err != nil {
		t.Fatalf("已挂载却判定未生效：%v", err)
	}
	if err := attach.Verify(&NetworkSnapshot{Interfaces: []Interface{{Name: "eth1"}}}); err == nil {
		t.Fatal("未挂载时应判定为未生效")
	}

	created := &NetChangeExpect{
		Action: NetChangeBridgeCreate, Bridge: "br1", Members: []string{"eth1", "eth2"},
	}
	ok := &NetworkSnapshot{Interfaces: []Interface{
		{Name: "br1", Type: "bridge", Members: []string{"eth1", "eth2"}},
	}}
	if err := created.Verify(ok); err != nil {
		t.Fatalf("网桥已建立却判定失败：%v", err)
	}
	missingMember := &NetworkSnapshot{Interfaces: []Interface{
		{Name: "br1", Type: "bridge", Members: []string{"eth1"}},
	}}
	if err := created.Verify(missingMember); err == nil {
		t.Fatal("成员没挂全时应判定为未生效")
	}

	deleted := &NetChangeExpect{Action: NetChangeBridgeDelete, Bridge: "br0"}
	if err := deleted.Verify(&NetworkSnapshot{Interfaces: []Interface{{Name: "eth0"}}}); err != nil {
		t.Fatalf("网桥已删除却判定失败：%v", err)
	}
	if err := deleted.Verify(&NetworkSnapshot{Interfaces: []Interface{{Name: "br0", Type: "bridge"}}}); err == nil {
		t.Fatal("网桥仍在时应判定为未生效")
	}

	up := &NetChangeExpect{Action: NetChangeLinkState, Interface: "eth1", Up: false}
	if err := up.Verify(&NetworkSnapshot{Interfaces: []Interface{{Name: "eth1", AdminUp: true}}}); err == nil {
		t.Fatal("接口仍处于启用态时应判定为未生效")
	}
	if err := up.Verify(&NetworkSnapshot{Interfaces: []Interface{{Name: "eth1", AdminUp: false}}}); err != nil {
		t.Fatalf("接口已停用却判定失败：%v", err)
	}
}

func TestShellQuoteEscapesSingleQuote(t *testing.T) {
	// 连接名由目标机上的用户决定，转义层必须能处理里面的单引号。
	got := shellQuote("it's mine")
	if got != `'it'\''s mine'` {
		t.Fatalf("转义结果错误：%s", got)
	}
	// 连接名白名单是更外层的防线：先于转义把可疑字符挡在门外。
	if connectionNamePattern.MatchString("it's mine") {
		t.Fatal("连接名正则不应放行单引号")
	}
	if connectionNamePattern.MatchString("eth0; reboot") {
		t.Fatal("连接名正则不应放行命令分隔符")
	}
	// 中文连接名是 NM 的默认形态，必须放行。
	if !connectionNamePattern.MatchString("有线连接 1") {
		t.Fatal("连接名正则应当放行中文与空格")
	}
}
