// Package sysinfo 从目标主机的 /proc 采集运行指标。
//
// 设计取舍：
//
//  1. 数据来源用 /proc 而不是 free / top / df 的文本输出。/proc 是内核 ABI，
//     格式稳定；而 free、top、ps 的输出列名与排版在不同发行版和版本间有差异，
//     解析容易在某个系统上悄悄失效。面板要兼容 Ubuntu / Debian / CentOS /
//     国产发行版，这个差异必须避开。
//
//  2. 一次 SSH 执行采集全部内容，用 ### 分节，避免多次往返带来的 RTT 累积。
//
//  3. CPU 使用率与网络速率必须由两次采样求差得出：/proc/stat 与 /proc/net/dev
//     给出的都是自开机以来的累计值，单次采样无法算出瞬时速率。因此采集器需要
//     按主机保存上一次采样，这是该功能无法省略的状态。
package sysinfo

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 分节标记，需与 lightMetricsCommand / heavyMetricsCommand 保持一致。
const (
	secUptime   = "###UPTIME"
	secLoadavg  = "###LOADAVG"
	secStat     = "###STAT"
	secMeminfo  = "###MEMINFO"
	secNetdev   = "###NETDEV"
	secCPUInfo  = "###CPUINFO"
	secUname    = "###UNAME"
	secHostname = "###HOSTNAME"
	secOS       = "###OSRELEASE"
	secDiskstat = "###DISKSTAT"
	secBlockDev = "###BLOCKDEV"
	secDF       = "###DF"
	secNVIDIA   = "###NVIDIA"
	secNPU      = "###NPU"
)

// lightMetricsCommand 是采集脚本的轻量部分：全部读 /proc 或用毫秒级的命令。
//
// 与 heavyMetricsCommand 分开，是为了让流式采集能按秒级节奏跑而不压榨目标机 ——
// 这一段里没有任何真实磁盘 I/O，也不起外部子进程。
const lightMetricsCommand = `echo '###UPTIME'; cat /proc/uptime 2>/dev/null
echo '###LOADAVG'; cat /proc/loadavg 2>/dev/null
echo '###STAT'; head -n1 /proc/stat 2>/dev/null
echo '###MEMINFO'; cat /proc/meminfo 2>/dev/null
echo '###NETDEV'; cat /proc/net/dev 2>/dev/null
echo '###CPUINFO'; grep -m1 'model name' /proc/cpuinfo 2>/dev/null; grep -m1 'Hardware' /proc/cpuinfo 2>/dev/null; grep -c '^processor' /proc/cpuinfo 2>/dev/null
echo '###UNAME'; uname -srm 2>/dev/null
echo '###HOSTNAME'; hostname 2>/dev/null
echo '###OSRELEASE'; grep -m1 '^PRETTY_NAME=' /etc/os-release 2>/dev/null || cat /etc/redhat-release 2>/dev/null
echo '###BLOCKDEV'; ls /sys/block 2>/dev/null
echo '###DISKSTAT'; cat /proc/diskstats 2>/dev/null
true`

// heavyMetricsCommand 是采集脚本的重量部分，三条都是"会真的花代价"的命令：
// df 可能卡在无响应的网络挂载点上，nvidia-smi / npu-smi 要起子进程去查询设备。
//
// 一次性调用（MetricsCommand）每次都跑它无所谓；流式采集必须把它降频到十秒级 ——
// 按同一节奏跑这三条，会比改造之前的轮询更费目标机。
const heavyMetricsCommand = `echo '###DF'; df -P -k 2>/dev/null
echo '###NVIDIA'; nvidia-smi --query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu --format=csv,noheader,nounits 2>/dev/null
echo '###NPU'; npu-smi info 2>/dev/null
true`

// MetricsCommand 返回全量采集命令（轻量 + 重量），供一次性调用方在远端执行。
//
// 末尾的 `; true` 是刻意的：单个 /proc 文件读取失败（例如容器里缺某项）不应该
// 让整次采集失败，缺失的分节由解析阶段判定并给出明确错误。
func MetricsCommand() string { return lightMetricsCommand + "\n" + heavyMetricsCommand }

