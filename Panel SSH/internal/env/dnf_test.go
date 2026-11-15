package env

import (
	"context"
	"testing"
	"time"
)

func TestRPMParsePackages(t *testing.T) {
	// glibc 带 epoch（2:），是回滚时必须保住的前缀。
	raw := "bash\t5.1.8-9.el9\tx86_64\n" +
		"glibc\t2:2.34-100.el9\tx86_64\n" +
		"gpg-pubkey\t87c2b723-665f4145\t(none)\n"
	m := &rpmManager{}
	pkgs := m.ParsePackages(raw)
	if len(pkgs) != 3 {
		t.Fatalf("包数量 = %d, want 3", len(pkgs))
	}
	if pkgs[1].Version != "2:2.34-100.el9" {
		t.Fatalf("epoch 前缀丢失: %q", pkgs[1].Version)
	}
}

func TestDNFUpgrades(t *testing.T) {
	installed := "bash\t5.1.8-9.el9\tx86_64\n" +
		"nginx\t1.20.1-19.el9\tx86_64\n"
	check := `
Last metadata expiration check: 0:12:41 ago on Mon Sep 15 10:00:00 2026.

nginx.x86_64                       1.20.1-20.el9                        appstream
bash.x86_64                        5.1.8-10.el9                        baseos
`
	ex := &sequenceExecutor{outputs: []string{check, installed}}
	m := &rpmManager{}
	// Upgrades 先取 check-update 再取已装清单；sequenceExecutor 依次吐出。
	ups, err := m.Upgrades(context.Background(), ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 {
		t.Fatalf("升级项数 = %d, want 2: %+v", len(ups), ups)
	}
	byName := map[string]Upgrade{}
	for _, u := range ups {
		byName[u.Name] = u
	}
	if byName["nginx"].Current != "1.20.1-19.el9" || byName["nginx"].Candidate != "1.20.1-20.el9" {
		t.Fatalf("nginx 升级项解析错误: %+v", byName["nginx"])
	}
	if byName["nginx"].Repo != "appstream" {
		t.Fatalf("仓库列解析错误: %+v", byName["nginx"])
	}
}

// sequenceExecutor 按调用顺序返回预置输出，供一个方法内有多次查询的场景使用。
type sequenceExecutor struct {
	outputs []string
	idx     int
}

func (e *sequenceExecutor) Run(_ context.Context, _ Command, _ time.Duration) (*ExecResult, error) {
	out := ""
	if e.idx < len(e.outputs) {
		out = e.outputs[e.idx]
		e.idx++
	}
	return &ExecResult{Stdout: out}, nil
}

func (e *sequenceExecutor) Stream(ctx context.Context, _ Command, _ time.Duration, _ LogSink) (*ExecResult, error) {
	return e.Run(ctx, Command{}, 0)
}

func TestRPMRenderSpec(t *testing.T) {
	m := &rpmManager{}
	if got := m.RenderSpec(Spec{Name: "nginx"}); got != "nginx" {
		t.Fatalf("无版本约束渲染错误: %q", got)
	}
	if got := m.RenderSpec(Spec{Name: "nginx", Version: "1.20.1-20.el9"}); got != "nginx-1.20.1-20.el9" {
		t.Fatalf("版本约束渲染错误: %q", got)
	}
}
