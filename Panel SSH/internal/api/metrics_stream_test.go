package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"panelssh/internal/sysinfo"
)

// clampStreamInterval 必须把越界档位收敛，而不是照单全收：
// 太快会让 CPU% 抖成锯齿（两次 /proc 读数之差里只剩计数器自身的粒度），
// 太慢则等于开了一条几乎不产出数据的常驻会话。
func TestClampStreamInterval(t *testing.T) {
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{0, metricsStreamMinInterval},
		{50, metricsStreamMinInterval},
		{200, 200 * time.Millisecond},
		{1000, time.Second},
		{5000, 5 * time.Second},
		{60000, metricsStreamMaxInterval},
	}
	for _, c := range cases {
		if got := clampStreamInterval(c.ms); got != c.want {
			t.Errorf("clampStreamInterval(%d) = %v，期望 %v", c.ms, got, c.want)
		}
	}
}

// 两个订阅者要收到同一个采样点，而不是各收一份 —— 这是"流级去重"的核心。
// 若退化成每窗口一条流，十个窗口就是十条 SSH 长会话、十份采集脚本。
func TestMetricsStreamFansOutToAllSubscribers(t *testing.T) {
	st := newMetricsStream(nil, metricsStreamKey{hostID: 1, interval: time.Second})
	a := st.addSub()
	b := st.addSub()

	st.broadcast(&sysinfo.Metrics{Hostname: "demo"})

	for name, sub := range map[string]*metricsSub{"a": a, "b": b} {
		select {
		case ev := <-sub.ch:
			if ev.Metrics == nil || ev.Metrics.Hostname != "demo" {
				t.Fatalf("订阅者 %s 收到的事件不对：%+v", name, ev)
			}
			if ev.HostID != 1 {
				t.Fatalf("事件应带上主机 ID，实际 %d", ev.HostID)
			}
		default:
			t.Fatalf("订阅者 %s 没有收到采样点", name)
		}
	}
}

// 订阅者来不及消费时应当丢点，绝不能阻塞采集循环去等最慢的那个窗口。
// 这个测试若不丢点就会直接死锁 —— 那正是要防的行为。
func TestMetricsStreamBroadcastNeverBlocks(t *testing.T) {
	st := newMetricsStream(nil, metricsStreamKey{hostID: 1, interval: time.Second})
	sub := st.addSub() // 刻意不消费

	for range metricsStreamSubBuffer * 4 {
		st.broadcast(&sysinfo.Metrics{})
	}

	if len(sub.ch) != metricsStreamSubBuffer {
		t.Fatalf("缓冲应停在深度 %d，实际 %d", metricsStreamSubBuffer, len(sub.ch))
	}
}

// 引用计数决定流的生死：还有人订阅就不能停，最后一个走了才停。
func TestMetricsStreamStopsOnlyAfterLastSubscriber(t *testing.T) {
	st := newMetricsStream(nil, metricsStreamKey{hostID: 1, interval: time.Second})
	a := st.addSub()
	b := st.addSub()

	if stop := st.removeSub(a); stop {
		t.Fatal("还有订阅者时不该停流")
	}
	if st.ctx.Err() != nil {
		t.Fatal("还有订阅者时 ctx 不该被取消")
	}

	if stop := st.removeSub(b); !stop {
		t.Fatal("最后一个订阅者离开时应当停流")
	}
	if st.ctx.Err() == nil {
		t.Fatal("最后一个订阅者离开后 ctx 应当被取消")
	}

	// 重复退订必须安全：HTTP 与 WebSocket 的清理路径都可能再调一次。
	if stop := st.removeSub(b); stop {
		t.Fatal("重复退订不该报告「应当停流」")
	}
}