// MetricsTickMarker 是流式采集里"一轮开始"的标记。
//
// 采集脚本用 ### 分节（###STAT 等），但没有任何"本轮到此结束"的标记 ——
// 消费方无从判断收到的那批分节是否已经收齐。在每轮开头补一个显式标记，
// 消费方就能在看见下一个标记时确认上一轮是完整的。
const MetricsTickMarker = "###TICK"

// StreamingMetricsCommand 组装流式采集脚本。
//
// intervalSec 是一轮之间的间隔秒数（允许小数，如 0.2）；heavyEvery 表示每多少轮
// 补一次重量分节，传 0 表示永不补（调用方另行获取磁盘与加速器信息）。
//
// 为什么在远端循环而不是面板侧定时：轮询模式下每个采样点都要一次完整的 SSH
// 往返，公网链路上 200ms 档位物理上不可能达成；远端按自己的节奏输出就没有这个
// 问题，面板只是被动接收，采样间隔也不再被 RTT 拉长。
//
// 依赖 `sleep` 接受小数：目标机是 Linux 发行版（GNU coreutils 或 busybox），
// 两者都支持；面板不面向 POSIX 纯净环境。
func StreamingMetricsCommand(intervalSec float64, heavyEvery int) string {
	var b strings.Builder
	b.WriteString("i=0\n")
	b.WriteString("while true; do\n")
	// 轮次标记必须排在所有分节之前：消费方靠它切出上一轮的边界。
	b.WriteString("echo '" + MetricsTickMarker + "'\n")
	b.WriteString(lightMetricsCommand + "\n")
	if heavyEvery > 0 {
		fmt.Fprintf(&b, "if [ $((i %% %d)) -eq 0 ]; then\n", heavyEvery)
		b.WriteString(heavyMetricsCommand + "\n")
		b.WriteString("fi\n")
	}
	b.WriteString("i=$((i + 1))\n")
	fmt.Fprintf(&b, "sleep %s\n", strconv.FormatFloat(intervalSec, 'f', -1, 64))
	b.WriteString("done")
	return b.String()
}

// Disk 是一个挂载点的容量信息，单位字节。
type Disk struct {
	MountPoint string  `json:"mountPoint"`
	Filesystem string  `json:"filesystem"`
	Total      uint64  `json:"total"`
	Used       uint64  `json:"used"`
	Available  uint64  `json:"available"`
	UsePercent float64 `json:"usePercent"`
}

// Metrics 是一次采集的结果。
type Metrics struct {
	Hostname      string  `json:"hostname"`
	Kernel        string  `json:"kernel"`
	// OSName 是 /etc/os-release 的 PRETTY_NAME（或 /etc/redhat-release 原文）。
	// 与 Kernel 分开：前者答"是什么发行版"，后者答"内核多新"，两者经常被混为一谈。
	OSName        string  `json:"osName"`
	CPUModel      string  `json:"cpuModel"`
	CPUCores      int     `json:"cpuCores"`
	CPUPercent    float64 `json:"cpuPercent"`
	Load1         float64 `json:"load1"`
	Load5         float64 `json:"load5"`
	Load15        float64 `json:"load15"`
	UptimeSeconds float64 `json:"uptimeSeconds"`

	MemTotal     uint64 `json:"memTotal"`
	MemAvailable uint64 `json:"memAvailable"`
	MemUsed      uint64 `json:"memUsed"`
	SwapTotal    uint64 `json:"swapTotal"`
	SwapFree     uint64 `json:"swapFree"`

	NetRxBytes uint64  `json:"netRxBytes"`
	NetTxBytes uint64  `json:"netTxBytes"`
	NetRxRate  float64 `json:"netRxRate"`
	NetTxRate  float64 `json:"netTxRate"`

	// 磁盘读写同样是自开机以来的累计量，速率由相邻两次采样求差得出。
	DiskReadBytes  uint64  `json:"diskReadBytes"`
	DiskWriteBytes uint64  `json:"diskWriteBytes"`
	DiskReadRate   float64 `json:"diskReadRate"`
	DiskWriteRate  float64 `json:"diskWriteRate"`

	Disks []Disk `json:"disks"`

	// GPU / NPU 等加速设备；机器上没有对应工具（nvidia-smi / npu-smi）时为空列表。
	Accelerators []Accelerator `json:"accelerators"`

	// DeltaReady 表示本次是否已经能算出速率类指标（CPU%、网络速率）。
	// 首次采样时没有基准值，只能给出累计量，此处为 false。
	DeltaReady bool      `json:"deltaReady"`
	SampledAt  time.Time `json:"sampledAt"`
}

