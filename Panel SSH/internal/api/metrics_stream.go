package api

import (
	"context"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"panelssh/internal/sysinfo"
)

const (
	// 采样间隔的允许区间。
	//
	// 下限对齐采集器的 minDeltaInterval（150ms）：比它更快时，两次 /proc 读数之差
	// 里只剩计数器自身的粒度，CPU% 会抖成锯齿。上限挡住"档位填个几天"这类值 ——
	// 那种流会一直占着一条 SSH 会话却几乎不产出数据。
	metricsStreamMinInterval = 200 * time.Millisecond
	metricsStreamMaxInterval = 30 * time.Second

	// metricsStreamHeavySeconds 是重量分节（df / nvidia-smi / npu-smi）的目标补采周期。
	//
	// 它与采样档位解耦：档位调快只该让曲线更密，不该让 df 也跟着跑得更勤 ——
	// 否则"高速档"就变成了"高速压榨目标机"。
	metricsStreamHeavySeconds = 30

	// metricsStreamReconnect 是长驻流断开后的重连间隔。
	metricsStreamReconnect = 3 * time.Second

	// metricsStreamMaxRun 是单次长驻执行的兜底上限。
	// 流的真实生命周期由订阅者决定（见 metricsStream.ctx），这个值只防"某条流
	// 因异常路径被遗忘后永远挂着"。
	metricsStreamMaxRun = 4 * time.Hour

	// metricsStreamSubBuffer 是单个订阅者的消息缓冲深度。
	// 取小值是有意的：订阅者来不及消费时应当丢点，而不是让采集循环停下来等它。
	metricsStreamSubBuffer = 4

	// maxStreamRoundBytes 是单轮输出的缓冲上限。
	// 正常一轮只有几 KB；设上限是防"远端输出里始终没有出现轮次标记"时这个缓冲
	// 无界增长 —— execOnHostStream 里的 limitedBuffer 保护的是它自己的正文，不是这里。
	maxStreamRoundBytes = 256 * 1024
)

// metricsStreamStart 是客户端的启动消息。
type metricsStreamStart struct {
	Type       string `json:"type"`
	HostID     int64  `json:"hostId"`
	IntervalMS int    `json:"intervalMs"`
}

