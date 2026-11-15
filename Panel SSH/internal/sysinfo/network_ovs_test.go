package sysinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ovsSnapshot 是 OVS 校验用的现状：
//
//   - enp1s0-ovs 是飞牛那类前置网桥，enp1s0 是它的上行口（承载默认路由），
//     enp2s0 也已挂在上面；
//   - br-int 只有一个虚拟内部口；
//   - enp3s0 空闲，enp4s0 挂在 Linux 网桥 br-linux 上，用来验证"不能同时属于两套桥接体系"。
//   - lo 用来验证回环口被挡住。
func ovsSnapshot() *NetworkSnapshot {
	return &NetworkSnapshot{
		Manager: "NetworkManager",
		Interfaces: []Interface{
			{Name: "enp1s0", Type: "ethernet"},
			{Name: "enp2s0", Type: "ethernet"},
			{Name: "enp3s0", Type: "ethernet"},
			{Name: "enp4s0", Type: "ethernet", Master: "br-linux"},
			{Name: "enp1s0-ovs", Type: "openvswitch"},
			{Name: "br-int", Type: "openvswitch"},
			{Name: "br-linux", Type: "bridge", Members: []string{"enp4s0"}},
			{Name: "lo", Type: "loopback"},
		},
		Routes: []Route{
			{Dst: "default", Dev: "enp1s0-ovs", Family: "ipv4"},
		},
		Ovs: &OvsState{
			Available: true,
			Bridges: []OvsBridge{
				{
					Name:                "enp1s0-ovs",
					CarriesDefaultRoute: true,
					Ports: []OvsPort{
						{Name: "enp1s0", Physical: true},
						{Name: "enp2s0", Physical: true},
					},
				},
				{
					Name:  "br-int",
					Ports: []OvsPort{{Name: "br-int", Physical: false}},
				},
			},
		},
	}
}

// ---------- 建桥 / 删桥 ----------

func TestPlanOvsBridgeCreate(t *testing.T) {
	plan, err := PlanNetworkChange(NetChangeRequest{
		Action:  NetChangeOvsBridgeCreate,
		Bridge:  "br-panel",
		Members: []string{"enp3s0"},
	}, ovsSnapshot())
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	if len(plan.Safe) != 0 {
		t.Fatalf("OVS 变更不应有同步段：%v", plan.Safe)
	}
	want := []string{
		"ovs-vsctl add-br 'br-panel'",
		// 标记必须紧跟建桥：回滚要删掉这座新建的桥，而恢复脚本只有认得出
		// "这是我们建的"才敢动手。排在挂端口之后的话，在挂端口那一步断连
		// 就会留下一座没有标记、谁也认领不了的桥。
		"ovs-vsctl set bridge 'br-panel' external-ids:" + ovsCreatedExternalID + "=1",
		"ovs-vsctl --may-exist add-port 'br-panel' 'enp3s0'",
	}
	if len(plan.Risky) != len(want) {
		t.Fatalf("延迟段命令数量错误：%v", plan.Risky)
	}
	for i := range want {
		if plan.Risky[i] != want[i] {
			t.Fatalf("第 %d 条命令错误：%s（期望 %s）", i, plan.Risky[i], want[i])
		}
	}

	if plan.Expect.Action != NetChangeOvsBridgeCreate || plan.Expect.Bridge != "br-panel" {
		t.Fatalf("期望状态错误：%+v", plan.Expect)
	}
}

func TestPlanOvsBridgeDeleteOnlyOwnBridges(t *testing.T) {
	// 面板建的桥可以删。
	snapshot := ovsSnapshot()
	ovsBridgeByName(t, snapshot, "br-int").Created = true

	plan, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeOvsBridgeDelete, Bridge: "br-int",
	}, snapshot)
	if err != nil {
		t.Fatalf("面板建的桥应当可删：%v", err)
	}
	if len(plan.Risky) != 1 || plan.Risky[0] != "ovs-vsctl --if-exists del-br 'br-int'" {
		t.Fatalf("延迟段命令错误：%v", plan.Risky)
	}

	// 目标机自己的网络服务建的桥不能删 —— 飞牛的 enp1s0-ovs 就是这一类，
	// 删掉会连带毁掉那台机器的网络配置。
	_, err = PlanNetworkChange(NetChangeRequest{
		Action: NetChangeOvsBridgeDelete, Bridge: "enp1s0-ovs",
	}, ovsSnapshot())
	if err == nil || !strings.Contains(err.Error(), "不是面板建的") {
		t.Fatalf("系统建的桥应当被拒绝，实际：%v", err)
	}
}

func TestPlanOvsBridgeCreateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		req  NetChangeRequest
	}{
		{
			name: "网桥名里带命令分隔符",
			req:  NetChangeRequest{Action: NetChangeOvsBridgeCreate, Bridge: "br0; reboot"},
		},
		{
			name: "名字被已有接口占用",
			req:  NetChangeRequest{Action: NetChangeOvsBridgeCreate, Bridge: "enp3s0"},
		},
		{
			name: "成员名里带命令分隔符",
			req:  NetChangeRequest{Action: NetChangeOvsBridgeCreate, Bridge: "br-panel", Members: []string{"enp3s0; reboot"}},
		},
		{
			name: "成员已挂在别的 OVS 网桥上",
			req:  NetChangeRequest{Action: NetChangeOvsBridgeCreate, Bridge: "br-panel", Members: []string{"enp2s0"}},
		},
		{
			name: "成员已挂在 Linux 网桥上",
			req:  NetChangeRequest{Action: NetChangeOvsBridgeCreate, Bridge: "br-panel", Members: []string{"enp4s0"}},
		},
		{
			name: "回环口不能入桥",
			req:  NetChangeRequest{Action: NetChangeOvsBridgeCreate, Bridge: "br-panel", Members: []string{"lo"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PlanNetworkChange(tc.req, ovsSnapshot()); err == nil {
				t.Fatal("应当被拒绝，实际通过了校验")
			}
		})
	}
}

func TestVerifyOvsBridge(t *testing.T) {
	create := &NetChangeExpect{
		Action: NetChangeOvsBridgeCreate, Bridge: "br-panel", Members: []string{"enp3s0"},
	}
	applied := ovsSnapshot()
	applied.Ovs.Bridges = append(applied.Ovs.Bridges, OvsBridge{
		Name: "br-panel", Created: true,
		Ports: []OvsPort{{Name: "br-panel"}, {Name: "enp3s0", Physical: true}},
	})
	if err := create.Verify(applied); err != nil {
		t.Fatalf("网桥已建立却判定失败：%v", err)
	}
	if err := create.Verify(ovsSnapshot()); err == nil {
		t.Fatal("网桥没建出来时应判定为未生效")
	}

	// 桥建出来了但成员没挂全，同样算未生效 —— 现象是"面板又能连上了"，
	// 只有逐项比对才分得清是成功了还是被看门狗回滚了。
	missingMember := ovsSnapshot()
	missingMember.Ovs.Bridges = append(missingMember.Ovs.Bridges,
		OvsBridge{Name: "br-panel", Created: true, Ports: []OvsPort{{Name: "br-panel"}}})
	if err := create.Verify(missingMember); err == nil {
		t.Fatal("成员没挂全时应判定为未生效")
	}

	del := &NetChangeExpect{Action: NetChangeOvsBridgeDelete, Bridge: "br-int"}
	if err := del.Verify(ovsSnapshot()); err == nil {
		t.Fatal("网桥仍在时应判定为未生效")
	}
	// 按名字摘掉，不用下标：手写夹具的顺序和采集排序后的顺序无关。
	gone := ovsSnapshot()
	kept := make([]OvsBridge, 0, len(gone.Ovs.Bridges))
	for _, bridge := range gone.Ovs.Bridges {
		if bridge.Name != "br-int" {
			kept = append(kept, bridge)
		}
	}
	gone.Ovs.Bridges = kept
	if err := del.Verify(gone); err != nil {
		t.Fatalf("网桥已删除却判定失败：%v", err)
	}
}

