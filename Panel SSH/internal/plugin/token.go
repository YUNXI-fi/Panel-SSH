package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// tokenFileName 是令牌文件的名字，落在插件自己的数据目录里。
const tokenFileName = "token"

// tokenBytes 是令牌的随机字节数。
//
// 32 字节：它是一份"进程即身份"的凭据，猜中就能以插件身份调用面板能力，
// 因此没有理由在这上面省熵。
const tokenBytes = 32

// TokenStore 保存"哪个令牌属于哪个插件"。
//
// 只存身份，不存权限：令牌回答"你是谁"，"你能做什么"每次都要按当前授权重新判断。
// 把授权快照缓存进令牌会制造一个漏洞窗口 —— 用户撤销了插件的某项权限，而插件
// 手里的令牌还带着撤销前的快照，在进程重启之前一直有效。
type TokenStore struct {
	mu       sync.RWMutex
	tokens   map[string]pluginIdentity
	byPlugin map[string]string
}

// pluginIdentity 是令牌对应的插件身份。
//
// 不带主机：插件进程面向哪台目标机是每次调用的参数，不是它的身份。
type pluginIdentity struct {
	Org  string
	Name string
}

func newTokenStore() *TokenStore {
	return &TokenStore{
		tokens:   make(map[string]pluginIdentity),
		byPlugin: make(map[string]string),
	}
}

// issue 为插件签发新令牌并写入它的数据目录，返回令牌原文。
//
// 同一插件同时只有一个有效令牌：进程重启即换一个新的，旧令牌立刻失效。
func (s *TokenStore) issue(org, name, dataDir string) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成令牌失败：%w", err)
	}
	token := hex.EncodeToString(raw)

	// 写文件而不是环境变量：环境变量会被 ps -e 与 /proc/<pid>/environ 暴露给
	// 同机的其他用户，文件有 0600 权限兜底。
	if err := os.WriteFile(filepath.Join(dataDir, tokenFileName), []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("写入令牌文件失败：%w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if old, ok := s.byPlugin[org+"/"+name]; ok {
		delete(s.tokens, old)
	}
	s.tokens[token] = pluginIdentity{Org: org, Name: name}
	s.byPlugin[org+"/"+name] = token
	return token, nil
}

// revoke 让某个插件的令牌立刻失效（进程停止、插件卸载）。
func (s *TokenStore) revoke(org, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if token, ok := s.byPlugin[org+"/"+name]; ok {
		delete(s.tokens, token)
		delete(s.byPlugin, org+"/"+name)
	}
}

// lookup 按令牌查出插件身份。
func (s *TokenStore) lookup(token string) (pluginIdentity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	identity, ok := s.tokens[token]
	return identity, ok
}

// LookupToken 按令牌查出插件身份，供回调接口做身份校验。
//
// 拿到身份只是第一步：能调用哪些能力要按该插件在当前主机上的实际授权判断，
// 这里给的答案只有"是谁"。
func (m *RuntimeManager) LookupToken(token string) (org, name string, ok bool) {
	identity, ok := m.tokens.lookup(token)
	return identity.Org, identity.Name, ok
}
