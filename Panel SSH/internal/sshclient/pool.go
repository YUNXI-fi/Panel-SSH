package sshclient

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// idleTTL 是空闲连接的保留时长。超过后且无人使用即回收，
	// 避免面板长期持有大量指向生产机的 SSH 连接。
	idleTTL = 3 * time.Minute
	// probeTimeout 是连接存活探测的上限，防止网络黑洞导致请求长时间挂起。
	probeTimeout = 3 * time.Second
)

type pooledConn struct {
	client   *ssh.Client
	refs     int
	lastUsed time.Time
	// retired 表示这条连接已被判定失效（探测失败或主机配置变更），
	// 不再借出；但要等引用计数归零才真正关闭，否则会掐断正在使用它的会话。
	retired bool
}

// Pool 按主机复用 SSH 连接。
//
// 文件浏览会高频发起请求，若每个请求都重新握手（TCP + 密钥交换 + 认证），
// 目录切换会出现明显的卡顿；复用连接是这类面板的基本要求。
// 采用引用计数：使用中的连接不会被回收。
type Pool struct {
	knownHostsPath string

	mu    sync.Mutex
	conns map[int64]*pooledConn

	// closed 之后不再接受新的拨号：Close 已经关闭了回收协程，
	// 其后新建的连接没有任何人会回收。
	closed bool

	stop chan struct{}
	once sync.Once
}

// NewPool 创建连接池并启动空闲回收协程。
func NewPool(knownHostsPath string) *Pool {
	p := &Pool{
		knownHostsPath: knownHostsPath,
		conns:          make(map[int64]*pooledConn),
		stop:           make(chan struct{}),
	}
	go p.janitor()
	return p
}

// Close 关闭全部连接并停止回收协程。
func (p *Pool) Close() {
	p.once.Do(func() { close(p.stop) })

	p.mu.Lock()
	p.closed = true
	stale := make([]*ssh.Client, 0, len(p.conns))
	for key, entry := range p.conns {
		stale = append(stale, entry.client)
		delete(p.conns, key)
	}
	p.mu.Unlock()

	for _, client := range stale {
		_ = client.Close()
	}
}

// Invalidate 丢弃指定主机的连接。
// 主机地址或凭据变更后必须调用，否则会继续复用旧凭据建立的连接。
//
// 若该连接仍被别的窗口在用（refs > 0），只标记为失效并等待最后一个使用者归还，
// 不立刻关闭：直接关会把正在跑的终端、文件浏览、监控采集一起掐断。
func (p *Pool) Invalidate(key int64) {
	p.mu.Lock()
	entry := p.conns[key]
	if entry == nil {
		p.mu.Unlock()
		return
	}
	entry.retired = true
	closable := entry.refs == 0
	if closable {
		delete(p.conns, key)
	}
	p.mu.Unlock()

	if closable {
		_ = entry.client.Close()
	}
}

// Lease 是一次连接借用。调用方必须 Release，否则连接无法被回收。
type Lease struct {
	Client *ssh.Client

	pool     *Pool
	key      int64
	released bool
}

// Release 归还连接。重复调用是安全的。
func (l *Lease) Release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	l.pool.release(l.key)
}

// Acquire 取到指定主机的可用连接，必要时新建。
func (p *Pool) Acquire(key int64, target Target, timeout time.Duration) (*Lease, error) {
	// 最多两轮：第一轮可能拿到已失效的连接，丢弃后第二轮重新拨号。
	for attempt := 0; attempt < 2; attempt++ {
		if lease, ok := p.tryReuse(key); ok {
			return lease, nil
		}

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("连接池已关闭")
		}
		p.mu.Unlock()

		client, err := Dial(target, p.knownHostsPath, timeout)
		if err != nil {
			return nil, err
		}

		p.mu.Lock()
		if existing := p.conns[key]; existing != nil && !existing.retired {
			// 并发下已被其他请求建立，复用现成的，丢弃本次多余连接。
			existing.refs++
			existing.lastUsed = time.Now()
			p.mu.Unlock()
			_ = client.Close()
			return &Lease{Client: existing.client, pool: p, key: key}, nil
		}
		p.conns[key] = &pooledConn{client: client, refs: 1, lastUsed: time.Now()}
		p.mu.Unlock()
		return &Lease{Client: client, pool: p, key: key}, nil
	}
	return nil, errors.New("无法建立 SSH 连接")
}

// tryReuse 尝试复用一个仍然存活的已有连接。
func (p *Pool) tryReuse(key int64) (*Lease, bool) {
	p.mu.Lock()
	entry := p.conns[key]
	if entry == nil || entry.retired {
		p.mu.Unlock()
		return nil, false
	}
	entry.refs++
	entry.lastUsed = time.Now()
	client := entry.client
	p.mu.Unlock()

	if Alive(client) {
		return &Lease{Client: client, pool: p, key: key}, true
	}

	// 连接已失效：归还本次引用，并把这条连接标记为待回收。
	//
	// 不能像旧实现那样直接按 key 删除：此刻可能已有别的请求把该 key 换成了
	// 新连接，按 key 删会误关崭新的那条，同时旧连接的引用计数被静默丢弃。
	p.mu.Lock()
	entry.refs--
	if p.conns[key] == entry {
		entry.retired = true
		if entry.refs == 0 {
			delete(p.conns, key)
		}
	}
	p.mu.Unlock()
	_ = client.Close()

	return nil, false
}

func (p *Pool) release(key int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry := p.conns[key]
	if entry == nil || entry.refs == 0 {
		return
	}
	entry.refs--
	entry.lastUsed = time.Now()

	// 最后一个使用者归还时才真正关掉已失效的连接。
	if entry.retired && entry.refs == 0 {
		delete(p.conns, key)
		go func() { _ = entry.client.Close() }()
	}
}

func (p *Pool) janitor() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.reapIdle()
		}
	}
}

func (p *Pool) reapIdle() {
	now := time.Now()

	p.mu.Lock()
	var stale []*ssh.Client
	for key, entry := range p.conns {
		if entry.refs == 0 && now.Sub(entry.lastUsed) > idleTTL {
			stale = append(stale, entry.client)
			delete(p.conns, key)
		}
	}
	p.mu.Unlock()

	for _, client := range stale {
		_ = client.Close()
	}
}

// Alive 通过 OpenSSH 扩展的 keepalive 请求探测连接是否仍然可用。
//
// 探测放在独立协程里并设上限：网络被黑洞时 SendRequest 可能长时间不返回，
// 直接调用会把 HTTP 请求一起拖住。超时后关闭连接即可让该协程自然结束。
//
// 导出是因为连接池之外还有别处需要长期持有一条连接（DSH 反代的隧道），
// 那类持有者同样要在取用前确认连接还活着；两处各写一份探测必然会漂。
func Alive(client *ssh.Client) bool {
	done := make(chan error, 1)
	go func() {
		// SendRequest 在连接已关闭时会立即返回错误；WantReply 为 true
		// 以保证服务器确实回应，否则探测没有意义。
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()

	select {
	case err := <-done:
		return err == nil
	case <-time.After(probeTimeout):
		// 超时说明这条连接已经不能用了：直接关掉它，让探测协程随之中止。
		// 旧实现把"关闭"的责任推给调用方，一旦调用路径变化，这个协程会永远
		// 挂在 SendRequest 上。
		_ = client.Close()
		return false
	}
}
