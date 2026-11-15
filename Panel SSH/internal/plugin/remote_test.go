package plugin

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"testing"
)

// fakeRemoteEnv 是一个只做记录的目标机替身。
//
// 落位脚本最终是要交给目标机的 shell 去跑的，测试里没有 shell 可用，
// 因此这里能做的是：把"传给目标机的字节"和"让目标机执行的那段脚本"抓下来，
// 校验面板这一侧有没有把正确的东西交出去。
type fakeRemoteEnv struct {
	home string

	// 记录最后一次上传
	uploadedDst  string
	uploadedBody string

	// 记录最后一次执行的脚本
	executedScript string

	// 与 Exec 的返回值一一对应，由用例预先设定。
	stdout   string
	stderr   string
	exitCode int
	execErr  error

	homeErr   error
	uploadErr error

	execCount   int
	uploadCount int
}

func (f *fakeRemoteEnv) HomeDir(ctx context.Context) (string, error) {
	return f.home, f.homeErr
}

func (f *fakeRemoteEnv) Upload(ctx context.Context, dst string, src io.Reader, size int64) error {
	f.uploadCount++
	if f.uploadErr != nil {
		return f.uploadErr
	}
	body, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	// 声明的长度必须与实际字节数一致，否则目标机上会收到截断的包。
	if int64(len(body)) != size {
		return errors.New("声明的长度与实际字节数不一致")
	}
	f.uploadedDst = dst
	f.uploadedBody = string(body)
	return nil
}

func (f *fakeRemoteEnv) Exec(ctx context.Context, script string) (string, string, int, error) {
	f.execCount++
	f.executedScript = script
	return f.stdout, f.stderr, f.exitCode, f.execErr
}

func remoteManifest(version string) *Manifest {
	return &Manifest{
		Org:     "example",
		Name:    "demo",
		Version: version,
		Entry:   "index.html",
	}
}

// 落位脚本是这段代码的全部产出，先把它单独钉死：路径怎么拼、清理顺序、
// 校验失败时的退出码，都是靠脚本内容区分的。
func TestRemoteInstallScriptLayout(t *testing.T) {
	root := "/home/u/.panelssh/plugins"
	manifest := remoteManifest("1.2.3")

	script := remoteInstallScript(root, path.Join(root, stagingDir, "example", "demo", "package.zip"), manifest)

	wants := []string{
		// 任何一步失败都必须中断，不能带着半个目录继续往下走。
		"set -e\n",
		// 暂存目录先清后解，且解压发生在动旧版本之前。
		`plugin_dir='/home/u/.panelssh/plugins/example/demo'`,
		`staged='/home/u/.panelssh/plugins/.staging/example/demo'`,
		`target='/home/u/.panelssh/plugins/example/demo/1.2.3'`,
		`archive='/home/u/.panelssh/plugins/.staging/example/demo/package.zip'`,
		`entry='index.html'`,
		// 入口缺失与落位失败要给出不同的退出码，否则排查时分不清断在哪一步。
		"exit 3",
		"exit 4",
		"exit 5",
		// 落位失败要把旧版本挪回去。
		`mv "$target.previous" "$target"`,
		// 最后清掉同级的其它版本目录，以及暂存区里这个插件（连 org 目录一起）。
		`rm -rf '/home/u/.panelssh/plugins/.staging/example'`,
	}
	for _, want := range wants {
		if !strings.Contains(script, want) {
			t.Fatalf("落位脚本缺少片段 %q\n---\n%s", want, script)
		}
	}

	// 暂存目录的清理必须在解压之前，否则会把刚解出来的内容删掉。
	cleanAt := strings.Index(script, `rm -rf "$staged"`)
	unzipAt := strings.Index(script, `unzip -q -o "$archive" -d "$staged"`)
	if cleanAt < 0 || unzipAt < 0 || cleanAt > unzipAt {
		t.Fatalf("暂存目录的清理顺序不对\n---\n%s", script)
	}
}

