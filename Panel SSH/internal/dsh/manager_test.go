package dsh

import (
	"sync"
	"testing"
	"time"
)

// fakeCommands 是内存版的命令存储，替代数据库。
//
// 这一层要验证的是"哪台主机拿到哪个实例、命令变了有没有换"，与 SQL 无关，
// 因此不必为它起一个库。
type fakeCommands struct {
	mu   sync.Mutex
	data map[int64]string
}

func newFakeCommands() *fakeCommands { return &fakeCommands{data: map[int64]string{}} }

func (f *fakeCommands) GetDshCommand(hostID int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[hostID], nil
}

func (f *fakeCommands) SetDshCommand(hostID int64, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[hostID] = command
	return nil
}

func (f *fakeCommands) DeleteDshCommand(hostID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, hostID)
	return nil
}

func (f *fakeCommands) get(hostID int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[hostID]
}

// newLocalManager 造一个所有主机都走本地启动器的 Manager。
func newLocalManager(t *testing.T) (*Manager, *fakeCommands) {
	t.Helper()

	commands := newFakeCommands()
	m := NewManager(commands, func(int64) Target {
		return Target{Launcher: NewLocalLauncher(), WorkDir: t.TempDir()}
	})
	t.Cleanup(m.StopAll)
	return m, commands
}

// 实例按主机隔离：同一台机器反复取到同一个实例，不同机器各拿各的。
//
// 共用实例的后果是把 A 机器的 dsh 当成 B 机器的 —— 两台机器上的工作台
// 会互相顶掉，而界面上看不出任何异常。
func TestManagerKeepsInstancesPerHost(t *testing.T) {
	m, commands := newLocalManager(t)
	for _, hostID := range []int64{0, 7} {
		if err := commands.SetDshCommand(hostID, fakeCommand(t, "exec sleep 30")); err != nil {
			t.Fatalf("准备命令失败：%v", err)
		}
	}

	host0 := m.Supervisor(0)
	if host0 != m.Supervisor(0) {
		t.Fatal("同一台主机应当复用同一个实例")
	}
	if m.Supervisor(7) == host0 {
		t.Fatal("不同主机不该共用实例")
	}
}

// 没配过命令、也没有部署级默认值的主机拿到的是"未配置"实例：
// 它不该有任何命令，更不该被拉起一个进程。
func TestManagerDisabledWithoutCommand(t *testing.T) {
	m, _ := newLocalManager(t)

	sup := m.Supervisor(9)
	if sup.Available() {
		t.Fatal("没有任何命令来源时不该是可用状态")
	}
	if got := sup.Status().State; got != StateDisabled {
		t.Fatalf("状态应为 disabled，实际 %s", got)
	}
}

// 部署级默认值只在"这台机器还没存过命令"时兜底。
func TestManagerDefaultCommandIsOnlyAFallback(t *testing.T) {
	commands := newFakeCommands()
	m := NewManager(commands, func(int64) Target {
		return Target{
			Launcher:       NewLocalLauncher(),
			WorkDir:        t.TempDir(),
			DefaultCommand: "default-dsh",
		}
	})
	t.Cleanup(m.StopAll)

	// 库里没有 → 用默认值（部署时通过启动参数给的那份）。
	if got := m.Supervisor(0).Command(); got != "default-dsh" {
		t.Fatalf("没有存过命令时应当用默认值，实际 %q", got)
	}

	// 库里有 → 用库里的，默认值让位：用户后填的命令理应覆盖部署时的默认值。
	if err := commands.SetDshCommand(1, "saved-dsh"); err != nil {
		t.Fatalf("准备命令失败：%v", err)
	}
	if got := m.Supervisor(1).Command(); got != "saved-dsh" {
		t.Fatalf("存过命令时应当用存的那份，实际 %q", got)
	}
}

// 改命令要停掉旧实例并换上新命令，否则用户会看到"改了命令但界面上还是旧进程"，
// 无从判断新命令到底有没有生效。
func TestManagerSetCommandRestartsInstance(t *testing.T) {
	m, commands := newLocalManager(t)
	if err := commands.SetDshCommand(0, fakeCommand(t, "exec sleep 30")); err != nil {
		t.Fatalf("准备命令失败：%v", err)
	}

	sup := m.Supervisor(0)
	go func() { _ = sup.EnsureRunning() }()
	waitForState(t, sup, StateStarting, 5*time.Second)

	if err := m.SetCommand(0, "/opt/new-dsh"); err != nil {
		t.Fatalf("保存命令失败：%v", err)
	}

	if got := sup.Status().State; got != StateStopped {
		t.Fatalf("改命令后应当回到未启动，实际 %s", got)
	}
	if got := sup.Command(); got != "/opt/new-dsh" {
		t.Fatalf("实例应当拿到新命令，实际 %q", got)
	}
	if got := commands.get(0); got != "/opt/new-dsh" {
		t.Fatalf("新命令应当落库，实际 %q", got)
	}
}

// 还没建过实例的主机改命令时只写库，不该顺手拉起一个实例。
func TestManagerSetCommandWithoutInstanceOnlyPersists(t *testing.T) {
	m, commands := newLocalManager(t)

	if err := m.SetCommand(5, "/opt/dsh"); err != nil {
		t.Fatalf("保存命令失败：%v", err)
	}
	if got := commands.get(5); got != "/opt/dsh" {
		t.Fatalf("命令应当落库，实际 %q", got)
	}
	// 之后取实例时才按新命令构造。
	if got := m.Supervisor(5).Command(); got != "/opt/dsh" {
		t.Fatalf("新建实例应当读到刚存的命令，实际 %q", got)
	}
}

// StopAll 要覆盖每一台已建的实例：面板退出时漏掉任何一台，
// 远端就会留下一个占几百 MB 内存的孤儿进程。
func TestManagerStopAllStopsEveryInstance(t *testing.T) {
	m, commands := newLocalManager(t)
	for _, hostID := range []int64{0, 7} {
		if err := commands.SetDshCommand(hostID, fakeCommand(t, "exec sleep 30")); err != nil {
			t.Fatalf("准备命令失败：%v", err)
		}
	}

	var sups []*Supervisor
	for _, hostID := range []int64{0, 7} {
		sup := m.Supervisor(hostID)
		sups = append(sups, sup)
		go func() { _ = sup.EnsureRunning() }()
	}
	for _, sup := range sups {
		waitForState(t, sup, StateStarting, 5*time.Second)
	}

	m.StopAll()

	for i, sup := range sups {
		if got := sup.Status().State; got != StateStopped {
			t.Fatalf("第 %d 个实例没被停掉，状态 %s", i, got)
		}
	}
}

// 卸载要连命令记录一起清掉：留着的话，重新安装时界面看起来已经配好，
// 实际拉起的却可能是一个已经被删掉的可执行文件。
func TestManagerForgetClearsCommandRecord(t *testing.T) {
	m, commands := newLocalManager(t)
	if err := commands.SetDshCommand(7, "/opt/dsh"); err != nil {
		t.Fatalf("准备命令失败：%v", err)
	}
	discarded := m.Supervisor(7)

	if err := m.Forget(7); err != nil {
		t.Fatalf("Forget 失败：%v", err)
	}
	if got := commands.get(7); got != "" {
		t.Fatalf("命令记录应当被清掉，实际 %q", got)
	}
	if m.Supervisor(7) == discarded {
		t.Fatal("Forget 之后应当重建实例，而不是复用被丢弃的那个")
	}
	if got := m.Supervisor(7).Command(); got != "" {
		t.Fatalf("重建的实例不该带着旧命令，实际 %q", got)
	}
}