package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 压缩/解压的进度靠"数输出行数"，而行是从两条拷贝协程（stdout / stderr）里按块
// 读回来的。一次网络读的边界通常不落在换行上，所以拆行必须能跨块 ——
// 拆错的表现是进度条时不时跳一下或干脆不走，而不是报错。
func TestLineWriterSplitsLines(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"按换行拆开", []string{"a\nb\nc\n"}, []string{"a", "b", "c"}},
		{"跨块拼接半行", []string{"ab", "c\nd", "e\n"}, []string{"abc", "de"}},
		{"没有换行就不发帧", []string{"abc"}, nil},
		{"一次写入含多个空行", []string{"\n\n"}, []string{"", ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			writer := newLineWriter(&mu, newLimitedBuffer(maxExecOutputBytes), func(line string) {
				got = append(got, line)
			})
			for _, chunk := range tc.chunks {
				if _, err := writer.Write([]byte(chunk)); err != nil {
					t.Fatalf("写入失败：%v", err)
				}
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("拆行结果 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 一直没有换行的输出不能把缓冲撑爆：它不受 limitedBuffer 的上限约束。
func TestLineWriterReleasesOverlongLine(t *testing.T) {
	var mu sync.Mutex
	var got []string
	writer := newLineWriter(&mu, newLimitedBuffer(maxExecOutputBytes), func(line string) {
		got = append(got, line)
	})

	if _, err := writer.Write([]byte(strings.Repeat("x", maxPendingLine+1))); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("超长行应被当作一行发出，实际发了 %d 行", len(got))
	}
	if len(got[0]) != maxPendingLine+1 {
		t.Fatalf("发出的长度 = %d，期望 %d", len(got[0]), maxPendingLine+1)
	}
}

func TestParseArchiveTotal(t *testing.T) {
	cases := []struct {
		text string
		want int64
		ok   bool
	}{
		{"PANELSSH_TOTAL 42", 42, true},
		{"PANELSSH_TOTAL 0", 0, true},
		// 真实输出里紧跟着的就是逐条文件名，不能让它们被认成分母。
		{"./src/a.txt", 0, false},
		{"PANELSSH_TOTAL abc", 0, false},
		{"PANELSSH_TOTAL -1", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseArchiveTotal(tc.text)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("parseArchiveTotal(%q) = (%d, %v)，期望 (%d, %v)", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

// TestArchiveItemPath 校验明细表里的路径解析。
//
// 各家工具的逐条输出格式不同，而明细表要展示的是文件名；解析错了不会报错，
// 只会让用户在一张满是 "adding: ... (deflated 65%)" 的表里找文件。
func TestArchiveItemPath(t *testing.T) {
	cases := []struct{ line, want string }{
		{"./src/a.txt", "src/a.txt"},       // tar 压缩：带 ./ 前缀
		{"src/sub/b.txt", "src/sub/b.txt"}, // tar 解压：裸路径
		{"  adding: src/a.txt (deflated 65%)", "src/a.txt"},
		{"  inflating: src/a.txt", "src/a.txt"},
		{"  extracting: src/a.txt", "src/a.txt"},
		{"  creating: src/dir/", "src/dir/"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := archiveItemPath(tc.line); got != tc.want {
			t.Fatalf("archiveItemPath(%q) = %q，期望 %q", tc.line, got, tc.want)
		}
	}
}

// TestSizeOf 校验"大小未知"与"确实是 0 字节"不会混为一谈。
func TestSizeOf(t *testing.T) {
	zero := int64(0)
	if got := sizeOf(&zero); got != 0 {
		t.Fatalf("0 字节应当原样返回 0，实得 %d", got)
	}
	if got := sizeOf(nil); got != -1 {
		t.Fatalf("拿不到大小时应当是 -1，实得 %d", got)
	}
}

// isolateChildLoaderEnv 让用例里 spawn 的 sh / find / tar 回到系统默认的库搜索路径。
//
// 这两个脚本用例验证的是逐行输出口径，而调用方环境里的 LD_LIBRARY_PATH 一旦
// 指向与系统不兼容的库，动态链接器就会往 stderr 打一行 "no version information
// available" 警告。脚本把 stderr 并进 stdout 是为了把失败原因带回来，那行警告
// 于是混进逐条结果里，让"分母与行数相等"这条断言无故失败。
// 这是宿主的库路径污染，不是脚本的行为，测试必须把它隔离掉。
func isolateChildLoaderEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LD_LIBRARY_PATH", "")
}

// 统计标记是面板与脚本之间的协议：脚本里 printf 的写法一变，进度就再也对不上，
// 而这类错误不会让任何一处编译或逻辑失败 —— 只会让进度条停在 0。
// 所以这里用真的 sh 和真的 tar 跑一遍，验证"分母"与"行数"确实相等。
func TestArchiveScriptTotalMatchesLines(t *testing.T) {
	isolateChildLoaderEnv(t)
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("环境里没有 tar")
	}

	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	for _, name := range []string{filepath.Join(src, "a.txt"), filepath.Join(src, "sub", "b.txt")} {
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("写文件失败：%v", err)
		}
	}

	dest := filepath.Join(base, "out.tar.gz")
	script := archiveScript(
		"find "+shellQuotePath(src),
		"tar -czf "+shellQuotePath(dest)+" -C "+shellQuotePath(base)+" -v -- "+shellQuotePath("./src"),
	)

	res, err := execLocal(context.Background(), script, 30*time.Second)
	if err != nil {
		t.Fatalf("执行脚本失败：%v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("脚本退出码 %d，输出：%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	var total int64
	var lines int
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if n, ok := parseArchiveTotal(line); ok {
			total = n
			continue
		}
		lines++
	}

	// 两个文件加两级目录，两侧都应数到 4 —— 分母与计数器口径一致，进度才会走到 100%。
	if total != 4 || lines != 4 {
		t.Fatalf("总数 %d、逐条行数 %d，期望都是 4；输出：%s", total, lines, res.Stdout)
	}
}
