package api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// 摘要解析是"目标机命令"这条路的地基：命令行输出与 stderr 混在同一个流里，
// 切错一次就会把一句报错当成摘要写进结果表，而结果表是用户拿去核对文件的凭据。
func TestParseHashLine(t *testing.T) {
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	cases := []struct {
		name   string
		text   string
		hexLen int
		want   string
		wantOK bool
	}{
		{"正常行", digest + "  /tmp/a.txt", 64, "/tmp/a.txt", true},
		{"路径含空格", digest + "  /tmp/my file.txt", 64, "/tmp/my file.txt", true},
		{"二进制模式的星号前缀", digest + " */tmp/a.txt", 64, "/tmp/a.txt", true},
		{"sha1 长度", strings.Repeat("a", 40) + "  /tmp/a.txt", 40, "/tmp/a.txt", true},
		// 读文件失败时 sha256sum 会把报错写在 stderr 上，而这里两条流是混在一起的。
		{"错误文案不是摘要", "sha256sum: /root/x: Permission denied", 64, "", false},
		{"摘要位含非十六进制字符", strings.Repeat("z", 64) + "  /tmp/a.txt", 64, "", false},
		{"分隔符只有一个空格", digest + " /tmp/a.txt", 64, "", false},
		{"长度不足", digest, 64, "", false},
		{"只有摘要没有路径", digest + "  ", 64, "", false},
		{"空行", "", 64, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, name, ok := parseHashLine(tc.text, tc.hexLen)
			if ok != tc.wantOK {
				t.Fatalf("parseHashLine(%q) ok = %v，期望 %v", tc.text, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if name != tc.want {
				t.Fatalf("路径 = %q，期望 %q", name, tc.want)
			}
			if got != strings.ToLower(tc.text[:tc.hexLen]) {
				t.Fatalf("摘要 = %q，与输入不一致", got)
			}
		})
	}
}

// 结果里的路径要能直接喂给 sha256sum -c，所以相对根的换算必须准确。
func TestRelativeUnder(t *testing.T) {
	roots := []hashRoot{
		{path: "/data/logs", isDir: true},
		{path: "/tmp/one.txt", isDir: false},
		{path: "/", isDir: true},
	}
	cases := []struct {
		abs  string
		want string
		ok   bool
	}{
		{"/data/logs/a.txt", "a.txt", true},
		{"/data/logs/sub/b.txt", "sub/b.txt", true},
		{"/tmp/one.txt", "one.txt", true},
		// 根是 "/" 时不能拼出 "//" 前缀，否则根目录下的结果全都算不出相对路径。
		{"/etc/hosts", "etc/hosts", true},
	}
	for _, tc := range cases {
		got, ok := relativeUnder(roots, tc.abs)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("relativeUnder(%q) = (%q, %v)，期望 (%q, %v)", tc.abs, got, ok, tc.want, tc.ok)
		}
	}

	// 不在任何选中根下时要如实返回 false，调用方据此退回绝对路径 ——
	// 硬凑一个相对路径会让导出的清单在目标机上校验失败。
	narrow := []hashRoot{{path: "/data/logs", isDir: true}}
	if got, ok := relativeUnder(narrow, "/data/other.txt"); ok {
		t.Fatalf("relativeUnder 返回了 (%q, true)，期望不归属任何根", got)
	}

	// 会话层的提示行也必须被判为"不归属任何根"：文件清单直接拿这个结论当过滤器用，
	// 一旦这里放过它，弹窗的清单里就会出现一个名字是这句话的"文件"。
	if got, ok := relativeUnder(narrow, "Could not chdir to home directory /home/admin: No such file or directory"); ok {
		t.Fatalf("sshd 的提示行被判成了文件：%q", got)
	}
}

// TestIsSessionNoise 锁住 sshd 提示行的形状。
//
// 这句话不是命令的输出，却会排在会话输出的最前面。识别失败的后果不是报错，而是
// 它会混进文件清单、或者被算进压缩/解压的进度里。
func TestIsSessionNoise(t *testing.T) {
	noise := []string{
		"Could not chdir to home directory /home/admin: No such file or directory",
		"Could not chdir to home directory /home/admin: Permission denied",
		// 会话输出里可能带 \r，判定不能因此失效。
		"Could not chdir to home directory /home/admin: No such file or directory\r",
	}
	for _, line := range noise {
		if !isSessionNoise(line) {
			t.Fatalf("没有认出会话提示：%q", line)
		}
	}

	// 正常的逐条输出不能被误吞：这些正是文件清单与进度要数的东西。
	keep := []string{
		"/data/logs/a.txt",
		"adding: src/a.txt (deflated 65%)",
		"PANELSSH_TOTAL 12",
		"",
	}
	for _, line := range keep {
		if isSessionNoise(line) {
			t.Fatalf("把正常输出当成了会话提示：%q", line)
		}
	}
}