type prevSample struct {
	at        time.Time
	cpuTotal  uint64
	cpuIdle   uint64
	netRx     uint64
	netTx     uint64
	diskRead  uint64
	diskWrite uint64
}

// Collector 保存各主机上一次采样，用于计算速率类指标。
type Collector struct {
	mu   sync.Mutex
	prev map[int64]*prevSample
}

// prevTTL 是历史采样的最长保留时间。
// 超过它的条目不会再被用到（速率计算只关心相邻两次采样），
// 留着只会随着"主机反复增删"缓慢累积。
const prevTTL = 30 * time.Minute

// minDeltaInterval 是能算出速率类指标的最小采样间隔（秒）。
//
// 这是"噪声"与"能不能用"之间的取舍：间隔越小，两次 /proc 读数之差越接近
// 计数器自身的粒度，CPU% 抖得越厉害。原先取 0.5，前提是前端固定 3 秒轮询、
// 不存在更密的采样需求；监控窗口现在提供 200ms 的「高速」档，0.5 会把
// CPU、网络、磁盘三条线直接变成"无数据"，因此下调到 0.15。
// 它仍然挡得住"近乎同时的两次采样"——那种差值里除了噪声没有信息。
const minDeltaInterval = 0.15

// NewCollector 创建采集器。
func NewCollector() *Collector {
	return &Collector{prev: make(map[int64]*prevSample)}
}

// Forget 丢弃某主机的历史采样。主机被删除或凭据变更后调用，避免残留状态。
func (c *Collector) Forget(hostID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.prev, hostID)
}

// reapStale 清掉过期条目。调用方必须已持有 c.mu。
//
// 正常情况下主机增删都会走 Forget，但面板崩溃、主键复用等路径可能留下孤儿条目；
// 每次采集时顺带做一次惰性清理，比额外起一个回收协程更简单。
func (c *Collector) reapStale(now time.Time) {
	for id, sample := range c.prev {
		if now.Sub(sample.at) > prevTTL {
			delete(c.prev, id)
		}
	}
}

