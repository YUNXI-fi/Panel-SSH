package env

import (
	"strings"
	"testing"
)

// 面板可能从 IDE/开发终端启动，那些环境里的 LD_LIBRARY_PATH 会让 apt-get
// 加载到非系统的 libstdc++。这里把"不传给系统命令"这条契约钉住。
func TestLocalCommandEnvDropsDynamicLinkerPaths(t *testing.T) {
	t.Setenv("LD_LIBRARY_PATH", "/tmp/poison")
	t.Setenv("LD_PRELOAD", "/tmp/poison.so")

	for _, item := range localCommandEnv() {
		if strings.HasPrefix(item, "LD_LIBRARY_PATH=") || strings.HasPrefix(item, "LD_PRELOAD=") {
			t.Fatalf("动态链接器变量不应传给系统命令：%s", item)
		}
	}
}
