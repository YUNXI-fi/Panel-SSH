//go:build !linux

package localterm

import "errors"

// Session 在非 Linux 平台上是占位类型：面板的本地终端依赖 Linux 的
// /dev/ptmx 与 TIOCSPTLCK，其它平台没有等价物时直接拒绝，而不是假装成功。
type Session struct{}

// Start 在非 Linux 平台返回错误。
func Start(cols, rows int) (*Session, error) {
	return nil, errors.New("当前平台不支持本地终端")
}
