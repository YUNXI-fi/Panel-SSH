package env

import (
	"strconv"
	"strings"
)

// 各家族模拟脚本的公共输出约定：
//
//	===CHANGES===   kind<TAB>name<TAB>from<TAB>to<TAB>arch 行序列
//	===CONFLICTS=== 求解器的阻断信息（逐行，可能为空）
//	===SIZE===      预计下载量（如 27.9 MB，可能为空）
//	===RAW===       原始输出尾部，用于无冲突行但求解失败的兜底
//	===RC===        模拟命令退出码
//
// 解析逻辑家族无关，差异只在各自生成脚本里。
func parseSimTSV(output string) *SimReport {
	sections := splitSections(output)
	// 切片一律初始化为空集合：nil 会序列化成 JSON null，
	// 前端按数组遍历的代码（以及"无冲突"判定）都会踩空。
	rpt := &SimReport{
		OK:        true,
		Install:   []ChangeItem{},
		Upgrade:   []ChangeItem{},
		Downgrade: []ChangeItem{},
		Remove:    []ChangeItem{},
		Conflicts: []string{},
	}

	for _, line := range strings.Split(sections["CHANGES"], "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 5 || fields[1] == "" {
			continue
		}
		item := ChangeItem{Name: fields[1], From: fields[2], To: fields[3], Arch: fields[4]}
		switch fields[0] {
		case "install":
			rpt.Install = append(rpt.Install, item)
		case "upgrade":
			rpt.Upgrade = append(rpt.Upgrade, item)
		case "downgrade":
			rpt.Downgrade = append(rpt.Downgrade, item)
		case "remove":
			rpt.Remove = append(rpt.Remove, item)
		}
	}

	for _, line := range strings.Split(sections["CONFLICTS"], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rpt.Conflicts = append(rpt.Conflicts, line)
		}
	}

	rpt.DownloadKB = parseSizeKB(strings.TrimSpace(sections["SIZE"]))

	rc, _ := strconv.Atoi(strings.TrimSpace(sections["RC"]))
	// 退出码非 0 分两种：dnf/yum 的"模拟成功等待确认"（有变更项），
	// 以及真正的求解失败（无变更项）。后者必须从 RAW 里补一条阻断信息。
	if rc != 0 && rpt.ChangeCount() == 0 && len(rpt.Conflicts) == 0 {
		for _, line := range strings.Split(sections["RAW"], "\n") {
			if line = strings.TrimSpace(line); line != "" {
				rpt.Conflicts = append(rpt.Conflicts, line)
			}
		}
	}
	if len(rpt.Conflicts) > 0 {
		rpt.OK = false
	}
	return rpt
}

// parseSizeKB 把 "27.9 MB"、"938 kB"、"1.2 GB" 换算为 KiB。
// apt/dnf 输出的是 1000 进制单位（kB/MB/GB），这里按 1000 换算。
func parseSizeKB(s string) int64 {
	if s == "" {
		return 0
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	unit := ""
	if len(fields) > 1 {
		unit = strings.ToUpper(fields[1])
	}
	switch {
	case strings.HasPrefix(unit, "G"):
		return int64(n * 1000 * 1000)
	case strings.HasPrefix(unit, "M"):
		return int64(n * 1000)
	case strings.HasPrefix(unit, "K"):
		return int64(n)
	default:
		return int64(n / 1024)
	}
}
