package env

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSh 在本机跑一段脚本（测试专用：这些脚本的存在意义就是被目标机的 sh 执行）。
func runSh(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("脚本执行失败：%v\n%s", err, out)
	}
	return string(out)
}

// TestVarSetScriptReplacesOldDefinition 守住"删旧再追加"这条不变量。
//
// 这是最容易被写错、且写错之后不报错只静默失效的一处：写入行里的变量名一旦
// 带上引号（export 'FOO'='bar'），删除判据就再也匹配不上它，于是每改一次就多堆
// 一行旧值，最后生效的是最早那行 —— 用户看到的是"我改了，值却没变"。
func TestVarSetScriptReplacesOldDefinition(t *testing.T) {
	file := filepath.Join(t.TempDir(), "profile")
	if err := os.WriteFile(file, []byte("export FOO=old\nexport BAR=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, value := range []string{"first", "second", "third"} {
		script, err := VarSetScript(file, "FOO", value)
		if err != nil {
			t.Fatalf("生成写入脚本失败：%v", err)
		}
		matches := VarMatches(runSh(t, script))
		if len(matches) != 1 {
			t.Fatalf("写入 %q 后应恰好剩一条 FOO 定义，实际 %d 条：%v", value, len(matches), matches)
		}
		if want := VarSetExpectation("FOO", value); matches[0] != want {
			t.Fatalf("写入行与复核期望对不上：实际 %q，期望 %q", matches[0], want)
		}
	}

	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(content), "FOO="); n != 1 {
		t.Fatalf("文件里应只剩一条 FOO 定义，实际 %d 条：\n%s", n, content)
	}
	if !strings.Contains(string(content), "BAR=keep") {
		t.Fatalf("不该动文件里其它变量：\n%s", content)
	}
}

// TestVarSetScriptKeepsSpecialValuesLiteral 确认值是按字面量写进文件的：
// $ 不被展开、空格不被拆开、引号被正确转义。
//
// 这些值一旦被 shell 二次解释，用户设的就不是他填的那个东西 —— 而这种事不会
// 报错，只会在下次登录时表现成"环境变量不对"，是最难查的一类问题。
func TestVarSetScriptKeepsSpecialValuesLiteral(t *testing.T) {
	file := filepath.Join(t.TempDir(), "profile")
	for _, value := range []string{"$PATH:/opt/bin", "a b c", "it's ok", `back\slash"quote`, ""} {
		script, err := VarSetScript(file, "FOO", value)
		if err != nil {
			t.Fatalf("生成脚本失败：%v", err)
		}
		matches := VarMatches(runSh(t, script))
		if len(matches) != 1 || matches[0] != VarSetExpectation("FOO", value) {
			t.Fatalf("值 %q 写入后复核对不上：%v", value, matches)
		}
		// 再让目标机的 shell 真读一遍这个文件，确认读出来就是填的那个字面值。
		read := ". " + shellSingleQuote(file) + `; printf '%s' "$FOO"`
		if got := runSh(t, read); got != value {
			t.Fatalf("shell 读出的值与填的不一致：填 %q，读出 %q", value, got)
		}
	}
}

// TestVarUnsetScriptRemovesAllDefinitions 覆盖"同一个变量被追加过多次"的历史文件，
// 同时确认名字以该变量开头的其它变量不会被连坐（FOO 与 FOOBAR）。
func TestVarUnsetScriptRemovesAllDefinitions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "profile")
	if err := os.WriteFile(file, []byte("FOO=1\n  FOO=2\nexport FOO=3\nFOOBAR=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script, err := VarUnsetScript(file, "FOO")
	if err != nil {
		t.Fatalf("生成删除脚本失败：%v", err)
	}
	if matches := VarMatches(runSh(t, script)); len(matches) != 0 {
		t.Fatalf("删除后不该还有 FOO：%v", matches)
	}

	content, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "FOO=") {
		t.Fatalf("FOO 没删干净：\n%s", content)
	}
	if !strings.Contains(string(content), "FOOBAR=keep") {
		t.Fatalf("误删了名字以 FOO 开头的其它变量：\n%s", content)
	}
}

// TestVarSetScriptRejectsBadInput 确认写入侧自身就拦住非法名字与值，
// 不指望每个调用方都记得先校验一遍。
func TestVarSetScriptRejectsBadInput(t *testing.T) {
	file := filepath.Join(t.TempDir(), "profile")
	if _, err := VarSetScript(file, "FOO;rm -rf /", "x"); err == nil {
		t.Fatal("非法变量名应在生成脚本前就被拒绝")
	}
	if _, err := VarSetScript(file, "FOO", "line1\nline2"); err == nil {
		t.Fatal("含换行的值应被拒绝")
	}
}

