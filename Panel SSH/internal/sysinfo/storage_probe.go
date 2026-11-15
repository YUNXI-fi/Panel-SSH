package sysinfo

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 按需探测：SMART 健康与目录用量。
//
// 这两项都不放进常规快照，原因是它们"重"且"不必一直有"：
// SMART 要唤醒可能正在休眠的盘，du 在几百 GB 的目录上要跑很久。
// 让它们跟着 5 秒一轮的轮询走，是拿目标机的 IO 换一个没人看的数字。

// ---------- SMART ----------

// SmartCommand 返回读取一块磁盘 SMART 信息的命令。
//
// 四种信息一次拿全：-a 覆盖 -H（健康）、-A（属性）、-i（型号序列号）。
// 不提权：与挂载/卸载保持一致，权限由目标机的账号配置决定 ——
// 面板在这里偷偷提权，等于把"这条命令以什么身份执行"变成一个黑盒。
func SmartCommand(device string) string {
	return fmt.Sprintf("smartctl -j -a -- %s 2>/dev/null", shellQuote(device))
}

// SmartAttribute 是一条 SMART 属性。
type SmartAttribute struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Value int    `json:"value"`
	Worst int    `json:"worst"`
	// Threshold 是厂商给的失效阈值，Value 跌到它以下就意味着要换盘了。
	Threshold int    `json:"threshold"`
	Raw       uint64 `json:"raw"`
	// Unit 说明 Raw 的单位（扇区、次、小时、百分比）。
	Unit string `json:"unit"`
	// Failed 表示已经跌破阈值。
	Failed bool `json:"failed"`
	// Critical 表示这是与数据完整性直接相关的属性，值得一直盯着。
	Critical bool `json:"critical"`
}

// SmartReport 是一块磁盘的健康报告。
type SmartReport struct {
	Device string `json:"device"`
	// Available 为假时 Note 说明原因（没输出、解析失败、设备不支持）。
	Available bool   `json:"available"`
	Note      string `json:"note"`
	Model     string `json:"model"`
	Serial    string `json:"serial"`
	// Passed 是 SMART 的总体健康判定。
	Passed bool `json:"passed"`
	// Temperature 与 PowerOnHours 为 -1 表示设备没有报告这项。
	Temperature  int `json:"temperature"`
	PowerOnHours int `json:"powerOnHours"`
	// Attributes 只包含"关键"与"已经异常"的项。
	//
	// 一块普通硬盘有三十多条属性，全列出来等于什么都不突出：用户真正要看的
	// 是"重分配扇区有没有涨"这一类。想看全集就自己跑 smartctl。
	Attributes []SmartAttribute `json:"attributes"`
}

// criticalAttrIDs 是与数据完整性直接相关的 ATA 属性。
//
// 这几项是判断"这块盘还能不能留"的通用依据：5/196/197/198 一涨就说明
// 盘面已经在出坏道，199 则指向数据线或背板。
var criticalAttrIDs = map[int]bool{
	5:   true, // Reallocated_Sector_Ct
	196: true, // Reallocated_Event_Count
	197: true, // Current_Pending_Sector
	198: true, // Offline_Uncorrectable
	199: true, // UDMA_CRC_Error_Count
}

