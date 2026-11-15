package env

import (
	"context"
	"testing"
	"time"
)

// 宿主机提权账号的语义：root 走 su（dedicated），运行用户走 sudo；
// 没配口令时必判为不可提权，不能悄悄退化成"可提权"。
func TestLocalExecutorEscalationSecret(t *testing.T) {
	plain := NewLocalExecutor()
	if _, _, ok := plain.EscalationSecret(); ok {
		t.Fatal("未配置口令时不应报告可提权")
	}

	root := NewLocalExecutorWithSecret("root", "pw")
	secret, dedicated, ok := root.EscalationSecret()
	if !ok || secret != "pw" || !dedicated {
		t.Fatalf("root 账号应为 su 路径，得到 secret=%q dedicated=%v ok=%v", secret, dedicated, ok)
	}

	user := NewLocalExecutorWithSecret("panelssh", "pw")
	_, dedicated, ok = user.EscalationSecret()
	if !ok || dedicated {
		t.Fatalf("普通账号应为 sudo 路径，得到 dedicated=%v ok=%v", dedicated, ok)
	}
}

// 需要口令却没有口令时必须直接报错，而不是以普通用户身份把命令跑一遍 ——
// 那样调用方会把"权限不足"误当成目标机的真实错误。
func TestLocalExecutorRefusesSecretWithoutPassword(t *testing.T) {
	_, err := NewLocalExecutor().Run(
		context.Background(),
		Command{Line: "true", Secret: true},
		time.Second,
	)
	if err == nil {
		t.Fatal("没有提权口令时执行 Secret 命令应当失败")
	}
}
