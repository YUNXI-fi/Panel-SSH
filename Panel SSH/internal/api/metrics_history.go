package api

import (
	"sync"
	"time"

	"panelssh/internal/sysinfo"
)

// metricsHistorySize 是每台主机保留的采样点数。
//
// 前端曲线窗口是 40 点（3 秒一轮 ≈ 2 分钟），这里多留一半：为的是"关掉监视窗口
// 几分钟后再打开"仍能回填出一张满的图，而不是从空图重新爬两分钟。
const metricsHistorySize = 60

// metricsPoint 是历史缓冲里的一个采样点。
//
// 只留曲线需要的字段：完整的 Metrics 里还带着挂载点列表这类大块内容，
// 按"主机 × 几十个点"存下来是纯浪费。
type metricsPoint struct {
	// At 是采样时刻的 epoch 毫秒 —— 前端要拿相邻两点的时间差判断曲线该不该断开。
	At         int64   `json:"at"`
	DeltaReady bool    `json:"deltaReady"`
	CPUPercent float64 `json:"cpuPercent"`
	MemPercent float64 `json:"memPercent"`

	NetRxRate     float64 `json:"netRxRate"`
	NetTxRate     float64 `json:"netTxRate"`
	DiskReadRate  float64 `json:"diskReadRate"`
	DiskWriteRate float64 `json:"diskWriteRate"`

	Accel []metricsPointAccel `json:"accel"`
}

// metricsPointAccel 是采样点里的加速设备利用率。
//
// 只存利用率：显存与温度看实时卡片就够了，曲线用不到。
type metricsPointAccel struct {
	Kind        string  `json:"kind"`
	ID          int     `json:"id"`
	Chip        int     `json:"chip"`
	UtilPercent float64 `json:"utilPercent"`
}

// metricsHistory 按主机保留最近若干个采样点，供监控窗口开窗时回填曲线。
//
// 刻意只放在内存里：它服务的是"窗口刚打开、或刚切回某台主机"的那一瞬间。
// 面板重启后从空图重新累积是可接受的，而落库会把一套数据生命周期问题
// （清理、迁移、与主机删除的一致性）引进来。
type metricsHistory struct {
	mu     sync.Mutex
	byHost map[int64][]metricsPoint
}

func newMetricsHistory() *metricsHistory {
	return &metricsHistory{byHost: make(map[int64][]metricsPoint)}
}

// memPercentOf 是内存占用百分比，口径与前端一致。
func memPercentOf(m *sysinfo.Metrics) float64 {
	if m.MemTotal == 0 {
		return 0
	}
	return float64(m.MemUsed) / float64(m.MemTotal) * 100
}

// pointOf 把一次采样压成历史点。
func pointOf(m *sysinfo.Metrics, at time.Time) metricsPoint {
	accel := make([]metricsPointAccel, 0, len(m.Accelerators))
	for _, a := range m.Accelerators {
		// 工具没给利用率时是 -1（未知），原样带出去：前端会把它当断点，
		// 而不是画成 0% —— 0 的含义是"设备在空闲"，两者不能混。
		accel = append(accel, metricsPointAccel{
			Kind: a.Kind, ID: a.ID, Chip: a.Chip, UtilPercent: a.UtilPercent,
		})
	}

	return metricsPoint{
		At:            at.UnixMilli(),
		DeltaReady:    m.DeltaReady,
		CPUPercent:    m.CPUPercent,
		MemPercent:    memPercentOf(m),
		NetRxRate:     m.NetRxRate,
		NetTxRate:     m.NetTxRate,
		DiskReadRate:  m.DiskReadRate,
		DiskWriteRate: m.DiskWriteRate,
		Accel:         accel,
	}
}

// record 记入一个采样点。
//
// 调用点放在"真的跑了一次采集"的地方（缓存命中不会走到那里），
// 因此同一台主机在 2 秒缓存窗口内被多个窗口轮询时，历史里只会有一条。
func (h *metricsHistory) record(hostID int64, m *sysinfo.Metrics, at time.Time) {
	if m == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	list := append(h.byHost[hostID], pointOf(m, at))
	if len(list) > metricsHistorySize {
		list = list[len(list)-metricsHistorySize:]
	}
	h.byHost[hostID] = list
}

// list 返回某台主机最近的采样点（按时间正序）。
//
// 返回副本：调用方拿到的快照不该被后续采样改写。
func (h *metricsHistory) list(hostID int64) []metricsPoint {
	h.mu.Lock()
	defer h.mu.Unlock()

	src := h.byHost[hostID]
	if len(src) == 0 {
		return []metricsPoint{}
	}
	out := make([]metricsPoint, len(src))
	copy(out, src)
	return out
}

// forget 丢弃某台主机的历史。
// 主机被删除或地址/凭据变更后必须调用，否则会把旧机器的曲线当成它的现状。
func (h *metricsHistory) forget(hostID int64) {
	h.mu.Lock()
	delete(h.byHost, hostID)
	h.mu.Unlock()
}
