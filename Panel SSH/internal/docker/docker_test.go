package docker

import (
	"strings"
	"testing"
)

// summary 的输出是"探测行 + 三段 JSON"，这里用一段接近真实的输出把整条链路走通。
func TestParseSummary(t *testing.T) {
	raw := strings.Join([]string{
		"PANELSSH_DOCKER=ok",
		"PANELSSH_SERVER=27.1.1",
		"PANELSSH_COMPOSE=v2.29.1",
		SectionContainers,
		`{"ID":"abc123","Names":"web","Image":"nginx:latest","State":"running","Status":"Up 2 hours","Ports":"0.0.0.0:8080->80/tcp","Labels":"com.docker.compose.project=demo,com.docker.compose.project.working_dir=/opt/demo,com.docker.compose.project.config_files=/opt/demo/compose.yaml"}`,
		SectionImages,
		`{"ID":"sha256:deadbeef","Repository":"nginx","Tag":"latest","Size":"187MB","CreatedSince":"2 days ago"}`,
		SectionNetworks,
		`{"ID":"net1","Name":"bridge","Driver":"bridge","Scope":"local","IPv6":"false","Internal":"false"}`,
	}, "\n")

	got := ParseSummary(raw)

	if got.Probe.Error != "" {
		t.Fatalf("探测不应报错：%s", got.Probe.Error)
	}
	if got.Probe.Version != "27.1.1" || got.Probe.ComposeVersion != "v2.29.1" {
		t.Fatalf("版本解析错误：%+v", got.Probe)
	}
	if len(got.Containers) != 1 || got.Containers[0].Project != "demo" {
		t.Fatalf("容器解析错误：%+v", got.Containers)
	}
	if len(got.Containers[0].ConfigFiles) != 1 || got.Containers[0].ConfigFiles[0] != "/opt/demo/compose.yaml" {
		t.Fatalf("compose 文件路径解析错误：%+v", got.Containers[0].ConfigFiles)
	}
	if len(got.Projects) != 1 || got.Projects[0].Running != 1 || got.Projects[0].Total != 1 {
		t.Fatalf("项目聚合错误：%+v", got.Projects)
	}
	if len(got.Images) != 1 || got.Images[0].Ref != "nginx:latest" {
		t.Fatalf("镜像解析错误：%+v", got.Images)
	}
	if len(got.Networks) != 1 || got.Networks[0].IPv6 || got.Networks[0].Internal {
		t.Fatalf("网络布尔字段解析错误：%+v", got.Networks)
	}
	if got.Errors != nil {
		t.Fatalf("不应有错误块：%+v", got.Errors)
	}
}

func TestParseSummaryWithoutDocker(t *testing.T) {
	got := ParseSummary("PANELSSH_DOCKER=missing\n")
	if got.Probe.Available {
		t.Fatal("没有 docker 时 Available 应为 false")
	}
	if !strings.Contains(got.Probe.Error, "没有找到 docker") {
		t.Fatalf("错误文案不符：%q", got.Probe.Error)
	}
}

// 守护进程连不上时，version 那一行拿到的是报错原文，必须落到 Error 上。
func TestParseProbeDaemonError(t *testing.T) {
	raw := strings.Join([]string{
		"PANELSSH_DOCKER=ok",
		"PANELSSH_SERVER=Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
	}, "\n")
	got := ParseProbe(raw)
	if !got.Available {
		t.Fatal("命令存在时 Available 应为 true")
	}
	if !strings.Contains(got.Error, "Cannot connect") {
		t.Fatalf("守护进程报错没有透出：%q", got.Error)
	}
}

func TestRunSpecBuild(t *testing.T) {
	spec := RunSpec{
		Image:   "nginx:latest",
		Name:    "web",
		Ports:   []string{"8080:80"},
		Env:     []string{"TZ=Asia/Shanghai"},
		Volumes: []string{"/srv/data:/data:ro"},
		Restart: "unless-stopped",
		Network: "app-net",
	}
	command, err := spec.Build()
	if err != nil {
		t.Fatalf("不应失败：%v", err)
	}
	for _, part := range []string{
		"docker run -d",
		"--name 'web'",
		"--restart 'unless-stopped'",
		"--network 'app-net'",
		"-p '8080:80'",
		"-e 'TZ=Asia/Shanghai'",
		"-v '/srv/data:/data:ro'",
		"'nginx:latest'",
	} {
		if !strings.Contains(command, part) {
			t.Fatalf("命令缺少 %q：%s", part, command)
		}
	}
}

func TestRunSpecRejectsShellInjection(t *testing.T) {
	cases := []RunSpec{
		{Image: "nginx; rm -rf /"},
		{Image: "nginx:latest", Name: "web;reboot"},
		{Image: "nginx:latest", Ports: []string{"99999:80"}},
		{Image: "nginx:latest", Env: []string{"NO_EQUALS"}},
		{Image: "nginx:latest", Volumes: []string{"/data"}},
		{Image: "nginx:latest", Restart: "sometimes"},
	}
	for _, spec := range cases {
		if _, err := spec.Build(); err == nil {
			t.Fatalf("应当拒绝：%+v", spec)
		}
	}
}

