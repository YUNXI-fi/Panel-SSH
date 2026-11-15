package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"panelssh/internal/env"
	"panelssh/internal/store"
)

// 差异是这个窗口的主要信息：加/删/改名各自落在哪一栏必须分清楚，
// 否则用户会把"升级"看成"换了个包"。
func TestDiffPackages(t *testing.T) {
	base := &store.EnvOp{ID: 7, CreatedAt: time.Unix(1700000000, 0)}
	before := []env.Package{
		{Name: "curl", Version: "7.88.1"},
		{Name: "nginx", Version: "1.24.0"},
		{Name: "vim", Version: "9.0"},
	}
	after := []env.Package{
		{Name: "curl", Version: "7.88.1"},
		{Name: "nginx", Version: "1.26.0"},
		{Name: "htop", Version: "3.3.0"},
	}

	diff := diffPackages(base, before, after)
	if diff.BaseOpID != 7 {
		t.Fatalf("基准操作 ID 有误：%d", diff.BaseOpID)
	}
	if len(diff.Added) != 1 || diff.Added[0].Name != "htop" {
		t.Fatalf("新增项有误：%+v", diff.Added)
	}
	if len(diff.Removed) != 1 || diff.Removed[0].Name != "vim" {
		t.Fatalf("删除项有误：%+v", diff.Removed)
	}
	if len(diff.Changed) != 1 || diff.Changed[0].Name != "nginx" ||
		diff.Changed[0].From != "1.24.0" || diff.Changed[0].To != "1.26.0" {
		t.Fatalf("版本变化有误：%+v", diff.Changed)
	}
}

// 两份清单完全一致时不能返回 nil：界面要显示的是"没有变化"，
// nil 会被当成"算不出来"。
func TestDiffPackagesNoChange(t *testing.T) {
	same := []env.Package{{Name: "curl", Version: "7.88.1"}}
	diff := diffPackages(&store.EnvOp{ID: 1}, same, same)
	if diff == nil {
		t.Fatal("无差异时应返回空差异而不是 nil")
	}
	if len(diff.Added)+len(diff.Removed)+len(diff.Changed) != 0 {
		t.Fatalf("相同清单不该有差异：%+v", diff)
	}
}

// 包管理器给的是自己的输出顺序，界面要稳定展示就必须按包名排序。
func TestDiffPackagesSorted(t *testing.T) {
	after := []env.Package{{Name: "zsh"}, {Name: "apt"}, {Name: "curl"}}
	diff := diffPackages(&store.EnvOp{ID: 1}, nil, after)
	names := []string{diff.Added[0].Name, diff.Added[1].Name, diff.Added[2].Name}
	if strings.Join(names, ",") != "apt,curl,zsh" {
		t.Fatalf("新增项未排序：%v", names)
	}
}

// 快照目录在磁盘上 → 有快照；目录不在但登记过路径且操作已结束 → 被清理。
// 这个区分必须准确：操作自己采集失败（路径为空）不该说成"快照被清理"。
func TestNewSnapshotView(t *testing.T) {
	dir := t.TempDir()
	opDir := filepath.Join(dir, "op-1")
	if err := os.MkdirAll(opDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opDir, "packages.tsv"), []byte("curl\t7.88.1\n\nnginx\t1.24.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opDir, "sources.tgz"), []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}

	view := newSnapshotView(&store.EnvOp{ID: 1, SnapshotDir: opDir, Status: store.EnvStatusSuccess})
	if !view.HasSnapshot || view.Pruned {
		t.Fatalf("目录在时应判为有快照：%+v", view)
	}
	if view.PackageCount != 2 {
		t.Fatalf("包数应只计非空行：%d", view.PackageCount)
	}
	if view.SnapshotBytes != int64(len("dummy"))+int64(len("curl\t7.88.1\n\nnginx\t1.24.0\n")) {
		t.Fatalf("快照体积应含两份材料：%d", view.SnapshotBytes)
	}

	missing := newSnapshotView(&store.EnvOp{
		ID: 2, SnapshotDir: filepath.Join(dir, "op-2"), Status: store.EnvStatusSuccess})
	if missing.HasSnapshot || !missing.Pruned {
		t.Fatalf("登记过路径但目录不在应判为已清理：%+v", missing)
	}

	// 采集阶段就失败的操作 snapshot_dir 为空：它是操作失败，不是快照被清理。
	failed := newSnapshotView(&store.EnvOp{ID: 3, Status: store.EnvStatusFailed})
	if failed.HasSnapshot || failed.Pruned {
		t.Fatalf("没有快照路径不应说成被清理：%+v", failed)
	}

	// 正在执行的操作快照目录可能还没建出来，此时不能说"已清理"。
	running := newSnapshotView(&store.EnvOp{
		ID: 4, SnapshotDir: filepath.Join(dir, "op-4"), Status: store.EnvStatusRunning})
	if running.Pruned {
		t.Fatalf("执行中的操作不该报已清理：%+v", running)
	}
}

// 面板日志是只增文件，读的是尾部。超过读窗口时第一行是半截的，必须丢掉；
// 同时行数上限要真的生效。
func TestReadTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panelssh.log")
	content := strings.Repeat("old line\n", 1000) + "last-1\nlast-2\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	lines, err := readTailLines(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[1] != "last-2" {
		t.Fatalf("应取到最后两行：%#v", lines)
	}

	// 用一个小得多的上限无法直接测（常量固定 512 KiB），改为验证"取到的行数
	// 不超过请求值"这一条不变式：无论窗口从哪里切进来，行数都不该超。
	all, err := readTailLines(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1002 {
		t.Fatalf("文件小于读窗口时应给出全部 1002 行，实际 %d", len(all))
	}
	if strings.HasPrefix(all[0], "line") {
		t.Fatalf("不应出现被切断的半行：%q", all[0])
	}
}

// 日志文件还没产生（刚部署、或日志目录不可写）不是错误：界面要显示
// "尚未产生"而不是一个红框。
func TestReadTailLinesMissingFile(t *testing.T) {
	if _, err := readTailLines(filepath.Join(t.TempDir(), "nope.log"), 10); !os.IsNotExist(err) {
		t.Fatalf("文件不存在应返回 os.IsNotExist 错误，实际：%v", err)
	}
}
