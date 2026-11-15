package api

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/pkg/sftp"
)

// "按需提权"的触发条件是"被权限拒绝"，判错两个方向都有代价：
//   - 漏判 → 用户只看到一句 permission denied，而面板本来能替他改；
//   - 误判 → 磁盘满、目录非空这类失败也去提一次权，白跑一次还把真正的错误
//     盖成提权错误。
func TestIsPermissionDenied(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"SFTP 权限状态码", sftp.ErrSSHFxPermissionDenied, true},
		{"被包装过的权限状态码", fmt.Errorf("create: %w", sftp.ErrSSHFxPermissionDenied), true},
		{"服务端只回文案", errors.New(`sftp: "open" failed: Permission denied`), true},
		{"文件不存在不该提权", sftp.ErrSSHFxNoSuchFile, false},
		{"通用失败（目录非空 / 磁盘满）不该提权", sftp.ErrSSHFxFailure, false},
		{"空错误", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermissionDenied(tc.err); got != tc.want {
				t.Fatalf("isPermissionDenied(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// 提权路径上的路径拼接必须绝对可靠：文件名里带单引号、$() 或反引号都是合法的，
// 拼错就会把后半段当成命令执行 —— 而这条命令是以 root 跑的。
//
// 这里用真的 sh 取值而不是比对字符串字面量：引号写错时，只有真跑才看得出来。
func TestShellQuotePath(t *testing.T) {
	paths := []string{
		"/etc/hosts",
		"/tmp/with space",
		"/tmp/it's",
		"/tmp/$(touch /tmp/panelssh-quote-escape)",
		"/tmp/`id`",
		`/tmp/back\slash`,
		`/tmp/double"quote`,
	}
	for _, p := range paths {
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuotePath(p)).Output()
		if err != nil {
			t.Fatalf("取值失败：%v", err)
		}
		if string(out) != p {
			t.Fatalf("引号拼接错误：得到 %q，期望 %q", string(out), p)
		}
	}
}
