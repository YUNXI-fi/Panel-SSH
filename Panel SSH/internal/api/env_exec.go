package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"panelssh/internal/env"
)

// sshEnvExecutor 把一条 SSH 连接适配为 env.Executor。
//
// 每次 Run/Stream 都新借连接、新开 session：环境步骤之间依靠目标机的
// 持久状态（dpkg 数据库、已写文件）而不是 shell 会话状态，因此不必维持
// 一条长会话；短会话还能避开"装一钟头，SSH 超时断链"的问题。
type sshEnvExecutor struct {
	s      *Server
	hostID int64
}

// 编译期确认该通道能提供提权口令：这是 su 与 sudo 两条口令路径成立的前提。
var _ env.SecretFeeder = (*sshEnvExecutor)(nil)

// EscalationSecret 返回可用于提权的口令、它的来源，以及有没有。
//
// 主机单独配了「提权口令」（root 口令）就给这一份（dedicated 为真），
// 没配时退回登录凭据；两者的取舍与边界见 Server.hostEscalationSecret。
func (e *sshEnvExecutor) EscalationSecret() (string, bool, bool) {
	return e.s.hostEscalationSecret(e.hostID)
}

func (e *sshEnvExecutor) Run(ctx context.Context, cmd env.Command, timeout time.Duration) (*env.ExecResult, error) {
	return e.run(ctx, cmd, timeout, nil)
}

func (e *sshEnvExecutor) Stream(ctx context.Context, cmd env.Command, timeout time.Duration, sink env.LogSink) (*env.ExecResult, error) {
	return e.run(ctx, cmd, timeout, sink)
}

func (e *sshEnvExecutor) run(ctx context.Context, cmd env.Command, timeout time.Duration, sink env.LogSink) (*env.ExecResult, error) {
	lease, err := e.s.leaseHost(e.hostID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	session, err := lease.Client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	var feeder *secretFeeder
	if cmd.Secret {
		secret, _, ok := e.EscalationSecret()
		if !ok {
			// 探测时说能提权、执行时口令没了：只可能是主机凭据被改过。
			return nil, errors.New("提权口令不可用：该主机当前没有可用的口令")
		}
		stdin, err := session.StdinPipe()
		if err != nil {
			return nil, err
		}
		if cmd.TTY {
			// su 从 /dev/tty 读口令，没有伪终端它会直接拒绝执行。
			// 关掉 ECHO 是为了堵住"提示符出现之前口令先被终端回显进日志"的窗口。
			if err := session.RequestPty("xterm", 40, 200, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
				return nil, err
			}
		}
		// su 要等它把提示打出来才读输入；sudo -S 的提示被 -p '' 清空了，
		// 没有可等的东西，直接写进去。
		feeder = newSecretFeeder(secret, stdin, cmd.TTY)
		defer feeder.stop()
	}

	stdout := newEnvSinkWriter("stdout", sink)
	stderr := newEnvSinkWriter("stderr", sink)
	session.Stdout = stdout
	session.Stderr = stderr
	if feeder != nil && cmd.TTY {
		session.Stdout = crStripper{out: feedWriter{out: stdout, hook: feeder.onOutput}}
		session.Stderr = crStripper{out: feedWriter{out: stderr, hook: feeder.onOutput}}
	}

	started := time.Now()
	if err := session.Start(cmd.Line); err != nil {
		return nil, err
	}
	if feeder != nil {
		// 必须在 Start 之后：见 newSecretFeeder 的说明。
		feeder.answerNow()
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(timeout):
		terminateRemote(session, done)
		runErr = errBatchTimeout
	case <-ctx.Done():
		terminateRemote(session, done)
		runErr = ctx.Err()
	}

	result := &env.ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if code, ok := exitStatus(runErr); ok {
		result.ExitCode = code
		return result, nil
	}
	if runErr != nil {
		return result, runErr
	}
	_ = started
	return result, nil
}

// secretFeeder 把提权口令送进提权命令的会话。
//
// su 必须等它把提示打出来才回答，因此要在输出里认提示；sudo -S 的提示被
// -p ” 清空了，没有可等的东西，只能立刻写（waitPrompt 为假）。
type secretFeeder struct {
	secret string
	write  func([]byte) (int, error)
	// waitPrompt 为真表示这条路径要把口令留给提示出现之后再写（su）；
	// 为假表示没有可等的提示，Start 之后立刻写（sudo -S）。
	waitPrompt bool

	mu   sync.Mutex
	sent bool
	// fallback 是"提示文本没认出来"时的兜底投喂。
	fallback *time.Timer
}

// promptWait 是等待 su 口令提示的时长上限。
const promptWait = 3 * time.Second

// promptNeedle 是提示文本里必然出现的一段（"Password:"、"password for"）。
// 两个常见实现的大小写不同，统一小写后比较；只认单词不认冒号，也就顺带
// 兼容了各种措辞。真正的兜底交给 fallback 定时器，不指望这个匹配万无一失。
var promptNeedle = []byte("assword")

// newSecretFeeder 准备投喂器。它不在构造时投喂：那时命令还没 Start，
// 远端没有进程在读这个通道，写进去会卡在通道窗口上（实测就是把会话挂死在这里）。
// 需要立刻投喂的路径由调用方在 Start 之后调 answerNow。
func newSecretFeeder(secret string, w io.Writer, waitPrompt bool) *secretFeeder {
	f := &secretFeeder{secret: secret, write: w.Write, waitPrompt: waitPrompt}
	if waitPrompt {
		// 认不出提示也不能让会话干等到步骤超时（15 分钟）：到点直接写，
		// 口令会留在伪终端的输入缓冲里，su 随后照样读得到 —— 脚本正文走的是
		// 命令参数，不会被这行口令污染。
		f.fallback = time.AfterFunc(promptWait, f.onTimeout)
	}
	return f
}

// answerNow 用于"没有可等提示"的路径（sudo -S 的提示被 -p ” 清空了）：
// 命令一跑起来就把口令写下去，剩下的交给它自己去读。
func (f *secretFeeder) answerNow() {
	if f.waitPrompt {
		return
	}
	f.markSent()
	f.writeSecret()
}

// onOutput 检查输出块并回答口令提示。
//
// 每次提示都回答：口令错时 su 会重新问，只答一次会让会话卡在第二次等待上，
// 最后表现为一个莫名其妙的超时，而不是"认证失败"。
func (f *secretFeeder) onOutput(p []byte) {
	if bytes.Contains(bytes.ToLower(p), promptNeedle) {
		f.markSent()
		f.writeSecret()
	}
}

// onTimeout 兜底：提示文本没认出来时补一次口令，仅一次。
func (f *secretFeeder) onTimeout() {
	if f.markSent() {
		f.writeSecret()
	}
}

func (f *secretFeeder) writeSecret() {
	_, _ = f.write([]byte(f.secret + "\n"))
}

// stop 停掉兜底定时器。
func (f *secretFeeder) stop() {
	if f.fallback != nil {
		f.fallback.Stop()
	}
}

// markSent 记录"已投喂过"，返回本次是否为首次投喂；首次投喂即停掉兜底定时器。
func (f *secretFeeder) markSent() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	first := !f.sent
	f.sent = true
	if first && f.fallback != nil {
		f.fallback.Stop()
	}
	return first
}

