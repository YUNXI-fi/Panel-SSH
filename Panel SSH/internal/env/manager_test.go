package env

import (
	"context"
	"time"
)

// stubExecutor 返回预置输出，用于测试纯文本解析路径（查询类方法）。
// 真正在目标机上执行 awk 的模拟脚本不走这个桩，而由 parseSimTSV 的
// fixture 测试覆盖其输出契约。
type stubExecutor struct {
	stdout   string
	stderr   string
	exitCode int
	// secret 非空时该桩同时充当 SecretFeeder，用于验证"需要口令的提权路径"判定。
	// dedicated 区分这份口令的来源：真值表示主机上另配的「提权口令」（root 的口令，
	// 只有 su 认），假值表示退回的登录凭据（执行者自己的口令，sudo 要的就是它）。
	secret    string
	dedicated bool
	// sudoDenied 让桩在"用口令实测 sudo"时返回非零：模拟口令错、或该账号不在 sudoers 里。
	sudoDenied bool
}

func (e *stubExecutor) Run(_ context.Context, cmd Command, _ time.Duration) (*ExecResult, error) {
	if e.sudoDenied && cmd.Secret {
		return &ExecResult{Stderr: "sudo: Authentication failure", ExitCode: 1}, nil
	}
	return &ExecResult{Stdout: e.stdout, Stderr: e.stderr, ExitCode: e.exitCode}, nil
}

func (e *stubExecutor) Stream(_ context.Context, _ Command, _ time.Duration, _ LogSink) (*ExecResult, error) {
	return &ExecResult{Stdout: e.stdout, Stderr: e.stderr, ExitCode: e.exitCode}, nil
}

func (e *stubExecutor) EscalationSecret() (string, bool, bool) {
	return e.secret, e.dedicated, e.secret != ""
}
