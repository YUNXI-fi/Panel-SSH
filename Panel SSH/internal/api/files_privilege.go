package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"panelssh/internal/env"
	"panelssh/internal/secretbox"
)

// 文件管理里的"按需提权"。
//
// 与其他模块不同，这里**不能提前判定**一条操作需不需要 root：那取决于目标文件
// 的属主与权限，而面板手上只有 mode、没有属主（SFTP stat 也没取）。因此判据是
// "先按登录用户做，被权限拒绝再用 root 重做一次"。
//
// 这么做在文件操作上是安全的：权限不足时 open/unlink/rename 都是原子失败，
// 不存在"改到一半"的中间态。
//
// 更要紧的是它避免了反向的副作用：若无脑提权，用户在自己目录里新建的文件会变成
// root 属主，之后他自己都改不了 —— 一个"方便"的功能换来一堆新麻烦。

// fileEscalateTimeout 覆盖大文件的特权拷贝（上传先落临时位置，再由 root 搬到目标）。
const fileEscalateTimeout = 2 * time.Minute

// isPermissionDenied 判断这个 SFTP 错误是不是"权限不足"。
//
// 只有权限类失败才值得用 root 重做一次：磁盘满、路径不存在再提权也不会成功，
// 白跑一次还会把真正的错误盖成提权错误。
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sftp.ErrSSHFxPermissionDenied) {
		return true
	}
	// 少数 sftp 服务端不做 errno 归一化，把 EACCES 归到通用失败里，
	// 这时只能看文案兜底。
	return strings.Contains(strings.ToLower(err.Error()), "permission denied")
}

