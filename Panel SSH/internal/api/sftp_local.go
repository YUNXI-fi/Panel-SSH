package api

import (
	"net"

	"github.com/pkg/sftp"
)

// newLocalSFTP 建立一个进程内的 SFTP 会话，根目录就是宿主机的真实根。
//
// 为什么不给宿主机单独写一套基于 os 的文件后端：文件管理有二十来个接口，
// 编码探测、哈希、压缩、跨主机传输还直接依赖 *sftp.File。把宿主机也接进
// SFTP 这条路，上面这些能力以及"被拒绝就用 root 重试"的提权逻辑全部自动成立，
// 不需要第二份实现。
//
// 用 net.Pipe 而不是监听 127.0.0.1：SFTP 协议本身不做认证（认证发生在 SSH 层），
// 一旦监听端口，本机任何用户都能不经过面板登录就读写全盘。管道只在进程内存在。
func newLocalSFTP() (*sftpSession, *sftpGuard, error) {
	serverConn, clientConn := net.Pipe()

	// 默认工作目录是进程 cwd；显式钉在 /，相对路径才不会落到面板的数据目录里。
	srv, err := sftp.NewServer(serverConn, sftp.WithServerWorkingDirectory("/"))
	if err != nil {
		_ = serverConn.Close()
		_ = clientConn.Close()
		return nil, nil, err
	}

	// Serve 必须先跑起来：NewClientPipe 会阻塞等待服务端的版本包，
	// 而 net.Pipe 是无缓冲的，两边谁都不先动就会死锁。
	go func() {
		// 连接由 sftpSession.Close 关闭；Serve 的返回值只可能是"连接已关"，
		// 对使用者没有意义，不再往外冒泡。
		_ = srv.Serve()
		_ = serverConn.Close()
	}()

	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		_ = serverConn.Close()
		_ = clientConn.Close()
		return nil, nil, err
	}

	return &sftpSession{client: client, localConn: serverConn}, &sftpGuard{client: client}, nil
}
