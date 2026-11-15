// Package auth 提供密码哈希与无状态会话。
//
// 会话采用 HMAC-SHA256 签名 Cookie，服务端不保存会话表：
// 单机面板没必要引入 Redis / 会话表，签名校验足以防篡改，
// 且重启服务不会导致所有人掉线。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const cookieName = "panelssh_session"

// Manager 负责签发与校验会话。
//
// 签名密钥支持运行期轮换（见 Rotate）：轮换后所有已签发会话立即失效。
// 这是无状态会话实现"改密码即让其他会话下线"的唯一手段。
type Manager struct {
	mu  sync.RWMutex
	key []byte
	ttl time.Duration
}

// NewManager 构造会话管理器，key 应为主密钥派生出的会话专用子密钥。
func NewManager(key []byte, ttl time.Duration) *Manager {
	return &Manager{key: key, ttl: ttl}
}

// Rotate 替换签名密钥，使此前签发的全部会话立即失效。
func (m *Manager) Rotate(key []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.key = key
}

func (m *Manager) signingKey() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.key
}

// Issue 签发会话 Cookie。
func (m *Manager) Issue(w http.ResponseWriter, r *http.Request, userID int64) {
	expiresAt := time.Now().Add(m.ttl).Unix()
	payload := fmt.Sprintf("%d:%d", userID, expiresAt)
	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) +
		"." + base64.RawURLEncoding.EncodeToString(m.sign(payload))

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(m.ttl.Seconds()),
		Secure:   requestIsHTTPS(r),
	})
}

// requestIsHTTPS 判断本次请求是否经由 HTTPS 到达。
//
// 面板通常跑在 TLS 反向代理之后，此时 r.TLS 为空而 X-Forwarded-Proto 为 https，
// 因此两种情况都要认。不设 Secure 的话，浏览器会在明文 HTTP 请求里也带上会话
// Cookie —— 只要用户用 http:// 访问过一次，会话就有被旁路截获的风险。
func requestIsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	// 只信任最靠近客户端的那一段，避免被追加的伪造头误导。
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// Current 校验请求携带的会话，返回用户 ID。
func (m *Manager) Current(r *http.Request) (int64, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return 0, false
	}
	payloadPart, sigPart, ok := strings.Cut(c.Value, ".")
	if !ok {
		return 0, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return 0, false
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return 0, false
	}
	payload := string(payloadBytes)

	// 先验签再解析内容：签名不通过的内容一律视为不可信输入。
	if subtle.ConstantTimeCompare(sig, m.sign(payload)) != 1 {
		return 0, false
	}

	idPart, expPart, ok := strings.Cut(payload, ":")
	if !ok {
		return 0, false
	}
	expiresAt, err := strconv.ParseInt(expPart, 10, 64)
	if err != nil || time.Now().Unix() > expiresAt {
		return 0, false
	}
	userID, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return 0, false
	}
	return userID, true
}

// Clear 清除会话 Cookie。
func (m *Manager) Clear(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
		// 清除时也要带上同样的 Secure 属性，否则在某些浏览器上
		// 会因为属性不匹配而删不掉原 Cookie。
		Secure: requestIsHTTPS(r),
	})
}

// dummyHash 是一个固定的 bcrypt 哈希，用于"用户不存在"时也走一次真实的
// 密码比对。不这样做的话，用户名不存在会立刻返回，而密码错误要等 bcrypt
// 算完（约几十毫秒），响应耗时差可以直接用来枚举账号 ——
// 错误文案统一了，但时间维度没有。
var dummyHash = func() string {
	hash, err := HashPassword("panelssh-timing-equalizer")
	if err != nil {
		// 该哈希只用于消耗等量时间，生成失败不应影响服务启动。
		return "$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidinva"
	}
	return hash
}()

// CheckPasswordConstantTime 无论账号是否存在都执行一次 bcrypt 比对。
// hash 为空表示账号不存在，此时对 dummyHash 比一次，让耗时保持一致。
func CheckPasswordConstantTime(hash, plain string) bool {
	if hash == "" {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(plain))
		return false
	}
	return CheckPassword(hash, plain)
}

func (m *Manager) sign(payload string) []byte {
	h := hmac.New(sha256.New, m.signingKey())
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// HashPassword 生成 bcrypt 哈希。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword 校验明文与 bcrypt 哈希是否匹配。
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

type ctxKey struct{}

// WithUser 把已认证用户 ID 写入上下文。
func WithUser(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// UserFrom 从上下文取出已认证用户 ID。
func UserFrom(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ctxKey{}).(int64)
	return id, ok
}