// Collect 解析远端输出并给出指标。
func (c *Collector) Collect(hostID int64, raw string, now time.Time) (*Metrics, error) {
	sections := splitSections(raw)

	cpuLine := firstLine(sections[secStat])
	memRaw := sections[secMeminfo]
	if cpuLine == "" || memRaw == "" {
		return nil, errors.New("目标系统缺少 /proc/stat 或 /proc/meminfo，无法采集指标")
	}

	cpuTotal, cpuIdle, err := parseCPUTimes(cpuLine)
	if err != nil {
		return nil, err
	}

	metrics := &Metrics{
		SampledAt: now,
		Hostname:  firstLine(sections[secHostname]),
		Kernel:    firstLine(sections[secUname]),
		OSName:    parseOSName(sections[secOS]),
		Disks:     parseDF(sections[secDF]),
	}
	metrics.CPUCores, metrics.CPUModel = parseCPUInfo(sections[secCPUInfo])

	metrics.UptimeSeconds = parseUptime(firstLine(sections[secUptime]))
	metrics.Load1, metrics.Load5, metrics.Load15 = parseLoadAvg(firstLine(sections[secLoadavg]))

	mem := parseMemInfo(memRaw)
	metrics.MemTotal = mem["MemTotal"]
	metrics.MemAvailable = mem["MemAvailable"]
	if metrics.MemTotal > metrics.MemAvailable {
		// 用 MemTotal - MemAvailable 而非 MemTotal - MemFree：
		// 后者会把页缓存算成"已用"，与 free / htop 的观感严重不符。
		metrics.MemUsed = metrics.MemTotal - metrics.MemAvailable
	}
	metrics.SwapTotal = mem["SwapTotal"]
	metrics.SwapFree = mem["SwapFree"]

	metrics.NetRxBytes, metrics.NetTxBytes = parseNetDev(sections[secNetdev])
	metrics.DiskReadBytes, metrics.DiskWriteBytes = parseDiskStats(sections[secDiskstat], parseBlockDevices(sections[secBlockDev]))
	metrics.Accelerators = parseAccelerators(sections[secNVIDIA], sections[secNPU])

	c.mu.Lock()
	c.reapStale(now)
	prev := c.prev[hostID]
	c.prev[hostID] = &prevSample{
		at:        now,
		cpuTotal:  cpuTotal,
		cpuIdle:   cpuIdle,
		netRx:     metrics.NetRxBytes,
		netTx:     metrics.NetTxBytes,
		diskRead:  metrics.DiskReadBytes,
		diskWrite: metrics.DiskWriteBytes,
	}
	c.mu.Unlock()

	if prev != nil {
		elapsed := now.Sub(prev.at).Seconds()
		// 间隔过小时差值噪声极大，直接沿用累计量而不给速率。
		if elapsed >= minDeltaInterval {
			if dTotal := cpuTotal - prev.cpuTotal; dTotal > 0 {
				dIdle := cpuIdle - prev.cpuIdle
				busy := float64(dTotal-dIdle) / float64(dTotal) * 100
				metrics.CPUPercent = clamp(busy, 0, 100)
			}
			if metrics.NetRxBytes >= prev.netRx {
				metrics.NetRxRate = float64(metrics.NetRxBytes-prev.netRx) / elapsed
			}
			if metrics.NetTxBytes >= prev.netTx {
				metrics.NetTxRate = float64(metrics.NetTxBytes-prev.netTx) / elapsed
			}
			if metrics.DiskReadBytes >= prev.diskRead {
				metrics.DiskReadRate = float64(metrics.DiskReadBytes-prev.diskRead) / elapsed
			}
			if metrics.DiskWriteBytes >= prev.diskWrite {
				metrics.DiskWriteRate = float64(metrics.DiskWriteBytes-prev.diskWrite) / elapsed
			}
			metrics.DeltaReady = true
		}
	}

	return metrics, nil
}

// ---------- 解析 ----------

func splitSections(raw string) map[string]string {
	sections := make(map[string]string)
	current := ""
	var buf strings.Builder

	flush := func() {
		if current != "" {
			sections[current] = buf.String()
		}
		buf.Reset()
	}

	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.HasPrefix(trimmed, "###") {
			flush()
			current = strings.TrimSpace(trimmed)
			continue
		}
		if current != "" {
			buf.WriteString(trimmed)
			buf.WriteByte('\n')
		}
	}
	flush()
	return sections
}

func firstLine(section string) string {
	lines := strings.Split(section, "\n")
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseCPUTimes(line string) (total, idle uint64, err error) {
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("无法解析 /proc/stat 首行：%q", line)
	}

	values := make([]uint64, 0, len(fields)-1)
	for _, field := range fields[1:] {
		v, convErr := strconv.ParseUint(field, 10, 64)
		if convErr != nil {
			break
		}
		values = append(values, v)
	}
	if len(values) < 4 {
		return 0, 0, errors.New("/proc/stat 字段不足")
	}

	for _, v := range values {
		total += v
	}
	// idle + iowait 都算空闲，这是 top / htop 的通行口径；
	// 只减 idle 会把等 IO 的进程算成满载。
	idle = values[3]
	if len(values) > 4 {
		idle += values[4]
	}
	return total, idle, nil
}

