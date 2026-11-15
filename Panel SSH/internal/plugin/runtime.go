package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 插件进程管理的固定名字。
const (
	// pidFileName / logFileName 都落在插件自己的数据目录里。
	pidFileName = "pid"
	logFileName = "runtime.log"
	// logLimit 是内存里保留的日志行数，供状态接口与失败原因使用。
	logLimit = 200
)

// 插件进程管理的等待时长（生产值）。
//
// 它们由 RuntimeTimings 注入给管理器，而不是被各处直接引用：后台协程会读这些值，
// 而测试需要把它们压到毫秒级 —— 包级可变变量会让两边产生数据竞争。
const (
	// readyTimeout 是等待进程通过 health 探测的上限。
	readyTimeout = 20 * time.Second
	// readyGrace 是探测协程万一没能收尾时的兜底宽限。
	readyGrace = 5 * time.Second
	// readyInterval 是两次 health 探测之间的间隔。
	readyInterval = 200 * time.Millisecond
	// healthTimeout 是单次 health 探测的超时。
	healthTimeout = 3 * time.Second

	// restartLimit 是连续失败后自动重启的次数上限。
	restartLimit = 3
	// restartBaseDelay 是首次重试前的等待，之后逐次翻倍。
	restartBaseDelay = time.Second
	// restartResetAfter 表示"稳定运行这么久之后再崩，重新算作第一次失败"：
	// 否则一个跑了一整天才崩一次的进程，会被几天前的失败次数拖累到不再重启。
	restartResetAfter = time.Minute

	// 空闲回收的扫描间隔。
	idleScanInterval = 30 * time.Second

	// maxUnixSocketPath 是 Unix socket 路径的上限（sun_path 字段 108 字节，含结尾 NUL）。
	//
	// 超了内核会直接拒绝 bind，而插件进程只会以一个看不懂的退出码死掉。
	// 路径长度取决于面板的数据目录深度，清单阶段算不出来，只能在启动前判。
	maxUnixSocketPath = 108
)

// RuntimeTimings 是三个可以注入的等待时长。
//
// 单独抽出来是为了可测：就绪超时、退避重启、空闲回收都是必须验证的行为，
// 按生产值走一遍要好几分钟，而它们又都在后台协程里被读取。
type RuntimeTimings struct {
	// ReadyTimeout 是等待进程通过 health 探测的上限。
	ReadyTimeout time.Duration
	// RestartDelay 是崩溃后首次重试前的等待，之后逐次翻倍。
	RestartDelay time.Duration
	// IdleScan 是空闲回收的扫描间隔。
	IdleScan time.Duration
}

// DefaultRuntimeTimings 是生产使用的时长。
func DefaultRuntimeTimings() RuntimeTimings {
	return RuntimeTimings{
		ReadyTimeout: readyTimeout,
		RestartDelay: restartBaseDelay,
		IdleScan:     idleScanInterval,
	}
}

// RuntimeState 是插件后端进程的状态。
type RuntimeState string

const (
	StateStopped  RuntimeState = "stopped"
	StateStarting RuntimeState = "starting"
	StateRunning  RuntimeState = "running"
	StateFailed   RuntimeState = "failed"
)

// RuntimeStatus 是插件进程的状态快照。
type RuntimeStatus struct {
	State     RuntimeState `json:"state"`
	Message   string       `json:"message"`
	Version   string       `json:"version,omitempty"`
	PID       int          `json:"pid,omitempty"`
	StartedAt time.Time    `json:"startedAt,omitempty"`
	Failures  int          `json:"failures"`
	Logs      []string     `json:"logs,omitempty"`
}

// RuntimeManager 管理面板机上全部的插件后端进程。
//
// 进程与插件一一对应（按 {org}/{name}），不按主机分：插件进程跑在面板机上，
// 面向哪台目标机只是 RPC 调用的参数（设计文档 5.3）。按主机分会在管 50 台
// 机器时起 50 个跑着同一份代码的进程。
type RuntimeManager struct {
	// pluginsRoot 是插件安装包根目录，可执行文件相对它解析。
	pluginsRoot string
	// runtimeRoot 是插件运行时数据根目录，socket、pid、日志都在这里。
	runtimeRoot string
	// timings 是等待时长，构造后不再变化，因此后台协程可以无锁读取。
	timings RuntimeTimings
	// tokens 持有各插件进程的调用凭据。
	tokens *TokenStore

	mu        sync.Mutex
	instances map[string]*runtimeInstance
	done      chan struct{}
	stopOnce  sync.Once
}

