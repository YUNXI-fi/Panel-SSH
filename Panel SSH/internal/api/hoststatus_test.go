package api

import "testing"

// 主机上下线告警的防抖是这套通知里最容易写错的地方：
// 阈值、计数清零、重复告警抑制三件事互相牵连，必须逐一钉死。

func TestHostWatchFirstProbeIsSilent(t *testing.T) {
	w := newHostWatch()
	const id int64 = 1

	// 首次探测只建立基准。那时"上一次状态"是未知的，而不是"在线"，
	// 据此报出"离线"属于凭空捏造。
	if got := w.observe(id, true); got != hostEventNone {
		t.Fatalf("首次成功探测不应产生事件，得到 %v", got)
	}
}

func TestHostWatchFirstProbeFailureIsSilent(t *testing.T) {
	w := newHostWatch()
	const id int64 = 2

	// 首次探测就失败：只建立"当前离线"的基准，不报事件。
	// 用户刚添加的主机如果本来就不通，他在主机列表里直接能看到，不需要通知再说一遍。
	if got := w.observe(id, false); got != hostEventNone {
		t.Fatalf("首次失败探测不应产生事件，得到 %v", got)
	}
	// 从未上线过的主机，失败再多次也不该报"离线" —— 它没有"变为离线"这个变化。
	for i := 0; i < 5; i++ {
		if got := w.observe(id, false); got != hostEventNone {
			t.Fatalf("第 %d 次失败不应报离线，得到 %v", i+1, got)
		}
	}
}

func TestHostWatchOfflineNeedsConsecutiveFailures(t *testing.T) {
	w := newHostWatch()
	const id int64 = 3

	if got := w.observe(id, true); got != hostEventNone {
		t.Fatalf("建立基准不应产生事件，得到 %v", got)
	}

	// 低于阈值的失败只是抖动，不能报离线 —— 这正是防抖存在的理由。
	for i := 1; i < hostOfflineThreshold; i++ {
		if got := w.observe(id, false); got != hostEventNone {
			t.Fatalf("第 %d 次失败（阈值 %d）不应报离线，得到 %v", i, hostOfflineThreshold, got)
		}
	}

	if got := w.observe(id, false); got != hostEventOffline {
		t.Fatalf("连续 %d 次失败应报离线，得到 %v", hostOfflineThreshold, got)
	}
}

func TestHostWatchOfflineIsNotRepeated(t *testing.T) {
	w := newHostWatch()
	const id int64 = 4

	w.observe(id, true)
	for i := 0; i < hostOfflineThreshold; i++ {
		w.observe(id, false)
	}

	// 已判定离线后继续失败不再告警：一台关机过夜的机器会一直失败，
	// 每轮推一条通知就成了刷屏。
	for i := 0; i < 5; i++ {
		if got := w.observe(id, false); got != hostEventNone {
			t.Fatalf("离线后第 %d 次失败不应重复告警，得到 %v", i+1, got)
		}
	}
}

func TestHostWatchRecoversOnFirstSuccess(t *testing.T) {
	w := newHostWatch()
	const id int64 = 5

	w.observe(id, true)
	for i := 0; i < hostOfflineThreshold; i++ {
		w.observe(id, false)
	}

	// 恢复方向不设阈值：一次成功即判上线。
	// "误报恢复"的代价远小于"漏报掉线"，而且恢复通知本身不会造成骚扰。
	if got := w.observe(id, true); got != hostEventOnline {
		t.Fatalf("恢复后应报上线，得到 %v", got)
	}
	if got := w.observe(id, true); got != hostEventNone {
		t.Fatalf("已在线时成功探测不应重复报上线，得到 %v", got)
	}
}

func TestHostWatchFailureStreakResetsOnSuccess(t *testing.T) {
	w := newHostWatch()
	const id int64 = 6

	w.observe(id, true)

	// 失败两次后成功一次，连续失败计数必须清零。
	w.observe(id, false)
	w.observe(id, false)
	if got := w.observe(id, true); got != hostEventNone {
		t.Fatalf("仍在线时成功探测不应产生事件，得到 %v", got)
	}

	// 若上面那次成功没把计数清零，这里失败一次就会提前触发离线。
	for i := 1; i < hostOfflineThreshold; i++ {
		if got := w.observe(id, false); got != hostEventNone {
			t.Fatalf("计数清零后第 %d 次失败不应报离线，得到 %v", i, got)
		}
	}
	if got := w.observe(id, false); got != hostEventOffline {
		t.Fatalf("重新累计到 %d 次失败应报离线，得到 %v", hostOfflineThreshold, got)
	}
}

func TestHostWatchForgetDropsBaseline(t *testing.T) {
	w := newHostWatch()
	const id int64 = 7

	w.observe(id, true) // 基准：在线
	w.forget(id)

	// forget 必须真的丢掉基准。若旧状态还在（在线、计数为 0），
	// 下面第三次失败就会报出一次离线告警 —— 而这时主机其实从未被成功探测过。
	for i := 1; i <= hostOfflineThreshold+1; i++ {
		if got := w.observe(id, false); got != hostEventNone {
			t.Fatalf("forget 后第 %d 次失败不应报离线，得到 %v", i, got)
		}
	}

	// 之后成功一次应报"恢复"：证明 forget 后重新建立了基准。
	if got := w.observe(id, true); got != hostEventOnline {
		t.Fatalf("forget 后首次恢复应报上线，得到 %v", got)
	}
}

func TestHostWatchIsolatesHosts(t *testing.T) {
	w := newHostWatch()

	w.observe(10, true)
	w.observe(20, true)

	// 一台主机的连续失败不该影响另一台的判定。
	for i := 0; i < hostOfflineThreshold; i++ {
		w.observe(10, false)
	}
	if got := w.observe(10, false); got != hostEventNone {
		t.Fatalf("已离线的主机不应重复告警，得到 %v", got)
	}
	if got := w.observe(20, true); got != hostEventNone {
		t.Fatalf("另一台主机保持在线，不应产生事件，得到 %v", got)
	}
}
