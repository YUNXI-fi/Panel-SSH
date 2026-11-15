package sysinfo

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Process 是目标主机上的一个进程。
type Process struct {
	PID     int     `json:"pid"`
	PPID    int     `json:"ppid"`
	User    string  `json:"user"`
	State   string  `json:"state"`
	CPU     float64 `json:"cpu"`
	Mem     float64 `json:"mem"`
	RSSKiB  uint64  `json:"rssKiB"`
	Elapsed string  `json:"elapsed"`
	Command string  `json:"command"`
}

// processesCommand 用「字段名=」语法让 ps 不打印表头，省掉一次表头解析。
// 排序放在本地做而不依赖 ps --sort：该选项并非所有 ps 实现都支持。
const processesCommand = `ps -eo pid=,ppid=,user=,stat=,pcpu=,pmem=,rss=,etime=,args= 2>/dev/null; true`

// ProcessesCommand 返回进程列表采集命令。
func ProcessesCommand() string { return processesCommand }

// maxProcesses 限制返回条数。一台机器可能有数千进程，
// 面板要回答的是"谁在吃资源"，而不是完整进程表。
const maxProcesses = 300

// maxCommandLen 限制命令行长度，避免超大 argv 把响应撑爆。
const maxCommandLen = 200

// ParseProcesses 解析 ps 输出，按 CPU 占用降序返回，并给出进程总数。
func ParseProcesses(raw string) ([]Process, int) {
	procs := make([]Process, 0, 256)

	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// 前 7 个字段不含空格，第 8 个 args 可能含空格，
		// 因此整体切分后把尾部重新拼回。
		fields := strings.Fields(trimmed)
		if len(fields) < 7 {
			continue
		}

		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		cpu, _ := strconv.ParseFloat(fields[4], 64)
		mem, _ := strconv.ParseFloat(fields[5], 64)
		rss, _ := strconv.ParseUint(fields[6], 10, 64)

		command := ""
		if len(fields) > 8 {
			command = strings.Join(fields[8:], " ")
		}
		if len(command) > maxCommandLen {
			command = command[:maxCommandLen] + "…"
		}

		elapsed := ""
		if len(fields) > 7 {
			elapsed = fields[7]
		}

		procs = append(procs, Process{
			PID:     pid,
			PPID:    ppid,
			User:    fields[2],
			State:   fields[3],
			CPU:     cpu,
			Mem:     mem,
			RSSKiB:  rss,
			Elapsed: elapsed,
			Command: command,
		})
	}

	total := len(procs)
	sort.SliceStable(procs, func(i, j int) bool {
		if procs[i].CPU != procs[j].CPU {
			return procs[i].CPU > procs[j].CPU
		}
		if procs[i].Mem != procs[j].Mem {
			return procs[i].Mem > procs[j].Mem
		}
		return procs[i].PID < procs[j].PID
	})

	if len(procs) > maxProcesses {
		procs = procs[:maxProcesses]
	}
	return procs, total
}

// 允许发送的终止信号。
const (
	// KillTerm 先给进程自我清理的机会。
	KillTerm = "TERM"
	// KillKill 仅在 TERM 无效时使用。
	KillKill = "KILL"
)

// KillCommand 构造结束进程的命令。
//
// 默认语义是 TERM 而不是 KILL：直接 SIGKILL 会让进程没有机会刷盘、
// 关闭连接或回收子进程。强杀必须由调用方显式选择。
func KillCommand(pid int, signal string) (string, error) {
	// PID 1 是 init/systemd，终止它等于让整机不可用。
	if pid <= 1 {
		return "", errors.New("不允许结束 PID 1（init/systemd）")
	}
	switch signal {
	case KillTerm, KillKill:
	default:
		return "", fmt.Errorf("不支持的信号：%s", signal)
	}
	return fmt.Sprintf("kill -%s %d", signal, pid), nil
}