// TestParseHashItem 校验"开始处理这个文件"那一帧。
//
// 路径里可以有空格，而字节数一定是纯数字 —— 所以只按第一个空格切一刀。
// 切错了不会报错，只会把进度贴到另一行上（或者干脆没有进度）。
func TestParseHashItem(t *testing.T) {
	cases := []struct {
		text string
		size int64
		path string
		ok   bool
	}{
		{"PANELSSH_HASH_ITEM 2048 /data/a.txt", 2048, "/data/a.txt", true},
		// 路径里有空格：右边整段都要原样留下。
		{"PANELSSH_HASH_ITEM 2048 /data/中文 双工 数据.zip", 2048, "/data/中文 双工 数据.zip", true},
		{"PANELSSH_HASH_ITEM 0 /data/empty", 0, "/data/empty", true},
		{"PANELSSH_HASH_ITEM x /data/a.txt", 0, "", false},
		{"PANELSSH_HASH_ITEM -1 /data/a.txt", 0, "", false},
		{"PANELSSH_HASH_ITEM 2048", 0, "", false},
		{"PANELSSH_HASH_ITEM 2048 ", 0, "", false},
		{"PANELSSH_HASH_PROGRESS 1 2", 0, "", false},
		{"", 0, "", false},
	}
	for _, tc := range cases {
		size, path, ok := parseHashItem(tc.text)
		if ok != tc.ok || (ok && (size != tc.size || path != tc.path)) {
			t.Fatalf("parseHashItem(%q) = (%d, %q, %v)，期望 (%d, %q, %v)",
				tc.text, size, path, ok, tc.size, tc.path, tc.ok)
		}
	}
}

