package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"panelssh/internal/store"
)

// 直连探测必须两条都带上：BatchMode 保证"要口令就失败"而不是挂住，
// StrictHostKeyChecking=yes 保证不为用户自动信任一台陌生的机器。
func TestDirectProbeCommandRequiresBatchModeAndKnownHost(t *testing.T) {
	host := &store.Host{Addr: "10.0.0.9", Port: 2222, Username: "deploy"}
	cmd := directProbeCommand(host)

	for _, want := range []string{"BatchMode=yes", "StrictHostKeyChecking=yes", "-p 2222", shellQuotePath("deploy@10.0.0.9")} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("探测命令缺少 %q：%s", want, cmd)
		}
	}
}

// 端口缺省时按 22 拼，不能让命令里出现 -p 0。
func TestHostPortFallsBackTo22(t *testing.T) {
	if got := hostPort(&store.Host{Port: 0}); got != 22 {
		t.Fatalf("hostPort = %d，期望 22", got)
	}
	if got := hostPort(&store.Host{Port: 2200}); got != 2200 {
		t.Fatalf("hostPort = %d，期望 2200", got)
	}
}

// 直连命令的本地那一段必须经得起真 shell：文件名里带空格、单引号时，
// 引号写错会截断参数甚至执行别的命令。
//
// 这里只跑 "tar -cf -" 那半截（ssh 那半截要去真机器上跑），验证打包出来
// 的条目名与源目录名完全一致。
func TestDirectTransferCommandQuotesSourcePath(t *testing.T) {
	base := t.TempDir()
	name := "it's dir"
	src := filepath.Join(base, name)
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("准备目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("准备文件失败：%v", err)
	}

	host := &store.Host{Addr: "target", Port: 22, Username: "root"}
	cmd := directTransferCommand(src, host, "/srv/incoming")

	local, _, found := strings.Cut(cmd, " | ssh ")
	if !found {
		t.Fatalf("直连命令里没有 ssh 段：%s", cmd)
	}

	out, err := exec.Command("sh", "-c", local+" | tar -tf -").Output()
	if err != nil {
		t.Fatalf("打包失败（多半是引号拼错）：%v", err)
	}
	if !strings.Contains(string(out), name+"/") {
		t.Fatalf("包里的条目名不对：%q", string(out))
	}
}

