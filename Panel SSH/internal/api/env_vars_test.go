package api

import (
	"strings"
	"testing"

	"panelssh/internal/env"
)

// TestValidateEnvVarReqRejectsPathEdit 守住"PATH 只能走加入 PATH 这条路"在 API 层的拦阻。
//
// 拦阻点放在连目标机之前（见 execEnvVar）：这条判断不依赖任何主机状态，先拒掉既省
// 一次 SSH 往返，也不会在目标机上留下痕迹。
func TestValidateEnvVarReqRejectsPathEdit(t *testing.T) {
	for _, action := range []string{env.ActionEnvSet, env.ActionEnvUnset} {
		req := env.VarRequest{Action: action, Name: env.VarPathName, Scope: env.ScopeUser, Value: "/opt/bin"}
		err := validateEnvVarReq(req)
		if err == nil {
			t.Fatalf("动作 %s 改 PATH 应该被拒", action)
		}
		// 拒绝理由必须指向正确入口，否则用户只知道"不行"、不知道去哪儿做。
		if !strings.Contains(err.Error(), "加入 PATH") {
			t.Fatalf("拒绝理由没有指向「加入 PATH」：%v", err)
		}
	}
}

// TestValidateEnvVarReqAllowsPathAdd 确认这条拦阻没有连带挡住加入 PATH 本身。
func TestValidateEnvVarReqAllowsPathAdd(t *testing.T) {
	req := env.VarRequest{Action: env.ActionEnvPathAdd, Dir: "/opt/bin", Scope: env.ScopeUser}
	if err := validateEnvVarReq(req); err != nil {
		t.Fatalf("用户级加入 PATH 不该被拒：%v", err)
	}
	req.Scope = env.ScopeSystem
	if err := validateEnvVarReq(req); err != nil {
		t.Fatalf("系统级加入 PATH 不该被拒：%v", err)
	}
}
