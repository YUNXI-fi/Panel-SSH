package plugin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// pluginProcess 是一个已启动的插件后端进程。
//
// 这里没有复用 internal/dsh 的 Launcher/Process：那个抽象的价值在于"本地与远端
// 两种落脚点共用同一套状态机"，而插件进程只跑面板所在机器（设计文档 5.3），
// 远端那一半收益为零；而它缺的两样东西恰恰是插件进程必需的 —— 进程 PID
// （写 pid 文件、展示状态）与收缩过的环境变量。补齐它们要改 dsh 的接口签名
// 与两个实现，那才是真的动已验证代码。
//
// 因此只照搬它验证过的三点：独立进程组、Pdeathsig、只在一处 Wait。
type pluginProcess struct {
	cmd    *exec.Cmd
	stdout io.Reader
	stderr io.Reader
}

// startProcess 启动插件进程。
//
// execPath 是绝对路径，args 不含 argv[0]：两者分开传给内核，不经过 shell，
// 参数边界不由字符串拼接决定。
func startProcess(execPath string, args, env []string, workDir string) (*pluginProcess, error) {
	if execPath == "" {
		return nil, errors.New("可执行文件路径为空")
	}
	// 宁可起不来也不能继承面板的环境：面板进程环境里有签发令牌用的秘密，
	// 子进程一旦继承，等于把面板的权限交给了插件代码。
	if len(env) == 0 {
		return nil, errors.New("环境变量为空，插件进程不能继承面板的环境")
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建工作目录失败：%w", err)
	}

	cmd := exec.Command(execPath, args...)
	cmd.Dir = workDir
	cmd.Env = env
	// 独立进程组：终止时能连同插件拉起的子进程一起回收。
	// Pdeathsig：面板被强杀（SIGKILL / OOM）时由内核顺手收掉插件进程，
	// 否则会留下一个占着内存、还占着数据库连接与 socket 的孤儿。
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
		return nil, fmt.Errorf("启动插件进程失败：%w", err)
	}
	return &pluginProcess{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

// PID 返回进程号。写 pid 文件与状态展示都要用它。
func (p *pluginProcess) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *pluginProcess) Stdout() io.Reader { return p.stdout }

func (p *pluginProcess) Stderr() io.Reader { return p.stderr }

// Wait 等待进程结束并回收资源，是唯一的回收点。
//
// 同一个进程调两次 Wait 会争抢 waitpid，其中一方必然拿到 ECHILD，
// 还可能干扰输出管道的收尾。因此只有 watch 协程可以调它，
// 停止流程等 watch 结束而不是自己再 Wait 一次。
func (p *pluginProcess) Wait() error { return p.cmd.Wait() }

func (p *pluginProcess) Terminate() { p.signal(syscall.SIGTERM) }

func (p *pluginProcess) Kill() { p.signal(syscall.SIGKILL) }

// signal 向整个进程组发信号：插件可能拉起子进程，只杀父进程会留下孤儿。
func (p *pluginProcess) signal(sig syscall.Signal) {
	if p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil {
		// 进程组可能已不存在，退化为只杀主进程。
		_ = p.cmd.Process.Signal(sig)
	}
}

// expandArgs 把启动参数里的占位符换成实际路径。
//
// 只做字符串替换，不经 shell：占位符的值是面板算出的绝对路径，
// 走 shell 解释就多了一条注入面。
func expandArgs(args []string, socketPath, dataDir string) []string {
	replacer := strings.NewReplacer(
		"{socket}", socketPath,
		"{dataDir}", dataDir,
	)
	expanded := make([]string, 0, len(args))
	for _, arg := range args {
		expanded = append(expanded, replacer.Replace(arg))
	}
	return expanded
}

// pluginEnv 组装插件进程的环境变量。
//
// 这是一份白名单，不是"面板环境加上几个变量"：面板进程的环境里有会话密钥、
// 数据库路径等，插件代码没有理由看到它们。插件需要知道的东西全部显式给出。
func pluginEnv(dataDir, socketPath, pkgDir string) []string {
	tmpDir := filepath.Join(dataDir, "tmp")
	_ = os.MkdirAll(tmpDir, 0o700)

	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	return []string{
		"PATH=" + path,
		// HOME 与 TMPDIR 都指向插件自己的数据目录：插件往"家目录"里写东西时，
		// 落点必须在它自己的地盘里，而不是面板用户的家目录。
		"HOME=" + dataDir,
		"TMPDIR=" + tmpDir,
		"LANG=C.UTF-8",
		// 下面几个是面板与插件之间的约定值，插件据此定位自己的资源与通道，
		// 不必把路径硬编码进清单参数。令牌文件只给路径，不给内容：
		// 路径不是秘密，令牌本身要由插件自己去读那个 0600 的文件。
		"PANEL_PLUGIN_SOCKET=" + socketPath,
		"PANEL_PLUGIN_TOKEN_FILE=" + filepath.Join(dataDir, tokenFileName),
		"PANEL_PLUGIN_DATA=" + dataDir,
		"PANEL_PLUGIN_DIR=" + pkgDir,
	}
}
