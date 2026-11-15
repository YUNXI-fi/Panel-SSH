// Package sshclient 封装 SSH 拨号与会话。
//
// 主机指纹策略为 TOFU（首次信任）：首次连接时把服务端公钥追加到
// 本地 known_hosts，之后按标准 knownhosts 规则校验。
// 不使用 ssh.InsecureIgnoreHostKey —— 那等于永久关闭中间人防护。
package sshclient

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// 认证方式常量。
const (
	AuthPassword = "password"
	AuthKey      = "key"
	// AuthNone 表示目标机不需要凭据就能登录。
	//
	// 这不是"还没填凭据"，而是一种真实存在的服务端配置：SSH 的 none 认证
	// （服务端直接放行该用户）与 PermitEmptyPasswords yes（接受空密码）。
	// 两者都不需要用户提供任何秘密，因此面板也不该强迫他填点什么。
	AuthNone = "none"
	// AuthLocalKey 表示用面板所在机器上已有的 SSH 私钥登录。
	//
	// 对应的真实场景：运维在跳板机/工作机上早就配好了密钥，`ssh host` 能直接进。
	// 要求他把私钥内容复制粘贴进面板，既多余又容易出错（粘贴丢失换行就解析失败）。
	// 面板进程本来就以这个用户身份运行、本来就能读这些文件，读它不扩大权限边界。
	AuthLocalKey = "localkey"
)

// localKeyPaths 是按优先级排列的本机私钥路径。
//
// 覆盖 ed25519 / rsa / ecdsa 三种常见默认名；不读 ~/.ssh/config 里的
// IdentityFile —— 那需要完整解析 ssh_config，而这里只要覆盖绝大多数情况。
var localKeyPaths = func() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".ssh", "id_rsa"),
		filepath.Join(home, ".ssh", "id_ecdsa"),
	}
}

// Target 描述一次 SSH 连接目标。
type Target struct {
	Addr     string
	Port     int
	Username string
	AuthType string
	Secret   string // 密码，或 PEM 编码的私钥
}

// Dial 建立 SSH 连接。
//
// knownHostsPath 为指纹库文件路径，不存在时会被创建。
func Dial(t Target, knownHostsPath string, timeout time.Duration) (*ssh.Client, error) {
	if t.Addr == "" {
		return nil, errors.New("主机地址为空")
	}
	if t.Port <= 0 {
		t.Port = 22
	}

	authMethod, err := buildAuthMethod(t)
	if err != nil {
		return nil, err
	}
	callback, err := hostKeyCallback(knownHostsPath)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            t.Username,
		Auth:            []ssh.AuthMethod{authMethod},
		HostKeyCallback: callback,
		Timeout:         timeout,
	}

	client, err := ssh.Dial("tcp", net.JoinHostPort(t.Addr, strconv.Itoa(t.Port)), cfg)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func buildAuthMethod(t Target) (ssh.AuthMethod, error) {
	switch t.AuthType {
	case AuthKey:
		signer, err := ssh.ParsePrivateKey([]byte(t.Secret))
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败：%w", err)
		}
		return ssh.PublicKeys(signer), nil
	case AuthPassword, "":
		if t.Secret == "" {
			return nil, errors.New("密码为空")
		}
		return ssh.Password(t.Secret), nil
	case AuthNone:
		// 无凭据登录。
		//
		// x/crypto/ssh 在尝试任何方法之前会先发一次 none 认证，所以服务端若
		// 直接放行该用户（none 认证成功），这里给什么都不影响；
		// 而 PermitEmptyPasswords 那类服务端要的是"空密码"这个具体凭据，
		// 因此补一个空密码方法兜住它。
		//
		// 两者都被拒时，框架报的是 "attempted methods [none password]"，
		// 足以让用户看出这是服务端不允许无凭据登录，而不是面板没发凭据。
		return ssh.Password(""), nil
	case AuthLocalKey:
		// 把本机所有可解析的私钥一起交给客户端，由它按服务端的
		// "server-sig-algs" 逐把尝试 —— 与 ssh 命令行发现多把密钥时的行为一致。
		//
		// 带 passphrase 的密钥在这里解析不了（x/crypto/ssh 没有解锁口令的入口），
		// 跳过它继续试下一把，而不是让整次连接失败：用户往往同时有
		// 一把旧的带口令密钥和一把新的，跳过比报错有用。
		var signers []ssh.Signer
		var firstErr error
		for _, path := range localKeyPaths() {
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			signer, err := ssh.ParsePrivateKey(raw)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s：%w", path, err)
				}
				continue
			}
			signers = append(signers, signer)
		}
		if len(signers) == 0 {
			if firstErr != nil {
				return nil, fmt.Errorf("本机私钥无法使用（可能是带口令的密钥，面板不支持解锁）：%w", firstErr)
			}
			return nil, errors.New("面板所在机器上没有可用的 SSH 私钥（已查找 ~/.ssh/id_ed25519、id_rsa、id_ecdsa）")
		}
		return ssh.PublicKeys(signers...), nil
	default:
		return nil, fmt.Errorf("不支持的认证方式：%s", t.AuthType)
	}
}

// hostKeyCallback 构造 TOFU 指纹校验回调。
func hostKeyCallback(path string) (ssh.HostKeyCallback, error) {
	if err := ensureFile(path); err != nil {
		return nil, err
	}
	base, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("加载 known_hosts 失败：%w", err)
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		// 服务端可能出示的是 host certificate 而不是裸公钥（InstaBox 之类
		// 托管 SSH 就是这么做的）。knownhosts 对证书要求库里已有
		// @cert-authority 记录，否则会直接返回 "ssh: no authorities for
		// hostname" —— 那不是 KeyError，下面的 TOFU 分支接不住，
		// 表现为"首次连接一台新主机却提示可能是中间人攻击"。
		//
		// 本项目的策略是 TOFU：只记录并比对指纹，不校验 CA 链。因此取出
		// 证书里的公钥来比对即可 —— 证书换发但公钥不变时连接照常可用，
		// 公钥真的变了仍然会报冲突，安全语义没有削弱。
		if cert, ok := key.(*ssh.Certificate); ok {
			key = cert.Key
		}

		err := base(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		// Want 为空表示该主机此前没有任何记录 —— 属于首次连接，
		// 此时信任并落盘；若 Want 非空说明指纹变了，必须报错让用户介入。
		if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
			return appendKnownHost(path, hostname, key)
		}
		return fmt.Errorf("主机指纹校验失败（可能存在中间人攻击，或服务器重装过系统）：%w", err)
	}, nil
}

// knownHostsMu 串行化 known_hosts 的追加写。
//
// 首次连接一台新主机时，多个请求可能同时走到这里（终端 + 文件 + 监控同时
// 打开同一台新机器）。没有互斥时，两次写入的字节可能交错成一行无法解析的
// 记录，导致这台机器此后所有连接都因指纹库损坏而失败。
// （并发首连同一台新主机仍可能追加两条内容相同的记录，knownhosts 能正常处理，
//  危害远小于写出损坏的行，因此不为它再加一层读校验。）
var knownHostsMu sync.Mutex

func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	_, err = f.WriteString(line + "\n")
	return err
}

func ensureFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}
