package api

import (
	"sync"
	"time"
)

// sampleBuffer 是"按主机去重 + 短时缓存"的采样器。
//
// 它解决两个真实问题：
//
//  1. 重复采样。同一台主机可以同时被多个监控窗口盯着，前端各自按固定间隔轮询，
//     请求量随窗口数线性放大，而每次采样都要在目标机上跑一串命令。
//     缓存窗口内复用同一份结果，多个窗口共享一次采样。
//
//  2. 采样过密导致速率算不出来。CPU% 与网络速率是两次采样的差值除以时间间隔，
//     间隔过短时采集器会判定基准不可用，表现为数值长期为 0 ——
//     多个窗口叠加轮询恰好会造成这种"越看越不准"。
//
// 同一主机的并发请求只会真正执行一次，其余等待同一份结果（singleflight 语义）。
type sampleBuffer[T any] struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[int64]*sampleEntry[T]
}

type sampleEntry[T any] struct {
	value T
	at    time.Time
	err   error

	// done 在本次采样完成后关闭；后来者等它，而不是另起一次采样。
	done chan struct{}
}

func newSampleBuffer[T any](ttl time.Duration) *sampleBuffer[T] {
	return &sampleBuffer[T]{ttl: ttl, entries: make(map[int64]*sampleEntry[T])}
}

// get 返回一台主机的采样结果：命中缓存直接返回，否则执行 fetch 并写入缓存。
func (b *sampleBuffer[T]) get(key int64, fetch func() (T, error)) (T, error) {
	return b.resolve(key, true, fetch)
}

// getFresh 忽略缓存窗口，强制重新采样。
//
// 用户主动点"立即采样"时必须走这条路：缓存是为了挡住前端轮询带来的放大，
// 而不是让按钮点下去没反应 —— 那等于按钮在说谎。
// 并发请求仍然会合并，避免连点造成重复采集。
func (b *sampleBuffer[T]) getFresh(key int64, fetch func() (T, error)) (T, error) {
	return b.resolve(key, false, fetch)
}

func (b *sampleBuffer[T]) resolve(key int64, useCache bool, fetch func() (T, error)) (T, error) {
	b.mu.Lock()
	if entry, ok := b.entries[key]; ok {
		select {
		case <-entry.done:
			// 已有结果且在缓存窗口内：直接复用。
			if useCache && entry.err == nil && time.Since(entry.at) < b.ttl {
				value := entry.value
				b.mu.Unlock()
				return value, nil
			}
			// 已过期、上次失败、或调用方要求强制刷新：落到下面重新采样。
		default:
			// 已有一次采样在进行：等它结束并直接采用这次结果。
			// 它刚刚完成，对"强制刷新"而言也足够新；再发起一次只会白跑一遍命令。
			// （写入发生在 close(done) 之前，因此这里读 value/err 不存在竞态。）
			b.mu.Unlock()
			<-entry.done
			if entry.err == nil {
				return entry.value, nil
			}
			// 上一次失败：由本次调用重新试一次。
			return b.resolve(key, false, fetch)
		}
	}

	current := &sampleEntry[T]{done: make(chan struct{})}
	b.entries[key] = current
	b.mu.Unlock()

	value, err := fetch()

	b.mu.Lock()
	current.value = value
	current.at = time.Now()
	current.err = err
	if err != nil {
		// 失败不缓存：下一次请求应当重新尝试，而不是把错误也缓存住。
		if b.entries[key] == current {
			delete(b.entries, key)
		}
	}
	b.mu.Unlock()
	close(current.done)

	return value, err
}

// forget 丢弃某台主机的缓存。
// 主机被删除或地址/凭据变更后必须调用，否则会把旧机器的数据当成它的现状返回。
func (b *sampleBuffer[T]) forget(key int64) {
	b.mu.Lock()
	delete(b.entries, key)
	b.mu.Unlock()
}

// put 直接写入一份已经算好的结果，供流式采集使用。
//
// 流式路径已经把点解析出来了，不必再走一次 fetch；但它必须落进这份缓存，
// 否则 AI 助手的 get_metrics 读到的仍是上一次的旧值，与监控窗口对不上 ——
// "同一个指标在两处显示不同的数"是最难被用户容忍的一类不一致。
//
// done 在这里就关闭：resolve 的等待者靠它判断结果已就绪，晚关会让它们白等一轮。
func (b *sampleBuffer[T]) put(key int64, value T) {
	// done 必须显式建出来再关：它的零值是 nil channel，close(nil) 会直接 panic ——
	// 而这条路径一旦 panic，倒下的不是这一次采样，是整个面板进程。
	entry := &sampleEntry[T]{value: value, at: time.Now(), done: make(chan struct{})}
	close(entry.done)

	b.mu.Lock()
	b.entries[key] = entry
	b.mu.Unlock()
}