// feedWriter 把输出原样转发，同时把每块内容交给 hook 看一眼。
type feedWriter struct {
	out  io.Writer
	hook func([]byte)
}

func (w feedWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.hook(p)
	return n, err
}

// crStripper 抹掉伪终端带回的 \r。
//
// 伪终端的行规程会把输出里的 \n 转成 \r\n，日志与实时流里会到处是 ^M；
// 这是面板自己申请伪终端造成的，理应在面板侧抹掉。
type crStripper struct{ out io.Writer }

func (w crStripper) Write(p []byte) (int, error) {
	clean := p
	if bytes.IndexByte(p, '\r') >= 0 {
		clean = bytes.ReplaceAll(p, []byte("\r"), nil)
	}
	if len(clean) > 0 {
		if _, err := w.out.Write(clean); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// envSinkWriter 收集输出（带上限）并可选实时转发给 WS 回调。
type envSinkWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	stream    string
	sink      env.LogSink
	sent      int
	truncated bool
}

func newEnvSinkWriter(stream string, sink env.LogSink) *envSinkWriter {
	return &envSinkWriter{stream: stream, sink: sink}
}

func (w *envSinkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.sink != nil && w.sent < env.MaxStepOutputBytes {
		chunk := p
		if w.sent+len(chunk) > env.MaxStepOutputBytes {
			chunk = chunk[:env.MaxStepOutputBytes-w.sent]
		}
		w.sink(w.stream, chunk)
		w.sent += len(chunk)
	}

	remaining := env.MaxStepOutputBytes - w.buf.Len()
	if remaining <= 0 || len(p) > remaining {
		w.truncated = true
		if remaining > 0 {
			w.buf.Write(p[:remaining])
		}
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *envSinkWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.buf.String()
	if w.truncated {
		s += "\n[输出超过上限，已截断]\n"
	}
	return s
}

// 编译期保证 io.Writer 实现。
var _ io.Writer = (*envSinkWriter)(nil)