// ovsSections 拼出 buildOvsState 要读的那几节，省得每个用例都手写一遍 map。
func ovsSections(bridges, created, persist string) map[string]string {
	return map[string]string{
		secOvs:        bridges,
		secOvsCreated: created,
		secOvsPersist: persist,
	}
}

func TestBuildOvsState(t *testing.T) {
	// 目标机上的真实输出形态：「网桥 端口…」，与 Linux 网桥那一节同构。
	section := "enp1s0-ovs enp1s0 enp2s0 \nbr-int br-int \n"
	physical := map[string]bool{"enp1s0": true, "enp2s0": true, "enp3s0": true}
	defaultDevs := map[string]bool{"enp1s0-ovs": true}

	tools := []string{"ip", "ovs-vsctl", "ss"}
	state := buildOvsState(ovsSections(section, "", ""), physical, tools, defaultDevs)

	if !state.Available {
		t.Fatal("工具列表里有 ovs-vsctl，应当判定为可用")
	}
	if len(state.Bridges) != 2 {
		t.Fatalf("网桥数量错误：%+v", state.Bridges)
	}
	// 名字排序保证同一台机器每次采集顺序一致，否则前端轮询时卡片会跳位。
	if state.Bridges[0].Name != "br-int" || state.Bridges[1].Name != "enp1s0-ovs" {
		t.Fatalf("网桥未按名字排序：%+v", state.Bridges)
	}

	bridge := state.Bridges[1]
	if !bridge.CarriesDefaultRoute {
		t.Fatal("默认路由出口是这个网桥，标记缺失会让界面漏掉断连警告")
	}
	if len(bridge.Ports) != 2 {
		t.Fatalf("端口数量错误：%+v", bridge.Ports)
	}
	// 物理口必须排在虚拟口前面：用户要操作的是网线，不是网桥的内部口。
	if bridge.Ports[0].Name != "enp1s0" || bridge.Ports[1].Name != "enp2s0" {
		t.Fatalf("物理口未前置或未按自然序排列：%+v", bridge.Ports)
	}
	for _, port := range bridge.Ports {
		if !port.Physical {
			t.Fatalf("%s 应当被判为物理口", port.Name)
		}
	}

	// br-int 的内部口在 /sys/class/net 下没有 device，不能算物理口。
	internal := state.Bridges[0]
	if len(internal.Ports) != 1 || internal.Ports[0].Physical {
		t.Fatalf("网桥内部口被误判成物理口：%+v", internal.Ports)
	}

	// 名字不在白名单里的端口直接丢弃：它随后会被拼进恢复脚本。
	dirty := buildOvsState(ovsSections("br0 eth1 $(reboot) \n", "", ""), physical, tools, nil)
	if len(dirty.Bridges) != 1 || len(dirty.Bridges[0].Ports) != 1 {
		t.Fatalf("非法端口名未被过滤：%+v", dirty.Bridges)
	}
}

func TestBuildOvsStateWithoutTool(t *testing.T) {
	// 没有 ovs-vsctl 的机器上，"按能力探测"必须得出"不可用"，
	// 而不是给出一份空网桥列表让用户以为机器上没有 OVS 网桥。
	state := buildOvsState(ovsSections("", "", ""), nil, []string{"ip", "nmcli"}, nil)
	if state.Available {
		t.Fatal("没有 ovs-vsctl 时应判定为不可用")
	}
	if state.Bridges == nil || len(state.Bridges) != 0 {
		t.Fatalf("不可用时网桥列表应为空数组而不是 nil：%+v", state.Bridges)
	}
}

func TestBuildOvsStatePersistentFlag(t *testing.T) {
	tools := []string{"ip", "ovs-vsctl"}

	// 采集侧读的是 multi-user.target.wants 下的软链，也就是"服务真的被启用"，
	// 而不是"单元文件存在"。面板要在界面上说清楚它在目标机留了个常驻服务，
	// 这个标记判错就会说反。
	if !buildOvsState(ovsSections("", "", "yes"), nil, tools, nil).Persistent {
		t.Fatal("enabled 软链存在时应判定为已装开机自恢复")
	}
	for _, section := range []string{"", "\n", "no"} {
		if buildOvsState(ovsSections("", "", section), nil, tools, nil).Persistent {
			t.Fatalf("采集结果 %q 不该被判为已启用", section)
		}
	}
}

func TestBuildOvsStateCreatedFlag(t *testing.T) {
	tools := []string{"ip", "ovs-vsctl"}
	// 面板建的桥带着 external-id，目标机自己的网络服务建的桥没有。
	// 这个标记决定了一座桥能不能从面板删掉，判错就会去删飞牛的 enp1s0-ovs。
	state := buildOvsState(
		ovsSections("br-panel br-panel \nenp1s0-ovs enp1s0\n", "br-panel\n", ""),
		nil, tools, nil,
	)

	byName := map[string]OvsBridge{}
	for _, bridge := range state.Bridges {
		byName[bridge.Name] = bridge
	}
	if !byName["br-panel"].Created {
		t.Fatal("带了面板 external-id 的网桥应当被判为面板建的")
	}
	if byName["enp1s0-ovs"].Created {
		t.Fatal("没有标记的网桥不能被当成面板建的")
	}
}

func TestPlanOvsAttach(t *testing.T) {
	plan, err := PlanNetworkChange(NetChangeRequest{
		Action:  NetChangeOvsAttach,
		Bridge:  "enp1s0-ovs",
		Members: []string{"enp3s0"},
	}, ovsSnapshot())
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}

	// OVS 的端口关系就是它的运行态，没有"先写配置再激活"的分段，
	// 因此同步段必须为空 —— 否则代码里那套"Safe 失败即回滚"会白白多跑一轮。
	if len(plan.Safe) != 0 {
		t.Fatalf("OVS 变更不应有同步段：%v", plan.Safe)
	}
	want := "ovs-vsctl --may-exist add-port 'enp1s0-ovs' 'enp3s0'"
	if len(plan.Risky) != 1 || plan.Risky[0] != want {
		t.Fatalf("延迟段命令错误：%v", plan.Risky)
	}
	if plan.Expect.Action != NetChangeOvsAttach || plan.Expect.Bridge != "enp1s0-ovs" {
		t.Fatalf("期望状态错误：%+v", plan.Expect)
	}
	if len(plan.Expect.Members) != 1 || plan.Expect.Members[0] != "enp3s0" {
		t.Fatalf("期望成员错误：%+v", plan.Expect.Members)
	}
}

func TestPlanOvsDetach(t *testing.T) {
	plan, err := PlanNetworkChange(NetChangeRequest{
		Action:    NetChangeOvsDetach,
		Interface: "enp2s0",
		Bridge:    "enp1s0-ovs",
	}, ovsSnapshot())
	if err != nil {
		t.Fatalf("生成计划失败：%v", err)
	}
	want := "ovs-vsctl --if-exists del-port 'enp1s0-ovs' 'enp2s0'"
	if len(plan.Risky) != 1 || plan.Risky[0] != want {
		t.Fatalf("延迟段命令错误：%v", plan.Risky)
	}

	// 不传网桥名时由现状反查，不能反过来要求调用方必须先知道答案。
	derived, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeOvsDetach, Interface: "enp2s0",
	}, ovsSnapshot())
	if err != nil {
		t.Fatalf("反查网桥失败：%v", err)
	}
	if derived.Expect.Bridge != "enp1s0-ovs" {
		t.Fatalf("反查出的网桥错误：%s", derived.Expect.Bridge)
	}
}

func TestPlanOvsRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		req  NetChangeRequest
	}{
		{
			name: "网口名里带命令分隔符",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp3s0; reboot"}},
		},
		{
			name: "没有选成员",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs"},
		},
		{
			name: "网桥不存在",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "br9", Members: []string{"enp3s0"}},
		},
		{
			name: "把 Linux 网桥当 OVS 网桥用",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "br-linux", Members: []string{"enp3s0"}},
		},
		{
			name: "网口已在该 OVS 网桥上",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp2s0"}},
		},
		{
			name: "网口已挂在 Linux 网桥上",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp4s0"}},
		},
		{
			name: "回环口不能入桥",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"lo"}},
		},
		{
			name: "网桥不能作为端口加入自己",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp1s0-ovs"}},
		},
		{
			name: "同一个网口选两次",
			req:  NetChangeRequest{Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp3s0", "enp3s0"}},
		},
		{
			name: "摘一个没挂在 OVS 上的网口",
			req:  NetChangeRequest{Action: NetChangeOvsDetach, Interface: "enp3s0"},
		},
		{
			name: "请求的网桥与现状不符",
			req:  NetChangeRequest{Action: NetChangeOvsDetach, Interface: "enp2s0", Bridge: "br-int"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PlanNetworkChange(tc.req, ovsSnapshot()); err == nil {
				t.Fatal("应当被拒绝，实际通过了校验")
			}
		})
	}
}

func TestPlanOvsWithoutOpenVSwitch(t *testing.T) {
	snapshot := ovsSnapshot()
	snapshot.Ovs = &OvsState{Available: false, Bridges: []OvsBridge{}}

	// 这台机器没有 OVS 时给出的是"没有 Open vSwitch"，
	// 而不是被 NetworkManager 那道门槛挡下 —— 两者是完全不同的原因。
	_, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp3s0"},
	}, snapshot)
	if err == nil || !strings.Contains(err.Error(), "Open vSwitch") {
		t.Fatalf("应当以缺少 OVS 为由拒绝，实际：%v", err)
	}
}

// TestOvsOpsAreRefusedWhenStateIsUnreadable 验证"读不到"在服务端也守得住。
//
// 界面上那排按钮会被禁用，但服务端的判断不能建立在"前端拦住了"之上：
// 一份空的现状与"这台机器上还没有网桥"在计划器看来一模一样，据此规划出来的
// 动作可能去删一座其实存在的桥，或者把一次已经被看门狗回滚的变更报成生效。
func TestOvsOpsAreRefusedWhenStateIsUnreadable(t *testing.T) {
	unread := ovsSnapshot()
	unread.Ovs.Unreadable = true
	unread.Ovs.Bridges = []OvsBridge{}

	_, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeOvsBridgeCreate, Bridge: "br-panel", Members: []string{"enp3s0"},
	}, unread)
	if err == nil || !strings.Contains(err.Error(), "读不到目标主机的 OVS 状态") {
		t.Fatalf("读不到 OVS 状态时不该给出变更计划，实际：%v", err)
	}

	attach := &NetChangeExpect{Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp3s0"}}
	if err := attach.Verify(unread); err == nil || !strings.Contains(err.Error(), "无法读取 OVS 状态") {
		t.Fatalf("读不到 OVS 状态时核对应当判为失败，实际：%v", err)
	}
}

func TestPlanOvsNotBlockedByManagerGate(t *testing.T) {
	snapshot := ovsSnapshot()
	snapshot.Manager = "systemd-networkd"

	// OVS 的改动不经过 NetworkManager，因此"必须是 NetworkManager"这道门槛
	// 不该挡住它 —— 飞牛这类的机器正是 NetworkManager 未必在管网络、
	// 而网口统统挂在 OVS 上的形态。
	if _, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp3s0"},
	}, snapshot); err != nil {
		t.Fatalf("OVS 变更不应被 NetworkManager 门槛挡住：%v", err)
	}

	// 但 NM 类变更仍然要被挡住，这条边界不能因为上面那次放行而消失。
	_, err := PlanNetworkChange(NetChangeRequest{
		Action: NetChangeAddress, Interface: "enp3s0", Method: "auto",
	}, snapshot)
	if err == nil {
		t.Fatal("NetworkManager 类变更仍应被挡住")
	}
}

// ovsBridgeByName 取夹具里的某个网桥。
//
// 刻意不用下标：手写夹具的顺序和 buildOvsState 排序后的顺序无关，
// 用下标指对象极易改错目标 —— 下面几处断言的全部意义就在于"改对了哪一个"。
func ovsBridgeByName(t *testing.T, snapshot *NetworkSnapshot, name string) *OvsBridge {
	t.Helper()
	for i := range snapshot.Ovs.Bridges {
		if snapshot.Ovs.Bridges[i].Name == name {
			return &snapshot.Ovs.Bridges[i]
		}
	}
	t.Fatalf("夹具里没有网桥 %s", name)
	return nil
}

func TestVerifyOvs(t *testing.T) {
	attach := &NetChangeExpect{
		Action: NetChangeOvsAttach, Bridge: "enp1s0-ovs", Members: []string{"enp3s0"},
	}
	applied := ovsSnapshot()
	ovsBridgeByName(t, applied, "enp1s0-ovs").Ports = append(
		ovsBridgeByName(t, applied, "enp1s0-ovs").Ports, OvsPort{Name: "enp3s0", Physical: true})
	if err := attach.Verify(applied); err != nil {
		t.Fatalf("端口已挂上却判定未生效：%v", err)
	}
	if err := attach.Verify(ovsSnapshot()); err == nil {
		t.Fatal("端口没挂上时应判定为未生效")
	}

	// 挂到了另一个网桥上也算没生效：现象同样是"主机又能连上"，
	// 只有比对到具体网桥才能把它和"被看门狗回滚了"区分开。
	wrongBridge := ovsSnapshot()
	ovsBridgeByName(t, wrongBridge, "br-int").Ports = append(
		ovsBridgeByName(t, wrongBridge, "br-int").Ports, OvsPort{Name: "enp3s0"})
	if err := attach.Verify(wrongBridge); err == nil {
		t.Fatal("端口挂错网桥时应判定为未生效")
	}

	detach := &NetChangeExpect{Action: NetChangeOvsDetach, Interface: "enp2s0", Bridge: "enp1s0-ovs"}
	detached := ovsSnapshot()
	target := ovsBridgeByName(t, detached, "enp1s0-ovs")
	target.Ports = target.Ports[:1]
	if err := detach.Verify(detached); err != nil {
		t.Fatalf("端口已摘除却判定未生效：%v", err)
	}
	if err := detach.Verify(ovsSnapshot()); err == nil {
		t.Fatal("端口仍在桥上时应判定为未生效")
	}

	// 读不到 OVS 数据时，"挂上了没有"必须判为失败：把它当成成功会把
	// 一次已经回滚的变更报成生效。
	empty := ovsSnapshot()
	empty.Ovs = nil
	if err := attach.Verify(empty); err == nil {
		t.Fatal("读不到 OVS 时不能把挂载动作判定为成功")
	}
}

func TestOvsBridgeOfIgnoresLinuxBridge(t *testing.T) {
	snapshot := ovsSnapshot()
	// enp4s0 挂在 Linux 网桥 br-linux 上，但不在任何 OVS 网桥里。
	if got := snapshot.ovsBridgeOf("enp4s0"); got != "" {
		t.Fatalf("Linux 网桥成员被误判为 OVS 端口：%s", got)
	}
	if got := snapshot.ovsBridgeOf("enp2s0"); got != "enp1s0-ovs" {
		t.Fatalf("反查结果错误：%s", got)
	}
	if got := snapshot.ovsBridgeOf("enp3s0"); got != "" {
		t.Fatalf("空闲网口不应有归属：%s", got)
	}
}

