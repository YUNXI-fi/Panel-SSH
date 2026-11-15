// Package config 负责加载运行期配置。
// 优先级：命令行参数 > 环境变量 > 内置默认值。
package config

import (
	"flag"
	"os"
	"path/filepath"
)

// LogName 是面板自身日志在数据目录下的文件名。
//
// 面板以普通进程运行、不是 systemd 服务，没有 journalctl 可查，进程一退
// 日志就随终端一起消失。「系统日志」窗口要能回看上次启动乃至崩溃现场，
// 只能自己留一份：启动时把 log 输出同时写到这个文件（见 cmd/panelssh）。
const LogName = "panelssh.log"

// Config 是服务的全部运行期配置。
type Config struct {
	// Addr 是 HTTP 监听地址，例如 ":8080"。
	Addr string
	// DataDir 存放数据库、主密钥、known_hosts 等本地状态。
	DataDir string
	// DevMode 为 true 时，前端由 Vite dev server 提供，后端只服务 API。
	DevMode bool
	// DshCommand 是调用 dsh 的命令（如 "node /path/to/dsh/lib/bin.js"）。
	// 面板只负责按这条命令把它跑起来，不代用户安装 dsh 及其 Node 运行时。
	// 为空表示 DSH 工作台不可用。
	DshCommand string
}

// Load 解析配置。
func Load() *Config {
	c := &Config{}
	flag.StringVar(&c.Addr, "addr", envOr("PANELSSH_ADDR", ":8080"), "HTTP 监听地址")
	flag.StringVar(&c.DataDir, "data", envOr("PANELSSH_DATA", "./data"), "数据目录")
	flag.BoolVar(&c.DevMode, "dev", os.Getenv("PANELSSH_DEV") == "1", "开发模式（前端走 Vite dev server）")
	flag.StringVar(&c.DshCommand, "dsh", envOr("PANELSSH_DSH", ""),
		"调用 dsh 的命令，例如 \"node /path/to/dsh/lib/bin.js\"；留空则 DSH 工作台不可用")
	flag.Parse()

	// 统一转成绝对路径，避免因工作目录变化导致数据写错地方。
	if abs, err := filepath.Abs(c.DataDir); err == nil {
		c.DataDir = abs
	}
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
