package api

import (
	"context"
	"sync"
	"time"

	"panelssh/internal/sysinfo"
)

const (
	// ovsSampleTTL 是 OVS 现状的复用窗口。
	//
	// 比 netTTL（2 秒）宽得多，因为采它一次要比采其它网络信息贵得多：OVS 的
	// 状态在它自己的数据库里，套接字只允许 root 与 openvswitch 组访问，
	// 因此必须换一个身份跑（见 sysinfo.OvsCommand）—— 在有 su 提权口令的目标机
	// 上就是一次真实的提权。而网络页每 3 秒轮询一次，不缓存等于每 3 秒往
	// 目标机的审计日志里写一条提权记录。
	//
	// 取 30 秒而不是更长：用户点完变更会 forget，所以"改完立刻看见"由它保证；
	// 这里只决定"别处手动改过 OVS 之后，面板多久跟上"。
	ovsSampleTTL = 30 * time.Second

	// ovsFailTTL 是采集失败后的静默窗口。
	//
	// 失败多半是"面板手里那份口令不是这台机器的提权口令"，它不会自己好起来。
	// 若按 ovsSampleTTL 反复重试，就是每 30 秒往目标机投一次认证失败 ——
	// 而 RHEL 系默认开着 pam_faillock，撞满会把用户的账号锁掉。
	//
	// 静默而不是放弃：用户去补上权限之后不必重启面板，最迟等这个窗口过去，
	// 下一次轮询就会重新尝试。
	ovsFailTTL = 5 * time.Minute
)

// ovsSampler 按主机缓存 OVS 现状的采集输出。
//
// 为什么不复用 network 那个 sampleBuffer：那里缓存的是一份网络快照，TTL
// 只有 2 秒（比前端 3 秒的轮询还短），它的用途仅仅是合并"多个窗口同时打开"
// 造成的重复请求，并不打算挡住轮询本身。OVS 那一次采集的代价完全不同，
// 两者的取舍也就完全不同，共用一个 TTL 必然是错的。
type ovsSampler struct {
	// buffer 负责成功结果的缓存与并发合并：同一台主机的并发请求只会在
	// 目标机上跑一次。
	buffer *sampleBuffer[string]

	mu sync.Mutex
	// silent 记录正处在失败静默期的主机。到点之前不再碰目标机，
	// 直接把上次那句"读不到"回给调用方。
	silent map[int64]ovsSilence
}

type ovsSilence struct {
	output string
	until  time.Time
}

func newOvsSampler() *ovsSampler {
	return &ovsSampler{
		buffer: newSampleBuffer[string](ovsSampleTTL),
		silent: make(map[int64]ovsSilence),
	}
}

// get 返回一台主机的 OVS 现状采集输出。
//
// 它不返回错误：对调用方而言"读不到"本身就是一种结论，而且必须与"这台机器
// 还没有网桥"区分开。采集失败因此被折成一段与目标机自己报的读不到完全相同
// 的输出（见 sysinfo.OvsUnreadableOutput），让解析与界面只需要处理一种情况。
//
// 失败原因一起带出去：界面那句警告会把它显示出来。同一句"读不到"背后的修法
// 完全不同（提权口令不对要去核对主机配置、套接字权限不足要加 openvswitch 组、服务没起
// 要去看 systemd），而让用户自己去目标机上复现一遍好把原因问出来，正是这个
// 功能本该替他省掉的事。
//
// ctx 只用来判断一件事：这次失败是不是"请求被取消了"。取消什么都说明不了，
// 不该让这台主机静默几分钟 —— 那会让用户切一下页面回来就再也点不动变更。
func (s *ovsSampler) get(ctx context.Context, hostID int64, fetch func() (string, error)) string {
	if output, ok := s.held(hostID); ok {
		return output
	}

	output, err := s.buffer.get(hostID, fetch)
	if err == nil {
		// 这一次读到了，此前若有静默，此刻已经没有意义：留着会让用户
		// 补好权限之后还要白等一个窗口才看得见网桥。
		s.release(hostID)
		return output
	}

	unreadable := sysinfo.OvsUnreadableOutput(err.Error())
	if ctx.Err() != nil {
		return unreadable
	}
	return s.hold(hostID, unreadable)
}

// held 返回这台主机是否仍在失败静默期内，是的话连同上次那句"读不到"。
func (s *ovsSampler) held(hostID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	silence, ok := s.silent[hostID]
	if !ok || !time.Now().Before(silence.until) {
		return "", false
	}
	return silence.output, true
}

// hold 让这台主机静默一阵子，并给出这段静默期里要回给调用方的输出。
func (s *ovsSampler) hold(hostID int64, output string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent[hostID] = ovsSilence{output: output, until: time.Now().Add(ovsFailTTL)}
	return output
}

// release 解除静默。
func (s *ovsSampler) release(hostID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.silent, hostID)
}

// forget 丢弃某台主机的缓存与静默记录。
//
// 与网络快照的 forget 必须同时发生，见 forgetNetwork。
func (s *ovsSampler) forget(hostID int64) {
	s.buffer.forget(hostID)
	s.release(hostID)
}

// fetchOvsRaw 采集一次 OVS 现状。
//
// 它以 root 执行（走 execEscalated）：OVS 的状态在它自己的数据库里，那个
// 套接字默认只允许 root 与 openvswitch 组访问，按登录账号跑必然被拒。
// escalatePreferred 而不是 Required，是因为提不了权的机器（普通账号、没有
// 可用的提权口令）照样要能把网络页打开 —— 那边会如实显示"读不到"，而不是变成
// 一个报错的页面。
func (s *Server) fetchOvsRaw(ctx context.Context, hostID int64) string {
	return s.ovs.get(ctx, hostID, func() (string, error) {
		return s.runEscalated(ctx, hostID, sysinfo.OvsCommand(), commandTimeout, escalatePreferred)
	})
}
