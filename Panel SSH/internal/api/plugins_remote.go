package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"panelssh/internal/plugin"
)

// 插件推送到远端主机的能力实现。
//
// 为什么单独一个文件而不是并进 plugins.go：这个类型是 plugin.RemoteEnv 的
// 适配层（SFTP + exec），它的正确性取决于"连接怎么建、错误怎么翻译"，
// 与 HTTP 处理逻辑无关；混在一起会让 plugins.go 同时承担两种关注点。

// remotePluginEnv 为目标主机构造一个插件落位环境。
//
// 这里只做构造，不建立任何连接：真正的 SFTP 会话在 Upload 被调用时才开，
// 而"目标机连不上"这类错误会带着主机信息一起报给用户 —— 在构造函数里
// 提前连一次再做一次连接只会白多一轮握手。
func (s *Server) remotePluginEnv(hostID int64) plugin.RemoteEnv {
	return &remotePluginEnv{server: s, hostID: hostID}
}

type remotePluginEnv struct {
	server *Server
	hostID int64
}

// remotePluginTimeout 是单次远端落位脚本的执行上限。
//
// 解压一个几十 MiB 的包在正常机器上是一两秒的事，给 5 分钟是为了兜住
// 磁盘很慢或包接近上限的情况；超出这个时间说明目标机已经不正常了。
const remotePluginTimeout = 5 * time.Minute

// HomeDir 读取目标机上登录账号的家目录。
//
// 用 shell 展开 ~ 而不是去问 SFTP：SFTP 协议的 realpath 返回的是服务端
// 眼里的路径，部分实现给的是 chroot 后的相对值，拿它拼绝对路径会落到
// 面板预料之外的位置。~ 由目标机的 shell 自己展开，语义最准。
func (e *remotePluginEnv) HomeDir(ctx context.Context) (string, error) {
	res, err := e.server.execOnHost(ctx, e.hostID, "printf '%s' \"$HOME\"", defaultExecTimeout)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", errors.New(strings.TrimSpace(res.Stderr))
	}

	home := strings.TrimSpace(res.Stdout)
	// 必须是绝对路径：用它拼出来的落位目录要写进远端文件系统，
	// 相对路径在不同登录 shell 下会指向不同位置。
	if home == "" || !path.IsAbs(home) {
		return "", errors.New("目标主机未能给出有效的家目录")
	}
	return path.Clean(home), nil
}

// Upload 把安装包写到目标机的指定路径。
func (e *remotePluginEnv) Upload(ctx context.Context, dst string, src io.Reader, size int64) error {
	sess, client, ok := e.server.openSFTPNoResponse(e.hostID)
	if !ok {
		return errors.New("无法建立到目标主机的 SFTP 会话")
	}
	defer sess.Close()

	// MkdirAll 覆盖整条链路（含 .panelssh 与 .staging）：第一次往这台机器
	// 上装插件时这些目录一个都不存在。
	if err := client.MkdirAll(path.Dir(dst)); err != nil {
		return fmt.Errorf("创建目录 %s 失败：%w", path.Dir(dst), err)
	}

	file, err := client.Create(dst)
	if err != nil {
		return fmt.Errorf("创建远端文件失败：%w", err)
	}

	// 安装包最小也有几十 KiB，链路慢时静默是可以预期的，因此用"是否还在
	// 流动"来判断死活，而不是设一个总超时。
	guard := newStallGuard(sftpStallTimeout, func() { _ = file.Close() })
	defer guard.Release()

	if _, err := io.Copy(progressWriter{w: file, g: guard}, progressReader{r: src, g: guard}); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入远端文件失败：%w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭远端文件失败：%w", err)
	}
	return nil
}

// Exec 在目标机上执行一段脚本。
func (e *remotePluginEnv) Exec(ctx context.Context, script string) (string, string, int, error) {
	res, err := e.server.execOnHost(ctx, e.hostID, script, remotePluginTimeout)
	if err != nil {
		return "", "", -1, err
	}
	return res.Stdout, res.Stderr, res.ExitCode, nil
}

// openSFTPNoResponse 与 openSFTP 同源，但把错误留给调用方处理。
//
// 之所以不能直接复用 openSFTP：那个函数在失败时会自己写出 HTTP 响应，
// 而插件推送的失败要带着"是推送到哪台主机时失败的"一并报给用户，
// 由 plugins.go 统一组装。两处对同一个连接过程各写一遍是错误的开始，
// 因此这里只做"建连接"，响应交给上层。
func (s *Server) openSFTPNoResponse(hostID int64) (*sftpSession, *sftpGuard, bool) {
	lease, err := s.leaseHost(hostID)
	if err != nil {
		return nil, nil, false
	}

	session, err := lease.Client.NewSession()
	if err != nil {
		lease.Release()
		return nil, nil, false
	}

	if err := session.RequestSubsystem("sftp"); err != nil {
		_ = session.Close()
		lease.Release()
		return nil, nil, false
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		lease.Release()
		return nil, nil, false
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		lease.Release()
		return nil, nil, false
	}

	client, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		_ = session.Close()
		lease.Release()
		return nil, nil, false
	}

	return &sftpSession{client: client, session: session, lease: lease}, &sftpGuard{client: client}, true
}