type smartJSON struct {
	ModelName    *string `json:"model_name"`
	SerialNumber *string `json:"serial_number"`
	SmartStatus  *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	AtaAttributes *struct {
		Table []struct {
			ID     int    `json:"id"`
			Name   string `json:"name"`
			Value  int    `json:"value"`
			Worst  int    `json:"worst"`
			Thresh int    `json:"thresh"`
			Raw    struct {
				Value uint64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NvmeLog *struct {
		Temperature    int    `json:"temperature"`
		PowerOnHours   int    `json:"power_on_hours"`
		PercentageUsed int    `json:"percentage_used"`
		MediaErrors    uint64 `json:"media_errors"`
		AvailableSpare int    `json:"available_spare"`
	} `json:"nvme_smart_health_information_log"`
	Messages []struct {
		String string `json:"string"`
	} `json:"messages"`
}

// CollectSmart 解析 smartctl 的 JSON 输出。
//
// 只看 stdout，不看退出码：smartctl 用位掩码表达状态，其中 bit3 正是
// "磁盘即将故障"—— 那是最需要报给用户的结果，绝不能因为它非零就当命令失败。
func CollectSmart(device, raw string) *SmartReport {
	report := &SmartReport{Device: device, Temperature: -1, PowerOnHours: -1}

	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		report.Note = "smartctl 没有输出，可能是设备不支持 SMART 或权限不足"
		return report
	}

	var parsed smartJSON
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		report.Note = "无法解析 smartctl 输出：" + err.Error()
		return report
	}

	report.Available = true
	report.Model = derefString(parsed.ModelName)
	report.Serial = derefString(parsed.SerialNumber)
	if parsed.SmartStatus != nil {
		report.Passed = parsed.SmartStatus.Passed
	}
	if parsed.Temperature != nil {
		report.Temperature = parsed.Temperature.Current
	}
	if parsed.PowerOnTime != nil {
		report.PowerOnHours = parsed.PowerOnTime.Hours
	}

	// NVMe 与 ATA 的健康数据在完全不同的字段里，这里收敛成同一种属性列表，
	// 免得前端为两种盘各写一套展示逻辑。
	if parsed.NvmeLog != nil {
		report.Temperature = parsed.NvmeLog.Temperature
		report.PowerOnHours = parsed.NvmeLog.PowerOnHours
		report.Attributes = nvmeAttributes(parsed.NvmeLog)
		return report
	}

	if parsed.AtaAttributes != nil {
		report.Attributes = ataAttributes(parsed.AtaAttributes.Table)
	}

	if len(report.Attributes) == 0 && len(parsed.Messages) > 0 {
		// 虚拟盘、USB 桥接后面不支持的盘会走到这里：把 smartctl 自己的解释
		// 带出去，比让用户对着一个空白面板猜要好。
		report.Note = strings.TrimSpace(parsed.Messages[0].String)
	}

	return report
}

func ataAttributes(table []struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Value  int    `json:"value"`
	Worst  int    `json:"worst"`
	Thresh int    `json:"thresh"`
	Raw    struct {
		Value uint64 `json:"value"`
	} `json:"raw"`
}) []SmartAttribute {
	attributes := make([]SmartAttribute, 0, len(criticalAttrIDs)+2)
	for _, row := range table {
		critical := criticalAttrIDs[row.ID]
		// 关键属性即使当前读数正常也要列出来（用户要的是"它涨没涨"），
		// 非关键属性只在跌破阈值时才出现，避免面板被三十行数字淹没。
		failed := row.Thresh > 0 && row.Value <= row.Thresh
		if !critical && !failed {
			continue
		}

		unit := ""
		switch row.ID {
		case 5, 196, 197, 198:
			unit = "扇区"
		case 199:
			unit = "次"
		}

		attributes = append(attributes, SmartAttribute{
			ID:        row.ID,
			Name:      row.Name,
			Value:     row.Value,
			Worst:     row.Worst,
			Threshold: row.Thresh,
			Raw:       row.Raw.Value,
			Unit:      unit,
			Failed:    failed,
			Critical:  critical,
		})
	}
	return attributes
}

func nvmeAttributes(log *struct {
	Temperature    int    `json:"temperature"`
	PowerOnHours   int    `json:"power_on_hours"`
	PercentageUsed int    `json:"percentage_used"`
	MediaErrors    uint64 `json:"media_errors"`
	AvailableSpare int    `json:"available_spare"`
}) []SmartAttribute {
	// NVMe 没有 ATA 那套"当前值/最差值/阈值"，它的对应指标是裸计数，
	// 所以这里按各自的实际情况给阈值：介质错误一出现就不正常。
	return []SmartAttribute{
		{
			Name: "Media_Errors", Raw: log.MediaErrors, Unit: "次",
			Failed: log.MediaErrors > 0, Critical: true,
		},
		{
			Name: "Percentage_Used", Raw: uint64(log.PercentageUsed), Unit: "%",
			Failed: log.PercentageUsed >= 90, Critical: true,
		},
		{
			Name: "Available_Spare", Raw: uint64(log.AvailableSpare), Unit: "%",
			Failed: log.AvailableSpare < 10, Critical: true,
		},
	}
}

