package docker

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseDaemonConfigMissing(t *testing.T) {
	cfg, raw, err := ParseDaemonConfig(daemonConfigMissing)
	if err != nil {
		t.Fatalf("文件不存在不应报错：%v", err)
	}
	if cfg.HasFile {
		t.Fatal("文件不存在时 HasFile 应为 false")
	}
	if len(raw) != 0 {
		t.Fatalf("文件不存在时原始配置应为空：%+v", raw)
	}
	if cfg.Mirrors == nil {
		t.Fatal("mirrors 应该给出空数组而不是 nil")
	}
}

func TestParseDaemonConfigExisting(t *testing.T) {
	text := strings.Join([]string{
		daemonConfigMarker,
		`{"registry-mirrors":["https://mirror.example"],"proxies":{"http-proxy":"http://proxy:3128","https-proxy":"https://proxy:3129","no-proxy":"localhost"},"data-root":"/data/docker"}`,
	}, "\n")

	cfg, raw, err := ParseDaemonConfig(text)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !cfg.HasFile {
		t.Fatal("HasFile 应为 true")
	}
	if len(cfg.Mirrors) != 1 || cfg.Mirrors[0] != "https://mirror.example" {
		t.Fatalf("加速源解析错误：%+v", cfg.Mirrors)
	}
	if cfg.Proxy != "https://proxy:3129" {
		t.Fatalf("代理应优先取 https-proxy：%q", cfg.Proxy)
	}
	if cfg.NoProxy != "localhost" {
		t.Fatalf("NO_PROXY 解析错误：%q", cfg.NoProxy)
	}
	if raw["data-root"] != "/data/docker" {
		t.Fatalf("其他字段必须原样保留：%+v", raw)
	}
}

func TestParseDaemonConfigInvalid(t *testing.T) {
	_, _, err := ParseDaemonConfig(daemonConfigMarker + "\n{not json")
	if err == nil {
		t.Fatal("非法 JSON 必须报错，不能静默覆盖")
	}
}

func TestMergeDaemonConfigKeepsOtherFields(t *testing.T) {
	raw := map[string]any{
		"data-root":        "/data/docker",
		"registry-mirrors": []any{"https://old.example"},
		"log-driver":       "json-file",
	}

	data, err := MergeDaemonConfig(raw, DaemonConfigUpdate{
		Mirrors: []string{"https://new.example"},
		Proxy:   "http://proxy:3128",
		NoProxy: "localhost,127.0.0.1",
	})
	if err != nil {
		t.Fatalf("合并失败：%v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("合并结果不是合法 JSON：%v", err)
	}
	if got["data-root"] != "/data/docker" || got["log-driver"] != "json-file" {
		t.Fatalf("其他字段被破坏了：%+v", got)
	}
	mirrors, _ := got["registry-mirrors"].([]any)
	if len(mirrors) != 1 || mirrors[0] != "https://new.example" {
		t.Fatalf("加速源未覆盖：%+v", got["registry-mirrors"])
	}
	proxies, _ := got["proxies"].(map[string]any)
	if len(proxies) != 3 ||
		proxies["http-proxy"] != "http://proxy:3128" ||
		proxies["https-proxy"] != "http://proxy:3128" ||
		proxies["no-proxy"] != "localhost,127.0.0.1" {
		t.Fatalf("代理未正确写入：%+v", proxies)
	}

	// 清空后应把三个键与空的 proxies 对象一并删除，而不是留一个空壳。
	cleared, err := MergeDaemonConfig(got, DaemonConfigUpdate{})
	if err != nil {
		t.Fatalf("清空失败：%v", err)
	}
	var empty map[string]any
	if err := json.Unmarshal(cleared, &empty); err != nil {
		t.Fatalf("清空结果不是合法 JSON：%v", err)
	}
	if _, ok := empty["registry-mirrors"]; ok {
		t.Fatal("清空后不应保留 registry-mirrors")
	}
	if _, ok := empty["proxies"]; ok {
		t.Fatal("清空后不应保留空的 proxies")
	}
	if empty["data-root"] != "/data/docker" {
		t.Fatal("清空不应影响其他字段")
	}
}

func TestMergeDaemonConfigRejectsBadInput(t *testing.T) {
	cases := []DaemonConfigUpdate{
		{Mirrors: []string{"ftp://mirror.example"}},
		{Mirrors: []string{"not-a-url"}},
		{Proxy: "ftp://proxy:3128"},
		{Proxy: "not-a-url"},
		{Proxy: "http://proxy:3128", NoProxy: "a\nb"},
	}
	for _, update := range cases {
		if _, err := MergeDaemonConfig(map[string]any{}, update); err == nil {
			t.Fatalf("应该拒绝非法输入：%+v", update)
		}
	}
}

func TestMergeDaemonConfigDeduplicatesMirrors(t *testing.T) {
	data, err := MergeDaemonConfig(map[string]any{}, DaemonConfigUpdate{
		Mirrors: []string{"https://m.example", " ", "https://m.example"},
	})
	if err != nil {
		t.Fatalf("合并失败：%v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	mirrors, _ := got["registry-mirrors"].([]any)
	if len(mirrors) != 1 {
		t.Fatalf("重复与空行应被去掉：%+v", mirrors)
	}
}

func TestProxySupported(t *testing.T) {
	for _, version := range []string{"23.0.1", "24.0.7", "27.1.1", "23.0.0+dfsg1"} {
		if !ProxySupported(version) {
			t.Fatalf("%s 应支持 proxies", version)
		}
	}
	for _, version := range []string{"20.10.24", "22.06.0", "", "unknown"} {
		if ProxySupported(version) {
			t.Fatalf("%s 不应被判定为支持 proxies", version)
		}
	}
}

func TestDaemonConfigApplyCommand(t *testing.T) {
	payload := []byte(`{"registry-mirrors":["https://mirror.example"]}`)
	cmd := DaemonConfigApplyCommand(payload)

	expected := base64.StdEncoding.EncodeToString(payload)
	if !strings.Contains(cmd, Quote(expected)) {
		t.Fatal("配置内容应以 base64 形式内联")
	}
	if !strings.Contains(cmd, "/etc/docker/daemon.json.panelssh.bak") {
		t.Fatal("必须先备份原文件")
	}
	if !strings.Contains(cmd, daemonRolledBackMarker) {
		t.Fatal("重启失败时必须回滚")
	}
	if !strings.Contains(cmd, "systemctl restart docker") {
		t.Fatal("应重启 Docker 让配置生效")
	}
}