// TestParseHashBytes 校验字节进度那一帧。
//
// 最容易出的错是"空字段被当成 0"：进度条会从 80% 突然跳回 0，看起来像重算了。
func TestParseHashBytes(t *testing.T) {
	cases := []struct {
		text  string
		done  int64
		total int64
		ok    bool
	}{
		{"PANELSSH_HASH_PROGRESS 1024 2048", 1024, 2048, true},
		// 读位置偶尔会略微超过文件末尾（缓冲），钳到 100% 即可。
		{"PANELSSH_HASH_PROGRESS 4096 2048", 2048, 2048, true},
		{"PANELSSH_HASH_PROGRESS  2048", 0, 0, false},
		{"PANELSSH_HASH_PROGRESS 1024 0", 0, 0, false},
		{"PANELSSH_HASH_PROGRESS abc 2048", 0, 0, false},
		{"PANELSSH_HASH_PROGRESS 1024", 0, 0, false},
		{"PANELSSH_HASH_ITEM 2048 /data/a.txt", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tc := range cases {
		done, total, ok := parseHashBytes(tc.text)
		if ok != tc.ok || (ok && (done != tc.done || total != tc.total)) {
			t.Fatalf("parseHashBytes(%q) = (%d, %d, %v)，期望 (%d, %d, %v)",
				tc.text, done, total, ok, tc.done, tc.total, tc.ok)
		}
	}
}

// TestHashScriptReportsItemsAndKeeepsDigest 用真的 sh 与真的 md5sum 跑一遍脚本。
//
// 这段脚本比它看起来重要的地方在于：它既要在读大文件时报出"正在读哪个、读到哪",
// 又要让摘要行保持工具的原生格式（解析那一侧完全没变）。写错一处的表现都不是报错，
// 而是"进度不动"或"结果少了几条"—— 所以必须跑真的。
func TestHashScriptReportsItemsAndKeeepsDigest(t *testing.T) {
	if _, err := exec.LookPath("md5sum"); err != nil {
		t.Skip("环境里没有 md5sum")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("环境里没有 sh")
	}

	base := t.TempDir()
	small := filepath.Join(base, "small.txt")
	if err := os.WriteFile(small, []byte("hello"), 0o644); err != nil {
		t.Fatalf("写小文件失败：%v", err)
	}

	// 大文件必须超过门槛，否则走的是"不多起任何进程"的那条老路，也就测不到进度。
	bigSize := int64(hashProgressMinBytes) + (1 << 20)
	big := filepath.Join(base, "big.bin")
	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte(i)
	}
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("建大文件失败：%v", err)
	}
	for written := int64(0); written < bigSize; written += int64(len(block)) {
		if _, err := f.Write(block); err != nil {
			t.Fatalf("写大文件失败：%v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭大文件失败：%v", err)
	}

	script := hashScript([]hashRoot{{path: base, isDir: true}}, "md5sum")
	res, err := execLocal(context.Background(), script, 2*time.Minute)
	if err != nil {
		t.Fatalf("执行脚本失败：%v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("脚本退出码 %d，输出：%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	// 面板把 stdout 与 stderr 合在一起看，测试也照这个口径。
	lines := strings.Split(strings.TrimSpace(res.Stdout+res.Stderr), "\n")

	items := map[string]int64{}
	digests := map[string]string{}
	var sawBytes bool
	for _, line := range lines {
		if size, rel, ok := parseHashItem(line); ok {
			items[rel] = size
			continue
		}
		if done, total, ok := parseHashBytes(line); ok {
			sawBytes = true
			if done > total {
				t.Fatalf("字节进度越界：%q", line)
			}
			continue
		}
		if digest, rel, ok := parseHashLine(line, 32); ok {
			digests[rel] = digest
		}
	}

	// 大文件报一次"开始处理"，小文件不报（它就是走老路的那一批）。
	if len(items) != 1 {
		t.Fatalf("应当只有大文件报开始处理，实际 %d 条：%v", len(items), items)
	}
	if size, ok := items[big]; !ok || size != bigSize {
		t.Fatalf("大文件报来的是 %v，期望 %s = %d", items, big, bigSize)
	}

	// 两个文件的摘要都要在，且与面板自己算的一致 —— 包一层 sh 不能动到摘要本身。
	if len(digests) != 2 {
		t.Fatalf("应当有 2 条摘要，实际 %d 条：%v", len(digests), digests)
	}
	for _, name := range []string{small, big} {
		got, ok := digests[name]
		if !ok {
			t.Fatalf("缺少 %s 的摘要", name)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读文件失败：%v", err)
		}
		sum := md5.Sum(data)
		want := hex.EncodeToString(sum[:])
		if got != want {
			t.Fatalf("%s 的摘要 = %s，期望 %s", name, got, want)
		}
	}

	// 进度行不是必然出现（读得快时脚本可能一次都没轮到），但出现过就必须能解析 ——
	// 上面的循环已经在解析它了，这里只是把"确实看到了"记下来便于排查。
	t.Logf("字节进度帧：%v", sawBytes)
}

// 五种算法的实现对已知向量。BLAKE3 是唯一一个非标准库实现，一旦换库或改输出长度，
// 算出来的摘要会与 b3sum 不一致 —— 这种错误在界面上完全看不出来。
func TestHashAlgoKnownVectors(t *testing.T) {
	// 全部是 "abc" 的摘要。
	want := map[string]string{
		"sha256": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		"sha512": "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a" +
			"2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f",
		"sha1":   "a9993e364706816aba3e25717850c26c9cd0d89d",
		"md5":    "900150983cd24fb0d6963f7d28e17f72",
		"blake3": "6437b3ac38465133ffb63b75273a8db548c558465d79db03fd359c6cd5bd9d85",
	}

	for _, algo := range hashAlgos {
		expected, ok := want[algo.Key]
		if !ok {
			t.Fatalf("算法 %s 没有对应的测试向量", algo.Key)
		}
		hasher := algo.New()
		hasher.Write([]byte("abc"))
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != expected {
			t.Fatalf("%s(\"abc\") = %s，期望 %s", algo.Key, got, expected)
		}
		// HexLen 用来定位输出里的分隔符，写错会让所有结果行都解析失败。
		if len(got) != algo.HexLen {
			t.Fatalf("%s 的 HexLen = %d，实际摘要长度 %d", algo.Key, algo.HexLen, len(got))
		}
	}
}

// 脚本与解析是两套代码，中间只靠"一行一个结果"这个约定连着。
// 用真的 sh、真的 find、真的 sha256sum 跑一遍，验证分母与结果行数确实相等。
func TestHashScriptTotalMatchesResults(t *testing.T) {
	isolateChildLoaderEnv(t)
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("环境里没有 sha256sum")
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

	roots := []hashRoot{{path: src, isDir: true}}
	res, err := execLocal(context.Background(), hashScript(roots, "sha256sum"), 30*time.Second)
	if err != nil {
		t.Fatalf("执行脚本失败：%v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("脚本退出码 %d，输出：%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}

	var total int64
	var results []string
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if n, ok := parseArchiveTotal(line); ok {
			total = n
			continue
		}
		_, abs, ok := parseHashLine(line, 64)
		if !ok {
			t.Fatalf("无法解析结果行：%q", line)
		}
		rel, ok := relativeUnder(roots, abs)
		if !ok {
			t.Fatalf("结果路径 %q 不在任何选中根下", abs)
		}
		results = append(results, rel)
	}

	// 两个文件，分母与结果数都应是 2 —— 两侧口径一致，进度才会走到 100%。
	if total != 2 || len(results) != 2 {
		t.Fatalf("总数 %d、结果行数 %d，期望都是 2；输出：%s", total, len(results), res.Stdout)
	}
	// 相对路径要能直接给 sha256sum -c 用，所以必须是不带前缀的包内路径。
	// 排序后再比：find 的输出顺序取决于目录内的存储顺序，不是稳定的。
	sort.Strings(results)
	if results[0] != "a.txt" || results[1] != "sub/b.txt" {
		t.Fatalf("相对路径 = %v，期望 [a.txt sub/b.txt]", results)
	}
}
