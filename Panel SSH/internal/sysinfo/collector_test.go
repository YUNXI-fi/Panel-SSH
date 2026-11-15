package sysinfo

import (
	"testing"
	"time"
)

// 采样间隔决定"能不能算出速率"：监控窗口提供 200ms 的「高速」档，
// 一旦最小间隔被调回 0.5 秒，CPU / 网络 / 磁盘三条线会整体退化成"无数据"——
// 而这种故障在界面上只表现为"曲线是空的"，很难反查到采集器这一行判断上。
func TestCollectorAcceptsHighSpeedInterval(t *testing.T) {
	host := int64(1)
	base := time.Unix(1700000000, 0)
	collector := NewCollector()

	first, err := collector.Collect(host, rawMetrics("cpu  100 0 50 3000 50 0 0 0"), base)
	if err != nil {
		t.Fatalf("首次采集失败：%v", err)
	}
	if first.DeltaReady {
		t.Fatal("首次采样没有基准，不该给出速率")
	}

	second, err := collector.Collect(host, rawMetrics("cpu  140 0 60 3400 50 0 0 0"), base.Add(200*time.Millisecond))
	if err != nil {
		t.Fatalf("第二次采集失败：%v", err)
	}
	if !second.DeltaReady {
		t.Fatal("200ms 的间隔应当能算出速率，否则高速档下速率曲线全是空的")
	}
	// 差值 450 tick、其中忙 100 tick。
	if second.CPUPercent <= 0 || second.CPUPercent >= 100 {
		t.Fatalf("CPU%% 应是 0 与 100 之间的正数，得到 %v", second.CPUPercent)
	}

	// 挨得太近的两次采样（低于最小间隔）仍应拒绝给速率：那种差值里只有噪声。
	close, err := collector.Collect(host, rawMetrics("cpu  142 0 60 3401 50 0 0 0"), base.Add(210*time.Millisecond))
	if err != nil {
		t.Fatalf("第三次采集失败：%v", err)
	}
	if close.DeltaReady {
		t.Fatal("间隔低于下限时不该给出速率")
	}
}

// rawMetrics 拼一份最小的采集输出：只有 /proc/stat 的首行与 meminfo 是必需的。
func rawMetrics(cpuLine string) string {
	return "###STAT\n" + cpuLine + "\n" +
		"###MEMINFO\nMemTotal:       16000000 kB\nMemAvailable:   12000000 kB\n"
}
