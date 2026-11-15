package dsh

import "sync"

// CommandStore 是 Manager 需要的存储能力。
//
// 用窄接口而不是具体类型：dsh 包因此不必依赖 store，可以脱离数据库单测，
// 而 store.Store 天然满足它。
type CommandStore interface {
	GetDshCommand(hostID int64) (string, error)
	SetDshCommand(hostID int64, command string) error
	DeleteDshCommand(hostID int64) error
}

// Target 描述 dsh 在一台主机上的落脚点。
//
// 把"用哪种启动器"和"工作目录在哪"放在一起，是因为它们本来就是同一件事的两面：
// 本地与远端的差异同时体现在这两处，拆成两个按主机的取数函数只会让调用方
// 把同样的分支写两遍。
type Target struct {
	Launcher Launcher

	// WorkDir 是 dsh 的工作目录，决定它默认的文件系统位置。
	// 远端主机上的值可以写成 ~/ 开头：目标机的家目录只有它自己知道，
	// 展开交给那边的 shell 做。
	WorkDir string

	// DefaultCommand 是这台主机还没存过命令时的兜底值。
	// 只有宿主机可能有部署级默认（来自启动参数），其余主机为空。
	DefaultCommand string

	// LogPath 是面板侧留存运行日志的文件路径。
	LogPath string
}

// Manager 管理每台主机上 dsh 实例的生命周期。
//
// 实例按主机懒建：用户在哪台机器上打开工作台，才会在那台机器上拉起 dsh。
// 这是"每台都常驻几百 MB 内存"与"按需付费"之间的取舍，也保留了改造前
// 懒启动的性质 —— 装了插件不等于十个实例同时开着。
type Manager struct {
	store     CommandStore
	targetFor func(hostID int64) Target

	mu    sync.Mutex
	items map[int64]*Supervisor
}

// NewManager 构造管理器。targetFor 决定每台主机用哪种启动器、落在哪个目录，
// 由调用方注入（只有它知道 SSH 凭据与 known_hosts 的位置）。
func NewManager(store CommandStore, targetFor func(hostID int64) Target) *Manager {
	return &Manager{store: store, targetFor: targetFor, items: map[int64]*Supervisor{}}
}

// Supervisor 取某台主机的实例，没有就按已存的命令建一个。
func (m *Manager) Supervisor(hostID int64) *Supervisor {
	m.mu.Lock()
	defer m.mu.Unlock()

	if item, ok := m.items[hostID]; ok {
		return item
	}
	item := m.build(hostID)
	m.items[hostID] = item
	return item
}

// build 按库里的命令构造实例。调用方必须已持有 m.mu。
func (m *Manager) build(hostID int64) *Supervisor {
	target := m.targetFor(hostID)

	// 读库失败时退回兜底值：对调用方来说"读不到"与"没配过"的结果是同一个
	// ——都用兜底值跑。为此把错误升级成"这台主机不可用"没有意义，
	// 那只会让用户以为自己填的命令丢了。
	command, _ := m.store.GetDshCommand(hostID)
	if command == "" {
		command = target.DefaultCommand
	}
	return newSupervisor(target.Launcher, command, target.WorkDir, target.LogPath)
}

// SetCommand 保存某台主机的启动命令，并让它在下次访问时生效。
//
// 实例已存在时顺手停掉旧进程，理由与旧实现相同：否则会出现"改了命令但界面上
// 还是旧进程"的状态，用户无从判断新命令到底有没有起作用。
// 实例尚未建（用户没打开过这台机器的工作台）时只写库，不必为此拉起一个实例。
func (m *Manager) SetCommand(hostID int64, command string) error {
	if err := m.store.SetDshCommand(hostID, command); err != nil {
		return err
	}

	m.mu.Lock()
	item := m.items[hostID]
	m.mu.Unlock()

	if item != nil {
		item.setCommand(command)
	}
	return nil
}

// Stop 停掉某台主机的实例，保留它的命令记录。
func (m *Manager) Stop(hostID int64) {
	m.mu.Lock()
	item := m.items[hostID]
	m.mu.Unlock()

	if item != nil {
		item.Stop()
	}
}

// Forget 停掉并丢弃某台主机的实例，连同它的命令记录一起清掉，供卸载插件时调用。
//
// 只停进程不删记录会留下一个指向"已卸掉的 dsh"的命令：用户重新安装后
// 界面看起来已经配好，实际拉起来的却是个不存在的可执行文件。
func (m *Manager) Forget(hostID int64) error {
	m.Stop(hostID)

	m.mu.Lock()
	delete(m.items, hostID)
	m.mu.Unlock()

	return m.store.DeleteDshCommand(hostID)
}

// StopAll 停掉全部实例，供面板退出时调用。
//
// 先复制一份再逐个停：Stop 会等进程退出（最长十来秒），握着 m.mu 做这件事
// 会让期间任何一次 Supervisor() 取实例的请求都卡住。
func (m *Manager) StopAll() {
	m.mu.Lock()
	items := make([]*Supervisor, 0, len(m.items))
	for _, item := range m.items {
		items = append(items, item)
	}
	m.mu.Unlock()

	for _, item := range items {
		item.Stop()
	}
}