// NewRuntimeManager 构造运行时管理器。
func NewRuntimeManager(pluginsRoot, runtimeRoot string) *RuntimeManager {
	return NewRuntimeManagerWithTimings(pluginsRoot, runtimeRoot, DefaultRuntimeTimings())
}

// NewRuntimeManagerWithTimings 同上，但等待时长由调用方给出。
func NewRuntimeManagerWithTimings(pluginsRoot, runtimeRoot string, timings RuntimeTimings) *RuntimeManager {
	// 取绝对路径：/proc/<pid>/cwd 读出来是绝对路径，清理残留时要拿它比对。
	root, err := filepath.Abs(runtimeRoot)
	if err != nil {
		root = runtimeRoot
	}

	manager := &RuntimeManager{
		pluginsRoot: pluginsRoot,
		runtimeRoot: root,
		timings:     timings,
		tokens:      newTokenStore(),
		instances:   make(map[string]*runtimeInstance),
		done:        make(chan struct{}),
	}
	manager.cleanupOrphans()
	go manager.idleLoop()
	return manager
}

// EnsureRunning 确保指定插件的后端进程已就绪。
//
// version 参与判断：同一插件只跑一个进程，装了两个版本时以当前请求的版本为准，
// 版本不同就先停掉旧的再起新的。
func (m *RuntimeManager) EnsureRunning(spec *RuntimeSpec, org, name, version string) error {
	if spec == nil || spec.Kind != RuntimeProcess {
		return errors.New("该插件没有声明后端进程")
	}

	select {
	case <-m.done:
		return errors.New("面板正在退出")
	default:
	}

	return m.instance(org, name).ensureRunning(
		spec, version, m.pkgDir(org, name, version), m.dataDir(org, name))
}

// SocketPath 返回该插件进程的 Unix Socket 路径；进程从未启动时返回空串。
func (m *RuntimeManager) SocketPath(org, name string) string {
	in := m.lookup(org, name)
	if in == nil {
		return ""
	}
	return in.socket()
}

// Call 调用插件后端进程的一个方法，并把这次调用计入空闲计时。
//
// 面板只做透传，不解释 method 与 params 的内容：那是插件自己的命名空间。
// 进程没在跑时返回 ErrPluginNotRunning，由调用方决定是拉起还是如实报错。
func (m *RuntimeManager) Call(ctx context.Context, org, name, method string, params json.RawMessage) (json.RawMessage, error) {
	in := m.lookup(org, name)
	if in == nil {
		return nil, ErrPluginNotRunning
	}

	in.touch()
	return rpcCall(ctx, in.socket(), method, params)
}

// Touch 记录一次调用，作为空闲回收的计时起点。
func (m *RuntimeManager) Touch(org, name string) {
	if in := m.lookup(org, name); in != nil {
		in.touch()
	}
}

// Stop 停止插件进程（禁用插件、手动停止、空闲回收）。
func (m *RuntimeManager) Stop(org, name string) {
	if in := m.lookup(org, name); in != nil {
		in.stop()
	}
}

// Status 返回状态快照；从未启动过的插件返回 stopped。
func (m *RuntimeManager) Status(org, name string) RuntimeStatus {
	in := m.lookup(org, name)
	if in == nil {
		return RuntimeStatus{State: StateStopped}
	}
	return in.status()
}

// Remove 停止进程并删掉运行时数据目录（卸载插件时调用）。
//
// 顺序是先停进程再删目录：反过来会留下一个还占着 socket、而数据目录已被删掉的
// 进程，面板再也管不到它。
func (m *RuntimeManager) Remove(org, name string) error {
	key := org + "/" + name
	m.mu.Lock()
	in := m.instances[key]
	delete(m.instances, key)
	m.mu.Unlock()

	if in != nil {
		in.stop()
	}

	if err := os.RemoveAll(m.dataDir(org, name)); err != nil {
		return fmt.Errorf("删除插件运行时目录失败：%w", err)
	}
	return nil
}