func TestOvsRestoreScriptContent(t *testing.T) {
	before := ovsSnapshot()
	script := OvsRestoreScript(before)

	// 恢复脚本只描述"变更前是什么样"，不描述"这次改了什么"。
	// 因此它必须为变更前就有的每个端口写一条 add-port 兜底。
	for _, want := range []string{
		"#!/bin/sh",
		"ovs-vsctl --if-exists del-port 'enp1s0-ovs' \"$p\"",
		"case \"$p\" in enp1s0|enp2s0) ;;",
		"ovs-vsctl --may-exist add-port 'enp1s0-ovs' 'enp1s0'",
		"ovs-vsctl --may-exist add-port 'enp1s0-ovs' 'enp2s0'",
		"ovs-vsctl --may-exist add-port 'br-int' 'br-int'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("恢复脚本缺少 %q：\n%s", want, script)
		}
	}
	if !strings.HasSuffix(script, "exit 0\n") {
		t.Fatalf("恢复脚本应以 exit 0 收尾，否则看门狗执行它会被判为失败：\n%s", script)
	}

	// 端口名必须经白名单过滤后再进脚本，脚本里不该出现任何能改变命令结构的字符。
	dirty := &NetworkSnapshot{Ovs: &OvsState{Available: true, Bridges: []OvsBridge{{
		Name:  "br0",
		Ports: []OvsPort{{Name: "eth1; reboot"}},
	}}}}
	if strings.Contains(OvsRestoreScript(dirty), "reboot") {
		t.Fatalf("非法端口名漏进了恢复脚本：\n%s", OvsRestoreScript(dirty))
	}
}

func TestOvsRestoreScriptActuallyRestores(t *testing.T) {
	// 这是整个自建保护里最不能出错的一环：脚本必须真的把 OVS 拉回变更前，
	// 而且要能重复执行（看门狗到点触发与用户手动点回滚可能同时发生）。
	// 用假的 ovs-vsctl 记录调用，验证它要求的状态，而不是靠肉眼读脚本。
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	statePath := filepath.Join(dir, "state")

	// 假 ovs-vsctl 把每次调用追加进日志，list-ports 按网桥名从状态文件取端口。
	// 状态必须按网桥分开：共用一份的话，对 br-int 的处理会被 enp1s0-ovs 的
	// 端口列表污染，测出来的结论毫无意义。
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "$OVS_LOG"
case "$*" in
  *list-ports*)
    for last in "$@"; do :; done
    awk -v b="$last" -F'|' '$1==b {print $2}' "$OVS_STATE" 2>/dev/null | tr ' ' '\n'
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ovs-vsctl"), []byte(fake), 0o755); err != nil {
		t.Fatalf("写入假 ovs-vsctl 失败：%v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("OVS_LOG", logPath)
	t.Setenv("OVS_STATE", statePath)

	// ports 是 enp1s0-ovs 在"变更执行之后"当下的端口列表；
	// br-int 全程只有一个内部口，用来确认恢复脚本没有跨网桥误伤。
	run := func(t *testing.T, ports string) string {
		t.Helper()
		state := "enp1s0-ovs|" + ports + "\nbr-int|br-int\n"
		if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
			t.Fatalf("写入端口状态失败：%v", err)
		}
		if err := os.WriteFile(logPath, nil, 0o644); err != nil {
			t.Fatalf("清空调用日志失败：%v", err)
		}
		scriptPath := filepath.Join(dir, "restore.sh")
		if err := os.WriteFile(scriptPath, []byte(OvsRestoreScript(ovsSnapshot())), 0o755); err != nil {
			t.Fatalf("写入恢复脚本失败：%v", err)
		}
		if out, err := exec.Command("sh", scriptPath).CombinedOutput(); err != nil {
			t.Fatalf("恢复脚本执行失败：%v\n%s", err, out)
		}
		calls, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("读取调用日志失败：%v", err)
		}
		return string(calls)
	}

	t.Run("变更多挂了一个端口", func(t *testing.T) {
		// 现场是 enp1s0 / enp2s0 / enp3s0，变更前只有前两个：只该删多出来的那个。
		calls := run(t, "enp1s0 enp2s0 enp3s0")
		if !strings.Contains(calls, "--if-exists del-port enp1s0-ovs enp3s0") {
			t.Fatalf("没有删掉多出来的网口：\n%s", calls)
		}
		if strings.Contains(calls, "del-port enp1s0-ovs enp1s0") ||
			strings.Contains(calls, "del-port enp1s0-ovs enp2s0") {
			t.Fatalf("删掉了变更前就存在的网口：\n%s", calls)
		}
		if !strings.Contains(calls, "--may-exist add-port enp1s0-ovs enp1s0") {
			t.Fatalf("没有为变更前存在的网口写回兜底：\n%s", calls)
		}
	})

	t.Run("变更摘掉了一个端口", func(t *testing.T) {
		// 现场只剩 enp1s0，变更前有两个：该把 enp2s0 加回来。
		calls := run(t, "enp1s0")
		if !strings.Contains(calls, "--may-exist add-port enp1s0-ovs enp2s0") {
			t.Fatalf("没有把被摘掉的网口加回来：\n%s", calls)
		}
		if strings.Contains(calls, "del-port enp1s0-ovs enp1s0") {
			t.Fatalf("误删了保留中的网口：\n%s", calls)
		}
	})

	t.Run("现场已经是变更前的样子", func(t *testing.T) {
		// 幂等：看门狗与手动回滚撞在一起时，第二次执行不能造成任何增删。
		calls := run(t, "enp1s0 enp2s0")
		if strings.Contains(calls, "del-port") {
			t.Fatalf("状态已一致时不该删任何端口：\n%s", calls)
		}
		if !strings.Contains(calls, "--may-exist add-port enp1s0-ovs enp1s0") {
			t.Fatalf("add-port 必须带 --may-exist，重复执行才不会报错：\n%s", calls)
		}
		// br-int 的唯一端口就在变更前的集合里，不能被别的网桥的端口列表牵连。
		if strings.Contains(calls, "del-port br-int") {
			t.Fatalf("跨网桥误伤了 br-int：\n%s", calls)
		}
	})
}

