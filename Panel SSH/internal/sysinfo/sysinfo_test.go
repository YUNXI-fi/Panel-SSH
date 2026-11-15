package sysinfo

import (
	"strings"
	"testing"
)

// TestMetricsCommandCoversAllSections 保证拆分后的全量脚本仍然包含每一个分节。
//
// 采集脚本被拆成轻量与重量两段后，"漏掉某一节"不再表现为编译错误或明显失败 ——
// 它表现为某个指标静默归零（磁盘容量恒为 0、加速器列表为空）。那种故障在界面上
// 只是少了几块内容，很难反查到脚本拼接这一行。
func TestMetricsCommandCoversAllSections(t *testing.T) {
	full := MetricsCommand()
	for _, section := range []string{
		secUptime, secLoadavg, secStat, secMeminfo, secNetdev, secCPUInfo,
		secUname, secHostname, secDiskstat, secBlockDev, secDF, secNVIDIA, secNPU,
	} {
		if !strings.Contains(full, section) {
			t.Errorf("全量采集脚本缺少分节 %s", section)
		}
	}
}

// TestStreamingMetricsCommandShape 校验流式脚本的三条硬约束。
//
// 这三条都不是"风格问题"：少一个轮次标记，消费方永远切不出完整的一轮；间隔没传对，
// 档位就是假的；重量分节漏进循环体，每秒跑一次 df 与 nvidia-smi 会比轮询更费目标机。
func TestStreamingMetricsCommandShape(t *testing.T) {
	script := StreamingMetricsCommand(0.2, 30)

	if !strings.Contains(script, "echo '"+MetricsTickMarker+"'") {
		t.Fatal("流式脚本必须每轮输出轮次标记")
	}
	if !strings.Contains(script, "sleep 0.2") {
		t.Fatalf("间隔应原样传给 sleep，实际脚本：\n%s", script)
	}
	if !strings.Contains(script, "if [ $((i % 30)) -eq 0 ]; then") {
		t.Fatal("重量分节必须被降频到每 30 轮一次")
	}

	// heavyEvery=0 表示调用方自己另行获取磁盘与加速器信息，脚本里不该再出现重量命令。
	light := StreamingMetricsCommand(1, 0)
	if strings.Contains(light, "df -P -k") || strings.Contains(light, "nvidia-smi") {
		t.Fatal("heavyEvery=0 时脚本不应包含重量分节")
	}
}

// sampleDFChinese 是中文 locale 下的真实 df -P -k 输出。
//
// 这台机器上 udev 被挂到 /dev、tmpfs 挂在 /run 与 /tmp、efivarfs 挂在 /sys 下，
// 还有一块 snap 的 loop 设备 —— 它们都不是真实存储，必须被过滤掉。
const sampleDFChinese = `文件系统        1024的块     已用      可用 容量 挂载点
udev            16223856        0  16223856   0% /dev
tmpfs            3263248     1892   3261356   1% /run
/dev/nvme0n1p2 236019592 61350528 162607164  28% /
tmpfs           16316228        0  16316228   0% /dev/shm
efivarfs             512      102       406  20% /sys/firmware/efi/efivars
tmpfs               5120        8      5112   1% /run/lock
tmpfs           16777216  2167548  14609668  13% /tmp
/dev/nvme0n1p1    997456    21320    976136   3% /boot/efi
/dev/nvme1n1p1 250048512 38575104 211473408  16% /media/yunxi/FE73-D3B5
/dev/loop0         128000   128000         0 100% /snap/core/1234
`

