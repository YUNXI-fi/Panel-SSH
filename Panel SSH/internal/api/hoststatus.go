package api

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// statusProbeTimeout 是单台主机 TCP 探测的上限。
	// 探测只建 TCP 连接，正常主机在几十毫秒内就有结论，2 秒足够覆盖跨地域线路；
	// 超过它基本等于不可达，再等下去只会拖长整个列表的响应。
	statusProbeTimeout = 2 * time.Second

	// statusTTL 是探测结果的缓存窗口。
	//
	// 前端按 15 秒轮询，缓存让同一轮里的多个窗口、多个客户端共享一次探测，
	// 而不是各连一遍目标机。窗口本身要比轮询间隔短，否则状态会比轮询还旧。
	statusTTL = 10 * time.Second

	// statusConcurrency 是在飞的探测数上限。TCP 建连开销小，但也不能不设限：
	// 几百台主机同时拨号会瞬间占满本机端口与文件描述符。
	statusConcurrency = 32
)

// hostStatus 是单台主机的在线状态。
//
// LatencyMs 只在 Online 为 true 时有意义，且不加 omitempty —— 本机回环
// 恰好 0 毫秒是合法结果，省略字段会让前端分不清"没有耗时"和"耗时为零"。
//
// 它是带小数的毫秒：局域网里一次建连只有零点几毫秒，取整会把这类结果压成
// "0 ms"，界面上看起来就像没测到数据。
type hostStatus struct {
	ID        int64   `json:"id"`
	Online    bool    `json:"online"`
	LatencyMs float64 `json:"latencyMs"`
}

// probeHostPort 探测主机的 SSH 端口是否可连接，返回 TCP 建连耗时。
//
// 只做 TCP 连接，不握手也不认证：状态列回答的是"这台机器现在通不通"。
// 用完整 SSH 握手会带来两个问题：一是每台主机每轮都要付出密钥交换与认证的
// 开销，二是凭据填错的主机会被显示成"下线" —— 机器明明在线，只是登录不上，
// 那是「测试连接」要回答的问题，不是在线状态。
func probeHostPort(id int64, addr string, port int) hostStatus {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, strconv.Itoa(port)), statusProbeTimeout)
	if err != nil {
		return hostStatus{ID: id, Online: false}
	}
	_ = conn.Close()
	return hostStatus{ID: id, Online: true, LatencyMs: latencyMs(time.Since(start))}
}

// hostOfflineThreshold 是判定一台主机下线所需的连续探测失败次数。
//
// 单次 TCP 探测失败完全不能说明机器下线 —— 网络抖动、目标机瞬时繁忙、
// 甚至本机端口耗尽都可能造成一次失败。若每次失败都推通知，通知中心会被
// 误报淹没，而用户被误报教育几次之后就再也不看通知了。这是通知系统
// 最常见的死法，必须靠"连续失败才判定"来避免。
//
// 前端每 15 秒轮询一次，3 次约 45 秒：既滤掉瞬时抖动，也不至于让真的掉线
// 拖太久才被发现。恢复方向则不设阈值 —— 一次成功即可判为在线，
// "误报恢复"的代价远小于"漏报掉线"，而且恢复通知本身不会造成骚扰。
const hostOfflineThreshold = 3

// hostEvent 是一次探测所判定出的状态变化。
type hostEvent int

const (
	// hostEventNone 表示状态未变，或尚未建立基准，无需通知。
	hostEventNone hostEvent = iota
	hostEventOffline
	hostEventOnline
)

// hostWatchState 是一台主机的告警判定状态。
type hostWatchState struct {
	// online 是判定结果，只有连续失败达到阈值才翻成 false。
	// 它不等于"最近一次探测结果" —— 后者可能只是一次抖动。
	online bool
	// fails 是连续失败次数，任何一次成功都清零。
	fails int
}

// hostWatch 保存各主机的在线判定状态。
//
// 必须活得比单次请求长：判定"连续 N 次失败"要求记住前几次的结果，
// 而每次探测都发生在独立的 HTTP 请求里。
// 状态只存在内存中，进程重启后重新建立基准 —— 重启本身就会打断探测序列，
// 把上一次的状态带到新的进程里反而会凭空造出一次"状态变化"。
type hostWatch struct {
	mu     sync.Mutex
	states map[int64]*hostWatchState
}

func newHostWatch() *hostWatch {
	return &hostWatch{states: make(map[int64]*hostWatchState)}
}

// observe 记录一次真实探测结果，返回由此产生的告警事件。
//
// 首次见到一台主机时只建立基准、不返回事件：那时"上一次状态"是未知的，
// 而不是"在线"，据此报出"离线"属于凭空捏造。
func (w *hostWatch) observe(id int64, online bool) hostEvent {
	w.mu.Lock()
	defer w.mu.Unlock()

	state, ok := w.states[id]
	if !ok {
		w.states[id] = &hostWatchState{online: online}
		return hostEventNone
	}

	if online {
		state.fails = 0
		if state.online {
			return hostEventNone
		}
		state.online = true
		return hostEventOnline
	}

	state.fails++
	// 已判定离线的主机继续失败不再重复告警：一台关机过夜的机器会一直失败，
	// 每轮推一条通知就成了刷屏。
	if !state.online || state.fails < hostOfflineThreshold {
		return hostEventNone
	}
	state.online = false
	return hostEventOffline
}

// forget 丢弃一台主机的判定状态。主机被删除后调用，避免残留条目。
func (w *hostWatch) forget(id int64) {
	w.mu.Lock()
	delete(w.states, id)
	w.mu.Unlock()
}

// handleHostStatus 返回全部主机的在线状态，并据此产生上下线通知。
//
// 结果按主机缓存：前端定时轮询，同一台主机在缓存窗口内只探一次。
func (s *Server) handleHostStatus(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询主机失败")
		return
	}

	results := make([]hostStatus, len(hosts))
	// events 与 results 同长，每个 goroutine 只写自己那一格，因此无需加锁。
	events := make([]hostEvent, len(hosts))
	sem := make(chan struct{}, statusConcurrency)
	var wg sync.WaitGroup

	for i, host := range hosts {
		wg.Add(1)
		go func(i int, id int64, addr string, port int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 探测本身不返回错误：连不上就是"下线"这个结论，不是失败，
			// 因此结果会正常进缓存，供本轮其余请求复用。
			status, _ := s.status.get(id, func() (hostStatus, error) {
				st := probeHostPort(id, addr, port)
				// 告警判定只挂在真正拨号的这一次上。缓存命中会把同一个结果
				// 重复上报给观察者，若在缓存命中处也计数，多开几个浏览器标签
				// 就会让"连续失败次数"成倍增长，防抖形同虚设。
				events[i] = s.hostWatch.observe(id, st.Online)
				return st, nil
			})
			results[i] = status
		}(i, host.ID, host.Addr, host.Port)
	}
	wg.Wait()

	// 通知在主协程串行发出，而不是各 goroutine 里各写各的库：
	// 写入顺序与主机列表一致，也避免并发写同一个数据库。
	for i, host := range hosts {
		switch events[i] {
		case hostEventOffline:
			s.notifyHostOffline(host.Name, host.Addr)
		case hostEventOnline:
			s.notifyHostOnline(host.Name, host.Addr, results[i].LatencyMs)
		}
	}

	writeJSON(w, http.StatusOK, results)
}