// 家目录里的字符会直接进入脚本，必须全部被单引号包住。
//
// 这里只验证"面板算出的这个路径被完整地交给了那个变量"：脚本的其余部分
// 只使用加引号的 "$var"，路径里的空格与单引号就都还原成字面量，不会被拆成两段。
func TestRemoteScriptsQuoteHomeWithSpecialChars(t *testing.T) {
	home := "/home/o'brien x"

	script := remoteInstallScript(path.Join(home, RemoteRoot), path.Join(home, RemoteRoot, "a.zip"), remoteManifest("1.0.0"))
	if !strings.Contains(script, "plugin_dir='"+shellQuote(path.Join(home, RemoteRoot, "example", "demo"))[1:]) {
		t.Fatalf("落位脚本未完整转义家目录\n---\n%s", script)
	}

	uninstall := remoteUninstallScript(path.Join(home, RemoteRoot, "example", "demo"))
	want := "target=" + shellQuote(path.Join(home, RemoteRoot, "example", "demo"))
	if !strings.Contains(uninstall, want) {
		t.Fatalf("卸载脚本未完整转义家目录，期望 %s\n---\n%s", want, uninstall)
	}
	// 卸载脚本必须操作那个变量，而不是把路径直接拼进命令里。
	if !strings.Contains(uninstall, `rm -rf "$target"`) {
		t.Fatalf("卸载脚本未以引号变量删除目标\n---\n%s", uninstall)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/plain/path":  `'/plain/path'`,
		"/with space/": `'/with space/'`,
		"/with'quote":  `'/with'\''quote'`,
	}
	for input, want := range cases {
		if got := shellQuote(input); got != want {
			t.Errorf("shellQuote(%q) = %s，期望 %s", input, got, want)
		}
	}
}

// 面板这一侧交给目标机的必须是与本地镜像同源的字节流：传的是包原文，
// 而不是解压后的目录。
func TestPushToRemoteUploadsArchiveVerbatim(t *testing.T) {
	archive := []byte("PK\x03\x04 假的安装包内容")
	env := &fakeRemoteEnv{home: "/home/u"}

	if err := PushToRemote(context.Background(), env, archive, remoteManifest("1.0.0")); err != nil {
		t.Fatalf("PushToRemote 失败：%v", err)
	}

	if env.uploadedBody != string(archive) {
		t.Fatalf("上传内容与安装包原文不一致：%q", env.uploadedBody)
	}
	// 包落在暂存区，由脚本解压后再落位 —— 直接传到目标版本目录会让半截内容立刻生效。
	want := "/home/u/.panelssh/plugins/.staging/example/demo/package.zip"
	if env.uploadedDst != want {
		t.Fatalf("上传目标路径为 %s，期望 %s", env.uploadedDst, want)
	}
	if env.execCount != 1 {
		t.Fatalf("落位脚本应当只执行一次，实际 %d 次", env.execCount)
	}
}

// 远端脚本非零退出时 err 为 nil（见 RemoteEnv 的约定），
// 因此调用方是否把退出码翻译成错误，直接决定用户会不会看到"安装成功"。
func TestPushToRemoteTranslatesNonZeroExit(t *testing.T) {
	env := &fakeRemoteEnv{
		home:     "/home/u",
		stderr:   "安装包根目录缺少 plugin.json\n",
		exitCode: 3,
	}

	err := PushToRemote(context.Background(), env, []byte("pkg"), remoteManifest("1.0.0"))
	if err == nil {
		t.Fatal("远端脚本非零退出时本应报错")
	}
	if !strings.Contains(err.Error(), "缺少 plugin.json") {
		t.Fatalf("错误信息未包含远端给出的原因：%v", err)
	}
}

// 通信层面的失败（连不上、超时）与业务失败要分开表述，
// 否则用户看到的是一句无从排查的"落位失败"。
func TestPushToRemoteReportsTransportFailure(t *testing.T) {
	env := &fakeRemoteEnv{home: "/home/u", execErr: errors.New("连接已断开")}

	err := PushToRemote(context.Background(), env, []byte("pkg"), remoteManifest("1.0.0"))
	if err == nil || !strings.Contains(err.Error(), "连接已断开") {
		t.Fatalf("通信失败未被转述：%v", err)
	}
}

