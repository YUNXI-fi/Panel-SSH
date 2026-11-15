package api

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"panelssh/internal/config"
	"panelssh/internal/sysinfo"
)

// 本文件是「系统日志」模块的后端：目标机的 journalctl / 日志文件 / dmesg，
// 以及面板自身那份落盘日志（见 config.LogName）。
//
// 命令构造与参数校验都在 sysinfo 里（纯函数、可单测），这里只负责提权与串联。
// 前三个来源一律走 escalatePreferred：dmesg 与部分日志文件普通用户读得到，
// 而 auth.log / secure 只能 root 读。能提权就提权，提不了也让命令真跑一次，
// 把目标机自己说的 "Permission denied" 原样交给界面，而不是在面板侧先拦下来。

// logReadTimeout 是单次读日志的时长上限。
//
// 比默认的 60s 短：这些都是本地文件读取，慢一定是目标机出了别的问题
// （磁盘卡死、journal 库损坏），让它早点了结比挂住窗口强。
const logReadTimeout = 15 * time.Second

// logView 是一次日志读取的返回。
type logView struct {
	Source string `json:"source"`
	// File/Unit/Priority 回显实际生效的取值：行数与参数都可能被归一化，
	// 界面显示的应该是"真跑的那条命令"，不是用户输入的原样。
	File     string   `json:"file,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Priority string   `json:"priority,omitempty"`
	Lines    []string `json:"lines"`
	// Note 是"没读到内容"时的原因说明（权限不足等），有内容时为空。
	Note string `json:"note,omitempty"`
	// Truncated 表示输出被 execOnHost 的体积上限截断了，取到的是日志开头一段。
	Truncated bool `json:"truncated"`
}

// handleLogSources 返回目标机上有哪些日志可看。
func (s *Server) handleLogSources(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	res, err := s.execEscalated(r.Context(), hostID, sysinfo.LogSourcesCommand(), logReadTimeout, escalatePreferred)
	if err != nil {
		s.writeHostError(w, err, "读取日志来源失败")
		return
	}
	writeJSON(w, http.StatusOK, sysinfo.ParseLogSources(res.Stdout))
}

// handleHostLogs 读取目标机的一次日志内容。
func (s *Server) handleHostLogs(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	lines := 0
	if raw := query.Get("lines"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "lines 参数无效")
			return
		}
		lines = parsed
	}

	// 参数直接拼进目标机上执行的命令，因此只接受能枚举出来的形状。
	// 校验与命令构造同源（sysinfo），避免两处各写一套白名单。
	lq, err := sysinfo.ValidateLogQuery(query.Get("source"), query.Get("file"),
		query.Get("unit"), query.Get("priority"), lines)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	res, execErr := s.execEscalated(r.Context(), hostID, lq.Command(), logReadTimeout, escalatePreferred)
	if execErr != nil && res == nil {
		s.writeHostError(w, execErr, "读取日志失败")
		return
	}

	view := logView{
		Source:    lq.Source,
		File:      lq.File,
		Unit:      lq.Unit,
		Priority:  lq.Priority,
		Lines:     sysinfo.SplitLogLines(res.Stdout),
		Truncated: res.StdoutClipped,
	}
	// 退出码非 0 不是错误：命令的报错已经并进 stdout 一起显示（见 Command 的注释），
	// 这里只额外补一句"为什么读不到"的解释。
	view.Note = sysinfo.LogPermissionNote(res.Stdout)
	if len(view.Lines) == 0 && view.Note == "" {
		view.Note = "这条来源没有返回任何内容。"
	}
	writeJSON(w, http.StatusOK, view)
}

// panelLogTailBytes 是面板日志单次读取的尾部字节数。
//
// 面板日志随运行时间增长，不能整份读进内存再切尾。512 KiB 足够覆盖
// 上千行日志，也保证了"上次启动到崩溃"那一段大概率能一次看全。
const panelLogTailBytes = 512 * 1024

// panelLogView 是面板自身日志的返回。
type panelLogView struct {
	Path  string   `json:"path"`
	Bytes int64    `json:"bytes"`
	Lines []string `json:"lines"`
	Note  string   `json:"note,omitempty"`
}

// handlePanelLogs 读取面板自身落盘的日志。
//
// 纯本地文件读，不经过 SSH，也不走 execEscalated —— 面板以普通进程运行、
// 不是 systemd 服务，没有 journalctl 可查，这份文件是唯一的回看入口。
func (s *Server) handlePanelLogs(w http.ResponseWriter, r *http.Request) {
	lines := sysinfo.DefaultLogLines
	if raw := r.URL.Query().Get("lines"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "lines 参数无效")
			return
		}
		if parsed > 0 {
			lines = parsed
		}
	}
	if lines > sysinfo.MaxLogLines {
		lines = sysinfo.MaxLogLines
	}

	path := filepath.Join(s.cfg.DataDir, config.LogName)
	view := panelLogView{Path: path, Lines: []string{}}

	read, err := readTailLines(path, lines)
	switch {
	case err == nil:
		fi, statErr := os.Stat(path)
		if statErr == nil {
			view.Bytes = fi.Size()
		}
		view.Lines = read
		if len(read) == 0 {
			view.Note = "日志文件是空的，面板还没有写出任何内容。"
		}
	case os.IsNotExist(err):
		// 刚部署、或面板启动到写第一条日志之间的窗口期。这不是错误。
		view.Note = "面板日志尚未产生：服务刚启动时可能还没有输出，或日志目录不可写。"
	default:
		view.Note = "面板日志读取失败：" + err.Error()
	}
	writeJSON(w, http.StatusOK, view)
}

// readTailLines 读取文件末尾若干行。
//
// 从尾部往回定位而不是整份读入：日志是只增不改的，只有末尾一段有用，
// 而整份读在有几十兆历史日志时要白白吃掉内存。
func readTailLines(path string, lines int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := int64(0)
	midLine := false
	if fi.Size() > panelLogTailBytes {
		offset = fi.Size() - panelLogTailBytes
		midLine = true
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	if midLine {
		// 起点落在某行中间：第一段是半行，丢掉它，否则界面第一行会是
		// 掐头去尾的半句话。
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}

	all := sysinfo.SplitLogLines(string(data))
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return all, nil
}