// TestOvsRestoreScriptRebuildsAndRemovesBridges 验证恢复脚本对网桥本身的处理。
//
// 建桥和删桥都超出了"对齐某座桥的端口"的范围：变更把一座桥删掉后回滚得把它
// 建回来，变更新建的桥回滚得删掉。而删桥是这个功能里最危险的一步 —— 删错了
// 会把目标机自己的网络服务建的桥一起带走，那台机器直接失联。
func TestOvsRestoreScriptRebuildsAndRemovesBridges(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	statePath := filepath.Join(dir, "state")

	// 假 ovs-vsctl：list-br / list-ports 读状态文件，get external-ids 只认
	// br-new 这个"面板建的"桥，其余一律返回空。
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "$OVS_LOG"
case "$*" in
  *list-br*) awk -F'|' '{print $1}' "$OVS_STATE" ;;
  *list-ports*)
    for last in "$@"; do :; done
    awk -v b="$last" -F'|' '$1==b {print $2}' "$OVS_STATE" 2>/dev/null | tr ' ' '\n'
    ;;
  *"get bridge br-new"*) echo '"1"' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ovs-vsctl"), []byte(fake), 0o755); err != nil {
		t.Fatalf("写入假 ovs-vsctl 失败：%v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("OVS_LOG", logPath)
	t.Setenv("OVS_STATE", statePath)

	run := func(t *testing.T, before *NetworkSnapshot, live string) string {
		t.Helper()
		if err := os.WriteFile(statePath, []byte(live), 0o644); err != nil {
			t.Fatalf("写入现场状态失败：%v", err)
		}
		if err := os.WriteFile(logPath, nil, 0o644); err != nil {
			t.Fatalf("清空调用日志失败：%v", err)
		}
		scriptPath := filepath.Join(dir, "restore.sh")
		if err := os.WriteFile(scriptPath, []byte(OvsRestoreScript(before)), 0o755); err != nil {
			t.Fatalf("写入恢复脚本失败：%v", err)
		}
		if out, err := exec.Command("sh", scriptPath).CombinedOutput(); err != nil {
			t.Fatalf("恢复脚本执行失败：%v\n%s", err, out)
		}
		calls, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("读取调用日志失败：%v", err)
		}
		return string(calls)
	}

	systemBridge := OvsBridge{Name: "br-sys", Ports: []OvsPort{{Name: "br-sys"}}}
	panelBridge := OvsBridge{Name: "br-old", Ports: []OvsPort{{Name: "enp3s0", Physical: true}}}

	t.Run("变更删掉了桥，要建回来", func(t *testing.T) {
		// 变更前有 br-old 与 br-sys，现场只剩 br-sys。
		before := &NetworkSnapshot{Ovs: &OvsState{
			Available: true, Bridges: []OvsBridge{panelBridge, systemBridge},
		}}
		calls := run(t, before, "br-sys|br-sys\n")

		if !strings.Contains(calls, "--may-exist add-br br-old") {
			t.Fatalf("没有把被删掉的网桥建回来：\n%s", calls)
		}
		if !strings.Contains(calls, "--may-exist add-port br-old enp3s0") {
			t.Fatalf("网桥建回来之后没有恢复它的端口：\n%s", calls)
		}
	})

	t.Run("变更新建了桥，要删掉", func(t *testing.T) {
		// 现场多出一座带面板标记的 br-new。
		before := &NetworkSnapshot{Ovs: &OvsState{
			Available: true, Bridges: []OvsBridge{panelBridge, systemBridge},
		}}
		calls := run(t, before, "br-old|enp3s0\nbr-sys|br-sys\nbr-new|br-new\n")

		if !strings.Contains(calls, "--if-exists del-br br-new") {
			t.Fatalf("没有删掉本次变更新建的网桥：\n%s", calls)
		}
		// 这是整个恢复脚本里最不能出错的一条：br-sys 是目标机自己的网络服务
		// 建的，删掉它那台机器就失联了。
		if strings.Contains(calls, "del-br br-sys") {
			t.Fatalf("删掉了变更前就存在的系统网桥：\n%s", calls)
		}
		if strings.Contains(calls, "del-br br-old") {
			t.Fatalf("删掉了变更前就存在的网桥：\n%s", calls)
		}
	})

	t.Run("变更前一座桥都没有", func(t *testing.T) {
		// 这是 existing 为空的分支：case 的模式退化成单个空格，匹配不上任何
		// 名字，于是现场每座桥都要过一遍标记检查。写成"没有就全删"会连
		// 系统桥一起清掉。
		before := &NetworkSnapshot{Ovs: &OvsState{Available: true, Bridges: []OvsBridge{}}}
		calls := run(t, before, "br-sys|br-sys\nbr-new|br-new\n")

		if !strings.Contains(calls, "--if-exists del-br br-new") {
			t.Fatalf("没有清理新建的网桥：\n%s", calls)
		}
		if strings.Contains(calls, "del-br br-sys") {
			t.Fatalf("变更前没有网桥时把系统网桥也当成新建的删了：\n%s", calls)
		}
	})
}

func TestOvsGuardCommandStructure(t *testing.T) {
	script := OvsGuardCommand("abc123", ovsSnapshot(), 120)
	files := OvsGuard("abc123")

	for _, want := range []string{
		"cat > " + files.Restore + " <<'PANELSSH_OVS_RESTORE_EOF'",
		"\nPANELSSH_OVS_RESTORE_EOF\n",
		"chmod 0700 " + files.Restore,
		// 残留的确认标记必须在上岗之前清掉，否则看门狗一醒来就以为已经确认过。
		"rm -f " + files.Confirm + " " + files.Watchdog,
		// 到点先看确认标记，没确认才恢复配置。
		"if [ ! -e " + files.Confirm + " ]; then sh " + files.Restore + "; fi",
		// 三个文件由看门狗自己收尾：确认与回滚都发生在主机可能正断连的时刻，
		// 指望面板清理必然在 /tmp 留下永远没人管的文件。
		"rm -f " + files.Confirm + " " + files.Restore + " " + files.Watchdog,
		"</dev/null >/dev/null 2>&1 &",
		"exit 1",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("保护命令缺少 %q：\n%s", want, script)
		}
	}

	// 结束标记必须落在恢复脚本之后、看门狗之前：写在中间会把后面的内容
	// 当成脚本文本，结果是"启动了一个什么都没干的看门狗"。
	end := strings.Index(script, "\nPANELSSH_OVS_RESTORE_EOF\n")
	if end < 0 {
		t.Fatalf("没有正确的 heredoc 结束标记：\n%s", script)
	}
	if setsid := strings.Index(script, "setsid"); setsid < end {
		t.Fatalf("setsid 落在 heredoc 内部：\n%s", script)
	}
	// 自检必须在看门狗之后：先写 pid 文件再检查，检查才有意义。
	if check := strings.Index(script, "看门狗进程未能启动"); check < strings.Index(script, "setsid") {
		t.Fatalf("启动自检落在了看门狗之前：\n%s", script)
	}
}

func TestOvsGuardFilesAreIsolatedPerChange(t *testing.T) {
	// 两次变更共用文件名意味着后一次会覆盖前一次的恢复脚本，
	// 前一次就再也回不去了。
	first := OvsGuard("aaaaaaaaaaaaaaaa")
	second := OvsGuard("bbbbbbbbbbbbbbbb")
	if first.Restore == second.Restore || first.Confirm == second.Confirm {
		t.Fatal("不同变更的保护文件不能共用路径")
	}
	if first.Restore == first.Confirm || first.Restore == first.Watchdog || first.Confirm == first.Watchdog {
		t.Fatal("同一次变更的三个文件路径不能相同")
	}
}

func TestOvsConfirmCommandKeepsTheMarker(t *testing.T) {
	files := OvsGuard("abc123")
	script := OvsConfirmCommand("abc123")

	if !strings.Contains(script, "touch "+files.Confirm) {
		t.Fatalf("确认命令必须写下确认标记：\n%s", script)
	}
	// 确认标记是让看门狗停手的唯一凭据。顺手把它删掉，等于反过来命令
	// 看门狗去恢复配置 —— 那会把一次成功的变更悄悄回滚掉。
	if strings.Contains(script, "rm -f "+files.Confirm) {
		t.Fatalf("确认命令不能删掉确认标记：\n%s", script)
	}
	// 恢复脚本也不能在这里删：看门狗要到点才醒，此刻它仍可能用得上。
	if strings.Contains(script, "rm -f") {
		t.Fatalf("确认命令不该删任何保护文件，收尾由看门狗统一负责：\n%s", script)
	}
}

