// Command panelssh 是 Panel SSH 的服务端入口。
//
// 设计目标：单个静态二进制、零外部依赖（数据库内嵌）、优雅退出。
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"panelssh/internal/api"
	"panelssh/internal/auth"
	"panelssh/internal/config"
	"panelssh/internal/secretbox"
	"panelssh/internal/sshclient"
	"panelssh/internal/store"
	"panelssh/internal/sysinfo"
)

const (
	sessionTTL     = 7 * 24 * time.Hour
	shutdownWindow = 5 * time.Second
	// logRotateBytes 是启动时轮转面板日志的阈值。
	logRotateBytes = 5 << 20
)

// logFile 持有日志文件句柄。
//
// 必须留在包级变量里：log 的写入目标是 *os.File，而 os.File 带终结器，
// 函数返回后若没有引用，GC 会把它关掉 —— 之后所有日志静默丢失，
// 且不会报任何错。
var logFile *os.File

func main() {
	cfg := config.Load()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatalf("创建数据目录失败：%v", err)
	}

	// 先把日志接上文件，再往下走：启动阶段失败（密钥、数据库）恰恰是
	// 最需要事后翻日志的那类故障。
	if err := setupLogFile(cfg.DataDir); err != nil {
		log.Printf("面板日志无法落盘（%v），日志将只输出到控制台", err)
	}

	box, err := secretbox.Load(filepath.Join(cfg.DataDir, "master.key"))
	if err != nil {
		log.Fatalf("加载主密钥失败：%v", err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "panelssh.db"))
	if err != nil {
		log.Fatalf("打开数据库失败：%v", err)
	}
	defer st.Close()

	if err := bootstrapAdmin(st); err != nil {
		log.Fatalf("初始化管理员失败：%v", err)
	}

	// 会话签名使用派生密钥，与凭据加密密钥职责分离；
	// 具体密钥由持久化的 session epoch 决定，便于运行期整体作废会话。
	sessions, err := api.NewSessionManager(st, box, sessionTTL)
	if err != nil {
		log.Fatalf("初始化会话密钥失败：%v", err)
	}

	// 连接池按主机复用 SSH 连接，供终端与文件管理共用；
	// 进程退出时统一关闭，不留悬挂连接。
	pool := sshclient.NewPool(filepath.Join(cfg.DataDir, "known_hosts"))
	defer pool.Close()

	// 采集器按主机保存上一次采样，用于把 /proc 的累计值换算成 CPU% 与网络速率。
	collector := sysinfo.NewCollector()

	// dsh 的受管实例由 api.Server 按主机持有（宿主机本地进程、其余主机走 SSH 隧道），
	// 这里只负责在退出时把它们停掉。启动命令的读取也一并收在那边：
	// 它按主机分别落库，不再是一个全局配置项。
	server := api.NewServer(cfg, st, box, sessions, pool, collector)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 监听失败必须回到主协程统一处理。
	//
	// 旧实现在这个 goroutine 里直接 log.Fatalf —— os.Exit 不执行任何 defer，
	// 于是数据库、连接池、dsh 子进程的清理全被跳过，进程会带着一个孤儿 dsh 退出。
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("Panel SSH 已启动： http://localhost%s", cfg.Addr)
		if cfg.DevMode {
			log.Printf("开发模式：前端请访问 Vite dev server（默认 http://localhost:5173）")
		}
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	// 优雅退出：收到信号后给在途请求一个收尾窗口，而不是直接终止进程。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serveErr:
		log.Printf("服务异常退出：%v", err)
		// 不在此处 os.Exit：让 main 正常返回，defer 里的清理照常执行。
	case <-stop:
		log.Println("收到退出信号，正在关闭...")
	}

	// WebSocket 连接是从 HTTP 连接 Hijack 出来的，http.Server.Shutdown 不会等它们；
	// 主动关闭并告知原因，前端才能显示"连接已断开"而不是一直转圈。
	server.CloseSockets("面板正在关闭，连接已断开")

	// 指标长驻流不挂在某条 WebSocket 连接上（一条流可能被多个窗口共享），
	// 只关连接收不掉它。这里显式停掉，才不会留下"面板已经退了、
	// 目标机上还挂着一个 while 采集循环"的残局。
	server.CloseMetricsStreams()

	// dsh 实例同理：本地子进程有 Pdeathsig 兜底，远端那些却没有 ——
	// 面板正常退出时必须逐个停掉，否则远端会一直挂着一个占几百 MB 的 node。
	server.CloseDshInstances()

	// 插件后端进程：不等它自然退出，否则面板已经退了、socket 还占着。
	server.ClosePluginRuntimes()

	// 插件申请的隧道同理：每条都在目标机上占着一个转发通道。
	server.ClosePluginTunnels()

	// 浏览器代理：本机端口让进程退出一并带走，但正在转发的连接要立刻断开。
	server.CloseBrowserProxies()

	// 常驻 SSH 通道放在最后关：停远端 dsh 实例、收代理连接都要靠它。
	server.CloseHostTunnels()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownWindow)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("关闭超时：%v", err)
	}
	log.Println("已退出")
}

// setupLogFile 让日志同时写进 data/panelssh.log。
//
// 面板以普通进程运行、不是 systemd 服务，没有 journalctl 可查，进程一退日志
// 就随终端一起消失。「系统日志」窗口要能回看上次启动乃至崩溃现场，只能自己
// 留一份。
//
// 轮转只在启动时做一次：这份日志是排障用的，不是审计数据，没有长期留存的需求；
// 运行期继续追加，超过阈值由下次启动切成 .1。覆盖而不是编号递增 —— 保留两份
// 的目的是"看得见上一次启动"，不是归档。
func setupLogFile(dataDir string) error {
	path := filepath.Join(dataDir, config.LogName)
	if fi, err := os.Stat(path); err == nil && fi.Size() > logRotateBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	logFile = f
	log.SetOutput(io.MultiWriter(os.Stdout, f))
	return nil
}

// bootstrapAdmin 在空库时创建初始管理员，并把随机密码打印到控制台。
func bootstrapAdmin(st *store.Store) error {
	count, err := st.CountUsers()
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	password, err := secretbox.RandomToken(16)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := st.CreateUser("admin", hash); err != nil {
		return err
	}

	log.Println("========================================================")
	log.Printf("  已创建初始管理员账号")
	log.Printf("  用户名：admin")
	log.Printf("  密  码：%s", password)
	log.Println("  该密码仅显示一次，请立即妥善保存，登录后请及时修改。")
	log.Println("========================================================")
	return nil
}