// parseOSName 取发行版名称：优先 /etc/os-release 的 PRETTY_NAME（带引号，需去掉），
// 回退到 /etc/redhat-release 的整行原文。
func parseOSName(raw string) string {
	line := strings.TrimSpace(firstLine(raw))
	if line == "" {
		return ""
	}
	line = strings.TrimPrefix(line, "PRETTY_NAME=")
	return strings.Trim(line, "\"'")
}

func parseUptime(line string) float64 {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}

func parseLoadAvg(line string) (float64, float64, float64) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 0, 0, 0
	}
	parse := func(s string) float64 {
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	return parse(fields[0]), parse(fields[1]), parse(fields[2])
}

func parseMemInfo(section string) map[string]uint64 {
	out := make(map[string]uint64)
	for _, line := range strings.Split(section, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		amount, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// /proc/meminfo 的数值统一以 kB 为单位。
		out[strings.TrimSpace(key)] = amount * 1024
	}
	return out
}

func parseCPUInfo(section string) (cores int, model string) {
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if n, err := strconv.Atoi(trimmed); err == nil {
			cores = n
			continue
		}
		if _, value, ok := strings.Cut(trimmed, ":"); ok && model == "" {
			model = strings.TrimSpace(value)
		}
	}
	return cores, model
}

// parseNetDev 累加所有非 lo 网卡的收发字节数。
func parseNetDev(section string) (rx, tx uint64) {
	for _, line := range strings.Split(section, "\n") {
		name, stats, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		iface := strings.TrimSpace(name)
		// 回环流量不代表真实网络负载，计入会让面板一直显示有流量。
		if iface == "" || iface == "lo" {
			continue
		}
		fields := strings.Fields(stats)
		if len(fields) < 9 {
			continue
		}
		r, err1 := strconv.ParseUint(fields[0], 10, 64)
		t, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
	}
	return rx, tx
}

// ---------- 磁盘 IO ----------

// /proc/diskstats 的扇区数固定按 512 字节折算，与设备实际扇区大小无关。
const diskStatsSectorSize = 512

// 虚拟块设备名前缀，两类都必须排除：
//   - loop / ram / zram / sr / fd 背后没有真实磁盘，计入只会稀释真实负载；
//   - dm- / md 是 LVM、软 RAID 的中间层，它们的 IO 已在底层物理盘上计过一次，
//     两边都算会让同一份读写翻倍。
var virtualBlockPrefixes = []string{"loop", "ram", "zram", "sr", "fd", "dm-", "md", "nbd", "rbd"}

