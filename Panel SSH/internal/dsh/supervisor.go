// Package dsh 以受管子进程的方式运行 DeepSeek Harness（dsh）的 Web UI，
// 供面板内的「DSH 工作台」插件反代访问。
//
// 几条刻意定下的边界：
//
//  1. dsh 本体不进面板二进制。它连同 Node 运行时接近 500MB，把这种体量的第三方
//     运行时塞进一个 19MB 的单二进制面板是不合理的；面板改为把它装在目标机上
//     （见 internal/env 的 dsh 配方），这里只负责"按用户给的命令把它跑起来"，
//     命令未配置时如实报告未配置。
//
//  2. 每台主机一个实例。dsh 监听各自主机的回环地址，面板按主机分别拉起、
//     分别反代，因此宿主机与远端主机各有自己的状态与生命周期。
//
//  3. 懒启动。dsh 常驻约 320MB 内存，面板启动时不该先付这笔开销；
//     只有用户真正打开某台主机的 DSH 工作台时才把它拉起来。
//
//  4. 端口交给操作系统分配（--port 0），再从输出里解析真实端口，
//     避免与宿主上其它服务抢固定端口。
package dsh

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// startTimeout 是等待 dsh 打印访问地址的上限。
	// 首次启动要装载数百个插件，实测需要十几秒，这里给足余量。
	startTimeout = 120 * time.Second
	// stopTimeout 是优雅退出的等待上限，超时后强杀。
	stopTimeout = 8 * time.Second
	// logLimit 是环形日志保留行数，用于启动失败时给出可读原因。
	logLimit = 200
)

// readyPattern 从 dsh 的启动输出里同时取出访问地址、实际端口与一次性令牌：
//
//	dsh web: http://127.0.0.1:13080/?token=xxxx
var readyPattern = regexp.MustCompile(`http://127\.0\.0\.1:(\d+)/\?token=(\S+)`)

// State 是 dsh 的运行状态。
type State string

const (
	// StateDisabled 表示没有配置启动命令，功能不可用。
	StateDisabled State = "disabled"
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFailed   State = "failed"
)