// 这两个标识参与远端路径拼接，非法值必须在面板侧就被挡住，不能发到目标机上去。
func TestPushToRemoteRejectsInvalidIdentifier(t *testing.T) {
	env := &fakeRemoteEnv{home: "/home/u"}

	bad := []*Manifest{
		{Org: "../etc", Name: "demo", Version: "1.0.0", Entry: "index.html"},
		{Org: "example", Name: "demo/../..", Version: "1.0.0", Entry: "index.html"},
		{Org: "example", Name: "demo", Version: "1.0", Entry: "index.html"},
	}
	for _, manifest := range bad {
		if err := PushToRemote(context.Background(), env, []byte("pkg"), manifest); err == nil {
			t.Fatalf("非法标识 %s/%s@%s 本应被拒绝", manifest.Org, manifest.Name, manifest.Version)
		}
	}
	if env.uploadCount != 0 || env.execCount != 0 {
		t.Fatal("标识校验未通过时不应向目标机发出任何请求")
	}
}

func TestPushToRemoteRequiresEnvAndManifest(t *testing.T) {
	if err := PushToRemote(context.Background(), nil, nil, remoteManifest("1.0.0")); err == nil {
		t.Fatal("执行环境缺失时本应报错")
	}
	if err := PushToRemote(context.Background(), &fakeRemoteEnv{home: "/home/u"}, nil, nil); err == nil {
		t.Fatal("清单缺失时本应报错")
	}
}

func TestUninstallRemoteRemovesPluginDir(t *testing.T) {
	env := &fakeRemoteEnv{home: "/home/u"}

	if err := UninstallRemote(context.Background(), env, "example", "demo"); err != nil {
		t.Fatalf("UninstallRemote 失败：%v", err)
	}

	want := "target=" + shellQuote("/home/u/.panelssh/plugins/example/demo")
	if !strings.Contains(env.executedScript, want) {
		t.Fatalf("卸载脚本未删除预期目录，期望 %s\n---\n%s", want, env.executedScript)
	}
}

// 卸载的目标是"它在目标机上了无痕迹"，不是"必须执行过一次删除"。
// 目标是目录不存在时报错会让用户无法清理一条残留记录。
func TestUninstallRemoteAcceptsMissingDir(t *testing.T) {
	env := &fakeRemoteEnv{home: "/home/u"}

	if err := UninstallRemote(context.Background(), env, "example", "demo"); err != nil {
		t.Fatalf("卸载不存在的目录本应成功：%v", err)
	}
}

func TestUninstallRemoteRejectsInvalidIdentifier(t *testing.T) {
	env := &fakeRemoteEnv{home: "/home/u"}

	if err := UninstallRemote(context.Background(), env, "example", "../.."); err == nil {
		t.Fatal("非法 name 本应被拒绝")
	}
	if env.execCount != 0 {
		t.Fatal("标识校验未通过时不应向目标机发出任何请求")
	}
}

// 报错优先取 stderr：脚本的主动报错都写在 stderr 上，而 stdout 可能混着
// unzip 的进度，直接回传会把真正的原因淹没在噪声里。
func TestRemoteFailureDetailPrefersStderr(t *testing.T) {
	got := remoteFailureDetail("unzip 进度...\n", "入口文件缺失\n", 4)
	if got != "入口文件缺失" {
		t.Fatalf("未优先采用 stderr：%q", got)
	}
}

func TestRemoteFailureDetailFallbacks(t *testing.T) {
	// 没有输出时退回 stdout
	if got := remoteFailureDetail("some failure\n", "  \n", 1); got != "some failure" {
		t.Fatalf("stderr 为空时未退回 stdout：%q", got)
	}
	// 两边都空时至少要说清它是怎么结束的
	got := remoteFailureDetail("", "", 137)
	if !strings.Contains(got, "137") {
		t.Fatalf("两边都无输出时未回退到退出码：%q", got)
	}
}

// 输出过长时保留尾部：报错总在最后一行，而前面可能是几百行 unzip 进度。
func TestClipRemoteOutputKeepsTail(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 20000; i++ {
		sb.WriteString("进度行\n")
	}
	sb.WriteString("真正的错误在最后\n")

	got := clipRemoteOutput(sb.String())
	if !strings.Contains(got, "真正的错误在最后") {
		t.Fatal("截断后丢失了末尾的错误信息")
	}
	if len(got) > maxRemoteOutput+64 {
		t.Fatalf("截断后长度仍然超标：%d", len(got))
	}

	// 短输出原样返回，不该被加上任何前缀。
	if got := clipRemoteOutput("短\n"); got != "短\n" {
		t.Fatalf("短输出被改动了：%q", got)
	}
}