// shellQuotePath 把路径安全地拼进 shell 命令。
//
// 文件名里带单引号是合法的；直接拼接会截断参数、把后半段当成命令执行 ——
// 而这条命令是以 root 跑的，绝不能出这种错。
func shellQuotePath(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// writeFileAsRoot 以 root 把内容写进目标文件。
//
// 用重定向就地截断，而不是"先删再建"或"临时文件 + mv"：就地写保留目标文件原有
// 的属主与权限位（/etc 下多是 0600/0640），否则一次保存就会把 sudoers 的权限
// 改成默认值 —— 这正是 SFTP 路径上已经踩过一次的坑。
//
// 收 []byte 而不是 string：编辑器保存时已经按文件原编码编码过，这里不能再
// 假定内容是 UTF-8。
func (s *Server) writeFileAsRoot(ctx context.Context, hostID int64, target string, content []byte) error {
	encoded := base64.StdEncoding.EncodeToString(content)
	script := "base64 -d > " + shellQuotePath(target) +
		" <<'PANELSSH_FILE_EOF'\n" + encoded + "\nPANELSSH_FILE_EOF\n"
	return s.runEscalatedFileStep(ctx, hostID, script, fileEscalateTimeout, "写入文件")
}

// mkdirAsRoot 以 root 创建目录。
func (s *Server) mkdirAsRoot(ctx context.Context, hostID int64, target string) error {
	return s.runEscalatedFileStep(ctx, hostID,
		"mkdir -p "+shellQuotePath(target), fileEscalateTimeout, "创建目录")
}

// renameAsRoot 以 root 移动或重命名。
//
// 用 mv 而不是 cp+rm：跨文件系统时 mv 自己会退化成拷贝加删除，同一份逻辑
// 不必在这里再判一次。
func (s *Server) renameAsRoot(ctx context.Context, hostID int64, from, to string) error {
	return s.runEscalatedFileStep(ctx, hostID,
		"mv -f -- "+shellQuotePath(from)+" "+shellQuotePath(to), fileEscalateTimeout, "重命名")
}

// removeAsRoot 以 root 删除文件或空目录。
//
// 目录只走 rmdir：递归删除不可撤销，与 SFTP 路径保持同一条边界。
func (s *Server) removeAsRoot(ctx context.Context, hostID int64, target string, isDir bool) error {
	command := "rm -f -- " + shellQuotePath(target)
	if isDir {
		command = "rmdir -- " + shellQuotePath(target)
	}
	return s.runEscalatedFileStep(ctx, hostID, command, fileEscalateTimeout, "删除")
}

// copyUploadAsRoot 把已上传到临时位置的文件以 root 落到目标路径。
//
// tmp 在 /tmp 下（登录用户可写），目标是系统目录里只有 root 能写的位置。
func (s *Server) copyUploadAsRoot(ctx context.Context, hostID int64, tmp, target string) error {
	// cp 就地截断会保留目标已有文件的属主与权限；目标不存在时按临时文件的权限
	// 创建，属主是 root —— 在系统目录里这正是期望的结果。
	script := "cp -f -- " + shellQuotePath(tmp) + " " + shellQuotePath(target) + "\n" +
		"rm -f -- " + shellQuotePath(tmp) + "\n"
	return s.runEscalatedFileStep(ctx, hostID, script, fileEscalateTimeout, "保存上传文件")
}

// uploadTempPath 给"先传临时位置"的上传挑一个目标机上的临时文件名。
func uploadTempPath() (string, error) {
	token, err := secretbox.RandomToken(16)
	if err != nil {
		return "", err
	}
	return "/tmp/panelssh-upload-" + token, nil
}

// chmodAsRoot 以 root 修改权限。
//
// mode 已经过 parseOctalMode 校验，只可能是八进制数字串，可以直接拼进命令。
func (s *Server) chmodAsRoot(ctx context.Context, hostID int64, target, mode string) error {
	return s.runEscalatedFileStep(ctx, hostID,
		"chmod "+mode+" -- "+shellQuotePath(target), fileEscalateTimeout, "修改权限")
}

// isPermissionDeniedText 判断一段命令输出是不是"权限不足"。
//
// 与 isPermissionDenied 分开：那个认的是 SFTP 的错误类型，这里认的是
// 远端 shell 写在 stderr 上的文案，两者拿不到同一个东西。
func isPermissionDeniedText(text string) bool {
	return strings.Contains(strings.ToLower(text), "permission denied")
}

// runFileStep 先以登录用户跑一步文件命令，只有确实被权限拒绝才用 root 重做。
//
// 为什么不直接用 runEscalatedFileStep：那条路是**无条件以 root 执行**的，
// 而复制、移动、压缩这些操作多数发生在用户自己的目录里。无脑提权会让新建出来
// 的文件变成 root 属主，用户之后连改都改不了 —— 见本文件开头的那段说明。
// 所以顺序必须是"先自己试，试不动再提权"，与 mkdir/rename/remove 的判据保持一致。
func (s *Server) runFileStep(ctx context.Context, hostID int64, script, what string) error {
	res, err := s.execOnHost(ctx, hostID, script, fileEscalateTimeout)
	if err != nil {
		return errors.New(what + "失败：" + err.Error())
	}
	if res.ExitCode == 0 {
		return nil
	}

	detail := strings.TrimSpace(res.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(res.Stdout)
	}
	// 非权限失败再提权也不会成功（磁盘满、命令不存在、目标已被占用），
	// 白跑一次还会把真正的原因盖成提权错误。
	if !isPermissionDeniedText(detail) {
		if detail == "" {
			detail = fmt.Sprintf("命令以退出码 %d 结束", res.ExitCode)
		}
		return errors.New(what + "失败：" + detail)
	}
	return s.runEscalatedFileStep(ctx, hostID, script, fileEscalateTimeout, what)
}

// runEscalatedFileStep 跑一步特权文件操作，并把失败翻译成用户能懂的话。
//
// 走到这里说明普通用户已经被权限拒绝了；如果这台机器又提不了权，用户需要知道
// 的是"为什么改不动、该怎么配"，而不是一句原始 errno。
func (s *Server) runEscalatedFileStep(ctx context.Context, hostID int64, script string, timeout time.Duration, what string) error {
	_, err := s.runEscalated(ctx, hostID, script, timeout, escalateRequired)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, env.ErrNoPrivilege):
		// 原样透出"提不了权"这个结论：调用方的错误前缀已经说明了是哪个操作，
		// 再包一层只会得到"删除失败：删除…"这种叠字。
		return err
	default:
		return errors.New(what + "失败：" + err.Error())
	}
}