// TestVarTargetFileWhitelist 确认"用户输入不能决定往哪写"。
func TestVarTargetFileWhitelist(t *testing.T) {
	if _, err := VarTargetFile(ScopeUser, "/home/u", "/etc/passwd"); err == nil {
		t.Fatal("用户级不该接受家目录之外的路径")
	}
	if _, err := VarTargetFile(ScopeSystem, "", "/etc/environment"); err == nil {
		t.Fatal("系统级只允许写面板自己的 profile.d 文件")
	}
	home, err := VarTargetFile(ScopeUser, "/home/u", "")
	if err != nil || home != "/home/u/"+DefaultUserVarFile {
		t.Fatalf("留空时应落到默认文件，得到 %q（%v）", home, err)
	}
	sys, err := VarTargetFile(ScopeSystem, "", "")
	if err != nil || sys != SystemVarFile {
		t.Fatalf("系统级默认文件不对：%q（%v）", sys, err)
	}
}

// TestParseVarsSourceAndSession 验证来源推断的三条口径：
// 会话里有、文件里也有 → 来源是最后定义它的文件；只有会话有 → 会话注入；
// 只有文件有 → 标为未生效并说明原因（这类最容易被误认为"已经配好了"）。
func TestParseVarsSourceAndSession(t *testing.T) {
	effective := base64.StdEncoding.EncodeToString([]byte("EDITOR=vim\x00FOO=fromfile\x00ONLYSESSION=x"))
	profile := base64.StdEncoding.EncodeToString([]byte("export FOO='fromfile'\nexport ORPHAN=defined\n"))
	raw := "===SHELL===\n/bin/bash\n" +
		"===HOME===\n/home/u\n" +
		"===EFFECTIVE_MODE===\nnul\n" +
		"===EFFECTIVE_B64===\n" + effective + "\n" +
		"===FILES===\nFILE\t/home/u/.profile\t48\t" + profile + "\n" +
		"===END===\ntrue\n"

	view := parseVars(raw)
	if view.Home != "/home/u" || view.Shell != "/bin/bash" {
		t.Fatalf("画像不对：home=%q shell=%q", view.Home, view.Shell)
	}

	byName := map[string]EnvVar{}
	for _, v := range view.Vars {
		byName[v.Name] = v
	}
	if len(byName) != 4 {
		t.Fatalf("应解出 4 个变量，实际 %d 个：%v", len(byName), byName)
	}

	if v := byName["EDITOR"]; !v.Effective || v.Scope != ScopeSession || v.File != "" {
		t.Fatalf("会话注入的变量不该硬凑来源：%+v", v)
	}
	if v := byName["FOO"]; !v.Effective || v.Scope != ScopeUser || v.File != "/home/u/.profile" || v.Line != 1 {
		t.Fatalf("FOO 的来源推断不对：%+v", v)
	}
	if v := byName["ORPHAN"]; v.Effective || v.Value != "defined" || !strings.Contains(v.Note, "并没有这个变量") {
		t.Fatalf("文件里定义了却没生效的变量应明确标注：%+v", v)
	}

	// 排序：按名字小写升序，界面直接照此渲染。
	for i := 1; i < len(view.Vars); i++ {
		if strings.ToLower(view.Vars[i-1].Name) > strings.ToLower(view.Vars[i].Name) {
			t.Fatalf("变量列表未按名字排序：%v", view.Vars)
		}
	}
}

// TestParseVarProbe 确认复核探针能区分"有值（可能为空串）"与"没有这个变量"。
func TestParseVarProbe(t *testing.T) {
	if got := ParseVarProbe("===VALUE===\nS:\n===END===\n"); !got.Set || got.Value != "" {
		t.Fatalf("空值也算已定义，应解出 Set=true：%+v", got)
	}
	if got := ParseVarProbe("===VALUE===\nS:a b\n===END===\n"); !got.Set || got.Value != "a b" {
		t.Fatalf("值里的空格不该被吃掉：%+v", got)
	}
	if got := ParseVarProbe("===VALUE===\nU\n===END===\n"); got.Set {
		t.Fatalf("未定义应解出 Set=false：%+v", got)
	}
}

// TestVarRestoreScriptCreatesOrDeletes 确认还原的两个分支：
// 原本存在的文件写回原内容，原本不存在的文件被删掉（而不是留一份空文件）。
func TestVarRestoreScriptCreatesOrDeletes(t *testing.T) {
	original := []byte("export FOO=old\n")
	file := filepath.Join(t.TempDir(), "profile")
	if err := os.WriteFile(file, []byte("export FOO=new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSh(t, VarRestoreScript(file, original, true))
	back, err := os.ReadFile(file)
	if err != nil || string(back) != string(original) {
		t.Fatalf("还原后内容不符：%q（%v）", back, err)
	}

	fresh := filepath.Join(t.TempDir(), "panel-env.sh")
	if err := os.WriteFile(fresh, []byte("export FOO=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runSh(t, VarRestoreScript(fresh, nil, false))
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("原本不存在的文件还原后应被删除，实际 err=%v", err)
	}
}