// Shutdown 停掉全部插件进程，供面板退出时调用。
//
// 并发停：默认停止等待是 10 秒，串行停十个插件会让面板退出僵住 100 秒。
func (m *RuntimeManager) Shutdown() {
	m.stopOnce.Do(func() {
		close(m.done)

		m.mu.Lock()
		list := make([]*runtimeInstance, 0, len(m.instances))
		for _, in := range m.instances {
			list = append(list, in)
		}
		m.instances = make(map[string]*runtimeInstance)
		m.mu.Unlock()

		var wg sync.WaitGroup
		for _, in := range list {
			wg.Add(1)
			go func(in *runtimeInstance) {
				defer wg.Done()
				in.stop()
			}(in)
		}
		wg.Wait()
	})
}

func (m *RuntimeManager) lookup(org, name string) *runtimeInstance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[org+"/"+name]
}

func (m *RuntimeManager) instance(org, name string) *runtimeInstance {
	key := org + "/" + name
	m.mu.Lock()
	defer m.mu.Unlock()

	if in, ok := m.instances[key]; ok {
		return in
	}
	in := &runtimeInstance{
		manager:  m,
		org:      org,
		name:     name,
		logs:     make([]string, 0, logLimit),
		lastUsed: time.Now(),
	}
	m.instances[key] = in
	return in
}

// pkgDir 是插件某个版本的安装目录，可执行文件相对它解析。
func (m *RuntimeManager) pkgDir(org, name, version string) string {
	return filepath.Join(m.pluginsRoot, org, name, version)
}

// dataDir 是插件实例的运行时数据目录。
//
// 不带版本号：socket、pid、日志与插件自己的数据要跨版本保留，
// 换个版本就换一套路径，升级等于把插件的数据丢了。
func (m *RuntimeManager) dataDir(org, name string) string {
	return filepath.Join(m.runtimeRoot, org, name)
}

// idleLoop 定期回收空闲太久的进程。
//
// 这正是"常驻占资源"的解法：默认常驻换取响应速度，但清单里的 idleStopSec
// 给了用户一个"用完就收"的开关。
func (m *RuntimeManager) idleLoop() {
	ticker := time.NewTicker(m.timings.IdleScan)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.reapIdle()
		}
	}
}

func (m *RuntimeManager) reapIdle() {
	now := time.Now()

	m.mu.Lock()
	targets := make([]*runtimeInstance, 0)
	for _, in := range m.instances {
		if in.idleExpired(now) {
			targets = append(targets, in)
		}
	}
	m.mu.Unlock()

	for _, in := range targets {
		in.stop()
	}
}

// cleanupOrphans 清理面板上次被强杀时留下的插件进程。
//
// Pdeathsig 只覆盖"面板被普通信号结束"这一种情况；面板被 SIGKILL 或机器断电时
// 子进程未必跟着走，而它此时还监听着 socket、可能连着数据库。面板启动时按 pid
// 文件核一遍，是这类残留唯一的清理时机。
func (m *RuntimeManager) cleanupOrphans() {
	orgs, err := os.ReadDir(m.runtimeRoot)
	if err != nil {
		return // 目录不存在说明从来没跑过插件进程
	}

	for _, orgEntry := range orgs {
		if !orgEntry.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(m.runtimeRoot, orgEntry.Name()))
		if err != nil {
			continue
		}
		for _, nameEntry := range names {
			if !nameEntry.IsDir() {
				continue
			}
			killOrphan(filepath.Join(m.runtimeRoot, orgEntry.Name(), nameEntry.Name()))
		}
	}
}