// ---------- 目录用量 ----------

// maxUsageEntries 是一轮用量统计最多返回的子项数量。
//
// 取 50 是因为再多的行数已超出"一眼看出谁占地方"的用途，
// 而每一行都要过一遍网络。
const maxUsageEntries = 50

// DiskUsageCommand 返回统计某个目录下各子目录占用空间的命令。
//
//   - -x 不跨文件系统：目标目录下的另一个挂载点（bind mount、嵌套挂载）
//     不属于这个目录的占用，算进去会得到一个偏大的假数字。
//   - -d 1 只下钻一层：递归到底在几百 GB 的目录上要跑很久，而"是哪一层
//     吃掉了空间"通常就是用户要找的答案；要往下看，再对该层跑一次。
//   - -k 用 KB 而不是 -B1：-B1 是 GNU 特有的，-k 在 busybox 上也有。
//
// 刻意不吞 stderr：du 读不到子目录时会写警告，而那种情况下它只统计到目录本身
// （几 KB），面板上看起来就像"这个盘是空的"。这个误解比那几行警告烦人得多。
func DiskUsageCommand(target string, depth int) string {
	return fmt.Sprintf(
		"du -x -k -d %d -- %s | sort -rn | head -n %d",
		depth, shellQuote(target), maxUsageEntries+1)
}

// DiskUsageEntry 是目录里的一个子项。
type DiskUsageEntry struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Bytes uint64 `json:"bytes"`
}

// DiskUsage 是一次目录用量统计的结果。
type DiskUsage struct {
	Target string `json:"target"`
	// Total 是目标目录自身的总占用。
	Total   uint64           `json:"total"`
	Entries []DiskUsageEntry `json:"entries"`
	// Truncated 表示还有更小的子项没列出来（输出被截断了）。
	Truncated bool `json:"truncated"`
	// Warning 是 du 报告的问题，多数是"有子目录读不到"。
	// 它非空意味着上面的数字只是能读到的那部分，不是全部。
	Warning string `json:"warning"`
}

// CollectDiskUsage 解析 du 的输出。
//
// du 的输出是"字节数 + 路径"，用第一个空白字符分隔而不是按制表符：
// 路径里可以有空格，但数字里不可能有。
func CollectDiskUsage(target, raw string) *DiskUsage {
	usage := &DiskUsage{Target: target, Entries: []DiskUsageEntry{}}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		size, path, ok := splitDuLine(line)
		if !ok {
			continue
		}
		kilobytes, err := strconv.ParseUint(size, 10, 64)
		if err != nil {
			continue
		}
		bytes := kilobytes * 1024

		// 目标目录自身那一行是总计，不作为"子项"重复列出。
		if path == target {
			usage.Total = bytes
			continue
		}

		usage.Entries = append(usage.Entries, DiskUsageEntry{
			Path:  path,
			Name:  pathBase(path),
			Bytes: bytes,
		})
	}

	// du 因为权限不足跳过的子目录不会出现在输出里，此时用子项之和兜底，
	// 免得面板上写着"总计 0 B"而下面列着几十 GB 的子项。
	// 求和要在截断之前做：截断是为了少传几行给前端，不该顺手改掉总计。
	if usage.Total == 0 {
		for _, entry := range usage.Entries {
			usage.Total += entry.Bytes
		}
	}

	if len(usage.Entries) > maxUsageEntries {
		usage.Truncated = true
		usage.Entries = usage.Entries[:maxUsageEntries]
	}

	return usage
}

func splitDuLine(line string) (string, string, bool) {
	for i, r := range line {
		if r == '\t' || r == ' ' {
			return line[:i], strings.TrimSpace(line[i+1:]), true
		}
	}
	return "", "", false
}

func pathBase(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		return path
	}
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}
