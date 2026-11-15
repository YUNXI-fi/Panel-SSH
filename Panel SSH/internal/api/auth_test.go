package api

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"panelssh/internal/auth"
	"panelssh/internal/store"
)

// tlsConnectionState 用于构造"直连 TLS"的请求。
var tlsConnectionState = tls.ConnectionState{}

func newTestSessionManager(t *testing.T) *auth.Manager {
	t.Helper()
	// 会话管理器只做 HMAC 签名，测试里给一段固定密钥即可。
	return auth.NewManager([]byte("panelssh-test-session-key-000000"), time.Hour)
}

// 改密码必须是原子的：密码哈希与新的会话 epoch 要么都写入，要么都不写。
// 分成两步写时，若第二步失败会留下"密码已改、历史会话却仍然有效"的状态。
func TestChangePasswordAndSetMetaWritesBoth(t *testing.T) {
	st := newTestStore(t)

	userID, err := st.CreateUser("admin", "old-hash")
	if err != nil {
		t.Fatalf("准备账号失败：%v", err)
	}

	if err := st.ChangePasswordAndSetMeta(userID, "new-hash", "session_epoch", "epoch-1"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	user, err := st.GetUserByID(userID)
	if err != nil {
		t.Fatalf("读取账号失败：%v", err)
	}
	if user.PasswordHash != "new-hash" {
		t.Fatalf("密码哈希未更新：%s", user.PasswordHash)
	}
	epoch, err := st.GetMeta("session_epoch")
	if err != nil {
		t.Fatalf("读取 epoch 失败：%v", err)
	}
	if epoch != "epoch-1" {
		t.Fatalf("epoch 未写入：%s", epoch)
	}
}

// 用户不存在时整个操作应当失败，且不能留下"只有 epoch 变了"的半截状态。
func TestChangePasswordAndSetMetaIsAtomicOnMissingUser(t *testing.T) {
	st := newTestStore(t)

	// 先写入一个已知的 epoch
	if err := st.SetMeta("session_epoch", "before"); err != nil {
		t.Fatalf("准备 epoch 失败：%v", err)
	}

	err := st.ChangePasswordAndSetMeta(99999, "new-hash", "session_epoch", "after")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}

	epoch, err := st.GetMeta("session_epoch")
	if err != nil {
		t.Fatalf("读取 epoch 失败：%v", err)
	}
	if epoch != "before" {
		t.Fatalf("失败的改密操作污染了 epoch：%s", epoch)
	}
}

// 会话 Cookie 必须带 HttpOnly 与 SameSite，且在 HTTPS 请求下带 Secure。
func TestSessionCookieAttributes(t *testing.T) {
	manager := newTestSessionManager(t)

	cases := []struct {
		name   string
		req    *http.Request
		secure bool
	}{
		{
			name:   "明文 HTTP 不带 Secure",
			req:    httptest.NewRequest(http.MethodPost, "/api/auth/login", nil),
			secure: false,
		},
		{
			name: "反向代理转发的 HTTPS 带 Secure",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				r.Header.Set("X-Forwarded-Proto", "https")
				return r
			}(),
			secure: true,
		},
		{
			name: "多段 X-Forwarded-Proto 只认第一段",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				r.Header.Set("X-Forwarded-Proto", "http, https")
				return r
			}(),
			secure: false,
		},
		{
			name: "直连 TLS 带 Secure",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
				r.TLS = &tlsConnectionState
				return r
			}(),
			secure: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			manager.Issue(rec, tc.req, 1)

			cookies := rec.Result().Cookies()
			if len(cookies) == 0 {
				t.Fatal("没有下发会话 Cookie")
			}
			cookie := cookies[0]
			if !cookie.HttpOnly {
				t.Error("缺少 HttpOnly")
			}
			if cookie.SameSite != http.SameSiteLaxMode {
				t.Errorf("SameSite 不符：%v", cookie.SameSite)
			}
			if cookie.Secure != tc.secure {
				t.Errorf("Secure 期望 %v，实际 %v", tc.secure, cookie.Secure)
			}
		})
	}
}

// 清除 Cookie 也要带上与签发时一致的 Secure，否则部分浏览器删不掉它。
func TestClearCookieKeepsSecureAttribute(t *testing.T) {
	manager := newTestSessionManager(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("X-Forwarded-Proto", "https")

	rec := httptest.NewRecorder()
	manager.Clear(rec, req)

	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("没有下发清除用的 Cookie")
	}
	if !cookies[0].Secure {
		t.Error("清除 Cookie 缺少 Secure")
	}
	if cookies[0].MaxAge >= 0 {
		t.Errorf("清除 Cookie 的 MaxAge 应为负值，实际 %d", cookies[0].MaxAge)
	}
}
