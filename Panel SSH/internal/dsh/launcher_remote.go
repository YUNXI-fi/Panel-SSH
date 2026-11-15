package dsh

import (
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHClientFactory 按主机取一条可用的 SSH 连接。
//
// 怎么建、怎么缓存这条连接由调用方决定 —— 只有它知道目标机的地址、凭据与
// known_hosts 在哪。dsh 包只要求"给我一条连上的 client"，因此不依赖
// store 与 sshclient，可以脱离数据库单测。
type SSHClientFactory func(hostID int64) (*ssh.Client, error)

// remoteLauncher 通过一条常驻 SSH 会话在目标机上起 dsh。
//
// 为什么复用承载 stdout 的那条会话而不是 nohup 到后台：
// 后台化会让会话立刻结束，就再也读不到 dsh 启动时打出的访问地址与令牌，
// 而那个地址是 --port 0 动态分配的，没有别的渠道能拿到。
type remoteLauncher struct {
	hostID  int64
	factory SSHClientFactory
}

// NewRemoteLauncher 构造目标主机使用的启动器。
func NewRemoteLauncher(hostID int64, factory SSHClientFactory) Launcher {
	return &remoteLauncher{hostID: hostID, factory: factory}
}

func (l *remoteLauncher) Start(argv []string, workDir string) (Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("启动命令为空")
	}
	client, err := l.factory(l.hostID)
	if err != nil {
		return nil, err
	}

	// 先把上一次可能残留的进程清掉。远端没有 Pdeathsig 的等价物：面板被强杀时
	// 承载它的 SSH 会话会断，但 dsh 未必跟着死，于是留下一个占着几百 MB 内存的
	// node。--port 0 让新实例不会撞端口，因此这种残留不会有任何症状，
	// 只会白占内存 —— 越是这样越要在启动前主动收掉。
	killStale(client, workDir)

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("建立远端会话失败：%w", err)
	}

	// 用 io.Pipe 把 sshd 写来的字节转成 Supervisor 能逐行扫描的 Reader。
	// 收尾时必须关掉写端（见 remoteProcess.Wait），否则扫描协程永远等不到 EOF。
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	session.Stdout = stdoutWriter
	session.Stderr = stderrWriter

	if err := session.Start(remoteStartScript(argv, workDir)); err != nil {
		_ = session.Close()
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		return nil, fmt.Errorf("启动远端 dsh 失败：%w", err)
	}

	return &remoteProcess{
		client:    client,
		session:   session,
		workDir:   workDir,
		stdout:    stdoutReader,
		stderr:    stderrReader,
		stdoutEnd: stdoutWriter,
		stderrEnd: stderrWriter,
	}, nil
}

// remoteProcess 是远端进程的句柄。
type remoteProcess struct {
	client  *ssh.Client
	session *ssh.Session
	workDir string

	stdout    *io.PipeReader
	stderr    *io.PipeReader
	stdoutEnd *io.PipeWriter
	stderrEnd *io.PipeWriter
}

func (p *remoteProcess) Stdout() io.Reader { return p.stdout }

func (p *remoteProcess) Stderr() io.Reader { return p.stderr }

// PID 对远端进程没有意义：那是目标机上的进程号，面板这边拿不到也不该猜。
// 需要展示时由 api 层读目标机上的 dsh.pid（见 launcher_remote 的启动脚本）。
func (p *remoteProcess) PID() int { return 0 }

// Wait 等待远端命令结束。
//
// 关管道写端是这里的一半职责：sshd 把数据写完不会让管子自己到 EOF，
// 本地实现由 os/exec 在 Wait 里替我们做了这件事，远端必须自己来。
// 漏掉的话，Supervisor 里扫描日志的协程会永久挂住，进程退出的状态也就传不出去。
func (p *remoteProcess) Wait() error {
	err := p.session.Wait()
	_ = p.stdoutEnd.Close()
	_ = p.stderrEnd.Close()
	_ = p.session.Close()
	return err
}

