package api

import "testing"

// 工作区页的三条规则：按 registry 顺序、归档会话不计数、孤儿记录也要出现。
//
// 它们不是展示细节：少算一个会话或漏掉一个工作区，用户会以为数据丢了。
func TestParseDshWorkspaces(t *testing.T) {
	raw := []byte(`{
		"global": {
			"initialized": true,
			"workspaceIds": ["w1", "missing"],
			"archivedSessionIds": ["s3"]
		},
		"tables": {
			"workspaces": {
				"w1": {
					"path": "/vol1/demo",
					"title": "demo",
					"sessionIds": ["s1", "s2", "s3"],
					"createdAt": "2026-09-01T00:00:00.000Z",
					"updatedAt": "2026-10-20T00:00:00.000Z"
				},
				"w2": {
					"path": "/vol1/orphan",
					"title": "orphan",
					"sessionIds": [],
					"createdAt": "2026-09-02T00:00:00.000Z",
					"updatedAt": "2026-09-02T00:00:00.000Z"
				}
			}
		}
	}`)

	views, err := parseDshWorkspaces(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(views) != 2 {
		t.Fatalf("应当返回 2 个工作区（顺序里的 w1 与孤儿 w2），实际 %d", len(views))
	}
	if views[0].ID != "w1" || views[1].ID != "w2" {
		t.Fatalf("顺序应当先 registry 后孤儿记录，实际 %q、%q", views[0].ID, views[1].ID)
	}
	if views[0].SessionCount != 2 {
		t.Fatalf("归档的 s3 不该计数，期望 2，实际 %d", views[0].SessionCount)
	}
	if views[0].Title != "demo" || views[0].Path != "/vol1/demo" {
		t.Fatalf("标题与路径没有正确带出：%+v", views[0])
	}

	if _, err := parseDshWorkspaces([]byte("not json")); err == nil {
		t.Fatal("坏 JSON 应当返回错误，而不是悄悄当成空列表")
	}
}

// 用户级安装的启动命令是 "<node> <bin.js>"，升级/重装必须能从中还原出 npm 前缀。
func TestDshUserRuntime(t *testing.T) {
	node, prefix, ok := dshUserRuntime(
		"/home/u/.panelssh/node/v24.9.0/bin/node /home/u/.panelssh/dsh-runtime/lib/node_modules/@deepseek-ai/dsh/lib/bin.js",
	)
	if !ok {
		t.Fatal("用户级安装的命令应当被识别")
	}
	if node != "/home/u/.panelssh/node/v24.9.0/bin/node" {
		t.Fatalf("node 路径解析错误：%q", node)
	}
	if prefix != "/home/u/.panelssh/dsh-runtime" {
		t.Fatalf("npm 前缀解析错误：%q", prefix)
	}

	// 系统安装是一条单命令，不该命中用户级分支。
	if _, _, ok := dshUserRuntime("/usr/local/bin/dsh"); ok {
		t.Fatal("系统安装的命令不该被当成用户级")
	}
	if _, _, ok := dshUserRuntime("node /opt/dsh/lib/bin.js"); ok {
		t.Fatal("任意 node + bin.js 不该被当成用户级")
	}
}

// 插件 spec 会拼进目标机的 shell，白名单是这道边界的第一层。
func TestDshPluginSpecPattern(t *testing.T) {
	valid := []string{
		"dshmarket",
		"dsh-cost-meter@latest",
		"@scope/plugin@^1.2.0",
		"file:/opt/plugin",
		"git+https://example.com/plugin.git",
	}
	for _, spec := range valid {
		if !dshPluginSpecPattern.MatchString(spec) {
			t.Fatalf("合法 spec 被拒绝：%q", spec)
		}
	}

	invalid := []string{
		"dshmarket; rm -rf /",
		"dshmarket && reboot",
		"dshmarket $(whoami)",
		"dshmarket `id`",
		"dshmarket\nrm -rf /",
		"",
	}
	for _, spec := range invalid {
		if dshPluginSpecPattern.MatchString(spec) {
			t.Fatalf("危险 spec 被放行：%q", spec)
		}
	}
}
