// Package secretbox 提供基于 AES-256-GCM 的对称加解密，
// 用于保护落库的 SSH 密码与私钥，避免明文存储。
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const keySize = 32

// Box 持有主密钥与 AEAD 实例。
type Box struct {
	key  []byte
	aead cipher.AEAD
}

// Load 从 keyFile 读取主密钥；不存在则生成一个权限为 0600 的新密钥。
func Load(keyFile string) (*Box, error) {
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{key: key, aead: aead}, nil
}

func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		// 长度不对说明文件被截断或被替换，直接报错而不是"尽力而为"地补齐，
		// 否则会导致所有历史密文永久无法解密。
		if len(b) != keySize {
			return nil, fmt.Errorf("主密钥长度异常：期望 %d 字节，实际 %d 字节", keySize, len(b))
		}
		return b, nil
	case errors.Is(err, os.ErrNotExist):
		// 继续往下生成
	default:
		return nil, err
	}

	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Derive 基于主密钥派生指定用途的子密钥，用于会话签名等场景，
// 避免同一把密钥承担多种密码学职责。
func (b *Box) Derive(label string) []byte {
	m := hmac.New(sha256.New, b.key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// Encrypt 返回 base64(nonce || ciphertext)。明文为空时返回空串。
func (b *Box) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nil, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

// Decrypt 还原 Encrypt 的输出。密文为空时返回空串。
func (b *Box) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("密文长度不足，无法包含 nonce")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// RandomToken 生成 n 个字符的随机口令。
//
// 字符集剔除了易混淆的 0/O/1/l/I，便于人工抄录初始密码。
// 采用拒绝采样剔除取模偏置，保证各字符等概率出现。
func RandomToken(n int) (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// len(alphabet) 是 int 常量，显式转成 byte 以便与随机字节比较。
	const maxAcceptable = byte(256 - (256 % len(alphabet)))

	out := make([]byte, 0, n)
	buf := make([]byte, 1)
	for len(out) < n {
		if _, err := io.ReadFull(rand.Reader, buf); err != nil {
			return "", err
		}
		if buf[0] >= maxAcceptable {
			continue
		}
		out = append(out, alphabet[int(buf[0])%len(alphabet)])
	}
	return string(out), nil
}