func TestOvsRollbackCommandOrder(t *testing.T) {
	files := OvsGuard("abc123")
	script := OvsRollbackCommand("abc123")

	// 必须先写确认标记再跑恢复脚本：反过来的话看门狗可能在恢复进行到
	// 一半时也醒来跑一遍，两份恢复脚本并发操作同一个网桥会互相踩踏。
	touch := strings.Index(script, "touch "+files.Confirm)
	restore := strings.Index(script, "sh "+files.Restore)
	if touch < 0 || restore < 0 || touch > restore {
		t.Fatalf("回滚命令的顺序不对：\n%s", script)
	}
}

// ---------- 开机自恢复 ----------

func TestDesiredOvsState(t *testing.T) {
	desired := DesiredOvsState(ovsSnapshot().Ovs)

	// 只固化物理口：br-int 上只有一个内部口，它由 OVS 随网桥重建，
	// 面板不该去管，所以整座网桥都不该出现在期望状态里。
	if len(desired) != 1 || desired[0].Bridge != "enp1s0-ovs" {
		t.Fatalf("期望状态错误：%+v", desired)
	}
	if strings.Join(desired[0].Ports, " ") != "enp1s0 enp2s0" {
		t.Fatalf("期望端口错误：%+v", desired[0].Ports)
	}

	// 一份没有物理口的现状等于"面板不需要维持任何东西"，
	// 调用方据此把服务卸掉，所以这里必须是空而不是一个空网桥条目。
	none := &OvsState{Available: true, Bridges: []OvsBridge{
		{Name: "br-int", Ports: []OvsPort{{Name: "br-int"}}},
	}}
	if got := DesiredOvsState(none); len(got) != 0 {
		t.Fatalf("没有物理口时应当没有任何期望：%+v", got)
	}
	if got := DesiredOvsState(nil); len(got) != 0 {
		t.Fatalf("没有 OVS 现状时应当没有任何期望：%+v", got)
	}

	// 端口名同样要过白名单：它会以纯文本写进目标机上的配置文件。
	dirty := &OvsState{Available: true, Bridges: []OvsBridge{{
		Name:  "br0",
		Ports: []OvsPort{{Name: "eth1; reboot", Physical: true}},
	}}}
	if got := DesiredOvsState(dirty); len(got) != 0 {
		t.Fatalf("非法端口名不该进期望状态：%+v", got)
	}
}

func TestOvsPersistCommandStructure(t *testing.T) {
	script := OvsPersistCommand(DesiredOvsState(ovsSnapshot().Ovs))

	for _, want := range []string{
		"mkdir -p " + ovsConfigDir,
		"cat > " + ovsDesiredPath + " <<'PANELSSH_OVS_DESIRED_EOF'",
		"enp1s0-ovs enp1s0 enp2s0",
		"cat > " + ovsApplyPath + " <<'PANELSSH_OVS_APPLY_EOF'",
		"cat > " + ovsUnitPath + " <<'PANELSSH_OVS_UNIT_EOF'",
		"chmod 0755 " + ovsApplyPath,
		"systemctl daemon-reload",
		"systemctl enable " + ovsUnitName,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("固化命令缺少 %q：\n%s", want, script)
		}
	}

	// 三个 heredoc 必须依次闭合，且各自的结束标记只出现一次 ——
	// 顺序错了会把后面的内容当成上一段脚本文本，装上去的是一个残废的服务。
	for _, marker := range []string{
		"PANELSSH_OVS_DESIRED_EOF", "PANELSSH_OVS_APPLY_EOF", "PANELSSH_OVS_UNIT_EOF",
	} {
		if n := strings.Count(script, marker); n != 2 {
			t.Fatalf("结束标记 %s 出现 %d 次，应当正好一开一合：\n%s", marker, n, script)
		}
	}

	// 单元文件的 ExecStart 必须指向真正被写下去的那个脚本路径，
	// 两处各写一遍字符串、改一处忘一处，现象是开机时服务找不到可执行文件。
	if !strings.Contains(ovsUnit, "ExecStart="+ovsApplyPath) {
		t.Fatalf("单元的 ExecStart 与脚本落盘路径不一致：\n%s", ovsUnit)
	}
	// 采集侧判断"服务装没装"读的是 enable 生成的软链，路径由单元名决定，
	// 写死在采集脚本里，因此这里把它钉住。
	if !strings.Contains(networkCommand, ovsUnitName) {
		t.Fatal("采集脚本里的服务名与 ovsUnitName 不一致")
	}
}

func TestOvsApplyTimeoutCoversItsOwnWait(t *testing.T) {
	rounds := regexp.MustCompile(`ROUNDS="\$\{2:-(\d+)\}"`).FindStringSubmatch(ovsApplyScript)
	timeout := regexp.MustCompile(`TimeoutStartSec=(\d+)`).FindStringSubmatch(ovsUnit)
	if rounds == nil || timeout == nil {
		t.Fatalf("没有找到等待轮数或单元超时：\n%s\n%s", ovsApplyScript, ovsUnit)
	}
	roundsNum, err := strconv.Atoi(rounds[1])
	if err != nil {
		t.Fatalf("等待轮数不是数字：%q", rounds[1])
	}
	timeoutNum, err := strconv.Atoi(timeout[1])
	if err != nil {
		t.Fatalf("单元超时不是数字：%q", timeout[1])
	}

	// 脚本等满一轮是 2 秒。单元超时若盖不住它，systemd 会在脚本还在等网口
	// 就绪时把服务杀掉，现象是"服务失败、端口没补上"，而真正的原因藏在超时
	// 里 —— 这种错很难从现象反推回去，所以直接钉住两个数字的关系。
	if timeoutNum <= roundsNum*2 {
		t.Fatalf("单元超时 %d 秒盖不住脚本自身的等待上限 %d 秒", timeoutNum, roundsNum*2)
	}

	// 等待本身也必须有限：期望里留了一个再也回不来的网口时，这个上限
	// 决定这台机器每次开机要多等多久。
	if roundsNum > 60 {
		t.Fatalf("等待轮数 %d 太多，过期条目会把每次开机都拖住", roundsNum)
	}
}

// TestOvsScriptsAreValidShell 让 sh 自己把生成的脚本读一遍。
//
// 固化命令里嵌了三层 heredoc，每一层的结束标记都可能会写错位置；而这类错误
// 在字符串断言下是看不出来的 —— 断言只能确认"该有的都在"，确认不了"整体
// 还是不是一段合法的 shell"。用 sh -n 只做语法检查、不执行任何东西。
func TestOvsScriptsAreValidShell(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		{"固化开机自恢复", OvsPersistCommand(DesiredOvsState(ovsSnapshot().Ovs))},
		{"卸载开机自恢复", OvsUnpersistCommand()},
		{"建立变更保护", OvsGuardCommand("abc123", ovsSnapshot(), 120)},
		{"执行恢复脚本", OvsRollbackCommand("abc123")},
		{"写恢复脚本", OvsRestoreScript(ovsSnapshot())},
		{"开机对齐脚本", ovsApplyScript},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// -n 只解析不执行，因此 systemctl / ovs-vsctl 不会被真的调用。
			if out, err := exec.Command("sh", "-n", "-c", tc.script).CombinedOutput(); err != nil {
				t.Fatalf("生成的脚本不是合法 shell：%v\n%s\n---\n%s", err, out, tc.script)
			}
		})
	}
}

// TestCollectCommandsAreValidShell 让 sh 把采集脚本各读一遍。
//
// 采集脚本是一整段拼出来的 shell，里面掺着 if / elif / for / case 的嵌套，
// 手写很容易漏掉一个 fi —— 而那种错在字符串断言下看不出来（该有的片段都在，
// 只是整段不再合法），现象却是整次网络采集全部落空。
func TestCollectCommandsAreValidShell(t *testing.T) {
	cases := map[string]string{
		"网络信息":     NetworkCommand(),
		"Open vSwitch": OvsCommand(),
		"运行指标":     MetricsCommand(),
		"指标流式采集": StreamingMetricsCommand(1, 30),
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
				t.Fatalf("采集脚本不是合法 shell：%v\n%s", err, out)
			}
		})
	}
}

