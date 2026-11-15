package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"panelssh/internal/appstore"
)

// 应用包动作的核心约束：参数只能以 shell 引号形式进入命令，
// 不能因为包作者写模板时漏了引号就变成注入点。
func TestPackageAppQuotesActionParams(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "demo")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := appstore.Manifest{
		SchemaVersion: appstore.SchemaVersion,
		Slug:          "demo", Name: "Demo", Version: "1.0.0",
		Category: "utility", Description: "演示", Icon: "icon.svg",
		DefaultAction: "run",
		Actions: []appstore.Action{{
			ID: "run", Label: "运行", Group: "inspect",
			Command: "echo {{value}}",
			Fields: []appstore.Field{{Key: "value", Label: "值", Type: "text", Required: true}},
		}},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(appDir, "app.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	server := &Server{appPackages: appstore.NewStore(dir, "")}
	app := server.packageApp("demo")
	if app == nil || len(app.Actions) != 1 {
		t.Fatalf("应用包没有转换成通用视图：%+v", app)
	}
	action := app.Actions[0]
	params, err := normalizeSecurityParams(action, map[string]string{"value": "a;b"})
	if err != nil {
		t.Fatalf("参数被意外拒绝：%v", err)
	}
	script, err := action.build(params)
	if err != nil {
		t.Fatalf("命令构造失败：%v", err)
	}
	if script != "echo 'a;b'" {
		t.Fatalf("参数没有被单引号包裹：%s", script)
	}
	if strings.Contains(script, "echo a;b") {
		t.Fatal("参数直接拼进了命令")
	}
}