// 降频轮次必须沿用上一次的重量数据，否则磁盘容量与 GPU 利用率会在曲线上忽有忽无 ——
// 看起来像故障，其实只是降频采集的正常表现。
func TestMetricsStreamMergeHeavy(t *testing.T) {
	st := newMetricsStream(nil, metricsStreamKey{hostID: 1, interval: time.Second})

	// 首轮带重量数据：记下基准。
	st.mergeHeavy(&sysinfo.Metrics{Disks: []sysinfo.Disk{{MountPoint: "/"}}})

	// 之后的轻量轮次都不带：应当被补上。
	light := &sysinfo.Metrics{}
	st.mergeHeavy(light)
	if len(light.Disks) != 1 || light.Disks[0].MountPoint != "/" {
		t.Fatalf("缺失的重量数据应当被沿用，实际：%+v", light.Disks)
	}

	// 重新采到重量数据时要更新基准，而不是一直用最初那份。
	st.mergeHeavy(&sysinfo.Metrics{Disks: []sysinfo.Disk{{MountPoint: "/data"}}})
	next := &sysinfo.Metrics{}
	st.mergeHeavy(next)
	if len(next.Disks) != 1 || next.Disks[0].MountPoint != "/data" {
		t.Fatalf("重量基准应当被更新，实际：%+v", next.Disks)
	}
}

// 宿主机上的整条流式链路：长驻脚本 → 逐行切轮 → 解析 → 缓存/历史 → 广播。
//
// 这条路径不需要 SSH，因此能在测试里完整跑一遍，而它恰好覆盖最容易出错的那段：
// 轮次切分。少切一轮或多切一轮，在界面上只表现为曲线缺一个点或冒一个尖刺，
// 真机上极难反查到切分逻辑。
func TestMetricsStreamPumpsLocalHost(t *testing.T) {
	s := &Server{
		collector: sysinfo.NewCollector(),
		metrics:   newSampleBuffer[*sysinfo.Metrics](metricsTTL),
		history:   newMetricsHistory(),
	}
	st := newMetricsStream(newMetricsStreamManager(s), metricsStreamKey{
		hostID:   LocalHostID,
		interval: 200 * time.Millisecond,
	})

	// 覆盖 ctx，好让 pump 在测试收尾时退出（真实场景里由订阅者离开触发）。
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	st.ctx = ctx

	sub := st.addSub()
	go st.pump()

	var got []*sysinfo.Metrics
	deadline := time.After(10 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-sub.ch:
			if ev.Type != "sample" || ev.Metrics == nil {
				t.Fatalf("应当收到采样事件，得到 %+v", ev)
			}
			got = append(got, ev.Metrics)
		case <-deadline:
			t.Fatalf("超时前只收到 %d 个采样点", len(got))
		}
	}

	last := got[len(got)-1]
	if last.CPUCores <= 0 || last.MemTotal == 0 {
		t.Fatalf("宿主机的静态信息没有读到：%+v", last)
	}
	// 第一个点只建立基准，第二个点才算得出速率。收到两个点却仍无速率，
	// 说明两次采样之间的时间差没有被正确传下去。
	if !last.DeltaReady {
		t.Fatal("第二个采样点应当已经能算出速率")
	}

	// 非流式消费者必须看到同一份数据：AI 助手的 get_metrics 读的就是这份缓存。
	cached, err := s.metrics.get(LocalHostID, func() (*sysinfo.Metrics, error) {
		return nil, errors.New("缓存里应当已有流写入的点，不该回落到重新采集")
	})
	if err != nil {
		t.Fatalf("采样缓存应当已被流写入：%v", err)
	}
	if cached.MemTotal != last.MemTotal {
		t.Fatal("流写入缓存的值与广播出去的值不一致")
	}
	if points := s.history.list(LocalHostID); len(points) == 0 {
		t.Fatal("流式采样点应当同时记入指标历史")
	}
}

// 撤销整条流必须让"还连着的"订阅者也结束，否则界面会冻在最后一帧：
// 连接还在，数据永远不来。主机被删、地址或凭据被改都会走到这里。
func TestMetricsStreamRevokeEndsSubscribers(t *testing.T) {
	st := newMetricsStream(nil, metricsStreamKey{hostID: 1, interval: time.Second})
	sub := st.addSub()

	st.revoke()

	select {
	case <-sub.done:
	default:
		t.Fatal("撤销后订阅者应当收到结束信号")
	}
	if st.ctx.Err() == nil {
		t.Fatal("撤销后采集循环应当被停掉")
	}

	// 重复撤销必须安全：forget 与 closeAll 可能先后都碰到同一条流，
	// 而重复关闭一个 channel 会 panic。
	st.revoke()
}