func (p *remoteProcess) Terminate() { p.signal("TERM") }

func (p *remoteProcess) Kill() { p.signal("KILL") }

// signal 另开一条会话去停进程。
//
// 不能复用承载 stdout 的那条：它正被 dsh 占着，在上面再跑命令会与 dsh
// 争抢同一个 channel。失败被吞掉是有意的 —— 终止路径上能做的补救只有
// 强杀，而强杀也失败时调用方已经无路可走，再往上抛错只会掩盖真正的原因。
func (p *remoteProcess) signal(name string) {
	session, err := p.client.NewSession()
	if err != nil {
		return
	}
	defer func() { _ = session.Close() }()
	_ = session.Run(killPidScript(p.workDir, name))
}

// pidPath 是 pid 文件在目标机上的位置。
//
// 用 path 而不是 filepath：这是目标机上的 Unix 路径，与面板自己所在平台无关。
func pidPath(workDir string) string { return path.Join(workDir, "dsh.pid") }

// remoteStartScript 生成远端启动脚本。
//
// 两个细节是刻意的：
//   - cd 到工作目录，决定 dsh 默认的文件系统位置（本地实现靠 cmd.Dir 达到同样效果）
//   - exec 让 pid 就是 dsh 本体而非外层 shell，pid 文件里那个号才能直接 kill
func remoteStartScript(argv []string, workDir string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, shellQuote(arg))
	}
	return "mkdir -p " + scriptPath(workDir) +
		" && cd " + scriptPath(workDir) +
		" && echo $$ > " + scriptPath(pidPath(workDir)) +
		" && exec " + strings.Join(quoted, " ")
}

// killStaleScript 读 pid 文件并收掉上一个实例。
//
// 判活用 kill -0（只探测不发信号）；拿到 pid 后先看它是不是纯数字，
// 否则 kill 会把 "-1" 之类的怪值解释成"发给所有进程"。
func killStaleScript(workDir string) string {
	return `f=` + scriptPath(pidPath(workDir)) + `
[ -f "$f" ] || exit 0
pid=$(cat "$f" 2>/dev/null)
case "$pid" in
  ''|*[!0-9]*) ;;
  *)
    if kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null
      sleep 1
      kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
    fi
    ;;
esac
rm -f "$f"
exit 0`
}

func killPidScript(workDir, signal string) string {
	return `f=` + scriptPath(pidPath(workDir)) + `
[ -f "$f" ] || exit 0
pid=$(cat "$f" 2>/dev/null)
case "$pid" in
  ''|*[!0-9]*) exit 0 ;;
esac
kill -` + signal + ` "$pid" 2>/dev/null
exit 0`
}

// killStale 尽力清掉残留实例。
//
// 失败不往上抛：清理是"顺手让下次启动更干净"，不是启动成功的前提。
// 真清不掉也只多一个占内存的进程，而因此让用户连"启动"这个动作都做不了
// 才是更糟的结果。
func killStale(client *ssh.Client, workDir string) {
	session, err := client.NewSession()
	if err != nil {
		return
	}
	defer func() { _ = session.Close() }()
	_ = session.Run(killStaleScript(workDir))
}

// shellQuote 把一段文本包成 shell 单引号字面量。
//
// 工作目录来自主机上的家目录，可能带空格或引号；路径直接拼进脚本的话，
// 一个含空格的目录就会让启动脚本被拆成两条命令。
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// scriptPath 把路径包成脚本里的字面量，并处理 ~/ 前缀。
//
// 目标机的家目录只有它自己知道，面板在本地拼不出绝对路径，因此 ~/ 的展开
// 必须留给那边的 shell —— 而直接把它连同引号一起包住（'~/x'）会让 ~ 变成
// 字面意义上的目录名。这里只对 ~ 单独放行，其余部分照旧引号包裹。
func scriptPath(dir string) string {
	if rest, ok := strings.CutPrefix(dir, "~/"); ok {
		return `"$HOME"/` + shellQuote(rest)
	}
	return shellQuote(dir)
}