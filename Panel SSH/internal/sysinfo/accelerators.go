package sysinfo

import (
	"strconv"
	"strings"
)

// 加速设备类型。目前覆盖两类目标环境里最常见的：
//   - gpu：NVIDIA 显卡，用 nvidia-smi 的 CSV 接口；
//   - npu：华为昇腾，用 npu-smi info 的表格输出。
//
// 两者都用厂商自带工具而不是直接读设备节点：节点口径（hwmon / dcmi / debugfs）
// 随驱动版本变，且非 root 经常读不到；CLI 的输出格式反而是厂商对外承诺的接口。
const (
	KindGPU = "gpu"
	KindNPU = "npu"
)

// Accelerator 是一块 GPU/NPU 的当前状态。
//
// 利用率与温度取 -1 表示工具没有给出该值（容器权限不足、虚拟 GPU 等场景），
// 前端据此显示「—」，不能当成 0 —— 0 的含义是"设备在空闲"。
// 显存统一换算成字节，与 Metrics 里其它容量字段口径一致。
type Accelerator struct {
	Kind string `json:"kind"`
	// ID 是厂商工具给出的设备编号；Chip 仅 NPU 多核模块使用（如 310B3 的双核）。
	ID   int    `json:"id"`
	Chip int    `json:"chip"`
	Name string `json:"name"`

	UtilPercent float64 `json:"utilPercent"`
	MemUsed     uint64  `json:"memUsed"`
	MemTotal    uint64  `json:"memTotal"`
	TempC       float64 `json:"tempC"`
}

// parseAccelerators 汇总 GPU 与 NPU 两节的解析结果。
// 两个分节为空（机器上没有对应工具）时返回空切片而不是 nil，
// 让 JSON 固定序列化成 [] 而不是 null。
func parseAccelerators(nvidiaSection, npuSection string) []Accelerator {
	out := make([]Accelerator, 0)
	out = append(out, parseNVIDIA(nvidiaSection)...)
	out = append(out, parseNPU(npuSection)...)
	return out
}

// parseMiB 把 nvidia-smi nounits 给出的 MiB 整数换算成字节；无法解析时返回错误。
func parseMiB(field string) (uint64, error) {
	miB, err := strconv.ParseUint(strings.TrimSpace(field), 10, 64)
	if err != nil {
		return 0, err
	}
	return miB * 1024 * 1024, nil
}

// parseOptionalFloat 解析可能缺失的数值；[N/A] 等非数字一律落到 -1。
func parseOptionalFloat(field string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
	if err != nil {
		return -1
	}
	return v
}

// parseNVIDIA 解析 nvidia-smi --query-gpu 的 CSV（noheader,nounits）输出。
//
// 每个字段都按位置取：--query-gpu 固定了列顺序，不依赖语言环境。
// 行形如：
//
//	0, NVIDIA GeForce GTX 1650, 0, 5, 4096, 38
//
// 分别是 index、name、利用率%、显存已用 MiB、显存总量 MiB、温度°C。
func parseNVIDIA(section string) []Accelerator {
	out := make([]Accelerator, 0)
	for _, line := range strings.Split(section, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 6 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			// nvidia-smi 的告警行会混进 stdout，首列不是编号，天然落在这里被跳过。
			continue
		}
		total, err := parseMiB(fields[4])
		if err != nil || total == 0 {
			continue
		}
		used, err := parseMiB(fields[3])
		if err != nil {
			used = 0
		}
		util := parseOptionalFloat(fields[2])
		if util >= 0 {
			util = clamp(util, 0, 100)
		}
		out = append(out, Accelerator{
			Kind:        KindGPU,
			ID:          id,
			Name:        strings.TrimSpace(fields[1]),
			UtilPercent: util,
			MemUsed:     used,
			MemTotal:    total,
			TempC:       parseOptionalFloat(fields[5]),
		})
	}
	return out
}

