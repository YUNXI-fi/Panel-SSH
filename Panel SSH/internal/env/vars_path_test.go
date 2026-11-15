package env

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestVarPathSetScriptIsIdempotent 守住这个功能的核心承诺：重复点"加入 PATH"
// 不会像参考脚本那样把同一批目录越堆越多。
//
// 参考脚本（path_tool.sh）每执行一次就再追加一批 export PATH="...:$PATH"，
// 三次之后 PATH 里同一个目录出现三遍。这里每次都是"整组重写"，
// 所以第 2、3 次的结果必须与第 1 次逐字节相同。
func TestVarPathSetScriptIsIdempotent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "panel-env.sh")
	dirs := []string{"/var/apps/node/target/bin", "/var/apps/python/target/bin"}

	var first string
	for round := 1; round <= 3; round++ {
		script, err := VarPathSetScript(file, dirs)
		if err != nil {
			t.Fatal(err)
		}
		runSh(t, script)
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if count := strings.Count(string(content), "export PATH="); count != 1 {
			t.Fatalf("第 %d 次写入后文件里有 %d 行 PATH 定义，应该只有 1 行：\n%s", round, count, content)
		}
		if round == 1 {
			first = string(content)
		} else if string(content) != first {
			t.Fatalf("第 %d 次写入的内容与第一次不同：\n第一次：\n%s\n第 %d 次：\n%s", round, first, round, content)
		}
	}

	// 真的能被 shell 读出来，且 $PATH 是被展开的（而不是冻结成字面量）。
	out := runSh(t, ". "+shellSingleQuote(file)+`; printf '%s' "$PATH"`)
	parts := strings.Split(out, ":")
	if parts[0] != dirs[0] || parts[1] != dirs[1] {
		t.Fatalf("新目录没有排在最前：%q", out)
	}
	if !strings.Contains(out, ":") || parts[len(parts)-1] == "" {
		t.Fatalf("PATH 末尾为空，说明 $PATH 没有展开：%q", out)
	}
	if strings.Contains(out, "$PATH") {
		t.Fatalf("文件里的 $PATH 被当成字面量写进去了：%q", out)
	}
}

// TestVarPathSetScriptReplacesOldGroup 守住"面板管的那一组只有一个"。
//
// 注意 dirs 是**完整的目标集合**（不是增量）：界面只提供"加入"，所以真实调用
// 传进来的是"新目录 + 从备份里解析出的旧目录"。这里故意只传新目录，
// 验的是"旧的一组会被整组取代"这条机制本身（也即是回滚之后的样子）。
func TestVarPathSetScriptReplacesOldGroup(t *testing.T) {
	file := filepath.Join(t.TempDir(), "panel-env.sh")
	content := "# 用户的文件\n" +
		"# panelssh-path: /old/bin\n" +
		"export PATH=\"/old/bin:$PATH\"\n" +
		"export EDITOR=vim\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := VarPathSetScript(file, []string{"/new/bin"})
	if err != nil {
		t.Fatal(err)
	}
	runSh(t, script)

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "/old/bin") {
		t.Fatalf("上一次的目录没有被取代：\n%s", text)
	}
	if !strings.Contains(text, "export EDITOR=vim") || !strings.Contains(text, "# 用户的文件") {
		t.Fatalf("文件里的无关内容被吃掉了：\n%s", text)
	}
	// 面板自己那组只留一处：重复执行不会越堆越多。
	if strings.Count(text, "# panelssh-path:") != 1 || strings.Count(text, "PATH=") != 1 {
		t.Fatalf("面板管的那组不止一处：\n%s", text)
	}
}