// 目标目录出现在 ssh 之后：本地 shell 先剥一层引号，ssh 再把参数用空格拼成
// 远程命令，远端 shell 还要再解析一次。三层里任何一层漏引号，带空格的路径
// 都会被拆成两个参数，文件就解到别处去了。
func TestDirectTransferCommandQuotesTarget(t *testing.T) {
	host := &store.Host{Addr: "target", Port: 22, Username: "root"}
	target := "/srv/a b"
	remote := directRemoteExtract(target)

	cmd := directTransferCommand("/data/a", host, target)
	// 本地这一层必须把整段远程命令当**一个**参数交给 ssh。
	if !strings.Contains(cmd, shellQuotePath(remote)) {
		t.Fatalf("远程命令没有整段加引号：%s", cmd)
	}

	// 远端那一次解析：路径仍然是一个参数。
	out, err := exec.Command("sh", "-c", "printf '%s\\n' "+remote).Output()
	if err != nil {
		t.Fatalf("远端命令解析失败：%v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if lines[len(lines)-1] != target {
		t.Fatalf("远端解析后目标是 %q，期望 %q", lines[len(lines)-1], target)
	}
}

// ---- 中转的目录展开 ----
//
// 递归、符号链接与"个别子目录读不了"这三件事在这里都没有报错可看：
// 错了只会表现为少搬几个文件，所以必须直接对着展开结果断言。

type fakeInfo struct {
	name string
	mode os.FileMode
	size int64
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

// fakeLister 用一张路径表冒充 SFTP。fail 里的路径模拟权限不足 / 坏挂载。
type fakeLister struct {
	info  map[string]os.FileInfo
	dirs  map[string][]string
	links map[string]string
	fail  map[string]error
}

func (f *fakeLister) Lstat(p string) (os.FileInfo, error) {
	if err, ok := f.fail[p]; ok {
		return nil, err
	}
	info, ok := f.info[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return info, nil
}

func (f *fakeLister) ReadDir(p string) ([]os.FileInfo, error) {
	if err, ok := f.fail[p]; ok {
		return nil, err
	}
	names, ok := f.dirs[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	entries := make([]os.FileInfo, 0, len(names))
	for _, name := range names {
		entries = append(entries, f.info[path.Join(p, name)])
	}
	return entries, nil
}

func (f *fakeLister) ReadLink(p string) (string, error) {
	if err, ok := f.fail[p]; ok {
		return "", err
	}
	target, ok := f.links[p]
	if !ok {
		return "", errors.New("not a symlink")
	}
	return target, nil
}

func TestPlanRelayTransferWalksDirsFilesAndLinks(t *testing.T) {
	lister := &fakeLister{
		info: map[string]os.FileInfo{
			"/src/data":       fakeInfo{"data", os.ModeDir | 0o755, 0},
			"/src/data/a.txt": fakeInfo{"a.txt", 0o644, 10},
			"/src/data/sub":   fakeInfo{"sub", os.ModeDir | 0o750, 0},
			"/src/data/sub/b": fakeInfo{"b", 0o600, 20},
			"/src/data/link":  fakeInfo{"link", os.ModeSymlink | 0o777, 0},
		},
		dirs: map[string][]string{
			"/src/data":     {"a.txt", "sub", "link"},
			"/src/data/sub": {"b"},
		},
		links: map[string]string{"/src/data/link": "../target"},
	}

	roots, files, dirs, links, err := planRelayTransfer(context.Background(), lister, []transferPair{
		{src: "/src/data", dst: "/dst/data"},
	})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	if len(roots) != 1 || roots[0].files != 2 || roots[0].failed != 0 {
		t.Fatalf("根条目统计不对：%+v", roots)
	}

	// 目录必须父在子前：建目录是按这个顺序跑的。
	if len(dirs) != 2 || dirs[0].dst != "/dst/data" || dirs[1].dst != "/dst/data/sub" {
		t.Fatalf("目录顺序不对：%+v", dirs)
	}
	if len(files) != 2 {
		t.Fatalf("文件数不对：%+v", files)
	}
	sizes := map[string]int64{}
	for _, file := range files {
		sizes[file.dst] = file.size
	}
	if sizes["/dst/data/a.txt"] != 10 || sizes["/dst/data/sub/b"] != 20 {
		t.Fatalf("文件落点或大小不对：%+v", sizes)
	}

	// 链接按链接搬：既不能跟进去，也不能当成普通文件。
	if len(links) != 1 || links[0].dst != "/dst/data/link" || links[0].target != "../target" {
		t.Fatalf("符号链接处理不对：%+v", links)
	}
}

func TestPlanRelayTransferKeepsSymlinkRootAsLink(t *testing.T) {
	lister := &fakeLister{
		info:  map[string]os.FileInfo{"/src/lnk": fakeInfo{"lnk", os.ModeSymlink | 0o777, 0}},
		links: map[string]string{"/src/lnk": "/data/target"},
	}

	roots, files, dirs, links, err := planRelayTransfer(context.Background(), lister, []transferPair{
		{src: "/src/lnk", dst: "/dst/lnk"},
	})
	if err != nil {
		t.Fatalf("展开失败：%v", err)
	}
	if len(files) != 0 || len(dirs) != 0 || len(links) != 1 {
		t.Fatalf("链接根被展开了：files=%d dirs=%d links=%d", len(files), len(dirs), len(links))
	}
	if roots[0].failed != 0 {
		t.Fatalf("链接根不该算失败：%+v", roots[0])
	}
}

// 子目录读不了时跳过它，其余内容照搬；这个根条目要标成失败，
// 移动时因此不会去删源 —— 半套数据的移动比"没动"糟得多。
func TestPlanRelayTransferToleratesUnreadableSubdir(t *testing.T) {
	lister := &fakeLister{
		info: map[string]os.FileInfo{
			"/src/data":     fakeInfo{"data", os.ModeDir | 0o755, 0},
			"/src/data/a":   fakeInfo{"a", 0o644, 5},
			"/src/data/sub": fakeInfo{"sub", os.ModeDir | 0o755, 0},
		},
		dirs: map[string][]string{"/src/data": {"a", "sub"}},
		fail: map[string]error{"/src/data/sub": errors.New("permission denied")},
	}

	roots, files, _, _, err := planRelayTransfer(context.Background(), lister, []transferPair{
		{src: "/src/data", dst: "/dst/data"},
	})
	if err != nil {
		t.Fatalf("单个子目录失败不该让整批作废：%v", err)
	}
	if roots[0].failed == 0 {
		t.Fatal("读不了的子目录必须把根条目标成失败（否则移动会删掉没搬全的源）")
	}
	if len(files) != 1 || files[0].dst != "/dst/data/a" {
		t.Fatalf("能读的文件应当照搬：%+v", files)
	}
}

func TestPlanRelayTransferStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lister := &fakeLister{
		info: map[string]os.FileInfo{"/src/a": fakeInfo{"a", 0o644, 1}},
	}
	if _, _, _, _, err := planRelayTransfer(ctx, lister, []transferPair{{src: "/src/a", dst: "/dst/a"}}); err == nil {
		t.Fatal("ctx 已取消时应当返回错误")
	}
}

// 同时选中 /a 与 /a/b 时只搬 /a：否则移动会先把 /a 删掉，
// 再对着已经不存在的 /a/b 报一条莫名其妙的失败。
func TestDropNestedPaths(t *testing.T) {
	got := dropNestedPaths([]string{"/a", "/a/b", "/a/b/c", "/ab", "/a", "/ab/c"})
	want := []string{"/a", "/ab"}
	if len(got) != len(want) {
		t.Fatalf("去嵌套结果 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("去嵌套结果 = %v，期望 %v", got, want)
		}
	}
}

// 前缀相同但不是子路径（/a 与 /ab）不能被误判成嵌套。
func TestDropNestedPathsKeepsSiblingPrefix(t *testing.T) {
	got := dropNestedPaths([]string{"/a", "/ab"})
	if len(got) != 2 {
		t.Fatalf("/a 与 /ab 不是嵌套关系，却被去掉了一个：%v", got)
	}
}