// TestOvsCommandReportsUnreadableWhenToolMissing 真的跑一遍提权后的情形。
//
// 提权会把 PATH 换成 root 的那一份（su 的登录环境），
// ovs-vsctl 未必在里面。这时脚本必须明确报"读不到"：如果它什么都不说，
// 识别层看到的就只是一份空的网桥列表，界面于是宣称"这台机器还没有网桥" ——
// 用户会去目标机上找一座明明就在那儿的桥，排查方向从一开始就是错的。
func TestOvsCommandReportsUnreadableWhenToolMissing(t *testing.T) {
	// 先取 sh 的绝对路径：下面的 PATH 会被清空，届时 LookPath 就找不到它了。
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("本机没有 sh，跳过")
	}
	t.Setenv("PATH", t.TempDir())

	out, err := exec.Command(shell, "-c", OvsCommand()).CombinedOutput()
	if err != nil {
		t.Fatalf("采集脚本执行失败：%v\n%s", err, out)
	}
	if strings.TrimSpace(splitSections(string(out))[secOvsUnread]) != "yes" {
		t.Fatalf("PATH 里没有 ovs-vsctl 时应当报读不到，实际输出：\n%s", out)
	}
	// 光说"读不到"还不够 —— 主采集（普通用户的 PATH）明明找到了 ovs-vsctl，
	// 而这里没找到，这个矛盾正是用户需要看到的那句话。
	if strings.TrimSpace(splitSections(string(out))[secOvsReason]) == "" {
		t.Fatalf("命令找不到时应当说明原因，实际输出：\n%s", out)
	}
}

// TestHasOvsTool 验证"要不要再去采一次 OVS"这个判断的判据。
//
// 判错的代价是不对称的：判成"有"只是白跑一次提权采集，判成"没有"则会让
// 一台装着 OVS 的机器在界面上彻底看不到 OVS —— 后者正是这个功能存在的理由。
func TestHasOvsTool(t *testing.T) {
	raw := "###TOOLS\nip\nnmcli\novs-vsctl\nss\n###LINK\n[]\n"
	if !HasOvsTool(raw) {
		t.Fatal("工具列表里有 ovs-vsctl，应当判定为装了 OVS")
	}

	// 判据只看 TOOLS 一节：别的节里出现同样的字符串不算数 ——
	// 例如某个网桥恰好叫这个名字，那不代表这台机器装了 ovs-vsctl。
	notTool := "###TOOLS\nip\nss\n###OVS\novs-vsctl ovs-vsctl\n"
	if HasOvsTool(notTool) {
		t.Fatal("OVS 那一节里出现 ovs-vsctl 不代表装了它")
	}

	for _, empty := range []string{"", "###TOOLS\nip\nss\n", "###LINK\n[]\n"} {
		if HasOvsTool(empty) {
			t.Fatalf("采集结果 %q 不该被判为装了 OVS", empty)
		}
	}
}

// TestBuildOvsStateUnreadable 验证"读不到"和"没有网桥"被分成两件事。
//
// 这是这次修复的核心。ovs-vsctl 读的是 OVS 自己的数据库，没有 root 或
// openvswitch 组权限时 list-br 会直接失败；失败被吞掉之后，输出里剩下的
// 空列表和"真的没有网桥"一模一样，界面就只能拿后者去描述前者 ——
// 用户会对着自己明明建好的桥反复琢磨是哪里没打开。
func TestBuildOvsStateUnreadable(t *testing.T) {
	tools := []string{"ip", "ovs-vsctl"}

	// 能读，只是确实还没有网桥。
	empty := buildOvsState(ovsSections("", "", ""), nil, tools, nil)
	if !empty.Available || empty.Unreadable {
		t.Fatalf("能读但没有网桥，不该标成读不到：%+v", empty)
	}

	// 找得到 ovs-vsctl 却读不到：和上面是完全不同的事实。
	sections := ovsSections("", "", "")
	sections[secOvsUnread] = "yes"
	unread := buildOvsState(sections, nil, tools, nil)
	if !unread.Unreadable {
		t.Fatal("采集报了读不到，状态里却没有这个标记")
	}
	if len(unread.Bridges) != 0 {
		t.Fatalf("读不到时不该凭空给出网桥：%+v", unread.Bridges)
	}

	// 压根没装 ovs-vsctl：既不是"可用"，也谈不上"读不到"。
	noTool := buildOvsState(sections, nil, []string{"ip"}, nil)
	if noTool.Available || noTool.Unreadable {
		t.Fatalf("没装 ovs-vsctl 时不该是可用或读不到：%+v", noTool)
	}

	// 采集彻底跑不完时，面板给的就是这一小段合成输出（见 OvsUnreadableOutput）。
	// 它必须落进上面同一条路径 —— 否则识别提权失败这件事会在最后一步失效，
	// 界面又退回成"这台机器还没有网桥"。
	synthetic := buildOvsState(
		splitSections(OvsUnreadableOutput("su: Authentication failure")), nil, tools, nil)
	if !synthetic.Available || !synthetic.Unreadable || len(synthetic.Bridges) != 0 {
		t.Fatalf("合成的“读不到”输出没有被正确解析：%+v", synthetic)
	}
	if synthetic.UnreadableReason != "su: Authentication failure" {
		t.Fatalf("合成的原因没有被解析出来：%q", synthetic.UnreadableReason)
	}

	// 原因在现场可能是好几行（采集收的是 stderr 原文），而界面上那条警告只放得
	// 下一句话 —— 取第一个非空行，也就是错误本身。
	multi := ovsSections("", "", "")
	multi[secOvsUnread] = "yes"
	multi[secOvsReason] = "\n\novs-vsctl: database connection failed\n请确认 OVS 服务在运行\n"
	if got := buildOvsState(multi, nil, tools, nil); got.UnreadableReason != "ovs-vsctl: database connection failed" {
		t.Fatalf("多行的原因没有被收敛成第一行：%q", got.UnreadableReason)
	}

	// 拿不到原因时留空串：界面据此区分"有原因可显示"和"只能说通用的话"。
	noReason := ovsSections("", "", "")
	noReason[secOvsUnread] = "yes"
	if got := buildOvsState(noReason, nil, tools, nil); got.UnreadableReason != "" {
		t.Fatalf("没有原因时应当留空：%q", got.UnreadableReason)
	}
}

func TestOvsPersistCommandUnpersistsWhenNothingToKeep(t *testing.T) {
	// 期望状态为空时不该留下一个什么都不做的常驻服务。
	script := OvsPersistCommand(nil)
	if script != OvsUnpersistCommand() {
		t.Fatalf("没有可维持的端口时应当卸载服务，实际：\n%s", script)
	}

	// 必须先 disable 再删单元文件：反过来的话 systemd 会留下一个指向
	// 已消失单元的软链，每次开机报一次 failed。
	disable := strings.Index(script, "systemctl disable")
	remove := strings.Index(script, "rm -f")
	if disable < 0 || remove < 0 || disable > remove {
		t.Fatalf("卸载命令的顺序不对：\n%s", script)
	}
	for _, path := range []string{ovsUnitPath, ovsApplyPath, ovsDesiredPath} {
		if !strings.Contains(script, path) {
			t.Fatalf("卸载命令没有清掉 %s：\n%s", path, script)
		}
	}
}

