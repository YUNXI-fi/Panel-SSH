package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"panelssh/internal/sshclient"
	"panelssh/internal/store"
)

// SFTP 的失败模式分两类，超时策略也相应分成两套：
//
//  1. 元数据类操作（Stat/ReadDir/Rename/Remove…）本身是"一问一答"，
//     正常应在毫秒级完成，因此用固定的总超时。
//  2. 流式传输（上传/下载）耗时取决于文件大小与链路带宽，不能设总超时，
//     改为监视"是否还在流动"：长时间没有任何字节推进即判定为停滞。
//
// 这套机制解决的核心问题：SFTP 调用没有超时以前，只要远端挂起
// （网络黑洞、无响应的网络挂载点），HTTP 请求会永久不返回，
// 连接池里的引用计数也永远不归零，该主机从此再也回收不掉连接。
const (
	// sftpStallTimeout 是流式传输可接受的静止时长。
	sftpStallTimeout = 60 * time.Second
)

// sftpOpTimeout 是元数据类 SFTP 操作的总时长上限。
//
// 声明为变量而非常量，是为了让单元测试能把它压到毫秒级 ——
// 否则验证超时行为本身就要让测试跑满 20 秒。
var sftpOpTimeout = 20 * time.Second

// errSFTPTimeout 表示远端在规定时间内没有响应。
var errSFTPTimeout = errors.New("远端未在预期时间内响应")

// sftpSession 持有一次文件操作用到的全部资源。
//
// 为什么自己创建 session 而不是用 sftp.NewClient(sshClient)：
// 后者内部另建的 ssh session 在 sftp.Client.Close() 时并不会被关闭，
// 只能等对端把通道收掉。等于每个文件请求都把一个 session 的回收
// 交给了远端实现，属实打实的资源泄漏。这里拿住 session 自己关。
type sftpSession struct {
	client  *sftp.Client
	session *ssh.Session
	lease   *sshclient.Lease
	// localConn 是宿主机会话的进程内管道（见 sftp_local.go），远端会话为 nil。
	// 必须显式关闭：内置 SFTP 服务端的 Serve 靠它退出，否则会留下一个常驻 goroutine。
	localConn net.Conn
}

// Close 释放本次操作的全部资源。可重复调用。
func (x *sftpSession) Close() {
	if x.client != nil {
		_ = x.client.Close()
	}
	if x.session != nil {
		_ = x.session.Close()
	}
	if x.localConn != nil {
		_ = x.localConn.Close()
	}
	if x.lease != nil {
		x.lease.Release()
	}
}

// sftpGuard 给 sftp.Client 的每次调用套上超时。
//
// 之所以做成透传的包装而不是在每个调用点写一遍 goroutine + select：
// 调用点有近二十处，逐个包闭包会淹没真正的业务逻辑。
type sftpGuard struct {
	client *sftp.Client
}

func zero[T any]() T {
	var v T
	return v
}

// call 在超时保护下执行一次 SFTP 调用。
//
// 超时后关闭 SFTP 客户端：阻塞中的调用会因通道关闭立刻返回错误。
// 这里只关本次请求自己的 client/session，不会碰到连接池中被其它窗口
// 共用的那条 SSH 连接。
func call[T any](g *sftpGuard, fn func() (T, error)) (T, error) {
	type outcome struct {
		value T
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := fn()
		done <- outcome{value, err}
	}()

	select {
	case r := <-done:
		return r.value, r.err
	case <-time.After(sftpOpTimeout):
		// client 为空只可能出现在单元测试里（那里不需要真正的连接），
		// 因此这里做一次判空而不是假定它一定非 nil。
		if g.client != nil {
			_ = g.client.Close()
		}
		return zero[T](), errSFTPTimeout
	}
}

func (g *sftpGuard) Stat(p string) (os.FileInfo, error) {
	return call(g, func() (os.FileInfo, error) { return g.client.Stat(p) })
}

func (g *sftpGuard) Lstat(p string) (os.FileInfo, error) {
	return call(g, func() (os.FileInfo, error) { return g.client.Lstat(p) })
}

func (g *sftpGuard) ReadDir(p string) ([]os.FileInfo, error) {
	return call(g, func() ([]os.FileInfo, error) { return g.client.ReadDir(p) })
}

func (g *sftpGuard) Open(p string) (*sftp.File, error) {
	return call(g, func() (*sftp.File, error) { return g.client.Open(p) })
}