// parseNPUChipCell 解析 npu-smi 表格芯片行的第三格：
//
//	0 957 / 10810        —— AICore 占用率、显存已用/总量（MiB）
//
// AICore 在个别形态下打印 NA，此时利用率记为 -1。
func parseNPUChipCell(cell string) (util float64, usedMiB, totalMiB uint64, ok bool) {
	fields := strings.Fields(cell)
	if len(fields) != 4 || fields[2] != "/" {
		return 0, 0, 0, false
	}
	usedMiB, err1 := strconv.ParseUint(fields[1], 10, 64)
	totalMiB, err2 := strconv.ParseUint(fields[3], 10, 64)
	if err1 != nil || err2 != nil || totalMiB == 0 {
		return 0, 0, 0, false
	}
	util = parseOptionalFloat(fields[0])
	if util >= 0 {
		util = clamp(util, 0, 100)
	}
	return util, usedMiB, totalMiB, true
}

// parseNPU 解析昇腾 npu-smi info 的表格输出。
//
// 每张卡由两行数据描述，310B1 官方文档原样：
//
//	| 0 310B1 | OK | 10.4 66 15 / 15 |
//	| 0 0     | NA | 0 957 / 10810   |
//
// 第一行给出 NPU 编号、型号、功耗、温度；第二行按芯片给出 AICore 利用率与显存。
// 多核模块（如 310B3 双核）会有多条芯片行，型号与温度沿用所属 NPU 行。
//
// 不同 CANN 版本的列结构有差异：310 系的芯片行首格是 "Chip Device" 两个编号，
// 910 系（CANN 24.x）只有一个 Chip 列：
//
//	| 0 | 0000:C1:00.0 | 100 32768 / 65536 |
//
// NPU 行的首格永远是 "编号+型号"，靠第二个 token 是否为数字区分两种芯片行。
//
// 判定靠"按竖线切出的单元格结构"，不依赖任何固定文字：表头行首格是
// "NPU Name"、"Chip Device"，首词不是数字，天然被跳过。
func parseNPU(section string) []Accelerator {
	out := make([]Accelerator, 0)

	type npuHead struct {
		name string
		temp float64
	}
	heads := make(map[int]npuHead)
	// 当前 NPU 行的编号。910 系芯片行只有本地芯片号（"| 0 |"），
	// NPU 归属只能靠"它紧跟在哪条 NPU 行后面"来确定。
	currentID := -1

	for _, line := range strings.Split(section, "\n") {
		if !strings.Contains(line, "|") {
			continue
		}
		cells := make([]string, 0, 3)
		for _, part := range strings.Split(line, "|") {
			if cell := strings.TrimSpace(part); cell != "" {
				cells = append(cells, cell)
			}
		}
		if len(cells) < 3 {
			continue
		}

		head := strings.Fields(cells[0])
		if len(head) == 0 {
			continue
		}
		id, err := strconv.Atoi(head[0])
		if err != nil {
			continue // 表头行（首词是 NPU / Chip 等文字）
		}

		// 芯片行有两种列结构：
		//   - 310 系 "Chip Device" 两个数字，首列就是 NPU 编号，次列是 NPU 内的芯片号；
		//   - 910 系只有一个本地芯片号，NPU 编号沿用最近一条 NPU 行。
		npuID := id
		chip := 0
		isChipRow := false
		if len(head) == 1 {
			if currentID < 0 {
				continue
			}
			npuID = currentID
			chip = id
			isChipRow = true
		} else if device, err := strconv.Atoi(head[1]); err == nil {
			chip = device
			isChipRow = true
		}
		if isChipRow {
			util, usedMiB, totalMiB, ok := parseNPUChipCell(cells[2])
			if !ok {
				continue
			}
			h := heads[npuID]
			out = append(out, Accelerator{
				Kind:        KindNPU,
				ID:          npuID,
				Chip:        chip,
				Name:        h.name,
				UtilPercent: util,
				MemUsed:     usedMiB * 1024 * 1024,
				MemTotal:    totalMiB * 1024 * 1024,
				TempC:       h.temp,
			})
			continue
		}

		// NPU 行：NPU Name | Health | power temp hugepages / hugepages
		temp := -1.0
		if stats := strings.Fields(cells[2]); len(stats) >= 2 {
			if v, err := strconv.ParseFloat(stats[1], 64); err == nil {
				temp = v
			}
		}
		heads[id] = npuHead{name: strings.Join(head[1:], " "), temp: temp}
		currentID = id
	}
	return out
}
