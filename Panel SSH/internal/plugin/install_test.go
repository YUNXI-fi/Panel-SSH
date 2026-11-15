package plugin

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// buildZip 构造一个安装包。files 的键是包内路径，值 是内容。
func buildZip(t *testing.T, manifest map[string]any, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	write := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("创建 zip 条目 %s 失败：%v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("写入 zip 条目 %s 失败：%v", name, err)
		}
	}

	if manifest != nil {
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("序列化清单失败：%v", err)
		}
		write("plugin.json", string(raw))
	}
	for name, content := range files {
		write(name, content)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败：%v", err)
	}
	return buf.Bytes()
}

func baseManifest(version string) map[string]any {
	return map[string]any{
		"org":         "example",
		"name":        "demo",
		"displayName": "示例插件",
		"version":     version,
		"entry":       "index.html",
		"capabilities": []string{
			"host:list",
		},
	}
}

func TestStageCommitProducesVersionDir(t *testing.T) {
	root := t.TempDir()
	raw := buildZip(t, baseManifest("1.0.0"), map[string]string{"index.html": "<html>v1</html>"})

	staged, err := Stage(root, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Stage 失败：%v", err)
	}
	if err := staged.Commit(); err != nil {
		t.Fatalf("Commit 失败：%v", err)
	}

	entry := filepath.Join(root, "example", "demo", "1.0.0", "index.html")
	content, err := os.ReadFile(entry)
	if err != nil {
		t.Fatalf("入口文件不存在：%v", err)
	}
	if string(content) != "<html>v1</html>" {
		t.Fatalf("入口内容不符：%s", content)
	}

	// 暂存目录必须已消失，否则会留下越积越多的垃圾。
	if _, err := os.Stat(filepath.Join(root, stagingDir, "example", "demo")); !os.IsNotExist(err) {
		t.Fatal("Commit 之后暂存目录仍然存在")
	}
}

// 这是本次修复的核心性质：一次失败的安装不能破坏当前正在工作的版本。
// 旧实现会在解压前先删掉同插件的其它版本目录，导致"安装失败 = 插件彻底打不开"。
func TestFailedStageLeavesInstalledVersionIntact(t *testing.T) {
	root := t.TempDir()

	// 先装好 1.0.0
	good := buildZip(t, baseManifest("1.0.0"), map[string]string{"index.html": "<html>v1</html>"})
	installed, err := Stage(root, bytes.NewReader(good), int64(len(good)))
	if err != nil {
		t.Fatalf("Stage 失败：%v", err)
	}
	if err := installed.Commit(); err != nil {
		t.Fatalf("Commit 失败：%v", err)
	}

	// 再装一个"清单合法但缺少入口文件"的坏包：应当失败
	broken := buildZip(t, baseManifest("2.0.0"), map[string]string{"other.html": "<html>broken</html>"})
	if _, err := Stage(root, bytes.NewReader(broken), int64(len(broken))); err == nil {
		t.Fatal("缺少入口文件的安装包本应被拒绝")
	}

	// 旧版本必须完好可用
	content, err := os.ReadFile(filepath.Join(root, "example", "demo", "1.0.0", "index.html"))
	if err != nil {
		t.Fatalf("失败的安装破坏了已安装版本：%v", err)
	}
	if string(content) != "<html>v1</html>" {
		t.Fatalf("已安装版本内容被改动：%s", content)
	}
}

func TestCommitReplacesSameVersionAndDropsOldVersions(t *testing.T) {
	root := t.TempDir()

	v1 := buildZip(t, baseManifest("1.0.0"), map[string]string{"index.html": "<html>v1</html>"})
	first, err := Stage(root, bytes.NewReader(v1), int64(len(v1)))
	if err != nil {
		t.Fatalf("Stage 失败：%v", err)
	}
	if err := first.Commit(); err != nil {
		t.Fatalf("Commit 失败：%v", err)
	}

	// 升级到 2.0.0：旧版本目录应被清掉，只留一个生效版本
	v2 := buildZip(t, baseManifest("2.0.0"), map[string]string{"index.html": "<html>v2</html>"})
	second, err := Stage(root, bytes.NewReader(v2), int64(len(v2)))
	if err != nil {
		t.Fatalf("Stage 失败：%v", err)
	}
	if err := second.Commit(); err != nil {
		t.Fatalf("Commit 失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "example", "demo", "1.0.0")); !os.IsNotExist(err) {
		t.Fatal("升级后旧版本目录仍然存在")
	}
	content, err := os.ReadFile(filepath.Join(root, "example", "demo", "2.0.0", "index.html"))
	if err != nil || string(content) != "<html>v2</html>" {
		t.Fatalf("新版本内容不正确：%v %s", err, content)
	}

	// 同版本重装：内容应被替换，且不留下 .previous 备份目录
	replace := buildZip(t, baseManifest("2.0.0"), map[string]string{"index.html": "<html>v2-fixed</html>"})
	third, err := Stage(root, bytes.NewReader(replace), int64(len(replace)))
	if err != nil {
		t.Fatalf("Stage 失败：%v", err)
	}
	if err := third.Commit(); err != nil {
		t.Fatalf("Commit 失败：%v", err)
	}
	content, err = os.ReadFile(filepath.Join(root, "example", "demo", "2.0.0", "index.html"))
	if err != nil || string(content) != "<html>v2-fixed</html>" {
		t.Fatalf("同版本重装未替换内容：%v %s", err, content)
	}
	if _, err := os.Stat(filepath.Join(root, "example", "demo", "2.0.0.previous")); !os.IsNotExist(err) {
		t.Fatal("重装后残留了 .previous 备份目录")
	}
}

func TestStageRejectsTraversalAndSymlinkEntries(t *testing.T) {
	root := t.TempDir()

	escape := buildZip(t, baseManifest("1.0.0"), map[string]string{"../evil.html": "x"})
	if _, err := Stage(root, bytes.NewReader(escape), int64(len(escape))); err == nil {
		t.Fatal("带 ../ 的条目本应被拒绝")
	}
}

func TestDiscardRemovesStagingOnly(t *testing.T) {
	root := t.TempDir()

	good := buildZip(t, baseManifest("1.0.0"), map[string]string{"index.html": "<html>v1</html>"})
	installed, err := Stage(root, bytes.NewReader(good), int64(len(good)))
	if err != nil {
		t.Fatalf("Stage 失败：%v", err)
	}
	if err := installed.Commit(); err != nil {
		t.Fatalf("Commit 失败：%v", err)
	}

	next := buildZip(t, baseManifest("1.1.0"), map[string]string{"index.html": "<html>v1.1</html>"})
	staged, err := Stage(root, bytes.NewReader(next), int64(len(next)))
	if err != nil {
		t.Fatalf("Stage 失败：%v", err)
	}
	staged.Discard()

	if _, err := os.Stat(filepath.Join(root, "example", "demo", "1.1.0")); !os.IsNotExist(err) {
		t.Fatal("Discard 之后不应出现新版本目录")
	}
	if _, err := os.Stat(filepath.Join(root, "example", "demo", "1.0.0", "index.html")); err != nil {
		t.Fatalf("Discard 影响了已安装版本：%v", err)
	}
}