func (g *sftpGuard) Create(p string) (*sftp.File, error) {
	return call(g, func() (*sftp.File, error) { return g.client.Create(p) })
}

func (g *sftpGuard) MkdirAll(p string) error {
	_, err := call(g, func() (struct{}, error) { return struct{}{}, g.client.MkdirAll(p) })
	return err
}

func (g *sftpGuard) Rename(from, to string) error {
	_, err := call(g, func() (struct{}, error) { return struct{}{}, g.client.Rename(from, to) })
	return err
}

func (g *sftpGuard) Remove(p string) error {
	_, err := call(g, func() (struct{}, error) { return struct{}{}, g.client.Remove(p) })
	return err
}

func (g *sftpGuard) RemoveDirectory(p string) error {
	_, err := call(g, func() (struct{}, error) { return struct{}{}, g.client.RemoveDirectory(p) })
	return err
}

func (g *sftpGuard) Chmod(p string, mode os.FileMode) error {
	_, err := call(g, func() (struct{}, error) { return struct{}{}, g.client.Chmod(p, mode) })
	return err
}

// ReadLink 读出符号链接指向的目标。属性面板要显示"它指向哪里"。
func (g *sftpGuard) ReadLink(p string) (string, error) {
	return call(g, func() (string, error) { return g.client.ReadLink(p) })
}

// Symlink 在目标端新建一个符号链接。跨主机复制时用来原样保留链接。
func (g *sftpGuard) Symlink(oldname, newname string) error {
	_, err := call(g, func() (struct{}, error) { return struct{}{}, g.client.Symlink(oldname, newname) })
	return err
}

// Chtimes 还原修改 / 访问时间。跨主机复制保留源端的时间戳。
func (g *sftpGuard) Chtimes(p string, atime, mtime time.Time) error {
	_, err := call(g, func() (struct{}, error) { return struct{}{}, g.client.Chtimes(p, atime, mtime) })
	return err
}

// writeSFTPError 把 SFTP 层的错误映射为合适的状态码。
//
// 超时归 504：这不是客户端的问题，而是上游在规定时间内没有回应，
// 前端据此可以给出"远端无响应，请检查目标机"这种可操作的提示。
func (s *Server) writeSFTPError(w http.ResponseWriter, err error, prefix string) {
	switch {
	case errors.Is(err, errSFTPTimeout):
		writeErr(w, http.StatusGatewayTimeout, prefix+"：远端未在 "+sftpOpTimeout.String()+" 内响应")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "主机不存在")
	default:
		writeErr(w, http.StatusInternalServerError, prefix+"："+err.Error())
	}
}

// stallGuard 监视流式传输的进度。
//
// 传输类操作没法用总超时（大文件本来就要传很久），但只要一段时间内
// 一个字节都没有流动，就说明链路已经死了 —— 继续等下去只会一直占着
// 一条 SSH 会话和一个 HTTP 连接。
type stallGuard struct {
	idle  time.Duration
	close func()

	mu    sync.Mutex
	last  time.Time
	stop  chan struct{}
	once  sync.Once
	timer *time.Ticker
}

func newStallGuard(idle time.Duration, closeFn func()) *stallGuard {
	g := &stallGuard{idle: idle, close: closeFn, last: time.Now(), stop: make(chan struct{})}
	g.timer = time.NewTicker(idle / 4)
	go g.watch()
	return g
}

func (g *stallGuard) watch() {
	for {
		select {
		case <-g.stop:
			return
		case <-g.timer.C:
			g.mu.Lock()
			silent := time.Since(g.last)
			g.mu.Unlock()
			if silent > g.idle {
				g.close()
				return
			}
		}
	}
}

func (g *stallGuard) touch() {
	g.mu.Lock()
	g.last = time.Now()
	g.mu.Unlock()
}

// Release 停止监视。调用方在传输结束后必须调用，否则监视协程会跟着计时器
// 一直转到下一次触发。
func (g *stallGuard) Release() {
	g.once.Do(func() {
		g.timer.Stop()
		close(g.stop)
	})
}

// progressReader / progressWriter 在每次有字节流动时刷新停滞计时。
// 两侧都要包：下载时远端可能不吐数据（读侧停滞），也可能客户端不接收（写侧停滞）。
type progressReader struct {
	r io.Reader
	g *stallGuard
}

func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.g.touch()
	}
	return n, err
}

type progressWriter struct {
	w io.Writer
	g *stallGuard
}

func (p progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.g.touch()
	}
	return n, err
}