func isVirtualBlockDevice(name string) bool {
	for _, prefix := range virtualBlockPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// parseBlockDevices 给出参与 IO 统计的整盘设备集合。
//
// 判定"整盘"用 /sys/block 而不是按设备名猜：只有整盘会在这里注册，分区
// （sda1、nvme0n1p2、mmcblk0p1）必然缺席。靠名字识别需要为每种命名规则写特例，
// 漏掉一种就会把分区和整盘一起累加，读数直接翻倍。
func parseBlockDevices(section string) map[string]bool {
	devices := make(map[string]bool)
	for _, line := range strings.Split(section, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || isVirtualBlockDevice(name) {
			continue
		}
		devices[name] = true
	}
	return devices
}

// parseDiskStats 累加指定设备的读写扇区数，返回字节数。
//
// 行形如：8  0  sda  1234  56  7890  120  234  45  6789  90  0  210  0
// 取第 6 列（读扇区）和第 10 列（写扇区）；后面的耗时、队列深度等这里用不到。
func parseDiskStats(section string, devices map[string]bool) (read, write uint64) {
	for _, line := range strings.Split(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !devices[fields[2]] {
			continue
		}
		sectorsRead, err1 := strconv.ParseUint(fields[5], 10, 64)
		sectorsWritten, err2 := strconv.ParseUint(fields[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		read += sectorsRead * diskStatsSectorSize
		write += sectorsWritten * diskStatsSectorSize
	}
	return read, write
}

// df -P -k 的数据行形如：/dev/sda1  102400000  50000000  50000000  50%  /
//
// 表头不按文字识别。它的内容随语言变化（中文环境是"文件系统 1024的块 已用 …"），
// 但结构永远一致：第二列起是数字。这个正则要求第二列必须是数字，表头因此
// 天然不匹配 —— 判定依据是"列结构"而不是"某个语言里的某个词"。
// 挂载点可能含空格，所以用正则整体匹配而不是按空格切分。
var dfLinePattern = regexp.MustCompile(`^(\S+)\s+(\d+)\s+(\d+)\s+(\d+)\s+(\d+)%\s+(.+)$`)

// virtualMountPoints 是内核与运行时暴露的挂载区域，挂在这里的都不是真实存储。
//
// 判"是不是真实存储"看挂载点位置，而不是挂载源的名字：df 对伪文件系统给出的
// 第一列是**设备名**而不是文件系统类型 —— /dev 那一行写的是 "udev"，它的类型
// 其实是 devtmpfs。按名字维护黑名单永远会漏（udev、systemd-1、sunrpc…），
// 而"挂在 /dev、/run、/sys 下"是内核约定，与语言、发行版、内核版本都无关。
var virtualMountPoints = []string{"/dev", "/run", "/sys", "/proc", "/snap"}

// ignoredFilesystems 是已知的伪文件系统类型名，作为兜底。
//
// 有些伪文件系统的挂载点并不在上面那几个区域里（管理员把 tmpfs 挂到 /tmp、
// 把 squashfs 镜像挂到 /opt），那时 df 的第一列恰好就是类型名，用得上这张表。
// 刻意不排除 overlay：容器里它就是实际根文件系统。
var ignoredFilesystems = map[string]bool{
	"tmpfs":      true,
	"devtmpfs":   true,
	"squashfs":   true,
	"iso9660":    true,
	"proc":       true,
	"sysfs":      true,
	"cgroup":     true,
	"cgroup2":    true,
	"debugfs":    true,
	"securityfs": true,
}

// isVirtualFilesystem 判断一个挂载点是否属于伪文件系统。
func isVirtualFilesystem(source, target string) bool {
	for _, prefix := range virtualMountPoints {
		if target == prefix || strings.HasPrefix(target, prefix+"/") {
			return true
		}
	}
	return ignoredFilesystems[lastPathSegment(source)]
}

func parseDF(section string) []Disk {
	disks := make([]Disk, 0)
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		match := dfLinePattern.FindStringSubmatch(trimmed)
		if match == nil {
			continue
		}

		filesystem := match[1]
		mountPoint := match[6]
		if isVirtualFilesystem(filesystem, mountPoint) {
			continue
		}

		blocks, _ := strconv.ParseUint(match[2], 10, 64)
		used, _ := strconv.ParseUint(match[3], 10, 64)
		available, _ := strconv.ParseUint(match[4], 10, 64)
		percent, _ := strconv.ParseFloat(match[5], 64)

		// df -k 的单位固定是 1024 字节块。
		total := blocks * 1024
		if total == 0 {
			continue
		}

		disks = append(disks, Disk{
			Filesystem: filesystem,
			MountPoint: mountPoint,
			Total:      total,
			Used:       used * 1024,
			Available:  available * 1024,
			UsePercent: percent,
		})
	}
	return disks
}

func lastPathSegment(filesystem string) string {
	if idx := strings.LastIndex(filesystem, "/"); idx >= 0 {
		return filesystem[idx+1:]
	}
	return filesystem
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
