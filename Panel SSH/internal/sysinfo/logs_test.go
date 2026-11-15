package sysinfo

import (
	"reflect"
	"strings"
	"testing"
)

// 这些参数会被拼进目标机上执行的 shell 命令，因此"不合法就拒绝"是硬约束：
// 一旦放过 "/etc/shadow" 或带分号的单元名，面板就成了命令执行的中转站。
func TestValidateLogQueryRejectsUnsafeInput(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		file     string
		unit     string
		priority string
	}{
		{"文件路径越界", LogSourceFile, "/etc/shadow", "", ""},
		{"用 .. 跳出目录", LogSourceFile, "/var/log/../../etc/shadow", "", ""},
		{"路径带命令拼接", LogSourceFile, "/var/log/syslog;rm -rf /", "", ""},
		{"单元名带分号", LogSourceJournal, "", "nginx.service;id", ""},
		{"单元名带空格", LogSourceJournal, "", "nginx service", ""},
		{"日志级别不在白名单", LogSourceJournal, "", "", "verbose"},
		{"未知来源", "syslog", "", "", ""},
	}
	for _, c := range cases {
		if _, err := ValidateLogQuery(c.source, c.file, c.unit, c.priority, 100); err == nil {
			t.Fatalf("%s：应当被拒绝，实际通过了", c.name)
		}
	}
}

// 行数不是安全边界（它只是个数字），超出范围按上下限收敛而不是报错：
// 让界面传 0 或 99999 都能拿到一份合理的日志，比抛 400 有用。
func TestValidateLogQueryClampsLines(t *testing.T) {
	for _, c := range []struct{ in, want int }{
		{0, DefaultLogLines},
		{-5, DefaultLogLines},
		{100, 100},
		{MaxLogLines + 1, MaxLogLines},
	} {
		got, err := ValidateLogQuery(LogSourceFile, "/var/log/syslog", "", "", c.in)
		if err != nil {
			t.Fatalf("lines=%d 不该报错：%v", c.in, err)
		}
		if got.Lines != c.want {
			t.Fatalf("lines=%d 应归一化为 %d，实际 %d", c.in, c.want, got.Lines)
		}
	}
}

// 生成的命令必须与校验结果一致：界面看到的是"真跑的那条命令"。
func TestLogQueryCommand(t *testing.T) {
	cases := []struct {
		name  string
		query LogQuery
		want  string
	}{
		{"journal 基础", LogQuery{Source: LogSourceJournal, Lines: 200},
			"journalctl -n 200 --no-pager -o short-iso 2>&1"},
		{"journal 带单元与级别", LogQuery{Source: LogSourceJournal, Lines: 50, Unit: "nginx.service", Priority: "err"},
			"journalctl -n 50 --no-pager -o short-iso -u nginx.service -p err 2>&1"},
		{"文件", LogQuery{Source: LogSourceFile, Lines: 20, File: "/var/log/syslog"},
			"tail -n 20 /var/log/syslog 2>&1"},
		{"dmesg", LogQuery{Source: LogSourceDmesg, Lines: 30},
			"dmesg 2>&1 | tail -n 30"},
	}
	for _, c := range cases {
		if got := c.query.Command(); got != c.want {
			t.Fatalf("%s：命令不符\n实际：%s\n期望：%s", c.name, got, c.want)
		}
	}
}

// 候选路径与一层通配会命中同一个文件，去重后按修改时间倒序 ——
// 排障时要看的多半是当前正在写的那个。
func TestParseLogSources(t *testing.T) {
	raw := "###JOURNAL\nyes\n###DMESG\nno\n###FILES\n" +
		"1024\t100\t/var/log/syslog\n" +
		"2048\t300\t/var/log/auth.log\n" +
		"1024\t100\t/var/log/syslog\n" +
		"乱码行\n" +
		"512\t200\t/etc/passwd\n" +
		"###END\n"

	sources := ParseLogSources(raw)
	if !sources.Journal || sources.Dmesg {
		t.Fatalf("来源探测有误：journal=%v dmesg=%v", sources.Journal, sources.Dmesg)
	}
	if len(sources.Files) != 2 {
		t.Fatalf("应剩 2 个文件（去重 + 剔除非 /var/log），实际 %d：%+v", len(sources.Files), sources.Files)
	}
	if sources.Files[0].Path != "/var/log/auth.log" || sources.Files[1].Path != "/var/log/syslog" {
		t.Fatalf("应按修改时间倒序：%+v", sources.Files)
	}
	if sources.Files[1].Bytes != 1024 {
		t.Fatalf("文件体积解析有误：%+v", sources.Files[1])
	}
}

// 缺少 journalctl/dmesg 时源清单应当是"没有"，而不是把整段输出判成有。
func TestParseLogSourcesMissingTools(t *testing.T) {
	sources := ParseLogSources("###JOURNAL\nno\n###DMESG\nno\n###FILES\n###END\n")
	if sources.Journal || sources.Dmesg {
		t.Fatalf("没有 journalctl/dmesg 时不应报可用：%+v", sources)
	}
	if len(sources.Files) != 0 {
		t.Fatalf("没有文件时应为空清单：%+v", sources.Files)
	}
}

// 目标机明确说了权限不足，界面就得把这句话翻译成"该怎么办"，
// 而不是给用户一片空白让他以为是面板坏了。
func TestLogPermissionNote(t *testing.T) {
	if note := LogPermissionNote("/var/log/auth.log: Permission denied\n"); note == "" {
		t.Fatal("Permission denied 应给出提示")
	}
	if note := LogPermissionNote("Feb 1 10:00:00 host sshd[1]: Accepted\n"); note != "" {
		t.Fatalf("正常日志不该产生提示：%s", note)
	}
}

func TestSplitLogLines(t *testing.T) {
	if got := SplitLogLines(""); !reflect.DeepEqual(got, []string{}) {
		t.Fatalf("空输出应返回空列表而不是 nil：%#v", got)
	}
	got := SplitLogLines("a\r\nb\n\n")
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("行切分有误：%#v", got)
	}
	// 命令有多条输出时不能把行粘在一起。
	if strings.Contains(SplitLogLines("x\ny")[0], "\n") {
		t.Fatal("行内不应残留换行")
	}
}
