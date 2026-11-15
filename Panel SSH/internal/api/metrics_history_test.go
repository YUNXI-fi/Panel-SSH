package api

import (
	"testing"
	"time"

	"panelssh/internal/sysinfo"
)

func testMetrics(cpu float64) *sysinfo.Metrics {
	return &sysinfo.Metrics{
		DeltaReady:   true,
		CPUPercent:   cpu,
		MemUsed:      4 << 30,
		MemTotal:     16 << 30,
		NetRxRate:    1024,
		DiskReadRate: 2048,
		Accelerators: []sysinfo.Accelerator{{Kind: "gpu", ID: 0, UtilPercent: -1}},
	}
}

// 历史缓冲是"开窗回填"的唯一数据来源：容量、顺序、按主机隔离任一写错，
// 曲线就会对不上时间轴，或者把别的机器的数据画进来。
func TestMetricsHistoryKeepsRecentPointsPerHost(t *testing.T) {
	base := time.Unix(1700000000, 0)
	h := newMetricsHistory()

	for i := 0; i < metricsHistorySize+5; i++ {
		h.record(7, testMetrics(50), base.Add(time.Duration(i)*3*time.Second))
	}
	h.record(9, testMetrics(10), base)

	points := h.list(7)
	if len(points) != metricsHistorySize {
		t.Fatalf("应只保留最近 %d 个点，得到 %d", metricsHistorySize, len(points))
	}
	// 留下的必须是最新的那批：最早一个点应当是第 5 次采样的时刻。
	wantFirst := base.Add(5 * 3 * time.Second).UnixMilli()
	if points[0].At != wantFirst {
		t.Fatalf("淘汰的是最新点而不是最旧点：首个点 %d，期望 %d", points[0].At, wantFirst)
	}
	for i := 1; i < len(points); i++ {
		if points[i].At <= points[i-1].At {
			t.Fatalf("时间必须正序递增：第 %d 个点 %d 不大于前一个 %d", i, points[i].At, points[i-1].At)
		}
	}
	if other := h.list(9); len(other) != 1 {
		t.Fatalf("主机之间必须互不影响，另一台机器得到 %d 个点", len(other))
	}
	if got := points[len(points)-1].MemPercent; got != 25 {
		t.Fatalf("内存百分比口径应为 4/16=25，得到 %v", got)
	}
}

// list 返回快照：后续采样不能改写调用方已经拿到的那份。
func TestMetricsHistoryListIsSnapshot(t *testing.T) {
	h := newMetricsHistory()
	h.record(1, testMetrics(30), time.Unix(1700000000, 0))

	snapshot := h.list(1)
	h.record(1, testMetrics(60), time.Unix(1700000003, 0))

	if len(snapshot) != 1 || snapshot[0].CPUPercent != 30 {
		t.Fatalf("快照被后续采样改写了：%+v", snapshot)
	}
	if len(h.list(1)) != 2 {
		t.Fatal("新采样没有进缓冲")
	}
}

// 删主机/改凭据后必须丢掉旧曲线，否则会把旧机器的趋势当成新配置的现状。
func TestMetricsHistoryForget(t *testing.T) {
	h := newMetricsHistory()
	h.record(3, testMetrics(20), time.Unix(1700000000, 0))
	h.forget(3)

	if points := h.list(3); len(points) != 0 {
		t.Fatalf("forget 之后不应再有数据，得到 %d 个点", len(points))
	}
}

// 工具没给利用率时是 -1（未知），必须原样带出去：前端据此断线，
// 若在这里被折成 0，曲线上会凭空多出一段"设备空闲"。
func TestMetricsHistoryKeepsUnknownAccelUtil(t *testing.T) {
	h := newMetricsHistory()
	h.record(5, testMetrics(10), time.Unix(1700000000, 0))

	points := h.list(5)
	if len(points) != 1 || len(points[0].Accel) != 1 {
		t.Fatalf("加速设备没有记入历史：%+v", points)
	}
	if got := points[0].Accel[0].UtilPercent; got != -1 {
		t.Fatalf("未知利用率应保留为 -1，得到 %v", got)
	}
}
