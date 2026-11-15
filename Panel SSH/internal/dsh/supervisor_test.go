package dsh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCommand 写一个假的 dsh 脚本。
// 真实 dsh 启动后会打印就绪行；这里的脚本只需要在收到 Stop 之前一直活着。
func fakeCommand(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-dsh.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("写脚本失败：%v", err)
	}
	return script
}

func waitForState(t *testing.T, s *Supervisor, want State, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Status().State == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待状态 %s 超时，当前为 %s", want, s.Status().State)
}

// Stop 必须在启动过程中立刻唤醒等待方。
// 旧实现不关闭 ready 通道，等待启动的调用方要一直等到 startTimeout（120 秒），
// 期间卸载插件、改启动命令这类操作都会僵在那里。
func TestStopDuringStartingWakesUpWaiters(t *testing.T) {
	s := newSupervisor(NewLocalLauncher(), fakeCommand(t, "exec sleep 30"), t.TempDir(), "")

	waiting := make(chan error, 1)
	go func() { waiting <- s.EnsureRunning() }()
	waitForState(t, s, "starting", 5*time.Second)

	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 超过 5 秒仍未返回")
	}

	if state := s.Status().State; state != "stopped" {
		t.Fatalf("Stop 之后状态应为 stopped，实际 %s", state)
	}

	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("等待启动的调用方没有被唤醒")
	}
}

// Stop 可以重复调用：面板退出流程里可能被多处触发。
func TestStopIsIdempotent(t *testing.T) {
	s := newSupervisor(NewLocalLauncher(), fakeCommand(t, "exec sleep 30"), t.TempDir(), "")

	go func() { _ = s.EnsureRunning() }()
	waitForState(t, s, "starting", 5*time.Second)

	done := make(chan struct{})
	go func() {
		s.Stop()
		s.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("重复 Stop 之后仍未返回")
	}
}

// 并发调用 EnsureRunning 只应拉起一个进程。
// 状态页每 3 秒刷新一次，如果没有合并，一次启动期间会被反复拉起。
func TestEnsureRunningCoalescesConcurrentStarts(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "starts.log")
	script := fakeCommand(t, "echo start >> "+counter+"\nexec sleep 30")
	s := newSupervisor(NewLocalLauncher(), script, t.TempDir(), "")

	var started int
	for i := 0; i < 5; i++ {
		go func() {
			_ = s.EnsureRunning()
		}()
		started++
	}
	waitForState(t, s, "starting", 5*time.Second)

	// 给并发调用留出"如果没合并就会再拉起一次"的时间
	time.Sleep(600 * time.Millisecond)
	s.Stop()

	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("读取启动次数失败：%v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(raw)), "start")
	if lines != 1 {
		t.Fatalf("并发 %d 次 EnsureRunning 拉起了 %d 个进程，期望 1 个", started, lines)
	}
}

// 启动失败要落到 failed 状态并给出原因，不能停在 starting 上让调用方白等。
func TestEnsureRunningFailsOnBadCommand(t *testing.T) {
	s := newSupervisor(NewLocalLauncher(), "/nonexistent/fake-dsh-binary", t.TempDir(), "")

	err := s.EnsureRunning()
	if err == nil {
		t.Fatal("启动不存在的命令应当报错")
	}
	if state := s.Status().State; state != "failed" {
		t.Fatalf("失败后状态应为 failed，实际 %s", state)
	}
}

// 未配置启动命令时不应尝试拉起进程。
func TestEnsureRunningRejectsWhenDisabled(t *testing.T) {
	s := newSupervisor(NewLocalLauncher(), "", t.TempDir(), "")

	if err := s.EnsureRunning(); err == nil {
		t.Fatal("未配置命令时应当报错")
	}
}
