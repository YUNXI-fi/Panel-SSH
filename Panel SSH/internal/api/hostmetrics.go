package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"panelssh/internal/sysinfo"
)

const (
	// metricsConcurrency 是在飞的指标采集数上限。
	//
	// 每次采集都要建一条 SSH 会话并在目标机上跑一段命令，与角色探测同量级
	// （见 roleConcurrency），因此取同一个数量级而不是状态探测的 32。
	metricsConcurrency = 8

	// metricsBudget 是整批采集的预算上限。
	//
	// 不可达的主机会一直耗到单次采集超时，逐台等下去会让整个请求挂住。
	// 到点就返回已采到的部分，其余留作「未知」由下一轮补上。
	metricsBudget = 20 * time.Second
)

// hostMetrics 是单台主机的指标摘要，供列表页的卡片视图使用。
//
// 只带卡片要显示的那几个数，不是完整的 Metrics：完整结构里还有挂载点列表、
// 加速设备这些大块内容，按"主机 × 每次轮询"传一遍是纯浪费。
type hostMetrics struct {
	ID int64 `json:"id"`

	// 三个百分比为 null 表示这次没采到（离线、超预算、首次采样尚无基准），
	// 与"采到了，值是 0"必须区分开 —— 后者才是"机器空闲"。
	CPUPercent  *float64 `json:"cpuPercent"`
	MemPercent  *float64 `json:"memPercent"`
	DiskPercent *float64 `json:"diskPercent"`

	// 容量明细：卡片上显示成 "1.3 / 1.73 GB" 这类读数。
	MemUsed   uint64 `json:"memUsed"`
	MemTotal  uint64 `json:"memTotal"`
	DiskUsed  uint64 `json:"diskUsed"`
	DiskTotal uint64 `json:"diskTotal"`

	SampledAt time.Time `json:"sampledAt"`
}

// handleHostMetrics 批量返回主机指标。
//
// 为什么是批量接口而不是让前端逐台调 /api/hosts/{id}/metrics：
// 卡片视图一屏就是十几台，逐台请求意味着十几个并发请求各建一条 SSH 会话，
// 且并发度与超时全都失控。批量把并发上限与整批预算收到服务端一处，
// 前端只发一次请求。这与 /api/hosts/roles 是同一套做法。
//
// 采集复用 sampleMetrics 的 2 秒缓存：监控窗口同时在看这台机器时，
// 只在目标机上跑一次命令，不会因为多了一个列表页就翻倍。
func (s *Server) handleHostMetrics(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询主机失败")
		return
	}

	// ids 是可选的过滤：前端已经按在线状态筛过一轮，离线的主机不必再去连 ——
	// 那只会白白耗到超时，还会在目标机上留下一串失败登录。
	wanted := parseIDFilter(r.URL.Query().Get("ids"))

	type sampled struct {
		index int
		value hostMetrics
	}
	targets := make([]int64, 0, len(hosts))
	for _, host := range hosts {
		if wanted != nil && !wanted[host.ID] {
			continue
		}
		targets = append(targets, host.ID)
	}
	// 宿主机不在主机表里，但列表页把它作为固定的一项展示，
	// 它的读数同样要随这一批一起采。
	if wanted == nil || wanted[LocalHostID] {
		targets = append(targets, LocalHostID)
	}

	ctx, cancel := context.WithTimeout(r.Context(), metricsBudget)
	defer cancel()

	results := make(chan sampled, len(targets))
	sem := make(chan struct{}, metricsConcurrency)
	for i, id := range targets {
		go func(i int, id int64) {
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- sampled{i, s.metricsBriefOf(ctx, id)}
		}(i, id)
	}

	out := make([]hostMetrics, len(targets))
	for i := range targets {
		out[i] = hostMetrics{ID: targets[i]}
	}
	// 已初始化的那份作为兜底：超预算时剩下的格子保持"未知"，而不是零值伪装成 0%。
	for range targets {
		select {
		case item := <-results:
			out[item.index] = item.value
		case <-ctx.Done():
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// parseIDFilter 解析 ids=a,b,c 形式的过滤条件。
//
// 返回 nil 表示"没有过滤"，即要全部主机 —— 与"过滤出一个空集合"必须区分：
// 前者是没传，后者是传了但没有一台匹配。
func parseIDFilter(raw string) map[int64]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	out := make(map[int64]bool)
	for part := range strings.SplitSeq(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		// 0 是宿主机的保留 ID，是合法目标；负数才是无效输入。
		if err != nil || id < 0 {
			continue
		}
		out[id] = true
	}
	return out
}

// metricsBriefOf 采一次指标并压成卡片要的那几个数。
//
// 采集失败（离线、超时）返回只带 ID 的空值：卡片据此显示 —，
// 而不是把失败当成 0% 画出来。
func (s *Server) metricsBriefOf(ctx context.Context, hostID int64) hostMetrics {
	brief := hostMetrics{ID: hostID}

	// 采集要能在预算到点时立刻放弃，因此先判一次 ctx：sampleMetrics 内部
	// 走的是带自身超时的执行路径，这里不额外传 ctx，靠外层预算收口。
	if ctx.Err() != nil {
		return brief
	}

	metrics, err := s.sampleMetrics(hostID, false)
	if err != nil {
		return brief
	}
	return briefOfMetrics(hostID, metrics)
}

// briefOfMetrics 把完整采样压成卡片用的摘要。抽成纯函数便于单测。
func briefOfMetrics(hostID int64, m *sysinfo.Metrics) hostMetrics {
	brief := hostMetrics{ID: hostID, SampledAt: m.SampledAt}
	if m.MemTotal > 0 {
		brief.MemUsed = m.MemUsed
		brief.MemTotal = m.MemTotal
		mem := memPercentOf(m)
		brief.MemPercent = &mem
	}
	// 首次采样没有计数器基准，此时 CPU% 是 0 —— 那是"算不出来"，不是"空闲"。
	if m.DeltaReady {
		cpu := clampPercent(m.CPUPercent)
		brief.CPUPercent = &cpu
	}

	if disk, ok := rootDisk(m.Disks); ok {
		brief.DiskUsed = disk.Used
		brief.DiskTotal = disk.Total
		used := clampPercent(disk.UsePercent)
		brief.DiskPercent = &used
	}
	return brief
}

// rootDisk 挑出代表"这台机器的存储占用"的那个文件系统。
//
// 优先根挂载点；容器或精简系统里没有单独的 / 时退回第一个 ——
// 全都没有（比如采集不到 df 输出）就返回 false，卡片显示 —。
func rootDisk(disks []sysinfo.Disk) (sysinfo.Disk, bool) {
	for _, disk := range disks {
		if disk.MountPoint == "/" && disk.Total > 0 {
			return disk, true
		}
	}
	for _, disk := range disks {
		if disk.Total > 0 {
			return disk, true
		}
	}
	return sysinfo.Disk{}, false
}

// clampPercent 把百分比收进 0–100。
//
// 采样偶尔会给出略超 100 的值（不同内核对同一块盘的统计口径不一致），
// 不夹紧的话进度条会画出格子外面。
func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
