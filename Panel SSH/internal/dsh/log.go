package dsh

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// logTimeLayout 与界面上展示的日志时间一致，方便直接对照。
const logTimeLayout = "2006-01-02 15:04:05"

// maxLogReadBytes 是读日志时最多回看的字节数。
//
// dsh 的输出会一直追加，文件可能长到几十兆；面板只需要最近的若干行，
// 从尾部定长读取比把整个文件读进内存更可控。
const maxLogReadBytes = 8 << 20

// AppendLog 往某台主机的 dsh 日志里追加一行面板侧事件。
//
// 日志按主机分文件、落在面板自己的数据目录下：远端主机上的 dsh 输出
// 是经 SSH 会话回到面板的，只能由面板这边留存，目标机上没有等价物。
func AppendLog(path, level, text string) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()

	level = strings.ToUpper(strings.TrimSpace(level))
	if level == "" {
		level = "INFO"
	}
	_, _ = fmt.Fprintf(file, "%s [%s] %s\n", time.Now().Format(logTimeLayout), level, text)
}

// ReadLogTail 返回日志文件末尾最多 maxLines 行。
//
// 文件不存在不是错误：新主机还没启动过 dsh 时本来就没有日志。
func ReadLogTail(path string, maxLines int) ([]string, error) {
	if maxLines <= 0 {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	start := int64(0)
	if info.Size() > maxLogReadBytes {
		start = info.Size() - maxLogReadBytes
		if _, err := file.Seek(start, 0); err != nil {
			return nil, err
		}
	}

	lines := make([]string, 0, maxLines)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > maxLines {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// TruncateLog 清空日志文件。
func TruncateLog(path string) error {
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}