// killOrphan 收掉一个残留进程并清掉它的 pid 与令牌文件。
func killOrphan(dataDir string) {
	pidPath := filepath.Join(dataDir, pidFileName)

	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	// 令牌文件也一并清掉：它属于上一个已经消失的进程，
	// 而新进程起来时会写一份新的。
	defer func() {
		_ = os.Remove(pidPath)
		_ = os.Remove(filepath.Join(dataDir, tokenFileName))
	}()

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}

	// pid 会被系统复用，只按号码杀进程可能误杀无关进程。判据取 /proc/<pid>/cwd：
	// 插件进程一律以自己的数据目录为工作目录，这个指向骗不了人。
	current, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil || current != dataDir {
		return
	}

	// 连整个进程组一起收：残留进程可能还带着它自己拉起的子进程。
	_ = syscall.Kill(-pid, syscall.SIGTERM)
}

// writePidFile 记录进程号。
//
// 它与 Pdeathsig 是两道保险：Pdeathsig 覆盖面板被普通信号结束的情况，
// pid 文件覆盖面板被 SIGKILL 或机器断电、内核来不及收子进程的情况。
func writePidFile(dataDir string, pid int) error {
	if pid <= 0 {
		return errors.New("进程号非法")
	}
	return os.WriteFile(filepath.Join(dataDir, pidFileName), []byte(strconv.Itoa(pid)), 0o600)
}

// runtimeInstance 是单个插件后端进程的全部状态。
type runtimeInstance struct {
	// manager 提供等待时长，构造后不再变化。
	manager *RuntimeManager
	org     string
	name    string

	mu         sync.Mutex
	spec       *RuntimeSpec
	version    string
	pkgDir     string
	dataDir    string
	socketPath string
	logFile    *os.File

	proc         *pluginProcess
	state        RuntimeState
	message      string
	startedAt    time.Time
	failures     int
	lastUsed     time.Time
	logs         []string
	ready        chan struct{}
	readyClosed  bool
	watchDone    chan struct{}
	stopping     bool
	restartTimer *time.Timer
	// generation 标识一次启动尝试，每次启动与每次停止都递增。
	//
	// 启动过程可能横跨一次停止（用户点了停止，同时正好有请求触发启动），
	// 启动方靠它判断自己这次启动是否已作废：否则会出现"进程还在跑、
	// 状态却是 stopped"，或者刚被停止的进程又被当成崩溃而自动重启。
	generation int
}

// timings 返回本次运行的等待时长。manager 构造后不再变化，读它无需加锁。
func (in *runtimeInstance) timings() RuntimeTimings { return in.manager.timings }

// ensureRunning 确保持续就绪，必要时拉起进程。
func (in *runtimeInstance) ensureRunning(spec *RuntimeSpec, version, pkgDir, dataDir string) error {
	in.mu.Lock()
	in.lastUsed = time.Now()
	state := in.state
	sameVersion := in.version == version
	wait := in.ready
	in.mu.Unlock()

	switch {
	case state == StateRunning && sameVersion:
		return nil
	case state == StateStarting && sameVersion:
		// 复用正在进行的那次启动，不重复拉起。
		select {
		case <-wait:
			return in.lastError()
		case <-time.After(in.timings().ReadyTimeout + readyGrace):
			return errors.New("等待插件进程就绪超时")
		}
	}

	// 装的是另一个版本：一个插件只有一条进程，先停旧的再起新的。
	if state == StateRunning || state == StateStarting {
		in.stop()
	}
	return in.launch(spec, version, pkgDir, dataDir)
}

// launch 执行一次完整的启动：拉进程，然后等它通过就绪探测。
func (in *runtimeInstance) launch(spec *RuntimeSpec, version, pkgDir, dataDir string) error {
	in.mu.Lock()
	if in.state == StateStarting {
		in.mu.Unlock()
		return errors.New("插件进程正在启动")
	}
	in.spec = spec
	in.version = version
	in.pkgDir = pkgDir
	in.dataDir = dataDir
	in.socketPath = filepath.Join(dataDir, filepath.FromSlash(spec.Socket))
	in.state = StateStarting
	in.message = "正在启动"
	in.stopping = false
	in.ready = make(chan struct{})
	in.readyClosed = false
	in.generation++
	generation := in.generation
	wait := in.ready
	in.mu.Unlock()

	if err := in.spawn(generation); err != nil {
		in.fail(generation, err.Error())
		return err
	}

	select {
	case <-wait:
		return in.readyResult(generation)
	case <-time.After(in.timings().ReadyTimeout + readyGrace):
		err := fmt.Errorf("等待插件进程就绪超时（%s）", in.timings().ReadyTimeout)
		in.fail(generation, err.Error())
		// 进程可能只是没就绪，收掉它，别留一个"起了但不可用"的进程。
		in.terminate()
		return err
	}
}

