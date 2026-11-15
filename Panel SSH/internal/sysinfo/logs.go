package sysinfo

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 本文件负责"系统日志"窗口的采集侧：生成在目标机上执行的只读命令，
// 以及把输出解析成结构化数据。命令的执行与提权在 api 层（见 api/logs.go），
// 与网络、存储两块的职责划分一致。

// 日志来源。三者的取法完全不同，因此不合成一个"读日志"命令。
const (
	LogSourceJournal = "journal"
	LogSourceFile    = "file"
	LogSourceDmesg   = "dmesg"
)

const (
	// DefaultLogLines 是界面默认取的行数。
	DefaultLogLines = 200
	// MaxLogLines 是单次取行的上限。日志可能每秒几千行，不设上限的话
	// 一次请求就能拉回几十兆，目标机与面板都吃不消。
	MaxLogLines = 2000
)

// LogFile 是目标机上一个可读的日志文件。
type LogFile struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	MTime int64  `json:"mtime"`
}

// LogSources 是"这台机器上有什么日志可看"。
type LogSources struct {
	Journal bool      `json:"journal"`
	Dmesg   bool      `json:"dmesg"`
	Files   []LogFile `json:"files"`
}

// logFileCandidates 是面板优先去看的日志文件。
//
// 刻意不遍历整个 /var/log：那里还堆着 btmp/wtmp/journal（二进制）与各服务的
// 私有目录，全列出来只会把真正有用的几个埋掉。固定候选 + 一层 *.log 通配
// 既覆盖主流发行版的差异，也避开了 GNU find 的 -printf（busybox 不支持）。
var logFileCandidates = []string{
	"/var/log/syslog",
	"/var/log/messages",
	"/var/log/auth.log",
	"/var/log/secure",
	"/var/log/kern.log",
	"/var/log/user.log",
	"/var/log/boot.log",
	"/var/log/dpkg.log",
	"/var/log/yum.log",
	"/var/log/dnf.log",
	"/var/log/cron",
	"/var/log/cron.log",
	"/var/log/maillog",
	"/var/log/nginx/access.log",
	"/var/log/nginx/error.log",
	"/var/log/apache2/access.log",
	"/var/log/apache2/error.log",
	"/var/log/mysql/error.log",
}

// LogSourcesCommand 生成采集日志来源清单的脚本。
func LogSourcesCommand() string {
	var b strings.Builder
	b.WriteString("export LC_ALL=C\n")
	b.WriteString("echo \"###JOURNAL\"\n")
	b.WriteString("command -v journalctl >/dev/null 2>&1 && echo yes || echo no\n")
	b.WriteString("echo \"###DMESG\"\n")
	b.WriteString("command -v dmesg >/dev/null 2>&1 && echo yes || echo no\n")
	b.WriteString("echo \"###FILES\"\n")

	// 先判存在再取元数据：stat 一个不存在的路径在不同平台报错文案不一，
	// 与其事后过滤错误文本，不如让不存在的情况干脆没有输出。
	//
	// stat -c 是 coreutils 与 busybox 都支持的最小交集（%s 字节数、
	// %Y 修改时间、%n 路径）。
	for _, path := range logFileCandidates {
		fmt.Fprintf(&b, "[ -f %s ] && stat -c '%%s\\t%%Y\\t%%n' %s 2>/dev/null\n", path, path)
	}
	// 一层通配覆盖候选之外的文件（发行版差异、服务自定义的日志路径）。
	b.WriteString("for f in /var/log/*.log; do [ -f \"$f\" ] && stat -c '%s\\t%Y\\t%n' \"$f\" 2>/dev/null; done\n")
	b.WriteString("echo \"###END\"\n")
	return b.String()
}

// ParseLogSources 解析 LogSourcesCommand 的输出，按修改时间倒序给出文件清单。
func ParseLogSources(output string) LogSources {
	sections := splitSections(output)

	sources := LogSources{
		Journal: strings.TrimSpace(sections["###JOURNAL"]) == "yes",
		Dmesg:   strings.TrimSpace(sections["###DMESG"]) == "yes",
		Files:   []LogFile{},
	}

	// 候选与通配可能命中同一个文件，按路径去重。
	seen := make(map[string]bool)
	for _, line := range strings.Split(sections["###FILES"], "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 3 {
			continue
		}
		size, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		mtime, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		path := fields[2]
		// 只认面板自己认可的位置：命令输出不该成为"文件列表里冒出一条奇怪路径"的来源。
		if !strings.HasPrefix(path, "/var/log/") || seen[path] {
			continue
		}
		seen[path] = true
		sources.Files = append(sources.Files, LogFile{Path: path, Bytes: size, MTime: mtime})
	}

	// 最近被写过的排前面：排障时要看的多半是当前正在写的那个。
	sort.Slice(sources.Files, func(i, j int) bool {
		if sources.Files[i].MTime != sources.Files[j].MTime {
			return sources.Files[i].MTime > sources.Files[j].MTime
		}
		return sources.Files[i].Path < sources.Files[j].Path
	})
	return sources
}

