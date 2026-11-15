package sysinfo

import (
	"errors"
	"fmt"
	"strings"
)

// NetworkManager 检查点相关的 D-Bus 坐标。
//
// 这里必须走 D-Bus 而不是 nmcli：nmcli 唯一与检查点有关的入口是
// `nmcli device checkpoint`，它会执行命令后停下来等人在终端上敲 "Yes"，
// 是给人用的交互式命令，没有可供程序调用的形式。
// 而检查点的创建/销毁/回滚在 NM 的 D-Bus 接口上是三个明确的非交互方法。
const (
	nmBusName        = "org.freedesktop.NetworkManager"
	nmBusPath        = "/org/freedesktop/NetworkManager"
	nmBusInterface   = "org.freedesktop.NetworkManager"
	checkpointPrefix = "/org/freedesktop/NetworkManager/Checkpoint/"
)

// checkpointFlags 是创建检查点时要求的行为。
//
// DELETE_NEW_CONNECTIONS(2)：回滚时删掉检查点之后新建的连接。
// DISCONNECT_NEW_DEVICES(4)：回滚时断开新出现的设备。
//
// 两个都必须带上。变更里包含"新建网桥"这类动作时，只回滚运行态是不够的 ——
// 新建的 br0 连接会作为一份无效配置永远留在机器上，用户之后还得手工去删。
const checkpointFlags = 2 | 4

// CheckpointCreateCommand 生成创建回滚检查点的命令。
//
// 优先 busctl（systemd 自带，服务器上几乎必然存在），回退到 gdbus（glib2）。
// 两者都没有时明确报错而不是静默跳过 —— 没有检查点就没有回滚能力，
// 此时继续下发变更正是最危险的做法。
func CheckpointCreateCommand(timeoutSec int) string {
	return fmt.Sprintf(`if command -v busctl >/dev/null 2>&1; then
  busctl call %s %s %s CheckpointCreate aouu 0 %d %d
elif command -v gdbus >/dev/null 2>&1; then
  gdbus call --system --dest %s --object-path %s --method %s.CheckpointCreate '[]' %d %d
else
  echo "目标主机上既没有 busctl 也没有 gdbus，无法创建回滚检查点" >&2
  exit 1
fi`,
		nmBusName, nmBusPath, nmBusInterface, timeoutSec, checkpointFlags,
		nmBusName, nmBusPath, nmBusInterface, timeoutSec, checkpointFlags)
}

// CheckpointDestroyCommand 生成销毁检查点的命令，即"确认变更"。
func CheckpointDestroyCommand(path string) (string, error) {
	quoted, err := quoteCheckpointPath(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`if command -v busctl >/dev/null 2>&1; then
  busctl call %s %s %s CheckpointDestroy o %s
elif command -v gdbus >/dev/null 2>&1; then
  gdbus call --system --dest %s --object-path %s --method %s.CheckpointDestroy %s
else
  echo "目标主机上既没有 busctl 也没有 gdbus" >&2
  exit 1
fi`,
		nmBusName, nmBusPath, nmBusInterface, quoted,
		nmBusName, nmBusPath, nmBusInterface, quoted), nil
}

// CheckpointRollbackCommand 生成回滚检查点的命令。
func CheckpointRollbackCommand(path string) (string, error) {
	quoted, err := quoteCheckpointPath(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`if command -v busctl >/dev/null 2>&1; then
  busctl call %s %s %s CheckpointRollback o %s
elif command -v gdbus >/dev/null 2>&1; then
  gdbus call --system --dest %s --object-path %s --method %s.CheckpointRollback %s
else
  echo "目标主机上既没有 busctl 也没有 gdbus" >&2
  exit 1
fi`,
		nmBusName, nmBusPath, nmBusInterface, quoted,
		nmBusName, nmBusPath, nmBusInterface, quoted), nil
}

// quoteCheckpointPath 校验并引用检查点路径。
//
// 这个路径来自命令输出，随后会被拼进另一条 shell 命令，因此必须当成
// 不可信输入对待：先确认它确实是 NM 的检查点路径，再用单引号包起来。
func quoteCheckpointPath(path string) (string, error) {
	if _, err := NormalizeCheckpointPath(path); err != nil {
		return "", err
	}
	return shellQuote(path), nil
}

// NormalizeCheckpointPath 校验一个 D-Bus 对象路径是不是合法的检查点路径。
func NormalizeCheckpointPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, checkpointPrefix) {
		return "", fmt.Errorf("%q 不是 NetworkManager 的检查点路径", path)
	}
	suffix := path[len(checkpointPrefix):]
	if suffix == "" || len(suffix) > 64 {
		return "", errors.New("检查点路径长度不合法")
	}
	// D-Bus 对象路径由 [A-Za-z0-9_] 组成，这里按同一字符集校验，
	// 顺带挡掉引号、分号、反引号这些能改变命令结构的字符。
	for i := 0; i < len(suffix); i++ {
		c := suffix[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '_':
		default:
			return "", fmt.Errorf("检查点路径 %q 含非法字符", path)
		}
	}
	return path, nil
}

// ParseCheckpointPath 从 D-Bus 客户端的输出里取出检查点路径。
//
// busctl 输出 `o "/org/freedesktop/NetworkManager/Checkpoint/5"`，
// gdbus 输出 `(objectpath '/org/freedesktop/NetworkManager/Checkpoint/5',)`。
// 两者都带引号但引号种类相同位置不同，与其分别解析，不如直接按路径的
// 字符集从头扫到尾 —— 这个字符集里不可能出现引号或空格，扫到即止。
func ParseCheckpointPath(output string) string {
	idx := strings.Index(output, checkpointPrefix)
	if idx < 0 {
		return ""
	}
	rest := output[idx:]
	end := 0
	for end < len(rest) {
		c := rest[end]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '/' || c == '_':
		default:
			return rest[:end]
		}
		end++
	}
	return rest
}
