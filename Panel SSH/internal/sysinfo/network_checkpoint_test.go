package sysinfo

import (
	"strings"
	"testing"
)

func TestParseCheckpointPath(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "busctl 输出",
			raw:  `o "/org/freedesktop/NetworkManager/Checkpoint/5"`,
			want: "/org/freedesktop/NetworkManager/Checkpoint/5",
		},
		{
			name: "gdbus 输出",
			raw:  "(objectpath '/org/freedesktop/NetworkManager/Checkpoint/12',)",
			want: "/org/freedesktop/NetworkManager/Checkpoint/12",
		},
		{
			name: "权限不足",
			raw:  "Call failed: Not authorized to checkpoint/rollback",
			want: "",
		},
		{
			name: "空输出",
			raw:  "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseCheckpointPath(tc.raw); got != tc.want {
				t.Fatalf("解析为 %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeCheckpointPath(t *testing.T) {
	if _, err := NormalizeCheckpointPath("/org/freedesktop/NetworkManager/Checkpoint/7"); err != nil {
		t.Fatalf("合法路径被拒：%v", err)
	}

	// 这条路径会被拼进后续的 shell 命令，任何能改变命令结构的字符都必须挡住。
	bad := []string{
		"",
		"/org/freedesktop/NetworkManager/Checkpoint/",
		"/org/freedesktop/NetworkManager/Devices/1",
		"/org/freedesktop/NetworkManager/Checkpoint/1; reboot",
		"/org/freedesktop/NetworkManager/Checkpoint/1'",
		"/org/freedesktop/NetworkManager/Checkpoint/1$(id)",
		"/tmp/Checkpoint/1",
	}
	for _, path := range bad {
		if _, err := NormalizeCheckpointPath(path); err == nil {
			t.Fatalf("非法路径 %q 未被拒绝", path)
		}
	}
}

func TestCheckpointCommands(t *testing.T) {
	create := CheckpointCreateCommand(120)
	// 两条客户端路径都要在：busctl 属于 systemd，服务器上几乎必然存在；
	// gdbus 来自 glib2，是精简系统里的兜底。
	if !strings.Contains(create, "busctl call") || !strings.Contains(create, "gdbus call") {
		t.Fatalf("创建命令缺少客户端分支：%s", create)
	}
	if !strings.Contains(create, "CheckpointCreate aouu 0 120 6") {
		t.Fatalf("创建命令的参数不对（超时 120、flags 应为 DELETE_NEW_CONNECTIONS|DISCONNECT_NEW_DEVICES=6）：%s", create)
	}

	const path = "/org/freedesktop/NetworkManager/Checkpoint/3"
	destroy, err := CheckpointDestroyCommand(path)
	if err != nil {
		t.Fatalf("生成销毁命令失败：%v", err)
	}
	if !strings.Contains(destroy, "CheckpointDestroy o '"+path+"'") {
		t.Fatalf("销毁命令未引用检查点：%s", destroy)
	}

	rollback, err := CheckpointRollbackCommand(path)
	if err != nil {
		t.Fatalf("生成回滚命令失败：%v", err)
	}
	if !strings.Contains(rollback, "CheckpointRollback o '"+path+"'") {
		t.Fatalf("回滚命令未引用检查点：%s", rollback)
	}

	if _, err := CheckpointRollbackCommand("/tmp/evil; reboot"); err == nil {
		t.Fatal("非法检查点路径应当被拒绝")
	}
}
