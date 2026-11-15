package appstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderQuotesValues(t *testing.T) {
	got := Render(`fail2ban-client set {{jail}} unbanip {{ip}}`, map[string]string{
		"jail": "sshd", "ip": "1.2.3.4'; rm -rf /",
	})
	if !strings.Contains(got, `'1.2.3.4'\''; rm -rf /'`) {
		t.Fatalf("单引号转义不符合预期：%s", got)
	}
}

func TestValidateManifestRejectsUnknownPlaceholder(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Slug:          "demo", Name: "Demo", Version: "1.0.0",
		Icon: "icon.svg",
		Actions: []Action{{
			ID: "run", Label: "运行", Command: "echo {{missing}}",
		}},
	}
	if err := ValidateManifest(&manifest); err == nil {
		t.Fatal("未声明的占位符应被拒绝")
	}
}

func TestInstallFromLocalSource(t *testing.T) {
	src := t.TempDir()
	dir := filepath.Join(src, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dir, "app.json"), Manifest{
		SchemaVersion: SchemaVersion,
		Slug:          "demo", Name: "Demo", Version: "1.0.0",
		Category: "utility", Description: "演示应用", Icon: "icon.svg",
		DefaultAction: "version",
		Actions: []Action{{
			ID: "version", Label: "版本信息", Group: "inspect",
			Command: "demo --version",
		}},
	})
	writeJSONFile(t, filepath.Join(dir, "ui.json"), UI{
		SchemaVersion: SchemaVersion, Layout: "generic",
		Sections: []Section{{Title: "查看与诊断", Actions: []string{"version"}}},
	})
	if err := os.WriteFile(filepath.Join(dir, "icon.svg"),
		[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"></svg>`), 0o644); err != nil {
		t.Fatal(err)
	}

	index := Index{SchemaVersion: SchemaVersion, GeneratedAt: "2026-01-01T00:00:00Z"}
	files, err := hashFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	index.Apps = []Entry{{
		Slug: "demo", Name: "Demo", Version: "1.0.0", Category: "utility",
		Description: "演示应用", Icon: "icon.svg", Manifest: "app.json", UI: "ui.json",
		Files: files,
	}}
	writeJSONFile(t, filepath.Join(src, "index.json"), index)

	t.Setenv("PANELSSH_APP_SOURCE_DIR", src)
	store := NewStore(t.TempDir(), "")

	idx, fromCache, err := store.Index(context.Background(), true)
	if err != nil {
		t.Fatalf("读取索引失败：%v", err)
	}
	if fromCache || len(idx.Apps) != 1 {
		t.Fatalf("索引结果不符合预期：cached=%v apps=%d", fromCache, len(idx.Apps))
	}

	manifest, ui, err := store.Install(context.Background(), "demo")
	if err != nil {
		t.Fatalf("安装失败：%v", err)
	}
	if manifest.Slug != "demo" || ui == nil || len(ui.Sections) != 1 {
		t.Fatalf("安装结果不完整：%+v / %+v", manifest, ui)
	}
	if version := store.InstalledVersion("demo"); version != "1.0.0" {
		t.Fatalf("已安装版本错误：%s", version)
	}
	if _, contentType, err := store.Icon(context.Background(), "demo"); err != nil || contentType != "image/svg+xml" {
		t.Fatalf("读取图标失败：%v / %s", err, contentType)
	}
	if err := store.Uninstall("demo"); err != nil {
		t.Fatalf("卸载失败：%v", err)
	}
	if version := store.InstalledVersion("demo"); version != "" {
		t.Fatalf("卸载后仍能读到版本：%s", version)
	}
}

func TestInstallRejectsHashMismatch(t *testing.T) {
	src := t.TempDir()
	dir := filepath.Join(src, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, filepath.Join(dir, "app.json"), Manifest{
		SchemaVersion: SchemaVersion, Slug: "demo", Name: "Demo", Version: "1.0.0",
		Icon: "icon.svg",
		Actions: []Action{{ID: "version", Label: "版本", Command: "demo --version"}},
	})
	icon := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"></svg>`)
	if err := os.WriteFile(filepath.Join(dir, "icon.svg"), icon, 0o644); err != nil {
		t.Fatal(err)
	}
	iconSum := sha256.Sum256(icon)
	writeJSONFile(t, filepath.Join(src, "index.json"), Index{
		SchemaVersion: SchemaVersion,
		Apps: []Entry{{
			Slug: "demo", Name: "Demo", Version: "1.0.0",
			Icon: "icon.svg", Manifest: "app.json", UI: "",
			Files: []FileRef{
				{Path: "app.json", SHA256: strings.Repeat("0", 64)},
				{Path: "icon.svg", SHA256: hex.EncodeToString(iconSum[:])},
			},
		}},
	})

	t.Setenv("PANELSSH_APP_SOURCE_DIR", src)
	store := NewStore(t.TempDir(), "")
	if _, _, err := store.Install(context.Background(), "demo"); err == nil {
		t.Fatal("摘要不一致时应拒绝安装")
	}
}

func TestVersionGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.2.0", "1.1.9", true},
		{"1.0.0", "1.0.0", false},
		{"v2.0", "1.9.9", true},
		{"dev", "1.0.0", true}, // 非数字版本按字符串比较，至少结果确定
	}
	for _, item := range cases {
		if got := VersionGreater(item.a, item.b); got != item.want {
			t.Fatalf("VersionGreater(%q,%q)=%v，期望 %v", item.a, item.b, got, item.want)
		}
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func hashFiles(dir string) ([]FileRef, error) {
	var files []FileRef
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		files = append(files, FileRef{Path: filepath.ToSlash(rel), SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	return files, err
}
