package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"panelssh/internal/sysinfo"
)

// expireSilence 把某台主机的静默窗口拨到过去，模拟"退避到点了"。
//
// 不用真的等 5 分钟：这里要验证的是"到点之后会重新尝试"这条路径本身，
// 而窗口的长度由 ovsFailTTL 一个常量决定，不该在用例里再复述一遍。
func expireSilence(sampler *ovsSampler, hostID int64) {
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	sampler.silent[hostID] = ovsSilence{
		output: sysinfo.OvsUnreadableOutput(""),
		until:  time.Now().Add(-time.Second),
	}
}

func TestOvsSamplerCachesSuccess(t *testing.T) {
	sampler := newOvsSampler()
	output := "###OVS\nbr0 eth0 \n"
	calls := 0
	fetch := func() (string, error) {
		calls++
		return output, nil
	}

	for i := 0; i < 3; i++ {
		if got := sampler.get(context.Background(), 1, fetch); got != output {
			t.Fatalf("第 %d 次取到的输出不对：%q", i, got)
		}
	}
	// 这是缓存存在的第一个理由：网络页每 3 秒轮询一次，而这里一次采集
	// 在有 su 提权口令的目标机上就是一次真实的提权。
	if calls != 1 {
		t.Fatalf("缓存窗口内不该重复采集，实际采了 %d 次", calls)
	}

	// 缓存按主机分开：不能把 A 机器的网桥显示给 B。
	sampler.get(context.Background(), 2, fetch)
	if calls != 2 {
		t.Fatalf("换了一台主机就该重新采集，实际 %d 次", calls)
	}
}

func TestOvsSamplerSilencesAfterFailure(t *testing.T) {
	sampler := newOvsSampler()
	calls := 0
	fail := func() (string, error) {
		calls++
		return "", errors.New("提权被拒")
	}

	// 采集失败与"这台机器还没有网桥"必须给出不同的结论，见
	// sysinfo.OvsUnreadableOutput；这里确认采样器走的正是那条路，
	// 并且把那句报错原样带了出去 —— 它是界面上唯一说得清原因的东西。
	want := sysinfo.OvsUnreadableOutput("提权被拒")
	if got := sampler.get(context.Background(), 1, fail); got != want {
		t.Fatalf("采集失败时应当给出“读不到”并附上原因，实际：%q", got)
	}
	if _, held := sampler.held(1); !held {
		t.Fatal("采集失败后应当进入静默，否则每次轮询都会往目标机投一次认证失败")
	}

	for i := 0; i < 3; i++ {
		if got := sampler.get(context.Background(), 1, fail); got != want {
			t.Fatalf("静默期内应当继续给出同一个结论，实际：%q", got)
		}
	}
	if calls != 1 {
		t.Fatalf("静默期内不该再碰目标机，实际执行了 %d 次", calls)
	}

	// 静默到点就得重新尝试：用户去把权限补上之后不该还要重启面板。
	expireSilence(sampler, 1)
	sampler.get(context.Background(), 1, fail)
	if calls != 2 {
		t.Fatalf("静默到期后应当重新采集，实际 %d 次", calls)
	}
}

func TestOvsSamplerRecoversAfterSuccess(t *testing.T) {
	sampler := newOvsSampler()
	output := "###OVS\nbr0 eth0 \n"
	failing := true
	fetch := func() (string, error) {
		if failing {
			return "", errors.New("提权被拒")
		}
		return output, nil
	}

	sampler.get(context.Background(), 1, fetch)
	expireSilence(sampler, 1)

	failing = false
	if got := sampler.get(context.Background(), 1, fetch); got != output {
		t.Fatalf("读到之后应当给出真实输出，实际：%q", got)
	}
	// 静默留在那儿会让下一次失败立刻生效，但更重要的是它会让"刚补好权限"
	// 这件事看起来没发生 —— 读到了就该解除。
	if _, held := sampler.held(1); held {
		t.Fatal("读到之后应当解除静默")
	}
}

func TestOvsSamplerCanceledRequestIsNotAFailure(t *testing.T) {
	sampler := newOvsSampler()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := sampler.get(ctx, 1, func() (string, error) { return "", context.Canceled })

	// 取消什么都说明不了，但也不能假装读到了：给"读不到"是诚实的。
	// 原因同样是那句话本身，界面照实显示即可。
	if want := sysinfo.OvsUnreadableOutput("context canceled"); got != want {
		t.Fatalf("被取消的采集应当给出“读不到”，实际：%q", got)
	}
	// 但它不该让这台主机静默几分钟 —— 那会让用户切一下页面回来，
	// 变更按钮就再也点不动了。
	if _, held := sampler.held(1); held {
		t.Fatal("请求被取消不该让这台主机静默")
	}
}

func TestOvsSamplerForget(t *testing.T) {
	sampler := newOvsSampler()
	output := "###OVS\nbr0 eth0 \n"
	calls := 0
	fetch := func() (string, error) {
		calls++
		return output, nil
	}

	sampler.get(context.Background(), 1, fetch)
	if _, held := sampler.held(1); held {
		t.Fatal("成功采集不该进入静默")
	}

	// forget 的用途是"这份快照已经不作数了"（主机被改动、网络发生了变更）。
	// 缓存没清掉的话，用户刚建好的网桥要等满一个窗口才出现在界面上。
	sampler.forget(1)
	sampler.get(context.Background(), 1, fetch)
	if calls != 2 {
		t.Fatalf("forget 之后应当重新采集，实际 %d 次", calls)
	}
}
