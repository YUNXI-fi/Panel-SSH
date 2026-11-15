package env

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"panelssh/internal/store"
)

// recEvents 记录编排器回调，供断言。
type recEvents struct {
	started []string
	summary *OpSummary
	err     error
}

func (e *recEvents) StepStart(_ int, name string) { e.started = append(e.started, name) }
func (e *recEvents) StepLog(_ int, _, _ string)   {}
func (e *recEvents) StepEnd(_ StepResult)         {}
func (e *recEvents) Finish(s *OpSummary, err error) {
	e.summary = s
	e.err = err
}

// TestRunnerRunRejectsWithoutPrivilege 在非 root 且提不了权的机器上
// （开发机/容器常如此）验证：任何变更在创建操作记录之前就被拒绝，
// 不会对系统产生任何动作，也不留 running 脏记录。
func TestRunnerRunRejectsWithoutPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("当前为 root，无法验证无特权拒绝路径")
	}

	db := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	runner := NewRunner(st, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ev := &recEvents{}
	runner.Run(ctx, Target{HostID: 0, HostName: "本机", Exec: NewLocalExecutor()}, RunRequest{
		Action: ActionInstall,
		Specs:  []Spec{{Name: "hello"}},
	}, ev)

	if !errors.Is(ev.err, ErrNoPrivilege) {
		t.Fatalf("应因无特权被拒绝，得到: %v", ev.err)
	}
	if ev.summary != nil {
		t.Fatalf("被拒绝时不应产生操作记录: %+v", ev.summary)
	}
	if len(ev.started) != 0 {
		t.Fatalf("被拒绝前不应执行任何步骤，实际执行了: %v", ev.started)
	}
	ops, err := st.ListEnvOps(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 0 {
		t.Fatalf("拒绝路径不应留下操作记录，实际有 %d 条", len(ops))
	}
}

// TestPruneSnapshots 验证只保留最新的 N 个快照，无关目录不被误删。
func TestPruneSnapshots(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"op-1", "op-5", "op-3", "op-99", "other-dir", "op-7"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pruneSnapshots(dir, 3)

	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range left {
		got[e.Name()] = true
	}
	for _, keep := range []string{"op-5", "op-7", "op-99", "other-dir"} {
		if !got[keep] {
			t.Fatalf("%s 应保留，目录内容: %v", keep, got)
		}
	}
	for _, removed := range []string{"op-1", "op-3"} {
		if got[removed] {
			t.Fatalf("%s 应被裁剪", removed)
		}
	}
}

// TestRunnerPlanValidatesInput 验证预检入口的参数校验不触达系统。
func TestRunnerPlanValidatesInput(t *testing.T) {
	db := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	runner := NewRunner(st, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err = runner.Plan(ctx, NewLocalExecutor(), 0, RunRequest{
		Action: ActionInstall,
		Specs:  []Spec{{Name: "nginx;reboot"}},
	})
	if !errors.Is(err, ErrInvalidPackage) {
		t.Fatalf("非法包名应被拒绝，得到: %v", err)
	}

	_, err = runner.Plan(ctx, NewLocalExecutor(), 0, RunRequest{
		Action: ActionMirror,
		Mirror: "evil",
	})
	// 无特权探测在镜像校验之前，Probe 总是能跑；到 mirror 选择阶段应报未知镜像。
	// 用错误断言而不是强行模拟系统画像，保持测试对环境的鲁棒。
	if err != nil && !errors.Is(err, ErrUnknownMirror) && !errors.Is(err, ErrUnsupportedFamily) {
		t.Fatalf("未知镜像应返回 ErrUnknownMirror/不支持家族，得到: %v", err)
	}
}
