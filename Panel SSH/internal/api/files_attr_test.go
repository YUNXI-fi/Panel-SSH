package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// stat 命令与它的解析器是一对必须同步的代码：字段个数、顺序、单位任意一处改动
// 都会让属性面板显示错值，而这类错误不会让任何一处编译或逻辑失败。
// 所以这里用真的 sh 和真的 stat 跑一遍。
func TestStatCommandRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("stat"); err != nil {
		t.Skip("环境里没有 stat")
	}

	base := t.TempDir()
	target := filepath.Join(base, "sample file.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o640); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	// 造一个硬链接：nlink 应当数到 2，这是唯一能验证该字段是否真的取到的办法。
	if err := os.Link(target, filepath.Join(base, "alias.txt")); err != nil {
		t.Fatalf("建硬链接失败：%v", err)
	}

	res, err := execLocal(context.Background(), statCommand(target), 30*time.Second)
	if err != nil {
		t.Fatalf("执行 stat 失败：%v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("stat 退出码 %d：%s", res.ExitCode, res.Stderr)
	}

	fields, err := parseStatLine(res.Stdout)
	if err != nil {
		t.Fatalf("解析失败：%v（原始输出 %q）", err, res.Stdout)
	}
	if fields.Owner == "" || fields.Group == "" {
		t.Fatalf("属主/属组为空，原始输出 %q", res.Stdout)
	}
	if fields.Nlink != 2 {
		t.Fatalf("硬链接数 = %d，期望 2", fields.Nlink)
	}
	if fields.Inode <= 0 {
		t.Fatalf("inode = %d，期望正数", fields.Inode)
	}
	// %b 的单位是 512 字节块：解析后必须已经乘回字节，否则界面上的占用会小 512 倍。
	if fields.Blocks < 512 || fields.Blocks%512 != 0 {
		t.Fatalf("占用 = %d 字节，期望 512 的正整数倍", fields.Blocks)
	}
	// ctime 是"元数据最后变更时间"，刚写完文件必然接近当前时刻。
	if fields.Ctime.IsZero() || time.Since(fields.Ctime) > time.Hour {
		t.Fatalf("ctime = %v，期望接近当前时刻", fields.Ctime)
	}
}

// 目标机的 stat 是 busybox 简化版或输出被截断时，解析必须失败而不是给出一堆错值 ——
// 调用方据此退回空值，界面显示"—"，好过显示一个错误的属主。
func TestParseStatLineRejectsBadOutput(t *testing.T) {
	cases := []string{
		"",
		"root",
		"root\troot\t1700000000",
		"root\troot\t1700000000\t2\t12345\t8\textra",
		"stat: cannot statx '/x': No such file or directory",
	}
	for _, text := range cases {
		if _, err := parseStatLine(text); err == nil {
			t.Fatalf("parseStatLine(%q) 应当失败", text)
		}
	}
}

// 权限位回填必须保住 setuid/setgid/sticky 那一位：丢了它，用户在属性面板里
// 点一次保存就会把 /usr/bin/sudo 这类文件的 setuid 悄悄抹掉。
func TestOctalPermKeepsSpecialBits(t *testing.T) {
	cases := []struct {
		mode os.FileMode
		want string
	}{
		{0o644, "644"},
		{0o755, "755"},
		{0o600, "600"},
		{os.FileMode(0o755) | os.ModeSetuid, "4755"},
		{os.FileMode(0o755) | os.ModeSetgid, "2755"},
		{os.FileMode(0o777) | os.ModeSticky, "1777"},
	}
	for _, tc := range cases {
		if got := octalPerm(tc.mode); got != tc.want {
			t.Fatalf("octalPerm(%v) = %q，期望 %q", tc.mode, got, tc.want)
		}
	}
}

// touch -t 的写法是面板与目标机之间的约定：时区前缀或格式写错，
// 用户设的 10:00 会落成另一个时刻，而界面上看不出任何异常。
func TestTouchStampUsesUTC(t *testing.T) {
	// 2025-09-18 10:30:45 UTC
	epoch := time.Date(2025, 9, 18, 10, 30, 45, 0, time.UTC).Unix()
	if got := touchStamp(epoch); got != "202509181030.45" {
		t.Fatalf("touchStamp = %q，期望 %q", got, "202509181030.45")
	}
}

// 用户名/组名的校验只挡会让 chown 语义变掉的东西，别的一律放过：
// 真正的注入防护在 shellQuotePath 里，这里多设白名单只会挡住合法的用户名。
func TestValidUserName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"root", true},
		{"www-data", true},
		{"user_1", true},
		{"", false},
		{"a:b", false},
		{"a b", false},
		{"a\nb", false},
	}
	for _, tc := range cases {
		if got := validUserName(tc.name); got != tc.want {
			t.Fatalf("validUserName(%q) = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}
