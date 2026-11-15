package dsh

import "io"

// Launcher 抽象「在一台主机上把 dsh 跑起来」这件事。
//
// dsh 有两种落脚点：面板所在的宿主机用本地子进程，其余主机用一条常驻 SSH 会话。
// 两者的差异只有"怎么起进程、怎么收进程" —— 子命令参数怎么补、就绪地址怎么解析、
// 状态机怎么流转完全一致。把差异收敛到这一层，Supervisor 不必知道自己在跟谁说话，
// 那套经过验证的 ready/watch 语义也不必写第二遍。
type Launcher interface {
	// Start 启动 dsh 并返回进程句柄。
	//
	// workDir 的含义随实现而定：本地实现是面板侧的真实路径，远端实现是
	// 目标机上的路径。两种实现各自负责把目录准备好。
	Start(argv []string, workDir string) (Process, error)
}

// Process 是一个已启动的 dsh 进程。
//
// 与 os/exec.Cmd 的关键差别是「终止」被拆成两个动作：本地实现能向整个进程组
// 发信号，远端实现只能再开一条 SSH 会话去 kill 那个 pid，能力不对等，
// 所以不能用一个笼统的 Kill 把它们抹平。
type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader

	// PID 返回进程号，0 表示拿不到。
	//
	// 本地实现直接读 os.Process；远端实现不知道 sshd 那边分配到的 pid
	// （那串数字在目标机上），因此返回 0，由调用方按需去读 pid 文件。
	PID() int

	// Wait 等待进程结束并回收资源，是唯一的回收点。
	//
	// 同一个进程调两次 Wait 会争抢 waitpid，其中一方必然拿到 ECHILD，
	// 还可能干扰输出管道的收尾 —— 这正是旧实现踩过的坑。因此只有
	// Supervisor 的 watch 协程可以调它，Stop 只能等 watch 结束。
	Wait() error

	// Terminate 请求进程优雅退出。
	Terminate()

	// Kill 强杀，用于优雅退出无响应时兜底。
	Kill()
}