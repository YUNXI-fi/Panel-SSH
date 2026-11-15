package api

import (
	"testing"
	"time"

	"panelssh/internal/sysinfo"
)

func TestParseIDFilter(t *testing.T) {
	// 没传与传了空集合必须区分：前者是"要全部主机"，后者是"一台都不要"。
	if got := parseIDFilter(""); got != nil {
		t.Fatalf("空串应当表示不过滤，实际 %v", got)
	}
	if got := parseIDFilter("   "); got != nil {
		t.Fatalf("全空白应当表示不过滤，实际 %v", got)
	}

	got := parseIDFilter(" 3,7 ,,9 ")
	want := map[int64]bool{3: true, 7: true, 9: true}
	if len(got) != len(want) {
		t.Fatalf("解析结果 = %v，期望 %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("应当包含 %d，实际 %v", id, got)
		}
	}

	// 非法项被跳过而不是让整批失败：前端拼串时多一个逗号不该导致整页没有指标。
	// 0 是宿主机的保留 ID，属于合法目标，不能跟着非法项一起丢。
	if got := parseIDFilter("0,-1,abc,5"); len(got) != 2 || !got[0] || !got[5] {
		t.Fatalf("非法项应当被跳过、宿主机应保留，实际 %v", got)
	}
}

func TestBriefOfMetrics(t *testing.T) {
	metrics := &sysinfo.Metrics{
		CPUPercent: 37.5,
		DeltaReady: true,
		MemUsed:    1024,
		MemTotal:   4096,
		Disks: []sysinfo.Disk{
			{MountPoint: "/boot", Total: 512, Used: 100, UsePercent: 19.5},
			{MountPoint: "/", Total: 10240, Used: 2048, UsePercent: 20},
		},
		SampledAt: time.Unix(1700000000, 0),
	}

	brief := briefOfMetrics(7, metrics)

	if brief.ID != 7 {
		t.Errorf("ID = %d，期望 7", brief.ID)
	}
	if brief.CPUPercent == nil || *brief.CPUPercent != 37.5 {
		t.Errorf("CPU = %v，期望 37.5", brief.CPUPercent)
	}
	if brief.MemPercent == nil || *brief.MemPercent != 25 {
		t.Errorf("内存百分比 = %v，期望 25", brief.MemPercent)
	}
	if brief.MemUsed != 1024 || brief.MemTotal != 4096 {
		t.Errorf("内存明细 = %d/%d，期望 1024/4096", brief.MemUsed, brief.MemTotal)
	}
	// 存储取根挂载点，而不是列表里的第一个（/boot 是干扰项）。
	if brief.DiskPercent == nil || *brief.DiskPercent != 20 {
		t.Errorf("存储百分比 = %v，期望取根挂载点的 20", brief.DiskPercent)
	}
	if brief.DiskTotal != 10240 {
		t.Errorf("存储总量 = %d，期望取根挂载点的 10240", brief.DiskTotal)
	}
}

// 首次采样算不出 CPU%：那是"算不出来"，不是"空闲"，必须留空而不是给 0。
func TestBriefOfMetricsMarksUnknownAsNull(t *testing.T) {
	brief := briefOfMetrics(1, &sysinfo.Metrics{DeltaReady: false, MemTotal: 0})

	if brief.CPUPercent != nil {
		t.Errorf("deltaReady=false 时 CPU 应为 null，实际 %v", *brief.CPUPercent)
	}
	if brief.MemPercent != nil {
		t.Errorf("没有内存总量时内存百分比应为 null，实际 %v", *brief.MemPercent)
	}
	if brief.DiskPercent != nil {
		t.Errorf("没有磁盘信息时存储百分比应为 null，实际 %v", *brief.DiskPercent)
	}
}

func TestRootDisk(t *testing.T) {
	// 优先根挂载点。
	disk, ok := rootDisk([]sysinfo.Disk{
		{MountPoint: "/data", Total: 100},
		{MountPoint: "/", Total: 200},
	})
	if !ok || disk.MountPoint != "/" {
		t.Fatalf("应当优先选根挂载点，实际 %+v（%v）", disk, ok)
	}

	// 没有根挂载点时退回第一个可用的。
	disk, ok = rootDisk([]sysinfo.Disk{
		{MountPoint: "/data", Total: 100},
	})
	if !ok || disk.MountPoint != "/data" {
		t.Fatalf("应当退回第一个可用挂载点，实际 %+v（%v）", disk, ok)
	}

	// 全是零容量（采集不到 df）时如实报"没有"，而不是拿一个 0 容量去算百分比。
	if _, ok := rootDisk([]sysinfo.Disk{{MountPoint: "/", Total: 0}}); ok {
		t.Error("零容量不应被视为可用")
	}
	if _, ok := rootDisk(nil); ok {
		t.Error("空列表不应被视为可用")
	}
}

func TestClampPercent(t *testing.T) {
	// 不同内核对同一块盘的统计口径不一致，偶尔会略超 100；
	// 不夹紧的话进度条会画出格子外面。
	if got := clampPercent(120.5); got != 100 {
		t.Errorf("clampPercent(120.5) = %v，期望 100", got)
	}
	if got := clampPercent(-3); got != 0 {
		t.Errorf("clampPercent(-3) = %v，期望 0", got)
	}
	if got := clampPercent(42); got != 42 {
		t.Errorf("clampPercent(42) = %v，期望 42", got)
	}
}
