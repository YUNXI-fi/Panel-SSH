package dsh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// localLauncher 在面板自己所在的机器（宿主机）上起 dsh。
type localLauncher struct{}

// NewLocalLauncher 构造宿主机使用的启动器。
func NewLocalLauncher() Launcher { return localLauncher{} }

func (localLauncher) Start(argv []string, workDir string) (Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("启动命令为空")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建工作目录失败：%w", err)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workDir
	// 独立进程组，退出时才能连同 dsh 拉起的子进程一起回收。
	// Pdeathsig 保证面板被强杀（SIGKILL / OOM）时内核会顺手收掉 dsh，
	// 否则会留下一个占着内存、还监听端口的孤儿 node 进程。
	// 远端拿不到这个保障，那边只能靠 pid 文件，见 launcher_remote.go。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 dsh 失败：%w", err)
	}
	return &localProcess{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

// localProcess 是本地子进程的句柄。
type localProcess struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr io.Reader
}

func (p *localProcess) Stdout() io.Reader { return p.stdout }

func (p *localProcess) Stderr() io.Reader { return p.stderr }

func (p *localProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *localProcess) Wait() error { return p.cmd.Wait() }

func (p *localProcess) Terminate() { signalGroup(p.cmd, syscall.SIGTERM) }

func (p *localProcess) Kill() { signalGroup(p.cmd, syscall.SIGKILL) }

// signalGroup 向整个进程组发信号：dsh 会拉起子进程，只杀父进程会留下孤儿。
func signalGroup(cmd *exec.Cmd, signal syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, signal); err != nil {
		// 进程组可能已不存在，退化为只杀主进程。
		_ = cmd.Process.Signal(signal)
	}
}