func TestComposeActionCommand(t *testing.T) {
	command, err := ComposeActionCommand("demo", []string{"/opt/demo/compose.yaml"}, "up")
	if err != nil {
		t.Fatalf("不应失败：%v", err)
	}
	for _, part := range []string{"docker compose", "-f '/opt/demo/compose.yaml'", "-p 'demo'", "up -d"} {
		if !strings.Contains(command, part) {
			t.Fatalf("命令缺少 %q：%s", part, command)
		}
	}

	// 项目名大写不被 compose 接受；文件必须是绝对路径。
	if _, err := ComposeActionCommand("Demo", []string{"/opt/demo/compose.yaml"}, "up"); err == nil {
		t.Fatal("非法项目名应被拒绝")
	}
	if _, err := ComposeActionCommand("demo", []string{"compose.yaml"}, "up"); err == nil {
		t.Fatal("相对路径应被拒绝")
	}
	if _, err := ComposeActionCommand("demo", nil, "up"); err == nil {
		t.Fatal("缺少文件路径应被拒绝")
	}
}

func TestNormalizeContainerPath(t *testing.T) {
	cases := map[string]string{
		"":            "/",
		"/":           "/",
		"/etc/../var": "/var",
		"relative":    "",
	}
	for input, want := range cases {
		got, err := NormalizeContainerPath(input)
		if want == "" {
			if err == nil {
				t.Fatalf("%q 应被拒绝", input)
			}
			continue
		}
		if err != nil || got != want {
			t.Fatalf("%q -> %q, %v（期望 %q）", input, got, err, want)
		}
	}
}

func TestParseContainerFiles(t *testing.T) {
	raw := "d\t0\tetc\nf\t1234\tapp.conf\nl\t0\tlink\nld\t0\tbin\n__PANELSSH_TRUNCATED__\n"
	got := ParseContainerFiles(raw, "/")
	if !got.Truncated {
		t.Fatal("截断标记没有生效")
	}
	if got.Parent != "" {
		t.Fatalf("根目录不该有上级：%q", got.Parent)
	}
	if len(got.Entries) != 4 {
		t.Fatalf("条目数 = %d，期望 4：%+v", len(got.Entries), got.Entries)
	}
	// 目录排在最前，指向目录的符号链接也算目录。
	if !got.Entries[0].Dir || got.Entries[0].Name != "bin" || !got.Entries[0].Link {
		t.Fatalf("第一个条目应当是 bin（链接目录）：%+v", got.Entries[0])
	}
	if !got.Entries[1].Dir || got.Entries[1].Name != "etc" {
		t.Fatalf("第二个条目应当是 etc：%+v", got.Entries[1])
	}
	if got.Entries[2].Name != "app.conf" || got.Entries[2].Size != 1234 || got.Entries[2].Path != "/app.conf" {
		t.Fatalf("文件条目解析错误：%+v", got.Entries[2])
	}
	if !got.Entries[3].Link || got.Entries[3].Dir {
		t.Fatalf("普通符号链接不该当成目录：%+v", got.Entries[3])
	}

	sub := ParseContainerFiles("f\t1\tx\n", "/etc")
	if sub.Parent != "/" || sub.Entries[0].Path != "/etc/x" {
		t.Fatalf("子目录解析错误：%+v", sub)
	}
}

func TestContainerFileCommands(t *testing.T) {
	command, err := ListContainerFilesCommand("4b73b3828a81", "/etc")
	if err != nil {
		t.Fatalf("不应失败：%v", err)
	}
	for _, part := range []string{"docker exec -u 0", "sh -c", "'/etc'"} {
		if !strings.Contains(command, part) {
			t.Fatalf("命令缺少 %q：%s", part, command)
		}
	}
	if _, err := ListContainerFilesCommand("ok", "relative"); err == nil {
		t.Fatal("相对路径应被拒绝")
	}
	if _, err := ReadContainerFileCommand("bad;id", "/etc/hostname"); err == nil {
		t.Fatal("非法容器标识应被拒绝")
	}
}

func TestParseNetworkDetail(t *testing.T) {
	raw := `[{"Id":"n1","Name":"app","Driver":"bridge","Scope":"local","EnableIPv6":false,` +
		`"IPAM":{"Config":[{"Subnet":"172.20.0.0/16","Gateway":"172.20.0.1"}]},` +
		`"Containers":{"c1":{"Name":"web","IPv4Address":"172.20.0.2/16","MacAddress":"02:42:ac:14:00:02"}}}]`
	got, problem := ParseNetworkDetail(raw)
	if problem != "" {
		t.Fatalf("不应失败：%s", problem)
	}
	if got.Name != "app" || len(got.Subnets) != 1 || got.Subnets[0].Gateway != "172.20.0.1" {
		t.Fatalf("网络详情解析错误：%+v", got)
	}
	if len(got.Containers) != 1 || got.Containers[0].IPv4 != "172.20.0.2" {
		t.Fatalf("接入容器解析错误：%+v", got.Containers)
	}
}