// TestVarPathSetScriptKeepsUserWrittenPath 守住"面板只动自己写的那两行"。
//
// 这条用例来自一次真实事故：早期判据按 "PATH=" 前缀删行，本意是顺手清掉用户写死的
// 面值行，结果把这种包在 if 里的**累加行**也删了 ——
//
//	if [ -d "$HOME/.local/bin" ] ; then
//	    PATH="$HOME/.local/bin:$PATH"
//	fi
//
// 删掉中间一行就留下空的 if 块，bash 报"未预期的记号 fi"，整个文件从此不再执行：
// 用户的环境被改坏，而面板那行排在语法错误之后也永远跑不到，复核只能如实报"没生效"。
//
// 它同时守住一条推论：漏删用户的累加行是无害的 —— 面板那行追加在文件末尾，
// 最后一条生效。
func TestVarPathSetScriptKeepsUserWrittenPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "profile")
	content := "# 用户的登录文件\n" +
		"if [ -d \"$HOME\" ] ; then\n" +
		"    PATH=\"$HOME/bin:$PATH\"\n" +
		"fi\n" +
		"  export   PATH=$HOME/.local/bin:$PATH\n" +
		"export FOO=1\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := VarPathSetScript(file, []string{"/a/bin"})
	if err != nil {
		t.Fatal(err)
	}
	runSh(t, script)

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	// 语法必须仍然正确：事故里最严重的现象就是这一条，直接用 bash 亲自检查。
	runSh(t, "bash -n "+shellSingleQuote(file))

	for _, keep := range []string{
		"if [ -d \"$HOME\" ] ; then",
		"PATH=\"$HOME/bin:$PATH\"",
		"fi",
		"  export   PATH=$HOME/.local/bin:$PATH",
		"export FOO=1",
	} {
		if !strings.Contains(text, keep) {
			t.Fatalf("用户自己的行被吃掉了（%q）：\n%s", keep, text)
		}
	}

	// 面板那行必须排在用户所有 PATH 定义之后 —— "最后一条生效"靠的就是这个位置。
	if strings.Index(text, "export PATH=\"/a/bin:$PATH\"") < strings.Index(text, "PATH=$HOME/.local/bin:$PATH") {
		t.Fatalf("面板的赋值行没有排在用户定义之后：\n%s", text)
	}

	// 真读一遍：面板目录排在最前，用户的两处目录也都在。
	out := runSh(t, ". "+shellSingleQuote(file)+`; printf '%s' "$PATH"`)
	if !strings.HasPrefix(out, "/a/bin:") {
		t.Fatalf("面板目录没有排在 PATH 最前：%q", out)
	}
	if !strings.Contains(out, "/bin:") || !strings.Contains(out, "/.local/bin") {
		t.Fatalf("用户自己的 PATH 累加行没有生效：%q", out)
	}
}

// TestVarPathSetScriptRejectsDangerousDirs：目录会被拼进双引号包围的赋值行，
// 所以含 $、反引号、引号、冒号、空格的路径必须在面板侧就被拒掉。
func TestVarPathSetScriptRejectsDangerousDirs(t *testing.T) {
	for _, dir := range []string{
		"",
		"relative/bin",
		"/a/$(rm -rf /)/bin",
		"/a/`whoami`/bin",
		"/a/\"x\"/bin",
		"/a\\b/bin",
		"/a b/bin",
		"/a:/b",
	} {
		if _, err := VarPathSetScript("/tmp/x", []string{dir}); err == nil {
			t.Fatalf("危险目录 %q 应该被拒掉", dir)
		}
	}
}

// TestVarPathSetScriptDedupes：同一个目录重复传进来只写一次。
func TestVarPathSetScriptDedupes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "panel-env.sh")
	script, err := VarPathSetScript(file, []string{"/a/bin", "/b/bin", "/a/bin"})
	if err != nil {
		t.Fatal(err)
	}
	runSh(t, script)
	got, _ := os.ReadFile(file)
	if strings.Count(string(got), "/a/bin") != 2 {
		// 标记行一次 + 赋值行一次。
		t.Fatalf("目录没有被去重：\n%s", got)
	}
}

// TestVarPathDirs 只认标记行：不去猜用户手写的 PATH 里有什么。
func TestVarPathDirs(t *testing.T) {
	content := []byte("export PATH=/usr/bin\n" +
		"# panelssh-path: /a/bin /b/bin\n" +
		"# panelssh-path: /b/bin /c/bin\n" +
		"export PATH=\"/a/bin:$PATH\"\n")
	got := VarPathDirs(content)
	want := []string{"/a/bin", "/b/bin", "/c/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("解析结果 %v，期望 %v", got, want)
	}
	// 少一个空格的变体不认：面板写出来的永远是带空格的那种，
	// 放宽判据只会让"这行是不是面板写的"变成一个需要猜的问题。
	if dirs := VarPathDirs([]byte("#panelssh-path: /x/bin\n")); len(dirs) != 0 {
		t.Fatalf("无空格变体不该被认作面板的标记行：%v", dirs)
	}
}

