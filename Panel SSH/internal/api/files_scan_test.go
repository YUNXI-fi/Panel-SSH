package api

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestScanBatches 校验清单分批。
//
// 切批的边界错了不会让任何一处报错：少一批是"有几个文件凭空没出现在清单里"，
// 只推一批是"大目录永远显示不全"—— 两种都只在界面上看得出来。
func TestScanBatches(t *testing.T) {
	paths := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = string(rune('a' + i%26))
		}
		return out
	}

	cases := []struct {
		name string
		in   int
		size int
		want []int
	}{
		{"空输入不产生批", 0, 3, nil},
		{"恰好一批", 3, 3, []int{3}},
		{"多出一条要再起一批", 4, 3, []int{3, 1}},
		{"整批切完，不补空批", 6, 3, []int{3, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanBatches(paths(tc.in), tc.size)
			if len(got) != len(tc.want) {
				t.Fatalf("批数 = %d，期望 %d", len(got), len(tc.want))
			}
			total := 0
			for i, chunk := range got {
				if len(chunk) != tc.want[i] {
					t.Fatalf("第 %d 批有 %d 条，期望 %d", i, len(chunk), tc.want[i])
				}
				total += len(chunk)
			}
			if total != tc.in {
				t.Fatalf("各批合计 %d 条，期望 %d —— 有文件在切批时丢了", total, tc.in)
			}
		})
	}
}

// 传了非法批大小（0 或负数）时退回默认值，而不是切成无限多个空批。
func TestScanBatchesFallsBackToDefaultSize(t *testing.T) {
	got := scanBatches(make([]string, scanBatchSize+1), 0)
	if len(got) != 2 || len(got[0]) != scanBatchSize || len(got[1]) != 1 {
		t.Fatalf("批大小 0 应退回默认值 %d，实际切成了 %d 批", scanBatchSize, len(got))
	}
}

// TestParseSizeLine 校验 "<字节数>\t<路径>" 的解析。
//
// 大小配错了不会报任何错：界面上只会出现一个"大小不对"的数字，而它看起来完全正常。
// 所以制表符切分、非数字行、缺路径这些边界都要钉住。
func TestParseSizeLine(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantSize int64
		wantPath string
		wantOK   bool
	}{
		{"普通一行", "2048\t/data/a.zip", 2048, "/data/a.zip", true},
		// 路径里可以有制表符与空格：字节数一定是纯数字，所以从左边切一刀就够。
		{"路径里带制表符", "7\t/data/带\t制表符 的名字.log", 7, "/data/带\t制表符 的名字.log", true},
		{"0 字节是真值，不是未知", "0\t/var/log/empty.log", 0, "/var/log/empty.log", true},
		// wc 那条路会多吐一些不成对的行；认不出来就跳过，绝不能当成某个文件的大小。
		{"只有数字没有路径", "1234", 0, "", false},
		{"报错文本", "wc: /x: Permission denied", 0, "", false},
		{"字节数不是数字", "abc\t/data/a.zip", 0, "", false},
		{"缺少字节数", "\t/data/a.zip", 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size, path, ok := parseSizeLine(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v，期望 %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if size != tc.wantSize || path != tc.wantPath {
				t.Fatalf("解析为 (%d, %q)，期望 (%d, %q)", size, path, tc.wantSize, tc.wantPath)
			}
		})
	}
}

// TestScanSizesCommandCountsPromptly 跑一遍生成的命令，确认它一次就把大小报齐。
//
// 命令写错了不会报错，只会让界面上的大小整列变成"—"——而这正是这次要修的症状。
// 这里连同"命令的输出形态"一起钉住：每条路径只吐一行裸字节数，与入参顺序一一对应。
func TestScanSizesCommandCountsPromptly(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.log")
	small := filepath.Join(dir, "小 文件.bin")
	if err := os.WriteFile(big, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	paths := []string{big, small}
	// 命令里没有 wc/find 依赖之外的东西：这里直接让 shell 跑它，与目标机上一致。
	cmd := exec.Command("sh", "-c", scanSizesCommand(paths))
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("命令执行失败：%v（stderr=%s）", err, stderr.String())
	}

	// 按行取，是因为解析端就是这么取第几条的：顺序错了大小就会配错文件。
	var got []int64
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		size, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			t.Fatalf("输出行 %q 不是裸字节数（输出=%q）", line, out.String())
		}
		got = append(got, size)
	}
	if len(got) != 2 || got[0] != 4096 || got[1] != 5 {
		t.Fatalf("报出的大小 = %v，期望 [4096 5]（输出=%q）", got, out.String())
	}
}

// TestScanSizesCommandKeepsGoingAfterMissingPath 校验个别路径读不到时整批不作废。
//
// 文件可以在 find 列出它之后、问大小的这一瞬被删掉，也可能没有 stat 权限。
// 那时只有那一条没有输出、退出码非 0，后面的必须照常报出来 ——
// 不能因为一条读不到就把整批 200 个大小全丢了。
//
// 读不到的那条在 stdout 里不留占位行，所以后面的号会往前挪。这一点是刻意的：
// 要让它占号就得往 stdout 打印一行非数字的占位文本，那样"输出里只有字节数"
// 这条契约就破了 —— 而契约破了，解析端就得反过来去认错误文本，正是要避免的。
// 号会挪也不会配错：文件被删发生在扫描之后，大小配的是扫描当时的那一批。
func TestScanSizesCommandKeepsGoingAfterMissingPath(t *testing.T) {
	dir := t.TempDir()
	exists := filepath.Join(dir, "在的.txt")
	missing := filepath.Join(dir, "不在的.txt")
	if err := os.WriteFile(exists, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", scanSizesCommand([]string{missing, exists}))
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("有路径不存在时命令不该失败：%v", err)
	}
	// 读不到的原因要留着给人看：这里没有 log，唯一的出口就是 stderr。
	if !strings.Contains(stderr.String(), "不在的.txt") {
		t.Fatalf("读不到的路径没在 stderr 里留下线索：%q", stderr.String())
	}

	sizes := resolveScanSizesFromOutput(out.String(), 2)
	if sizes[0] != 3 {
		t.Fatalf("存在的那条应照常报出 3，实际 %d（输出=%q）", sizes[0], out.String())
	}
	if sizes[1] != -1 {
		t.Fatalf("少了一条时后面的号应留在 -1，实际 %d（输出=%q）", sizes[1], out.String())
	}
}

// resolveScanSizesFromOutput 用与 resolveScanSizes 相同的规则把一段输出还原成大小，
// 好让测试不必真的连一台主机。规则必须与那边一致，否则测的就不是同一个东西了。
func resolveScanSizesFromOutput(stdout string, n int) []int64 {
	sizes := make([]int64, n)
	for i := range sizes {
		sizes[i] = -1
	}
	seen := 0
	for _, line := range strings.Split(stdout, "\n") {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		size, err := strconv.ParseInt(text, 10, 64)
		if err != nil || size < 0 {
			continue
		}
		if seen < n {
			sizes[seen] = size
			seen++
		}
	}
	return sizes
}
