//go:build linux

// Package localterm 在面板所在主机上起一个真正的 PTY 会话。
//
// 终端的「宿主机」不是 SSH 到 127.0.0.1：那要求面板机上恰好跑着 sshd、
// 还要求用户自己的账号有密码或密钥。这里直接 fork 一个登录 shell，
// 把它接到一对伪终端上 —— 与在面板机上开一个本地终端完全等价。
package localterm

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Session 是一个运行中的本地终端。
//
// Read/Write 都作用在主设备（ptmx）上：写进去的字节会被 shell 当成键盘输入，
// 读出来的字节是 shell 绘制屏幕的输出。
type Session struct {
	ptm *os.File
	cmd *exec.Cmd
}

// Start 启动一个本地登录 shell。
func Start(cols, rows int) (*Session, error) {
	ptm, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("打开 /dev/ptmx 失败：%w", err)
	}

	fd := int(ptm.Fd())
	// 解锁从设备：不解锁的话 /dev/pts/N 打开会得到 EIO。
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		_ = ptm.Close()
		return nil, fmt.Errorf("解锁 PTY 失败：%w", err)
	}

	index, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		_ = ptm.Close()
		return nil, fmt.Errorf("获取 PTY 编号失败：%w", err)
	}

	pts, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", index), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = ptm.Close()
		return nil, fmt.Errorf("打开从设备失败：%w", err)
	}

	session := &Session{ptm: ptm}
	if err := session.Resize(cols, rows); err != nil {
		_ = pts.Close()
		_ = ptm.Close()
		return nil, err
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Stdin = pts
	cmd.Stdout = pts
	cmd.Stderr = pts
	// Setsid + Setctty 让 shell 成为会话首进程并把 PTY 作为控制终端；
	// 少了它们，作业控制（Ctrl+C、Ctrl+Z、前后台切换）会全部失效。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = pts.Close()
		_ = ptm.Close()
		return nil, fmt.Errorf("启动 shell 失败：%w", err)
	}

	// 父进程不持有从设备：只要 shell 还活着，主设备读取就不会提前返回 EOF。
	_ = pts.Close()
	session.cmd = cmd
	return session, nil
}

func (s *Session) Read(p []byte) (int, error) {
	return s.ptm.Read(p)
}

func (s *Session) Write(p []byte) (int, error) {
	return s.ptm.Write(p)
}

// Resize 把终端尺寸同步给内核，让 shell 里的程序拿到正确的 COLUMNS/LINES。
func (s *Session) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return unix.IoctlSetWinsize(int(s.ptm.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: uint16(rows),
		Col: uint16(cols),
	})
}

// Wait 阻塞到 shell 退出。
func (s *Session) Wait() error {
	return s.cmd.Wait()
}

// Terminate 结束会话：关闭主设备会让内核向该终端的前台进程组发 SIGHUP，
// 再补一次 Kill 兜底那些忽略 SIGHUP 的程序。
func (s *Session) Terminate() {
	_ = s.ptm.Close()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}
