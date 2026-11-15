package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"panelssh/internal/sysinfo"
)

// 宿主机（LocalHostID = 0）是"面板自己所在的那台机器"这一内置目标：
// 它走本地执行而不是 SSH，右上角胶囊默认就盯着它。这条路径一旦断掉，
// 用户看到的是常驻红灯，所以在这里把边界钉死。

func TestRunLocalCollectsMetrics(t *testing.T) {
	output, err := runLocal(sysinfo.MetricsCommand())
	if err != nil {
		t.Fatalf("宿主机采集命令执行失败：%v", err)
	}

	collector := sysinfo.NewCollector()
	// 首次采样只建立计数器基准，解析不该因此失败。
	metrics, err := collector.Collect(LocalHostID, output, time.Now())
	if err != nil {
		t.Fatalf("宿主机采集结果解析失败：%v", err)
	}

	// 只断言"确实读到了宿主机的 /proc"，具体数值随机器变化，不做假设。
	if metrics.CPUCores <= 0 {
		t.Fatalf("CPU 核数为 %d，没有读到 /proc/cpuinfo", metrics.CPUCores)
	}
	if metrics.MemTotal == 0 {
		t.Fatal("内存总量为 0，没有读到 /proc/meminfo")
	}
	if metrics.Hostname == "" {
		t.Fatal("主机名为空，没有读到 hostname")
	}
}

func TestPathIDAllowsLocalHost(t *testing.T) {
	s := &Server{}

	cases := []struct {
		raw  string
		want bool
	}{
		{"0", true},  // 宿主机
		{"12", true}, // 普通主机
		{"-1", false},
		{"abc", false},
		{"", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/hosts/x/metrics", nil)
		r.SetPathValue("id", tc.raw)
		w := httptest.NewRecorder()

		if _, ok := s.pathID(w, r); ok != tc.want {
			t.Fatalf("pathID(%q) = %v，期望 %v", tc.raw, ok, tc.want)
		}
	}
}

// 宿主机没有可借的 SSH 连接：终端、文件管理、命令执行以及网络/存储变更都从这里进来，
// 所以"宿主机只可读"这条边界只在这一处成立。
func TestLeaseHostRejectsLocalHost(t *testing.T) {
	s := &Server{}
	if _, err := s.leaseHost(LocalHostID); !errors.Is(err, errLocalHostUnsupported) {
		t.Fatalf("leaseHost(宿主机) 应返回 errLocalHostUnsupported，得到 %v", err)
	}
}

// 结束进程不再对宿主机设限：宿主机在这个面板里和远端主机是同一类目标，
// 用户看得见它的进程就该能结束它。这条用例改为守住真正该守的那条线 ——
// 存储与网络变更是"改到面板自己所在的系统"，对宿主机必须拒绝。
func TestSystemChangeRejectsLocalHost(t *testing.T) {
	s := &Server{}

	cases := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"存储变更", s.handleStorageApply},
		{"网络变更", s.handleNetworkApply},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/api/hosts/0/apply", nil)
		r.SetPathValue("id", "0")
		w := httptest.NewRecorder()

		tc.handler(w, r)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s：对宿主机应被拒绝（400），得到 %d", tc.name, w.Code)
		}
	}
}

// 宿主机没有 SSH 可借，但命令照样要能跑 —— 这是本地执行通道的基本契约。
func TestExecLocalRunsCommand(t *testing.T) {
	res, err := execLocal(context.Background(), "echo hello; echo oops >&2; exit 3", 5*time.Second)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if got := strings.TrimSpace(res.Stdout); got != "hello" {
		t.Fatalf("stdout = %q，期望 hello", got)
	}
	if got := strings.TrimSpace(res.Stderr); got != "oops" {
		t.Fatalf("stderr = %q，期望 oops", got)
	}
	// 退出码非 0 是"跑完了但命令自己报错"，不是执行失败。
	if res.ExitCode != 3 {
		t.Fatalf("退出码 = %d，期望 3", res.ExitCode)
	}
}

// 超时必须连整组进程一起带走。
//
// 这条断言靠的不是计时而是 os/exec 的行为：cmd.Run 会等 stdout/stderr 的复制
// goroutine 收尾，而它们要等管道 EOF。若只杀了 sh 而留下 sleep 持有写端，
// Run 会一直等到 sleep 自然结束（30 秒）—— 测试会因此失败，正是我们要防的
// "命令没跑完却已经给出结果"。
func TestExecLocalKillsProcessGroupOnTimeout(t *testing.T) {
	start := time.Now()
	_, err := execLocal(context.Background(), "sleep 30", 300*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, errExecTimeout) {
		t.Fatalf("超时应返回 errExecTimeout，得到 %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("超时后进程组没有被及时终止，耗时 %v", elapsed)
	}
}