// spawn 拉起进程并起好三件事：日志消费、进程回收、就绪探测。
func (in *runtimeInstance) spawn(generation int) error {
	in.mu.Lock()
	if in.generation != generation {
		in.mu.Unlock()
		return errors.New("启动已被取消")
	}
	spec, dataDir, pkgDir, socketPath := in.spec, in.dataDir, in.pkgDir, in.socketPath
	in.mu.Unlock()

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("创建插件数据目录失败：%w", err)
	}
	if dir := filepath.Dir(socketPath); dir != dataDir {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建 socket 目录失败：%w", err)
		}
	}
	// 路径过长时先给出人话：内核只会拒绝 bind，插件进程随后的死法没有任何线索。
	if len(socketPath) >= maxUnixSocketPath {
		return fmt.Errorf("socket 路径过长（%d 字节，上限 %d）：面板的数据目录太深，请把它挪到更短的路径下",
			len(socketPath), maxUnixSocketPath-1)
	}
	// 上一次留下的 socket 文件会让新进程 bind 失败，而它已经没有监听者了。
	_ = os.Remove(socketPath)

	execPath := filepath.Join(pkgDir, filepath.FromSlash(spec.Exec))
	args := expandArgs(spec.Args, socketPath, dataDir)

	// 令牌先于进程落地：插件一起来就可能要凭它调面板能力。
	// 进程重启即换新令牌，旧的那份在 issue 里被顶掉。
	if _, err := in.manager.tokens.issue(in.org, in.name, dataDir); err != nil {
		return err
	}

	proc, err := startProcess(execPath, args, pluginEnv(dataDir, socketPath, pkgDir), dataDir)
	if err != nil {
		// 进程没起来，令牌不该留在映射里。
		in.manager.tokens.revoke(in.org, in.name)
		return err
	}

	in.mu.Lock()
	in.proc = proc
	in.startedAt = time.Now()
	in.watchDone = make(chan struct{})
	in.openLogLocked()
	watchDone := in.watchDone
	in.mu.Unlock()

	// pid 文件写失败不算启动失败：它只影响"面板被强杀后能否清残留"，
	// 为此把已经起来的进程杀掉反而更糟。
	_ = writePidFile(dataDir, proc.PID())

	go in.consume(proc.Stdout())
	go in.consume(proc.Stderr())
	go in.watch(proc, watchDone, generation)
	go in.probe(socketPath, spec.HealthPath, generation)

	// 起进程这段时间里可能被停止：先登记再判代次，让进程一律经由 watch 回收，
	// 不在没有回收者的情况下杀它（那会留下僵尸）。
	in.mu.Lock()
	cancelled := in.generation != generation
	in.mu.Unlock()
	if cancelled {
		in.terminate()
		return errors.New("启动已被取消")
	}
	return nil
}

// probe 轮询 health 方法，直到进程就绪、本次启动已作废或超时。
//
// 就绪靠主动探测而不是"进程还活着"：插件进程起来之后可能还在连数据库、
// 建表，此时它在 socket 上已经能收请求，但还没有能力回答任何业务问题。
func (in *runtimeInstance) probe(socketPath, method string, generation int) {
	deadline := time.Now().Add(in.timings().ReadyTimeout)

	for time.Now().Before(deadline) {
		in.mu.Lock()
		starting := in.state == StateStarting && in.generation == generation
		in.mu.Unlock()
		if !starting {
			return
		}

		// socket 由插件进程创建，权限受它的 umask 影响，完全可能是 0755 ——
		// 那意味着同机其他用户能绕开面板直接调用插件。面板在它可用的第一刻收紧。
		if _, err := os.Stat(socketPath); err == nil {
			_ = os.Chmod(socketPath, 0o600)
		}

		ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
		_, err := rpcCall(ctx, socketPath, method, nil)
		cancel()

		if err == nil {
			in.markRunning(generation)
			return
		}
		time.Sleep(readyInterval)
	}

	in.fail(generation, fmt.Sprintf("插件进程未能在 %s 内就绪", in.timings().ReadyTimeout))
	in.terminate()
}