// metricsStreamEvent 是推给前端的一条流事件。
type metricsStreamEvent struct {
	// Type 取 sample 或 error。前端只把 error 当作"这条流当前不通"，
	// 不当作需要打断用户的故障 —— 重连成功后一切照旧。
	Type    string           `json:"type"`
	HostID  int64            `json:"hostId"`
	Metrics *sysinfo.Metrics `json:"metrics,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// metricsStreamKey 标识一条可复用的流：同一台主机 + 同一个采样档位。
//
// 把档位并进 key，而不是"按主机一条、取最密的那个档"：不同档位要在远端 sleep
// 不同的时长，合成一条就得在订阅者增减时重启远端脚本 —— 那点开销比多跑一条
// 轻量流还大，而轻量流本身几乎不消耗目标机。
type metricsStreamKey struct {
	hostID   int64
	interval time.Duration
}

// metricsStreamManager 管理按 (主机, 档位) 复用的指标长驻流。
//
// 为什么必须复用：流不是"每个窗口一条"。十个窗口看同一台机器就建十条 SSH 长会话、
// 跑十份采集脚本，比改造之前的轮询还费目标机。这里让第一个订阅者把流开起来、
// 最后一个订阅者离开时把它停掉，中间的订阅者共享同一条流的解析结果。
type metricsStreamManager struct {
	srv *Server

	mu      sync.Mutex
	streams map[metricsStreamKey]*metricsStream
}

func newMetricsStreamManager(srv *Server) *metricsStreamManager {
	return &metricsStreamManager{srv: srv, streams: make(map[metricsStreamKey]*metricsStream)}
}

// subscribe 订阅一条流，必要时先把它启动起来。
func (m *metricsStreamManager) subscribe(key metricsStreamKey) *metricsSub {
	m.mu.Lock()
	st := m.streams[key]
	if st == nil {
		st = newMetricsStream(m, key)
		m.streams[key] = st
		go st.run()
	}
	sub := st.addSub()
	m.mu.Unlock()
	return sub
}

// unsubscribe 退订；引用计数归零时停掉流，并从表里摘掉。
func (m *metricsStreamManager) unsubscribe(key metricsStreamKey, sub *metricsSub) {
	m.mu.Lock()
	defer m.mu.Unlock()

	st := m.streams[key]
	if st == nil {
		return
	}
	// 只在"这条流确实已经没人订阅"时才摘除表项。并发的 subscribe 与 unsubscribe
	// 交错时，删掉一条还有人用的流会让那些人永远收不到数据。
	if st.removeSub(sub) {
		delete(m.streams, key)
	}
}

// forget 停掉某台主机的全部流（所有档位）。
//
// 主机被删除、地址或凭据变更后必须调用，理由与采样缓存、指标历史的 forget 相同：
// 那条长驻会话是按旧配置建立的，留着它既连不上新地址，又会把旧机器的数据当成
// 现状继续推给界面。
func (m *metricsStreamManager) forget(hostID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, st := range m.streams {
		if key.hostID != hostID {
			continue
		}
		st.revoke()
		delete(m.streams, key)
	}
}

// closeAll 停掉所有流，供优雅退出时调用。
func (m *metricsStreamManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, st := range m.streams {
		st.revoke()
		delete(m.streams, key)
	}
}

// metricsSub 是一个订阅者。事件从 ch 上取。
type metricsSub struct {
	ch chan metricsStreamEvent

	// done 在整条流被撤销（主机被删、地址或凭据变更、面板退出）时关闭。
	//
	// 处理器据此结束这条连接，前端随后重连并按新配置重新订阅。少了它，
	// 界面会停在最后一帧 —— 看起来像"监控坏了"，其实那条流已经不存在了。
	done chan struct{}
}

// metricsStream 是一条按 (主机, 档位) 复用的长驻采集流。
type metricsStream struct {
	mgr *metricsStreamManager
	key metricsStreamKey

	// ctx 的生命周期绑的是"还有没有人在看"，不是某条 WebSocket 连接。
	// 用 Background 而不是请求的 ctx：一个订阅者断开时，别的订阅者可能还在看。
	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	subs map[*metricsSub]struct{}
	// revoked 表示整条流已被撤销（主机被删、配置变更或面板退出）。
	// 它挡住重复撤销：重复关闭 sub.done 会 panic。
	revoked bool
	// 最近一次重量采集的结果，供降频的那些轮次复用（见 mergeHeavy）。
	lastDisks []sysinfo.Disk
	lastAccel []sysinfo.Accelerator
}

func newMetricsStream(mgr *metricsStreamManager, key metricsStreamKey) *metricsStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &metricsStream{
		mgr:    mgr,
		key:    key,
		ctx:    ctx,
		cancel: cancel,
		subs:   make(map[*metricsSub]struct{}),
	}
}

func (st *metricsStream) addSub() *metricsSub {
	sub := &metricsSub{
		ch:   make(chan metricsStreamEvent, metricsStreamSubBuffer),
		done: make(chan struct{}),
	}
	st.mu.Lock()
	st.subs[sub] = struct{}{}
	st.mu.Unlock()
	return sub
}

// removeSub 移除订阅者；返回 true 表示这条流已经没人订阅、应当停止。
func (st *metricsStream) removeSub(sub *metricsSub) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	if _, ok := st.subs[sub]; !ok {
		return false
	}
	delete(st.subs, sub)
	if len(st.subs) == 0 {
		st.cancel()
		return true
	}
	return false
}

// revoke 撤销整条流：停掉采集循环，并让所有订阅者结束自己的连接。
//
// 与"最后一个订阅者离开"（removeSub）的区别在于主动方：那是"没人要了"，
// 这是"这条流依据的配置已经不作数了"—— 主机被删、地址或凭据被改、面板要退出。
// 两种情况都要让前端重新来一次：前者的连接已经断开，后者还在连着却再也收不到数据。
func (st *metricsStream) revoke() {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.revoked {
		return
	}
	st.revoked = true

	st.cancel()
	for sub := range st.subs {
		close(sub.done)
	}
}

// run 是流的采集循环：连上、采集、断开、再连。
//
// 断线不通知用户也不上报错误：网络抖一下、目标机重启 sshd，几秒后就自己恢复了。
// 为这种事弹一次窗，换来的只是让用户学会忽略弹窗。
func (st *metricsStream) run() {
	defer st.cancel()

	for {
		if st.ctx.Err() != nil {
			return
		}

		st.pump()

		select {
		case <-st.ctx.Done():
			return
		case <-time.After(metricsStreamReconnect):
		}
	}
}

// pump 跑一轮长驻采集，直到流被取消、远端断开或撞上兜底上限。
func (st *metricsStream) pump() {
	// 轮次缓冲：远端按 MetricsTickMarker 分轮，收满一轮才交给解析。
	// onLine 由 execOnHostStream 串行调用，这里不需要额外加锁。
	var round strings.Builder

	onLine := func(line string) {
		if strings.TrimSpace(line) == sysinfo.MetricsTickMarker {
			st.settle(round.String())
			round.Reset()
			return
		}
		// 输出里始终没有轮次标记时（远端 shell 异常），不能让缓冲无界增长。
		if round.Len() > maxStreamRoundBytes {
			round.Reset()
			return
		}
		round.WriteString(line)
		round.WriteByte('\n')
	}

	_, err := st.mgr.srv.execOnHostStream(
		st.ctx, st.key.hostID, st.command(), metricsStreamMaxRun, onLine)

	// ctx 已取消属于正常收尾（订阅者走光了），不当故障广播。
	if err != nil && st.ctx.Err() == nil {
		st.notifyError(err.Error())
	}
}

// command 生成这一档位对应的长驻采集脚本。
func (st *metricsStream) command() string {
	seconds := st.key.interval.Seconds()

	// 重量分节的补采周期与档位无关，换算成"每多少轮补一次"。
	// 取 max(1, ...)：档位本身比补采周期还慢时（例如间隔 30 秒），每轮都该带重量分节。
	heavyEvery := max(1, int(math.Round(metricsStreamHeavySeconds/seconds)))
	return sysinfo.StreamingMetricsCommand(seconds, heavyEvery)
}

// settle 结算一轮原始输出：解析成指标、喂给缓存与历史、广播给订阅者。
func (st *metricsStream) settle(raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}

	at := time.Now()
	m, err := st.mgr.srv.collector.Collect(st.key.hostID, raw, at)
	if err != nil {
		st.notifyError(err.Error())
		return
	}

	st.mergeHeavy(m)

	// 与非流式消费者对齐：AI 助手的 get_metrics 读的就是这份缓存与历史，
	// 不写回去的话，"AI 看到的指标"会和监控窗口显示的对不上。
	st.mgr.srv.metrics.put(st.key.hostID, m)
	st.mgr.srv.history.record(st.key.hostID, m, at)

	st.broadcast(m)
}

// mergeHeavy 用最近一次重量采集的结果，补上本轮缺失的磁盘与加速器信息。
//
// 磁盘容量与 GPU 利用率是每 30 秒才采一次的重数据，而曲线是每秒一个点。
// 不做这层沿用的话，那两块内容会在图上忽有忽无 —— 看起来像故障，其实是降频采集
// 的正常表现。反过来，本轮真的采到了重量数据就更新基准，供后面几十轮复用。
func (st *metricsStream) mergeHeavy(m *sysinfo.Metrics) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if len(m.Disks) > 0 {
		st.lastDisks = m.Disks
	} else if st.lastDisks != nil {
		m.Disks = st.lastDisks
	}

	if len(m.Accelerators) > 0 {
		st.lastAccel = m.Accelerators
	} else if st.lastAccel != nil {
		m.Accelerators = st.lastAccel
	}
}

// broadcast 把一个采样点推给所有订阅者。
//
// 订阅者来不及消费时丢点而不是阻塞：监控曲线丢一两个点无所谓，
// 让采集循环停下来等最慢的那个窗口才是真问题。
func (st *metricsStream) broadcast(m *sysinfo.Metrics) {
	ev := metricsStreamEvent{Type: "sample", HostID: st.key.hostID, Metrics: m}

	st.mu.Lock()
	defer st.mu.Unlock()
	for sub := range st.subs {
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

// notifyError 把"这条流当前不通"告诉订阅者。
//
// 只发状态、不弹错误：前端据此把监控窗口标成"重连中"，而不是打断用户。
func (st *metricsStream) notifyError(msg string) {
	ev := metricsStreamEvent{Type: "error", HostID: st.key.hostID, Error: msg}

	st.mu.Lock()
	defer st.mu.Unlock()
	for sub := range st.subs {
		select {
		case sub.ch <- ev:
		default:
		}
	}
}

// clampStreamInterval 把前端给的档位收敛到允许区间。
func clampStreamInterval(ms int) time.Duration {
	d := time.Duration(ms) * time.Millisecond
	if d < metricsStreamMinInterval {
		return metricsStreamMinInterval
	}
	if d > metricsStreamMaxInterval {
		return metricsStreamMaxInterval
	}
	return d
}

// CloseMetricsStreams 停掉所有指标长驻流，供优雅退出时调用。
//
// 为什么不能只靠关闭 WebSocket 连接：流的生命周期不挂在某一条连接上（一条流可能
// 被多个窗口共享），关连接只让处理器返回。退出前显式收掉它们，才不会留下
// "面板已经退了、目标机上还挂着一个采集循环"的残局。
func (s *Server) CloseMetricsStreams() {
	s.metricsStreams.closeAll()
}

// handleMetricsStream 把指标以长驻流的方式持续推给前端。
//
// 与 GET /api/hosts/{id}/metrics 的分工：那条回答"现在是多少"（AI 工具、单次采样），
// 这条回答"持续给我"（监控窗口）。两者读的是同一份采样缓存与指标历史。
func (s *Server) handleMetricsStream(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(1 << 16)
	defer conn.CloseNow()

	if !s.registerSocket(conn) {
		_ = conn.Close(websocket.StatusGoingAway, "服务正在关闭")
		return
	}
	defer s.unregisterSocket(conn)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var start metricsStreamStart
	if err := wsjson.Read(ctx, conn, &start); err != nil {
		return
	}
	if start.Type != "start" {
		_ = conn.Close(websocket.StatusUnsupportedData, "首条消息必须是 start")
		return
	}

	// 主机必须存在。连接已经升级成 WebSocket，不能再走 HTTP 错误响应，
	// 因此把原因当作一条普通事件发回去。
	if start.HostID != LocalHostID {
		if _, err := s.store.GetHost(start.HostID); err != nil {
			_ = wsjson.Write(ctx, conn, metricsStreamEvent{
				Type:   "error",
				HostID: start.HostID,
				Error:  "主机不存在",
			})
			return
		}
	}

	key := metricsStreamKey{hostID: start.HostID, interval: clampStreamInterval(start.IntervalMS)}
	sub := s.metricsStreams.subscribe(key)
	defer s.metricsStreams.unsubscribe(key, sub)

	// 客户端不会再发消息，CloseRead 让"对端关闭"直接反映到 ctx 上 ——
	// 下面写失败时才能及时退出，而不是等一次写超时。
	writeCtx := conn.CloseRead(ctx)
	writer := &wsWriter{conn: conn}

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.done:
			// 流被整体撤销（主机被删、地址或凭据变更、面板退出）：结束这条连接，
			// 前端重连后按新配置重新订阅。继续挂着只会让界面冻在最后一帧。
			return
		case ev := <-sub.ch:
			if err := writer.write(writeCtx, ev); err != nil {
				return
			}
		}
	}
}