func TestOvsApplyScriptNeverRemovesPorts(t *testing.T) {
	// 这是开机脚本与变更保护用的恢复脚本最关键的区别，也是它最不能出错的地方：
	// 网桥上还挂着目标机自己的网络服务建的上行口，开机脚本一旦会删端口，
	// 就有可能在重启后把上行口摘掉，那台机器直接失联。
	//
	// 这条性质不是靠断言"没删对端口"来保证的，而是靠脚本里根本没有删除命令。
	if strings.Contains(ovsApplyScript, "del-port") {
		t.Fatalf("开机脚本里出现了 del-port：\n%s", ovsApplyScript)
	}
	// 恢复脚本则必须能删 —— 两次变更之间被摘掉的端口得靠它加回来，
	// 而变更多挂上去的端口得靠它删掉。两者语义不同，不能合并。
	if !strings.Contains(OvsRestoreScript(ovsSnapshot()), "del-port") {
		t.Fatal("变更保护用的恢复脚本必须能删掉多余的端口")
	}
}

func TestOvsApplyScriptActuallyApplies(t *testing.T) {
	// 真的跑一遍：确认它补齐了期望里的网口、跳过了不存在的网口与网桥。
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")

	// 假 ovs-vsctl：只认 br0 存在，其余网桥一律不存在；记录每次调用。
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "$OVS_LOG"
case "$*" in
  *br-exists*br0*) exit 0 ;;
  *br-exists*) exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ovs-vsctl"), []byte(fake), 0o755); err != nil {
		t.Fatalf("写入假 ovs-vsctl 失败：%v", err)
	}

	// lo 是本机一定存在的网口，用它验证"网口存在就补上"；
	// 另一个名字故意不存在，验证"网口还没就绪就跳过而不是硬挂"。
	conf := filepath.Join(dir, "desired.conf")
	body := "br0 lo panel-nonexistent\nbr-absent lo\n"
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatalf("写入期望状态失败：%v", err)
	}

	scriptPath := filepath.Join(dir, "apply.sh")
	if err := os.WriteFile(scriptPath, []byte(ovsApplyScript), 0o755); err != nil {
		t.Fatalf("写入开机脚本失败：%v", err)
	}

	// 期望里故意留了不存在的东西（一个网口、一座网桥），所以等待条件
	// 永远不会成立。第二个参数把它压成 2 轮，用例才不会真等满 60 秒 ——
	// 顺带也验证了"等待用尽之后照样把能补的补上"这条退路。
	cmd := exec.Command("sh", scriptPath, conf, "2")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+":"+os.Getenv("PATH"),
		"OVS_LOG="+logPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("开机脚本执行失败：%v\n%s", err, out)
	}

	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取调用日志失败：%v", err)
	}
	log := string(calls)

	if !strings.Contains(log, "--may-exist add-port br0 lo") {
		t.Fatalf("期望里的网口没有被补上：\n%s", log)
	}
	if strings.Contains(log, "panel-nonexistent") {
		t.Fatalf("网口还不存在时不该去挂它：\n%s", log)
	}
	if strings.Contains(log, "add-port br-absent") {
		t.Fatalf("网桥还不存在时不该去挂端口：\n%s", log)
	}
	if strings.Contains(log, "del-port") {
		t.Fatalf("开机脚本不该删任何端口：\n%s", log)
	}
}

// TestOvsGuardLifecycle 真的把保护装置跑一遍。
//
// 前面几个用例都只看生成的字符串，而这一整套的说服力全在"到点真的会自己
// 恢复""确认过就真的不动手""跑完不留下垃圾"这三件事上 —— 它们只能靠真执行
// 来验证。这里用的是目标机上真实的 /tmp 路径，因为保护文件本来就该落在那儿。
func TestOvsGuardLifecycle(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("本机没有 setsid，跳过看门狗生命周期测试")
	}

	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	statePath := filepath.Join(dir, "state")

	// 假 ovs-vsctl 只做两件事：记下每次调用；list-ports 时报告"变更后"的端口。
	fake := `#!/bin/sh
printf '%s\n' "$*" >> "$OVS_LOG"
case "$*" in
  *list-ports*) echo enp1s0; echo enp2s0; echo enp3s0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ovs-vsctl"), []byte(fake), 0o755); err != nil {
		t.Fatalf("写入假 ovs-vsctl 失败：%v", err)
	}
	if err := os.WriteFile(statePath, nil, 0o644); err != nil {
		t.Fatalf("初始化状态文件失败：%v", err)
	}

	// 每次用一个新的变更 ID，并保证测试结束后清理 —— 保护文件落在真实的
	// /tmp 下，这是目标机上的实际行为，测试不该为此改用另一套路径。
	seq := 0
	newID := func() string {
		seq++
		id := "fffffffffffff" + string(rune('a'+seq))
		t.Cleanup(func() {
			files := OvsGuard(id)
			for _, path := range []string{files.Confirm, files.Restore, files.Watchdog} {
				_ = os.Remove(path)
			}
		})
		return id
	}

	runGuard := func(t *testing.T, id string, timeoutSec int) {
		t.Helper()
		cmd := exec.Command("sh", "-c", OvsGuardCommand(id, ovsSnapshot(), timeoutSec))
		cmd.Env = append(os.Environ(),
			"PATH="+dir+":"+os.Getenv("PATH"),
			"OVS_LOG="+logPath,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("建立保护失败：%v\n%s", err, out)
		}
		files := OvsGuard(id)
		if _, err := os.Stat(files.Restore); err != nil {
			t.Fatalf("恢复脚本没有落到目标机上：%v", err)
		}
	}

	readCalls := func(t *testing.T) string {
		t.Helper()
		calls, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("读取调用日志失败：%v", err)
		}
		return string(calls)
	}

	assertCleanedUp := func(t *testing.T, id string) {
		t.Helper()
		files := OvsGuard(id)
		for name, path := range map[string]string{
			"恢复脚本": files.Restore, "确认标记": files.Confirm, "看门狗 pid": files.Watchdog,
		} {
			if _, err := os.Stat(path); err == nil {
				t.Fatalf("%s 未被清理：%s", name, path)
			}
		}
	}

	t.Run("窗口内没有确认，到点自动恢复", func(t *testing.T) {
		id := newID()
		if err := os.WriteFile(logPath, nil, 0o644); err != nil {
			t.Fatalf("清空日志失败：%v", err)
		}
		runGuard(t, id, 2)

		// 这是整个机制的底线能力：没有面板、没有任何人再发指令，
		// 目标机也必须自己把配置恢复回去。
		time.Sleep(4 * time.Second)

		calls := readCalls(t)
		if !strings.Contains(calls, "--if-exists del-port enp1s0-ovs enp3s0") {
			t.Fatalf("看门狗到点没有执行恢复脚本：\n%s", calls)
		}
		assertCleanedUp(t, id)
	})

	t.Run("窗口内确认过，到点什么都不做", func(t *testing.T) {
		id := newID()
		if err := os.WriteFile(logPath, nil, 0o644); err != nil {
			t.Fatalf("清空日志失败：%v", err)
		}
		runGuard(t, id, 2)

		// 确认就是面板那一侧唯一要做的事。
		confirm := exec.Command("sh", "-c", OvsConfirmCommand(id))
		if out, err := confirm.CombinedOutput(); err != nil {
			t.Fatalf("写确认标记失败：%v\n%s", err, out)
		}

		time.Sleep(4 * time.Second)

		// 确认过还去动配置，等于把一次成功的变更悄悄回滚掉。
		if calls := readCalls(t); strings.Contains(calls, "del-port") || strings.Contains(calls, "add-port") {
			t.Fatalf("已确认的变更仍被看门狗改动了：\n%s", calls)
		}
		assertCleanedUp(t, id)
	})
}