func TestParseDFChineseLocale(t *testing.T) {
	disks := parseDF(sampleDFChinese)

	// 只剩三块真实存储。udev、tmpfs、efivarfs、snap 的 loop 设备都必须消失：
	// 它们的容量对"这台机器还有多少空间"这个问题没有任何意义。
	want := []struct {
		filesystem string
		mountPoint string
	}{
		{"/dev/nvme0n1p2", "/"},
		{"/dev/nvme0n1p1", "/boot/efi"},
		{"/dev/nvme1n1p1", "/media/yunxi/FE73-D3B5"},
	}
	if len(disks) != len(want) {
		t.Fatalf("保留的挂载点数量错误：%+v", disks)
	}
	for i, item := range want {
		if disks[i].Filesystem != item.filesystem || disks[i].MountPoint != item.mountPoint {
			t.Fatalf("第 %d 项错误：%+v，期望 %s → %s", i, disks[i], item.filesystem, item.mountPoint)
		}
	}

	// 中文表头那一行必须被跳过：它靠列结构（第二列不是纯数字）被识别，
	// 而不是靠 "Filesystem" 这个英文词。
	for _, disk := range disks {
		if disk.Filesystem == "文件系统" {
			t.Fatal("中文表头被当成了数据行")
		}
	}

	// 单位换算：df -k 的块是 1024 字节。
	if disks[0].Total != 236019592*1024 {
		t.Fatalf("容量换算错误：%d", disks[0].Total)
	}
	if disks[0].UsePercent != 28 {
		t.Fatalf("使用率解析错误：%v", disks[0].UsePercent)
	}
}

func TestParseDFEnglishLocale(t *testing.T) {
	// 英文 locale 的表头同样要跳过。它由正则自然挡住，不依赖任何关键字匹配。
	const english = `Filesystem     1024-blocks     Used Available Capacity Mounted on
udev               16223856        0  16223856       0% /dev
/dev/sda1         236019592 61350528 162607164      28% /
`
	disks := parseDF(english)
	if len(disks) != 1 || disks[0].MountPoint != "/" {
		t.Fatalf("英文表头未被正确跳过：%+v", disks)
	}
}

func TestParseDFMountPointWithSpace(t *testing.T) {
	// 挂载点可以含空格，这也是当初改用正则整体匹配的原因。
	const raw = `/dev/sdb1  102400000  50000000  50000000  50%  /mnt/my disk
`
	disks := parseDF(raw)
	if len(disks) != 1 || disks[0].MountPoint != "/mnt/my disk" {
		t.Fatalf("含空格的挂载点解析错误：%+v", disks)
	}
}

func TestParseDFKeepsRealStorageOutsideSystemAreas(t *testing.T) {
	// 反过来也要成立：挂在数据目录下的真实存储不能被误杀。
	// /srv 与 /data 不在受管区域里，必须保留。
	const raw = `/dev/sdb1  102400000  50000000  50000000  50%  /srv
/dev/sdc1  102400000  50000000  50000000  50%  /data
/dev/sdd1  102400000  50000000  50000000  50%  /mnt/backup
`
	disks := parseDF(raw)
	if len(disks) != 3 {
		t.Fatalf("真实存储被误过滤：%+v", disks)
	}
}

func TestIsVirtualFilesystem(t *testing.T) {
	cases := []struct {
		source string
		target string
		want   bool
	}{
		// 挂载在受管区域：与源的名字无关，只看位置。
		{"udev", "/dev", true},
		{"tmpfs", "/dev/shm", true},
		{"tmpfs", "/run/lock", true},
		{"efivarfs", "/sys/firmware/efi/efivars", true},
		{"systemd-1", "/proc/sys/fs/binfmt_misc", true},
		{"/dev/loop0", "/snap/core/1234", true},
		// 类型名兜底：挂载点不在受管区域，但源就是伪文件系统的类型名。
		{"tmpfs", "/tmp", true},
		{"squashfs", "/opt/image", true},
		// 真实存储。
		{"/dev/nvme0n1p2", "/", false},
		{"/dev/sdb1", "/mnt/backup", false},
		{"/dev/sdc1", "/data", false},
		{"UUID=abcd-1234", "/srv", false},
		// 名字里带 dev 但不是 /dev 这一级，不该被前缀误伤。
		{"/dev/sda1", "/development", false},
	}

	for _, tc := range cases {
		if got := isVirtualFilesystem(tc.source, tc.target); got != tc.want {
			t.Fatalf("isVirtualFilesystem(%q, %q) = %v，期望 %v", tc.source, tc.target, got, tc.want)
		}
	}
}