func (in *runtimeInstance) markRunning(generation int) {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.state != StateStarting || in.generation != generation {
		return
	}
	in.state = StateRunning
	in.message = ""
	in.closeReadyLocked()
}

// closeReadyLocked 关闭 ready 通道（已关闭或是 nil 时是空操作）。
// 启动成功、启动失败、进程提前退出、启动中被 Stop 打断，四条路径都可能关闭它，
// 没有这个标记就会出现重复 close 的 panic。
//
// 调用方必须已经写好 state 与 message 再调用它：等待方被唤醒后立刻读这些字段，
// 先关通道再改状态会让它们读到上一次的结论。
func (in *runtimeInstance) closeReadyLocked() {
	if in.ready == nil || in.readyClosed {
		return
	}
	in.readyClosed = true
	close(in.ready)
}

func (in *runtimeInstance) fail(generation int, message string) {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.generation != generation || in.stopping {
		return
	}
	in.state = StateFailed
	in.message = message
	in.closeReadyLocked()
}

// readyResult 在启动等待结束后给出结论。
func (in *runtimeInstance) readyResult(generation int) error {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.generation != generation {
		return errors.New("插件启动过程已被打断")
	}
	if in.state == StateFailed {
		return errors.New(in.message)
	}
	if in.state != StateRunning {
		return fmt.Errorf("插件进程未就绪（%s）", in.state)
	}
	return nil
}

func (in *runtimeInstance) lastError() error {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.state == StateFailed {
		return errors.New(in.message)
	}
	return nil
}

// terminate 只用一次优雅终止收尾，不等回收。
// 供"状态已置为失败、只想把进程收掉"的路径使用，进程真正退出由 watch 走完。
func (in *runtimeInstance) terminate() {
	in.mu.Lock()
	proc := in.proc
	in.mu.Unlock()

	if proc != nil {
		proc.Terminate()
	}
}

// consume 逐行保留子进程输出，供状态接口与失败原因使用。
func (in *runtimeInstance) consume(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		in.appendLog(scanner.Text())
	}
	// 进程被强杀时管道会带着错误关闭，这条线索比"进程已退出"具体得多。
	if err := scanner.Err(); err != nil {
		in.appendLog("读取进程输出失败：" + err.Error())
	}
}

// watch 是唯一的进程回收点，并负责崩溃后的退避重启。
func (in *runtimeInstance) watch(proc *pluginProcess, watchDone chan struct{}, generation int) {
	defer close(watchDone)

	err := proc.Wait()

	in.mu.Lock()
	in.proc = nil

	// 主动停止，或本次启动已被后来的停止/启动作废：不覆盖状态，也不重启。
	if in.stopping || in.generation != generation {
		in.mu.Unlock()
		return
	}

	message := "插件进程已退出"
	if err != nil {
		message = "插件进程已退出：" + err.Error()
	}

	// 稳定跑过一段时间再崩，说明不是"起了就死"，这次重新算作第一次失败。
	if time.Since(in.startedAt) > restartResetAfter {
		in.failures = 0
	}
	in.failures++
	failures := in.failures

	in.state = StateFailed
	if failures <= restartLimit {
		retryIn := in.timings().RestartDelay << (failures - 1)
		in.message = fmt.Sprintf("%s，%s 后第 %d 次重试", message, retryIn, failures)
		in.restartTimer = time.AfterFunc(retryIn, in.restart)
	} else {
		in.message = fmt.Sprintf("%s（已连续失败 %d 次，不再自动重试）", message, failures)
	}
	logLine := in.message
	// 状态写好再唤醒等待者，否则它们会读到上一次的结论。
	in.closeReadyLocked()
	in.mu.Unlock()

	in.appendLog(logLine)
}

