package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"panelssh/internal/config"
	"panelssh/internal/env"
)

// 提权意图决定"拿不到 root 时怎么办"，这两种行为必须分开：
//   - escalateRequired（格式化/挂载/改网卡配置）：拿不到 root 就直接报错，
//     而不是让用户对着 "Permission denied" 猜；
//   - escalatePreferred（结束进程）：退化成普通执行，保住"杀自己的进程"这类
//     本来就不需要 root 的用法。
//
// 用宿主机（hostID = 0）验证：它不需要主机库，且本地通道没有登录口令可用，
// 因此"提不了权"这条分支在本机必然成立（除非面板自己就是 root 跑的，那就两条
// 路都该跑通 —— 用例按实际画像分支断言）。
func TestExecEscalatedIntentContract(t *testing.T) {
	s := &Server{profiles: newSampleBuffer[*env.Profile](profileTTL)}
	ctx := context.Background()

	profile, err := s.hostProfile(ctx, LocalHostID, false)
	if err != nil {
		t.Fatalf("本机画像探测失败：%v", err)
	}

	const script = "echo hi"
	intents := []escalateIntent{escalateRequired, escalatePreferred}
	if profile.CanPrivileged() {
		// 本机已是 root：两种意图都应当照常跑通。
		for _, intent := range intents {
			res, err := s.execEscalated(ctx, LocalHostID, script, 5*time.Second, intent)
			if err != nil {
				t.Fatalf("本机可提权（%s）时不该失败：%v", profile.PrivilegeMode(), err)
			}
			if strings.TrimSpace(res.Stdout) != "hi" {
				t.Fatalf("输出不符：%q", res.Stdout)
			}
		}
		return
	}

	_, err = s.execEscalated(ctx, LocalHostID, script, 5*time.Second, escalateRequired)
	if !errors.Is(err, env.ErrNoPrivilege) {
		t.Fatalf("必须提权却提不了时应返回 ErrNoPrivilege，得到 %v", err)
	}

	res, err := s.execEscalated(ctx, LocalHostID, script, 5*time.Second, escalatePreferred)
	if err != nil {
		t.Fatalf("建议提权的命令在提不了权时应退化为普通执行：%v", err)
	}
	if strings.TrimSpace(res.Stdout) != "hi" {
		t.Fatalf("退化执行没有跑到命令：%+v", res)
	}
}

// 提权自检是整个界面里唯一会"真的提一次权"的只读按钮，结论的去向只有两条：
//   - HTTP 200 + ok=false → 弹窗展示 su 的原话（用户据此决定怎么配目标机）；
//   - 非 2xx              → 被前端当成请求错误，只弹一条转瞬即逝的浮层。
//
// 所以这里走真实路由表，把"路径注册对不对、鉴权在不在、失败结论走哪个状态码"
// 一起钉住 —— 只调 handler 是测不出路径写错这类问题的。

// 宿主机（LocalHostID = 0）是唯一不需要主机库就能跑的目标，用它验证契约。
// 它没有"提权口令"这条提权路径（本地通道不实现 env.SecretFeeder），结论只可能
// 来自 root 登录，其余必须落到"无可用路径"。
func TestPrivilegeCheckRouteContract(t *testing.T) {
	s := &Server{
		cfg:      &config.Config{DevMode: true},
		sessions: newTestSessionManager(t),
		profiles: newSampleBuffer[*env.Profile](profileTTL),
	}

	// 借会话签发逻辑拿一份 Cookie，不必真跑登录接口。
	issued := httptest.NewRecorder()
	s.sessions.Issue(issued, httptest.NewRequest(http.MethodPost, "/api/auth/login", nil), 1)

	req := httptest.NewRequest(http.MethodPost, "/api/hosts/0/privilege-check", nil)
	req.Header.Set(csrfHeader, "1")
	for _, c := range issued.Result().Cookies() {
		req.AddCookie(c)
	}

	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("自检结论必须以 200 返回（否则界面只剩一条浮层提示），得到 %d：%s", w.Code, w.Body.String())
	}
	var got privilegeCheckResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%s）", err, w.Body.String())
	}
	if got.Detail == "" {
		t.Fatal("结论为空：弹窗里没有可读的原因")
	}
	// 结论随运行环境变化（root / 都没有），打出来便于对账。
	t.Logf("宿主机自检结论：%s", w.Body.String())

	switch {
	case got.OK:
		// 本地通道不实现 env.SecretFeeder，宿主机没有提权口令可用，
		// 因此成功只可能来自 root 登录。
		if got.Mode != string(env.PrivRoot) {
			t.Fatalf("宿主机成功时只可能是 root 登录：%+v", got)
		}
	case got.Mode != string(env.PrivNone):
		t.Fatalf("宿主机失败时只可能是「无可用路径」：%+v", got)
	}
}

// 自检不是匿名可达的：它会真的提一次权，必须先过会话这一关。
func TestPrivilegeCheckRequiresAuth(t *testing.T) {
	s := &Server{cfg: &config.Config{DevMode: true}, sessions: newTestSessionManager(t)}

	req := httptest.NewRequest(http.MethodPost, "/api/hosts/0/privilege-check", nil)
	req.Header.Set(csrfHeader, "1")

	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应被挡下（401），得到 %d：%s", w.Code, w.Body.String())
	}
}

// su 路径下的会话带伪终端：su 的口令提示不回显也不换行，会与自检标记挤在
// 同一行（"Password: PANEL_UID=0"）。解析只认标记，且不能要求它出现在行首 ——
// 否则提权明明成功，界面却报"提权失败"。
func TestParsePrivilegeUID(t *testing.T) {
	cases := []struct {
		name  string
		out   string
		want  string
		found bool
	}{
		{"普通输出", privilegeUIDMarker + "0\n", "0", true},
		{"伪终端 CRLF 且提示行未换行", "Password: " + privilegeUIDMarker + "0\r\n", "0", true},
		{"提示与标记分行", "Password: \n" + privilegeUIDMarker + "0\r\n", "0", true},
		{"标记后面还有别的输出", privilegeUIDMarker + "1000\nwarning: x\n", "1000", true},
		{"提权失败没有标记", "Password: su: Authentication failure\n", "", false},
		{"空输出", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePrivilegeUID(tc.out)
			if ok != tc.found || got != tc.want {
				t.Fatalf("parsePrivilegeUID(%q) = (%q, %v)，期望 (%q, %v)", tc.out, got, ok, tc.want, tc.found)
			}
		})
	}
}

// 失败原因覆盖两条流，并抹掉面板自己造成的 "Password: " 前缀。
func TestPrivilegeFailureText(t *testing.T) {
	cases := []struct {
		name string
		res  *env.ExecResult
		want string
	}{
		{
			"两条流都有内容时优先取 stderr",
			&env.ExecResult{Stdout: "无关输出", Stderr: "su: Authentication failure\n"},
			"su: Authentication failure",
		},
		{
			"伪终端：stderr 被并进 stdout，并抹掉面板触发的口令提示",
			&env.ExecResult{Stdout: "Password: su: Authentication failure\n"},
			"su: Authentication failure",
		},
		{
			"两条流都空也要给出一句话",
			&env.ExecResult{},
			"命令没有返回任何信息，请查看目标机的认证日志",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := privilegeFailureText(tc.res); got != tc.want {
				t.Fatalf("privilegeFailureText = %q，期望 %q", got, tc.want)
			}
		})
	}
}
