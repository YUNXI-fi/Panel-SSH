package sysinfo

import (
	"fmt"
	"strings"
	"testing"
)

// ---------- SMART ----------

// 样例取自 smartctl -j -a 的真实输出（只保留面板用到的字段）。
const sampleAtaSmart = `{
  "model_name": "WDC WD5000LPCX-24VHAT0",
  "serial_number": "WD-WX11A11",
  "smart_status": {"passed": true},
  "temperature": {"current": 34},
  "power_on_time": {"hours": 12345},
  "ata_smart_attributes": {"table": [
    {"id": 1, "name": "Raw_Read_Error_Rate", "value": 200, "worst": 200, "thresh": 51, "raw": {"value": 0}},
    {"id": 5, "name": "Reallocated_Sector_Ct", "value": 100, "worst": 100, "thresh": 36, "raw": {"value": 0}},
    {"id": 9, "name": "Power_On_Hours", "value": 71, "worst": 71, "thresh": 0, "raw": {"value": 12345}},
    {"id": 197, "name": "Current_Pending_Sector", "value": 200, "worst": 200, "thresh": 0, "raw": {"value": 8}},
    {"id": 199, "name": "UDMA_CRC_Error_Count", "value": 200, "worst": 200, "thresh": 0, "raw": {"value": 3}}
  ]}
}`

const sampleNvmeSmart = `{
  "model_name": "SAMSUNG MZVLB256HBHQ",
  "smart_status": {"passed": true},
  "nvme_smart_health_information_log": {
    "temperature": 41,
    "power_on_hours": 9876,
    "percentage_used": 7,
    "media_errors": 0,
    "available_spare": 100
  }
}`

func TestCollectSmart(t *testing.T) {
	report := CollectSmart("/dev/sdb", sampleAtaSmart)
	if !report.Available || report.Note != "" {
		t.Fatalf("解析应当成功：%+v", report)
	}
	if !report.Passed || report.Temperature != 34 || report.PowerOnHours != 12345 {
		t.Fatalf("总体信息解析错误：%+v", report)
	}

	// 一块普通盘有三十多条属性，全列出来等于什么都不突出：
	// 只保留与数据完整性相关的几项，以及任何跌破阈值的项。
	var names []string
	for _, attr := range report.Attributes {
		names = append(names, attr.Name)
	}
	want := []string{"Reallocated_Sector_Ct", "Current_Pending_Sector", "UDMA_CRC_Error_Count"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("关键属性筛选错误：%v", names)
	}

	// 待定扇区已经攒了 8 个 —— 盘面在出坏道，这个数字本身就该被看见。
	for _, attr := range report.Attributes {
		if attr.Name != "Current_Pending_Sector" {
			continue
		}
		if attr.Raw != 8 || !attr.Critical || attr.Failed {
			t.Fatalf("待定扇区解析错误：%+v", attr)
		}
	}
}

func TestCollectSmartFailing(t *testing.T) {
	// value 跌破 thresh：这是厂商定义的"该换盘了"。
	report := CollectSmart("/dev/sdb", `{
	  "smart_status": {"passed": false},
	  "ata_smart_attributes": {"table": [
	    {"id": 5, "name": "Reallocated_Sector_Ct", "value": 20, "worst": 20, "thresh": 36, "raw": {"value": 1400}}
	  ]}
	}`)
	if report.Passed {
		t.Fatal("smart_status 为 false 时不该报告健康")
	}
	if len(report.Attributes) != 1 || !report.Attributes[0].Failed {
		t.Fatalf("跌破阈值的属性应被标为失败：%+v", report.Attributes)
	}
}

func TestCollectSmartNvme(t *testing.T) {
	// NVMe 的健康数据在完全不同的字段里，但对外必须是同一种结构，
	// 否则前端要为两种盘各写一套展示。
	report := CollectSmart("/dev/nvme0n1", sampleNvmeSmart)
	if !report.Available || report.Temperature != 41 || report.PowerOnHours != 9876 {
		t.Fatalf("NVMe 总体信息解析错误：%+v", report)
	}
	if len(report.Attributes) != 3 {
		t.Fatalf("NVMe 应当给出三项健康指标：%+v", report.Attributes)
	}
	for _, attr := range report.Attributes {
		if attr.Failed {
			t.Fatalf("这份样例里的 NVMe 指标都正常：%+v", attr)
		}
	}
}

func TestCollectSmartUnavailable(t *testing.T) {
	// 空输出（设备不支持 SMART、或没装 smartctl）不能报成"健康"。
	report := CollectSmart("/dev/sdb", "")
	if report.Available {
		t.Fatal("没有输出时不该声称可用")
	}
	if report.Note == "" {
		t.Fatal("不可用时必须说明原因")
	}
}

// ---------- 目录用量 ----------

const sampleDuOutput = `1048576	/mnt/data/videos
524288	/mnt/data/images
131072	/mnt/data/with space
4096	/mnt/data/docs
1708032	/mnt/data
`

func TestCollectDiskUsage(t *testing.T) {
	usage := CollectDiskUsage("/mnt/data", sampleDuOutput)

	if usage.Total != 1708032*1024 {
		t.Fatalf("总计解析错误：%d", usage.Total)
	}
	// 目标目录自身那一行是总计，不能作为子项再出现一次。
	if len(usage.Entries) != 4 {
		t.Fatalf("子项数量错误：%+v", usage.Entries)
	}
	for _, entry := range usage.Entries {
		if entry.Path == "/mnt/data" {
			t.Fatal("目标目录自身不该出现在子项里")
		}
	}
	if usage.Entries[0].Name != "videos" || usage.Entries[0].Bytes != 1048576*1024 {
		t.Fatalf("子项解析错误：%+v", usage.Entries[0])
	}
	// 路径里带空格时，不能把大小和路径拆错。
	if usage.Entries[2].Name != "with space" || usage.Entries[2].Bytes != 131072*1024 {
		t.Fatalf("含空格的路径解析错误：%+v", usage.Entries[2])
	}
}

func TestCollectDiskUsageTruncated(t *testing.T) {
	// 超出上限时截断，并把"还有更多"标出来 —— 否则用户会以为这就是全部。
	var builder strings.Builder
	for i := 0; i < maxUsageEntries+5; i++ {
		fmt.Fprintf(&builder, "%d\t/mnt/data/dir%02d\n", 1024*(i+1), i)
	}

	usage := CollectDiskUsage("/mnt/data", builder.String())
	if !usage.Truncated {
		t.Fatal("超出上限时应当标记为已截断")
	}
	if len(usage.Entries) != maxUsageEntries {
		t.Fatalf("截断后的子项数量错误：%d", len(usage.Entries))
	}
}

func TestCollectDiskUsageFallsBackToSum(t *testing.T) {
	// du 因为权限不足没输出目标目录那一行时，用子项之和兜底，
	// 免得面板上写着"总计 0 B"而下面列着几十 GB 的子项。
	usage := CollectDiskUsage("/mnt/data", "1048576\t/mnt/data/videos\n")
	if usage.Total != 1048576*1024 {
		t.Fatalf("缺少总计行时应当用子项之和兜底：%d", usage.Total)
	}
}
