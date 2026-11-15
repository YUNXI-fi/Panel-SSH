package sshclient

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// none 是"真的不需要凭据"，password + 空 secret 是"还没填凭据"。
//
// 这两者在前端都会让 hasSecret 为 false，很容易被合成一条分支处理；
// 一旦合成，要么逼着用户给无需凭据的主机编一个假密码，要么把占位主机
// 放进连接流程然后报一条"密码为空"。这条测试把区别钉在认证方法这一层。
func TestBuildAuthMethodForNone(t *testing.T) {
	method, err := buildAuthMethod(Target{AuthType: AuthNone})
	if err != nil {
		t.Fatalf("免凭据登录不该在构建认证方法时失败：%v", err)
	}
	if method == nil {
		t.Fatal("免凭据登录必须给出一个非空认证方法：nil 会让 ssh 客户端在遍历时崩溃")
	}
}

// 空密码的 password 认证仍然要被挡在面板这一层。
//
// 目标机即便开着 PermitEmptyPasswords，用户选的也是 password 而不是 none ——
// 那说明他以为自己配了密码。静默当空密码发出去，会把"配置漏了"伪装成
// "服务端拒绝"，排查方向从一开始就错了。
func TestBuildAuthMethodRejectsEmptyPassword(t *testing.T) {
	if _, err := buildAuthMethod(Target{AuthType: AuthPassword}); err == nil {
		t.Fatal("password 认证留空时应当报错")
	}
}

// 未知认证方式必须明确拒绝，而不是退化成某种默认行为。
func TestBuildAuthMethodRejectsUnknownType(t *testing.T) {
	if _, err := buildAuthMethod(Target{AuthType: "carrier-pigeon"}); err == nil {
		t.Fatal("未知认证方式应当报错")
	}
}

// 本机密钥认证要能真的读出一把私钥。
//
// 用临时目录替换查找路径，测试不依赖运行环境里是否配过 SSH 密钥。
func TestBuildAuthMethodLocalKey(t *testing.T) {
	path := writeTestKey(t, t.TempDir())
	restore := replaceLocalKeyPaths([]string{path})
	t.Cleanup(restore)

	method, err := buildAuthMethod(Target{AuthType: AuthLocalKey})
	if err != nil {
		t.Fatalf("读取本机私钥失败：%v", err)
	}
	if method == nil {
		t.Fatal("本机密钥认证必须给出非空认证方法")
	}
}

// 一把都找不到时要给出可执行的说明，而不是一句底层报错。
//
// 用户选了「本机 SSH 密钥」却连不上，最可能的原因就是这台机器根本没配密钥。
func TestBuildAuthMethodLocalKeyMissing(t *testing.T) {
	restore := replaceLocalKeyPaths([]string{filepath.Join(t.TempDir(), "not-exist")})
	t.Cleanup(restore)

	if _, err := buildAuthMethod(Target{AuthType: AuthLocalKey}); err == nil {
		t.Fatal("找不到本机密钥时应当报错")
	}
}

// 带口令的密钥无法解锁，必须被跳过而不是让整次连接失败；
// 只要还有一把能用的，就应当正常工作。
func TestBuildAuthMethodLocalKeySkipsEncrypted(t *testing.T) {
	dir := t.TempDir()

	// 生成一把真正带口令的 ed25519 私钥：ParsePrivateKey 会以
	// "ssh: this private key is passphrase protected" 拒绝它。
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成测试密钥失败：%v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("test-passphrase"))
	if err != nil {
		t.Fatalf("序列化加密密钥失败：%v", err)
	}
	encrypted := filepath.Join(dir, "id_locked")
	if err := os.WriteFile(encrypted, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	usable := writeTestKey(t, dir)
	restore := replaceLocalKeyPaths([]string{encrypted, usable})
	t.Cleanup(restore)

	if _, err := buildAuthMethod(Target{AuthType: AuthLocalKey}); err != nil {
		t.Fatalf("应当跳过无法解锁的密钥并继续使用可用的那把：%v", err)
	}
}

// writeTestKey 在 dir 下生成一把无口令的 ed25519 私钥，返回路径。
func writeTestKey(t *testing.T, dir string) string {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成测试密钥失败：%v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("序列化测试密钥失败：%v", err)
	}

	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("写入测试密钥失败：%v", err)
	}
	return path
}

// replaceLocalKeyPaths 临时替换查找路径，返回恢复函数。
func replaceLocalKeyPaths(paths []string) func() {
	original := localKeyPaths
	localKeyPaths = func() []string { return paths }
	return func() { localKeyPaths = original }
}