// restart 按退避重试一次启动。
func (in *runtimeInstance) restart() {
	in.mu.Lock()
	if in.stopping || in.state == StateStopped {
		in.mu.Unlock()
		return
	}
	spec, version, pkgDir, dataDir := in.spec, in.version, in.pkgDir, in.dataDir
	in.mu.Unlock()

	if err := in.launch(spec, version, pkgDir, dataDir); err != nil {
		in.appendLog("重试失败：" + err.Error())
	}
}

// stop 停止进程并等它被回收。
func (in *runtimeInstance) stop() {
	in.mu.Lock()
	in.stopping = true
	// 作废进行中的启动：它可能正卡在 spawn 或就绪探测里。
	in.generation++
	if in.restartTimer != nil {
		in.restartTimer.Stop()
		in.restartTimer = nil
	}
	proc := in.proc
	watchDone := in.watchDone
	dataDir := in.dataDir
	timeout := defaultStopTimeoutSec
	if in.spec != nil {
		timeout = in.spec.StopTimeoutSec
	}
	in.state = StateStopped
	in.message = ""
	// 启动中的等待者必须被唤醒：否则它们会一直等到启动超时，
	// 期间禁用插件、卸载这类操作都会僵在那里。
	in.closeReadyLocked()
	in.mu.Unlock()

	// 令牌与进程同生命周期：进程一停，它的令牌立刻失效 ——
	// 否则一个已经被停掉的插件还能拿着旧凭据继续调面板能力。
	in.manager.tokens.revoke(in.org, in.name)
	if dataDir != "" {
		_ = os.Remove(filepath.Join(dataDir, tokenFileName))
	}

	if proc == nil {
		in.releaseLog()
		return
	}

	wait := time.Duration(timeout) * time.Second
	proc.Terminate()

	select {
	case <-watchDone:
	case <-time.After(wait):
		// 优雅退出无响应时强杀，避免留下孤儿白占内存与数据库连接。
		proc.Kill()
		select {
		case <-watchDone:
		case <-time.After(wait):
		}
	}

	in.mu.Lock()
	// watch 已结束（stopping 让它不再重启），放开标记允许下次启动。
	in.stopping = false
	in.mu.Unlock()

	in.releaseLog()
}

// releaseLog 关闭日志文件句柄。
func (in *runtimeInstance) releaseLog() {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.logFile != nil {
		_ = in.logFile.Close()
		in.logFile = nil
	}
}

// openLogLocked 打开日志文件。打开失败只影响日志落盘，不影响进程。
func (in *runtimeInstance) openLogLocked() {
	if in.logFile != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(in.dataDir, logFileName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	in.logFile = file
}

// appendLog 记录一行输出：内存里留最近若干行，同时追加到日志文件。
//
// 与 dsh 的做法有一处有意不同：重启不清空日志。崩溃原因恰恰是最需要它的时刻，
// 重启就清空等于把唯一的线索抹掉。
func (in *runtimeInstance) appendLog(line string) {
	in.mu.Lock()
	defer in.mu.Unlock()

	if len(in.logs) >= logLimit {
		in.logs = in.logs[1:]
	}
	in.logs = append(in.logs, line)

	if in.logFile != nil {
		_, _ = in.logFile.WriteString(line + "\n")
	}
}

func (in *runtimeInstance) touch() {
	in.mu.Lock()
	in.lastUsed = time.Now()
	in.mu.Unlock()
}

func (in *runtimeInstance) socket() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.socketPath
}

// idleExpired 判断进程是否已经空闲太久，该被回收。
func (in *runtimeInstance) idleExpired(now time.Time) bool {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.state != StateRunning || in.spec == nil || in.spec.IdleStopSec <= 0 {
		return false
	}
	return now.Sub(in.lastUsed) > time.Duration(in.spec.IdleStopSec)*time.Second
}

func (in *runtimeInstance) status() RuntimeStatus {
	in.mu.Lock()
	defer in.mu.Unlock()

	pid := 0
	if in.proc != nil {
		pid = in.proc.PID()
	}
	logs := make([]string, len(in.logs))
	copy(logs, in.logs)

	return RuntimeStatus{
		State:     in.state,
		Message:   in.message,
		Version:   in.version,
		PID:       pid,
		StartedAt: in.startedAt,
		Failures:  in.failures,
		Logs:      logs,
	}
}
