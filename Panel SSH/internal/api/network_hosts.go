package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"panelssh/internal/sysinfo"
)

// 目标机 /etc/hosts 的编辑。
//
// 与网络变更（handleNetworkApply）刻意分开：改 hosts 不会切断面板赖以操作的
// SSH，写下去的又是面板算好的整份文本，因此不需要检查点、确认与回滚那一套。
// 唯一必须守住的是并发：界面上改一条记录不可能把整份文件发上来，所以用
// "提交方读到的摘要"做乐观锁（见 sysinfo.HostsDigest），而不是拿旧内容去覆盖。

// handleHostsChange 修改目标机的 /etc/hosts。
//
// 顺序是刻意的：重读原文 → 比对摘要 → 面板侧算好新内容 → 写入 → 复核。
// 任何一步都不允许"基于旧内容继续往下走"：那正是这类功能最容易造成的
// 静默破坏 —— 把别人刚加进去的记录整段盖掉，还没有任何提示。
func (s *Server) handleHostsChange(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	// 宿主机就是面板自己所在的系统，与存储、网络变更同一道边界：面板对它只读。
	if s.rejectLocalHostChange(w, hostID, "Hosts 编辑") {
		return
	}

	var req sysinfo.HostChangeRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}

	// 提不了权的机器提前说清楚：写 /etc/hosts 必须 root。否则用户点保存、
	// 等一圈 SSH，最后拿到的只是一句权限错误，而该做什么他并不知道。
	profile, err := s.hostProfile(r.Context(), hostID, false)
	if err != nil {
		s.writeHostError(w, err, "读取主机信息失败")
		return
	}
	if !profile.CanPrivileged() {
		writeErr(w, http.StatusBadRequest,
			"该主机既非 root 登录，也没有可用的提权路径（sudo/su），无法修改 "+sysinfo.HostsPath)
		return
	}

	original, err := s.readHosts(r.Context(), hostID)
	if err != nil {
		s.writeHostError(w, err, "读取 Hosts 失败")
		return
	}
	if sysinfo.HostsDigest(original) != req.HostsMd5 {
		writeErr(w, http.StatusConflict, "Hosts 文件已被他人修改，请刷新后重试")
		return
	}

	updated, err := sysinfo.ApplyHostChange(original, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// 备份必须落在写入之前：写入失败时它保住的是一份与目标机一致的原文，
	// 而写完之后再备份，保住的已经是改过的那一份了。
	if err := s.backupHosts(hostID, original); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存变更前备份失败："+err.Error())
		return
	}

	if err := s.writeFileAsRoot(r.Context(), hostID, sysinfo.HostsPath, []byte(updated)); err != nil {
		s.writeHostError(w, err, "修改 Hosts 失败")
		return
	}

	// 快照里存着改之前的 hosts，不作废的话界面刷新后还是老样子。
	s.forgetNetwork(hostID)

	// 复核用摘要逐字节比对：写下去的内容是面板算好的整份文本，读回来不一致
	// 就说明落盘的并不是我们算出来的那一份（提权路径截断、磁盘出错、别的
	// 进程同时写）。这时必须说"没验上"，而不是回一句成功。
	after, err := s.readHosts(r.Context(), hostID)
	switch {
	case err != nil:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"verified":   false,
			"verifyNote": "修改已写入，但复核时读不到 Hosts 文件：" + err.Error(),
		})
	case sysinfo.HostsDigest(after) != sysinfo.HostsDigest(updated):
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"verified":   false,
			"verifyNote": "修改已写入，但复核发现文件内容与预期不一致，请检查 " + sysinfo.HostsPath,
		})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verified": true})
	}
}

// readHosts 读目标机的 /etc/hosts 原文。
//
// 用普通用户读（hosts 文件人人可读），只有写才需要提权：读也要提权的话，
// 每次刷新页面都会多一次提权，而提权在部分机器上是要口令的。
func (s *Server) readHosts(ctx context.Context, hostID int64) (string, error) {
	res, err := s.execOnHost(ctx, hostID, "cat "+sysinfo.HostsPath, commandTimeout)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("命令以退出码 %d 结束", res.ExitCode)
		}
		return "", errors.New("读取失败：" + detail)
	}
	return res.Stdout, nil
}

// backupHosts 把变更前的原文存到面板侧。
//
// 存面板侧而不是目标机：目标机上多出来的 /etc/hosts.panelssh.bak 会被用户看见、
// 被配置管理工具当成一份待纳管的文件，而这份备份只对面板有意义。
// 固定文件名、每次覆盖：要的是"最近一次改动之前是什么样"，不是审计流水，
// 带时间戳只会让这个目录随时间无限增长。
func (s *Server) backupHosts(hostID int64, content string) error {
	dir := filepath.Join(s.cfg.DataDir, "hosts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, fmt.Sprintf("host-%d.before", hostID)), []byte(content), 0o600)
}