// LogQuery 是一次已校验的日志读取请求。
type LogQuery struct {
	Source   string
	File     string
	Unit     string
	Priority string
	Lines    int
}

var (
	// 路径锁死在 /var/log 下，字符集也收紧到"日志文件名该有的样子"。
	// 命令是用 sh 跑的，参数直接进命令行，这条正则就是"读不到 /etc/shadow"的底线。
	logFilePathRe = regexp.MustCompile(`^/var/log/[A-Za-z0-9._/-]+$`)
	// systemd 单元名的合法字符：字母数字与 @ . : - 。
	logUnitRe = regexp.MustCompile(`^[A-Za-z0-9@._:-]+$`)
)

// logPriorities 是 journalctl -p 能接受的取值（数字与名称两种写法）。
var logPriorities = map[string]bool{
	"0": true, "1": true, "2": true, "3": true,
	"4": true, "5": true, "6": true, "7": true,
	"emerg": true, "alert": true, "crit": true, "err": true,
	"warning": true, "notice": true, "info": true, "debug": true,
}

// ValidateLogQuery 校验并归一化入参。
//
// 这些参数会被拼进目标机上执行的 shell 命令，所以只接受"能枚举出来的形状"，
// 不做转义后拼接的宽容处理 —— 不合法就报错，让调用方改。
// 行数不是安全边界（它只是个数字），超出范围按上限收敛。
func ValidateLogQuery(source, file, unit, priority string, lines int) (LogQuery, error) {
	query := LogQuery{Source: source, Lines: lines}
	if query.Lines <= 0 {
		query.Lines = DefaultLogLines
	}
	if query.Lines > MaxLogLines {
		query.Lines = MaxLogLines
	}

	switch source {
	case LogSourceJournal:
		if unit != "" {
			if !logUnitRe.MatchString(unit) {
				return LogQuery{}, fmt.Errorf("systemd 单元名不合法")
			}
			query.Unit = unit
		}
		if priority != "" {
			normalized := strings.ToLower(priority)
			if !logPriorities[normalized] {
				return LogQuery{}, fmt.Errorf("日志级别不合法")
			}
			query.Priority = normalized
		}
	case LogSourceFile:
		if !logFilePathRe.MatchString(file) || strings.Contains(file, "..") {
			return LogQuery{}, fmt.Errorf("日志文件路径不合法，只允许 /var/log 下的文件")
		}
		query.File = file
	case LogSourceDmesg:
	default:
		return LogQuery{}, fmt.Errorf("未知的日志来源：%s", source)
	}
	return query, nil
}

// Command 生成读取日志的命令。
//
// 一律把 stderr 并进 stdout：读不到日志时的原因（Permission denied /
// command not found）比一片空白有用得多，界面直接把它显示出来。
func (q LogQuery) Command() string {
	switch q.Source {
	case LogSourceJournal:
		command := fmt.Sprintf("journalctl -n %d --no-pager -o short-iso", q.Lines)
		if q.Unit != "" {
			command += " -u " + q.Unit
		}
		if q.Priority != "" {
			command += " -p " + q.Priority
		}
		return command + " 2>&1"
	case LogSourceFile:
		return fmt.Sprintf("tail -n %d %s 2>&1", q.Lines, q.File)
	default:
		// dmesg 自己不带 -n 这类行数选项（各平台实现不一致），用 tail 统一收敛。
		return fmt.Sprintf("dmesg 2>&1 | tail -n %d", q.Lines)
	}
}

// LogPermissionNote 从输出里识别"因为权限不足而读不到"。
//
// 不靠"换一条命令再试一次"：目标机明确说了 Permission denied 就是结论。
// 界面据此提示用户该来源需要 root，比给一片空白有用。
func LogPermissionNote(raw string) string {
	for _, marker := range []string{"Permission denied", "Operation not permitted", "permission denied"} {
		if strings.Contains(raw, marker) {
			return "目标机拒绝了这次读取（" + marker + "）：该日志需要 root 权限，而面板在这台主机上无法提权。请在面板里为该主机配置可用的提权口令，或改用 root 登录。"
		}
	}
	return ""
}

// SplitLogLines 把命令输出切成行，供界面直接渲染。
func SplitLogLines(raw string) []string {
	raw = strings.TrimRight(raw, "\r\n")
	if raw == "" {
		return []string{}
	}
	lines := strings.Split(raw, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines
}