// TestParseScanSplitsInPathAndShadows 守住扫描结果里那两个判据：
// 目录在不在 PATH 里、加进去会遮住哪些现有命令。
func TestParseScanSplitsInPathAndShadows(t *testing.T) {
	raw := strings.Join([]string{
		"===HOME===",
		"/home/u",
		"===PATH===",
		"/var/apps/node/target/bin:/usr/bin:/bin",
		"===DIRS===",
		// node 已在这个目录里（说明目录已在 PATH 里）；python 解析到 /usr/bin（会被遮住）。
		"CMD\t/var/apps/node/target/bin\tnode\t/var/apps/node/target/bin/node",
		"CMD\t/var/apps/node/target/bin\tpython\t/usr/bin/python",
		"DIR\t/var/apps/node/target/bin\tfnos\t2\t9",
		"CMD\t/home/u/.cargo/bin\tcargo\t",
		"DIR\t/home/u/.cargo/bin\tcargo\t1\t1",
		"DENIED\t/opt/secret/bin\topt",
		"===END===",
		"",
	}, "\n")

	view := parseScan(raw)
	if view.Home != "/home/u" {
		t.Fatalf("家目录解析错误：%q", view.Home)
	}
	if len(view.PathDirs) != 3 {
		t.Fatalf("PATH 目录解析错误：%v", view.PathDirs)
	}
	if len(view.Dirs) != 3 {
		t.Fatalf("目录数量错误：%d", len(view.Dirs))
	}

	node := view.Dirs[0]
	if node.Label != "飞牛应用" || node.Total != 9 {
		t.Fatalf("来源标签或总数错误：%+v", node)
	}
	if !node.InPath {
		t.Fatal("这个目录在 PATH 里，应判为 InPath")
	}
	// python 解析到 /usr/bin → 会被遮住；node 解析到自己 → 不算。
	if !reflect.DeepEqual(node.Shadows, []string{"python"}) {
		t.Fatalf("遮住名单错误：%v", node.Shadows)
	}

	cargo := view.Dirs[1]
	if cargo.InPath || cargo.Label != "Rust 工具" {
		t.Fatalf("cargo 目录判定错误：%+v", cargo)
	}
	if len(cargo.Shadows) != 0 {
		t.Fatalf("解析不到的命令不该算遮住：%v", cargo.Shadows)
	}
	if cargo.Commands[0].Resolved != "" {
		t.Fatalf("Resolved 应为空：%+v", cargo.Commands[0])
	}

	if !view.Dirs[2].Denied {
		t.Fatal("DENIED 目录应标记为无权限")
	}
}

// TestParseScanDirsSurviveMissingDirLine：脚本先出 CMD 再出 DIR（遍历完才知道总数），
// 解析不能依赖两者成对出现。
func TestParseScanDirsSurviveMissingDirLine(t *testing.T) {
	raw := "===PATH===\n/usr/bin\n===DIRS===\nCMD\t/x/bin\tfoo\t/usr/bin/foo\n===END===\n"
	view := parseScan(raw)
	if len(view.Dirs) != 1 || view.Dirs[0].Path != "/x/bin" {
		t.Fatalf("只有 CMD 行时应该仍解析出目录：%+v", view.Dirs)
	}
	if view.Dirs[0].Total != 0 {
		t.Fatalf("没有 DIR 行时总数应为 0：%+v", view.Dirs[0])
	}
}

// TestValidateVarWritableRejectsPath 守住"PATH 只走加入 PATH 这一条路"。
//
// 两条路的分工就是这个功能的边界：path-add 整组重写、只动面板自己写的行；
// set/unset 按 "PATH=" 前缀删行，落到 PATH 上会把用户写在 if 里的累加行删掉、
// 让整个启动文件语法错误（真实事故，见 vars_path.go 文件头）。
func TestValidateVarWritableRejectsPath(t *testing.T) {
	if err := ValidateVarWritable(VarPathName); err == nil {
		t.Fatal("PATH 应该被拒：它只能走加入 PATH 那条路")
	}
	// 只是名字里带 PATH 的变量不受影响：它们没有累加语义，也不会被 shell 特殊对待。
	for _, name := range []string{"MANPATH", "GOPATH", "PATHX", "FOO"} {
		if err := ValidateVarWritable(name); err != nil {
			t.Fatalf("%s 不该被拒：%v", name, err)
		}
	}
}