// Status 是对外暴露的状态快照。
type Status struct {
	State     State     `json:"state"`
	Message   string    `json:"message"`
	Port      int       `json:"port"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	Logs      []string  `json:"logs"`
}

// Supervisor 管理**一台主机**上 dsh 子进程的生命周期。
//
// 它不关心自己在跟本地子系统还是远端 SSH 会话打交道 —— 那件事由 launcher
// 承担。同一份状态机同时服务两种落脚点，是"每台主机一个实例"能低成本
// 落地的前提：宿主机与远端主机的就绪判定、失败恢复、日志留存没有一处不同。
type Supervisor struct {
	// launcher 负责"怎么在这台主机上起进程"，构造后不再变化。
	launcher Launcher
	// workDir 是 dsh 的工作目录，决定它默认的文件系统位置。
	workDir string
	// logPath 是面板侧留存的运行日志文件；为空表示不留存。
	logPath string
	// command 是启动命令，可在运行期被替换（见 setCommand）。
	command []string

	mu        sync.Mutex
	proc      Process
	state     State
	message   string
	port      int
	pid       int
	token     string
	startedAt time.Time
	logs      []string
	ready     chan struct{}
	// readyClosed 记录 ready 是否已关闭。
	// 启动成功、启动失败、进程提前退出、以及启动中被 Stop 打断，
	// 这四条路径都可能关闭它；没有这个标记就会出现重复 close 的 panic。
	readyClosed bool
	// watchDone 在 watch 协程回收完进程后关闭。
	// 进程回收只能有一处（proc.Wait），Stop 等待它而不是再 Wait 一次。
	watchDone chan struct{}
}

// closeReadyLocked 关闭 ready 通道（已关闭时是空操作）。调用方必须已持有 s.mu。
func (s *Supervisor) closeReadyLocked() {
	if s.ready == nil || s.readyClosed {
		return
	}
	s.readyClosed = true
	close(s.ready)
}

// newSupervisor 构造一台主机上的管理器。
//
// command 是启动 dsh 的命令（如 "dsh" 或 "node /path/to/dsh/lib/bin.js"），
// 为空表示这台主机尚未配置、功能不可用。
func newSupervisor(launcher Launcher, command, workDir, logPath string) *Supervisor {
	fields := strings.Fields(command)

	state := StateStopped
	message := ""
	if len(fields) == 0 {
		state = StateDisabled
		message = "尚未配置 dsh 启动命令"
	}

	return &Supervisor{
		launcher: launcher,
		workDir:  workDir,
		logPath:  logPath,
		command:  fields,
		state:    state,
		message:  message,
		logs:     make([]string, 0, logLimit),
	}
}

// Available 表示功能是否已配置。
func (s *Supervisor) Available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.command) > 0
}

// Command 返回当前的启动命令原文，供配置界面回显。
func (s *Supervisor) Command() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.command, " ")
}

// setCommand 替换启动命令。只由 Manager 调用 —— 命令要落库，
// 单独改内存里的这一份会让重启后面板拉起的东西与界面显示的不是一回事。
//
// 无论新旧命令是否相同都先停掉当前进程：否则会出现"改了命令但界面上还是旧进程"
// 的状态，用户无从判断新命令到底有没有起作用。新命令在下次访问工作台时拉起。
func (s *Supervisor) setCommand(command string) {
	s.Stop()

	fields := strings.Fields(command)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.command = fields
	s.proc = nil
	if len(fields) == 0 {
		s.state = StateDisabled
		s.message = "尚未配置 dsh 启动命令"
		return
	}
	s.state = StateStopped
	s.message = ""
}

// Status 返回当前状态快照。
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	logs := make([]string, len(s.logs))
	copy(logs, s.logs)

	return Status{
		State:     s.state,
		Message:   s.message,
		Port:      s.port,
		PID:       s.pid,
		StartedAt: s.startedAt,
		Logs:      logs,
	}
}

// Endpoint 返回反代目标地址，未就绪时返回空串。
func (s *Supervisor) Endpoint() (string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateRunning || s.port == 0 {
		return "", "", false
	}
	return fmt.Sprintf("http://127.0.0.1:%d", s.port), s.token, true
}

// EnsureRunning 确保 dsh 已启动，并等待它就绪。
// 已在运行或正在启动时复用同一次启动过程，不会重复拉起。
func (s *Supervisor) EnsureRunning() error {
	if !s.Available() {
		return errors.New("尚未配置 dsh 启动命令")
	}

	s.mu.Lock()
	if s.state == StateRunning {
		s.mu.Unlock()
		return nil
	}
	if s.state == StateStarting {
		wait := s.ready
		s.mu.Unlock()
		select {
		case <-wait:
			return s.lastError()
		case <-time.After(startTimeout):
			return errors.New("等待 dsh 启动超时")
		}
	}

	// 重新开始一次启动
	s.state = StateStarting
	s.message = "正在启动 dsh…"
	s.logs = s.logs[:0]
	s.ready = make(chan struct{})
	s.readyClosed = false
	wait := s.ready
	s.mu.Unlock()

	if err := s.spawn(); err != nil {
		s.fail(err.Error())
		return err
	}

	select {
	case <-wait:
		return s.lastError()
	case <-time.After(startTimeout):
		err := fmt.Errorf("等待 dsh 就绪超时（%s）", startTimeout)
		s.fail(err.Error())
		s.terminate()
		return err
	}
}

func (s *Supervisor) lastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StateFailed {
		return errors.New(s.message)
	}
	return nil
}

func (s *Supervisor) spawn() error {
	// 命令可以在运行期被替换，这里取一次快照，避免与 setCommand 交错。
	s.mu.Lock()
	command := s.command
	s.mu.Unlock()

	if len(command) == 0 {
		return errors.New("尚未配置 dsh 启动命令")
	}

	// 面板自己补齐它需要的子命令与参数：调用方只需给出"怎么调用 dsh"。
	argv := append([]string{}, command[1:]...)
	argv = append(argv, "web", "--no-open", "--host", "127.0.0.1", "--port", "0")
	argv = append([]string{command[0]}, argv...)

	proc, err := s.launcher.Start(argv, s.workDir)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.proc = proc
	s.pid = proc.PID()
	s.startedAt = time.Now()
	s.watchDone = make(chan struct{})
	watchDone := s.watchDone
	s.mu.Unlock()

	go s.consume(proc.Stdout())
	go s.consume(proc.Stderr())
	go s.watch(proc, watchDone)

	return nil
}

// consume 逐行读取子进程输出：既用于识别就绪，也留作失败时的可读线索。
func (s *Supervisor) consume(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		s.appendLog(line)

		match := readyPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		port := 0
		if _, err := fmt.Sscanf(match[1], "%d", &port); err != nil || port == 0 {
			continue
		}

		s.mu.Lock()
		if s.state == StateStarting {
			s.state = StateRunning
			s.port = port
			s.token = match[2]
			s.message = ""
			s.closeReadyLocked()
		}
		s.mu.Unlock()
	}
}

// watch 是唯一的进程回收点：等待进程结束，并在非主动停止时记录为失败。
// 结束后关闭 watchDone，让 Stop 知道进程已被回收，不必也不能再 Wait 一次。
func (s *Supervisor) watch(proc Process, watchDone chan struct{}) {
	defer close(watchDone)

	err := proc.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()

	// 主动停止时状态已由 Stop 置好，这里不再覆盖。
	if s.state == StateStopped {
		return
	}

	s.port = 0
	s.pid = 0
	s.token = ""
	// 无论是"启动中退出"还是"运行中退出"，等待方都该被唤醒。
	s.closeReadyLocked()
	s.state = StateFailed
	if err == nil {
		s.message = "dsh 进程已退出"
	} else {
		s.message = "dsh 进程已退出：" + err.Error()
	}
}

func (s *Supervisor) fail(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closeReadyLocked()
	s.state = StateFailed
	s.message = message
}

func (s *Supervisor) appendLog(line string) {
	s.mu.Lock()
	if len(s.logs) >= logLimit {
		s.logs = s.logs[1:]
	}
	s.logs = append(s.logs, line)
	logPath := s.logPath
	s.mu.Unlock()

	// 落盘放在锁外：磁盘慢的时候不该把状态机一起拖住。
	AppendLog(logPath, "INFO", line)
}

// Stop 停止 dsh 并回收进程，供面板退出与卸载插件时调用。
//
// 进程回收只有 watch 一处（proc.Wait）。旧实现在这里又对同一个 os.Process
// 调了一次 Wait：两次并发 Wait 会争抢 waitpid，其中一方拿到 ECHILD，
// 还可能干扰输出管道的收尾。现在改为等 watch 结束。
func (s *Supervisor) Stop() {
	s.mu.Lock()
	proc := s.proc
	watchDone := s.watchDone
	s.state = StateStopped
	s.message = ""
	s.port = 0
	s.pid = 0
	s.token = ""
	// 启动中的等待者必须被唤醒：否则它们会一直等到 startTimeout（120 秒），
	// 期间卸载插件、改启动命令这类操作都会僵在那里。
	s.closeReadyLocked()
	s.mu.Unlock()

	if proc == nil {
		return
	}
	proc.Terminate()

	if watchDone == nil {
		return
	}
	select {
	case <-watchDone:
	case <-time.After(stopTimeout):
		// 优雅退出无响应时强杀，避免留下孤儿进程白占内存。
		proc.Kill()
		// 强杀之后给回收留一点时间，但不必无限等。
		select {
		case <-watchDone:
		case <-time.After(stopTimeout):
		}
	}
}

// terminate 用一次优雅终止收尾，供启动超时等"不等回收"的路径使用。
//
// 与 Stop 的差别是它不等进程退出：调用方（EnsureRunning 超时）已经把
// 状态置成失败，此时只需要把进程收掉，进程真正退出会由 watch 走完剩下的路。
func (s *Supervisor) terminate() {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()

	if proc != nil {
		proc.Terminate()
	}
}
