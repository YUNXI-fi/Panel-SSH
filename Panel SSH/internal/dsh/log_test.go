package dsh

import (
	"path/filepath"
	"strings"
	"testing"
)

// 日志落盘是"面板重启后还能回看上次为什么没起来"的唯一依赖。
func TestAppendAndReadLogTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "0.log")

	AppendLog(path, "INFO", "启动 dsh")
	AppendLog(path, "error", "端口被占用")

	lines, err := ReadLogTail(path, 10)
	if err != nil {
		t.Fatalf("读取日志失败：%v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("期望 2 行，实际 %d：%v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "[INFO] 启动 dsh") {
		t.Fatalf("第一行内容不对：%q", lines[0])
	}
	if !strings.Contains(lines[1], "[ERROR] 端口被占用") {
		t.Fatalf("级别应当被统一成大写：%q", lines[1])
	}

	tail, err := ReadLogTail(path, 1)
	if err != nil {
		t.Fatalf("读取尾部失败：%v", err)
	}
	if len(tail) != 1 || !strings.Contains(tail[0], "端口被占用") {
		t.Fatalf("尾部应当只剩最后一行：%v", tail)
	}

	if err := TruncateLog(path); err != nil {
		t.Fatalf("清空日志失败：%v", err)
	}
	empty, err := ReadLogTail(path, 10)
	if err != nil {
		t.Fatalf("清空后读取失败：%v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("清空后不该还有内容：%v", empty)
	}
}

// 文件不存在不是错误：新主机本来就没有日志。
func TestReadLogTailMissingFile(t *testing.T) {
	lines, err := ReadLogTail(filepath.Join(t.TempDir(), "missing.log"), 10)
	if err != nil {
		t.Fatalf("文件不存在应当返回空而不报错：%v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("期望空列表，实际 %v", lines)
	}
}
