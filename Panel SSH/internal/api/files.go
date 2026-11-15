package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"

	"panelssh/internal/sshclient"
	"panelssh/internal/store"
)

const (
	// dialTimeout 是复用到新目标时的 SSH 握手上限。
	dialTimeout = 15 * time.Second
	// maxTextEditSize 是允许在浏览器内编辑的文件上限。
	// 超过这个体积用文本编辑器打开并不现实，直接拒绝好过让浏览器卡死。
	maxTextEditSize = 1 << 20 // 1 MiB
	// maxUploadSize 是单次上传请求的体积上限。
	maxUploadSize = 512 << 20 // 512 MiB
	// linkResolveWorkers 是解析符号链接真实类型的并发上限。
	// 详见 handleListFiles 中的说明。
	linkResolveWorkers = 8
)

type fileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"modTime"`
	IsLink  bool      `json:"isLink"`
}

type listResponse struct {
	Path    string      `json:"path"`
	Parent  string      `json:"parent"`
	Entries []fileEntry `json:"entries"`
}

type pathRequest struct {
	Path string `json:"path"`
}

type renameRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type deleteRequest struct {
	Paths []string `json:"paths"`
}

type writeTextRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Encoding 是读取时识别出的原编码（见 text_encoding.go）。保存要按它写回：
	// 把 GBK 的 CSV 改一行再保存，编码不能悄悄变成 UTF-8。
	Encoding string `json:"encoding"`
}

type chmodRequest struct {
	Paths []string `json:"paths"`
	// Mode 是八进制字符串（如 "755"）。用字符串而不是数字：八进制在 JSON 里
	// 没有对应写法，用户输入的 "0755" 一旦被前端当成十进制解析就变成别的权限。
	Mode string `json:"mode"`
}

// transferRequest 是复制与移动共用的入参：把 Paths 里的条目搬进 Target 目录。
type transferRequest struct {
	Paths  []string `json:"paths"`
	Target string   `json:"target"`
}

type compressRequest struct {
	Path string `json:"path"`
	// Name 是压缩包文件名（含扩展名），落在被压缩条目的同级目录下。
	Name string `json:"name"`
	// Format 取 tar.gz 或 zip。
	Format string `json:"format"`
}

type extractRequest struct {
	Path string `json:"path"`
}

// parseOctalMode 解析用户输入的权限值。
//
// 只收 3-4 位八进制：3 位是常规权限，4 位用来表达 setuid/setgid/sticky。
// 解析出来的 FileMode 直接交给 SFTP，字符串形式交给 chmod 命令。
func parseOctalMode(raw string) (os.FileMode, string, bool) {
	text := strings.TrimSpace(raw)
	if len(text) < 3 || len(text) > 4 {
		return 0, "", false
	}
	value, err := strconv.ParseUint(text, 8, 32)
	if err != nil || value > 0o7777 {
		return 0, "", false
	}
	return os.FileMode(value), text, true
}

// requireSingleName 校验一个"只能当文件名用"的输入。
//
// 压缩包名字来自用户输入，必须挡住路径分隔符：否则可以把产物写到目录之外，
// 或者借 ".." 覆盖别处的东西。
//
// 还要挡住以 - 开头的名字：打包用的 Info-ZIP zip 不认 -- 这个选项分隔符
// （会直接报 "can't use -- before archive name"），名字只能紧跟在选项后面，
// 于是 "-x" 这样的名字会被当成选项执行。
func requireSingleName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || name == "." || name == ".." {
		return "", false
	}
	if strings.ContainsAny(name, "/\x00") || strings.HasPrefix(name, "-") {
		return "", false
	}
	return name, true
}

// requireTargetDir 校验复制/移动的目标目录。
func (s *Server) requireTargetDir(w http.ResponseWriter, client *sftpGuard, raw string) (string, bool) {
	dir, ok := requireAbsPath(w, raw)
	if !ok {
		return "", false
	}
	info, err := client.Stat(dir)
	if err != nil {
		writeErr(w, http.StatusNotFound, "目标目录不存在："+dir)
		return "", false
	}
	if !info.IsDir() {
		writeErr(w, http.StatusBadRequest, "目标不是目录："+dir)
		return "", false
	}
	return dir, true
}

// leaseHost 借用某主机的 SSH 连接，调用方必须 Release。
//
// 宿主机没有 SSH 连接可借：终端与批量执行走这里，于是"宿主机没有这些能力"
// 这条边界只在这一个地方成立，不必在每个接口里各判一次。命令执行、只读采集
// 与文件管理已经能在宿主机上跑（见 execOnHost / runOnHost / openSFTP），
// 不再走这里。
func (s *Server) leaseHost(hostID int64) (*sshclient.Lease, error) {
	if hostID == LocalHostID {
		return nil, errLocalHostUnsupported
	}
	host, err := s.store.GetHost(hostID)
	if err != nil {
		return nil, err
	}
	secret, err := s.hostSecret(hostID)
	if err != nil {
		return nil, err
	}
	return s.pool.Acquire(hostID, sshclient.Target{
		Addr:     host.Addr,
		Port:     host.Port,
		Username: host.Username,
		AuthType: host.AuthType,
		Secret:   secret,
	}, dialTimeout)
}

// openSFTP 为指定主机建立一次文件操作会话；失败时已写好 HTTP 响应。
// 调用方必须 defer sess.Close()。
//
// 宿主机不走 SSH：它在进程内起一份 SFTP 服务端（见 sftp_local.go），
// 与远端共用同一套接口、超时与提权重试逻辑。
//
// 远端这里显式持有 ssh session 与 sftp client 两者：sftp.NewClient(sshClient)
// 内部另建的 session 不会随 client.Close() 关闭，会把回收责任推给对端。
func (s *Server) openSFTP(w http.ResponseWriter, hostID int64) (*sftpSession, *sftpGuard, bool) {
	if hostID == LocalHostID {
		sess, guard, err := newLocalSFTP()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "打开本机文件系统失败："+err.Error())
			return nil, nil, false
		}
		return sess, guard, true
	}

	lease, err := s.leaseHost(hostID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "主机不存在")
		default:
			writeErr(w, http.StatusBadGateway, "SSH 连接失败："+err.Error())
		}
		return nil, nil, false
	}

	session, err := lease.Client.NewSession()
	if err != nil {
		lease.Release()
		writeErr(w, http.StatusBadGateway, "创建 SSH 会话失败："+err.Error())
		return nil, nil, false
	}

	if err := session.RequestSubsystem("sftp"); err != nil {
		_ = session.Close()
		lease.Release()
		writeErr(w, http.StatusBadGateway, "目标主机不支持 SFTP 子系统："+err.Error())
		return nil, nil, false
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		lease.Release()
		writeErr(w, http.StatusBadGateway, "获取会话输出失败："+err.Error())
		return nil, nil, false
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		lease.Release()
		writeErr(w, http.StatusBadGateway, "获取会话输入失败："+err.Error())
		return nil, nil, false
	}

	client, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		_ = session.Close()
		lease.Release()
		writeErr(w, http.StatusBadGateway, "初始化 SFTP 失败："+err.Error())
		return nil, nil, false
	}

	sess := &sftpSession{client: client, session: session, lease: lease}
	return sess, &sftpGuard{client: client}, true
}

// requireAbsPath 校验并归一化远端绝对路径。
func requireAbsPath(w http.ResponseWriter, raw string) (string, bool) {
	if raw == "" || !path.IsAbs(raw) {
		writeErr(w, http.StatusBadRequest, "路径必须是以 / 开头的绝对路径")
		return "", false
	}
	return path.Clean(raw), true
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	dir, ok := requireAbsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	if dir == "." {
		dir = "/"
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	info, err := client.Stat(dir)
	if err != nil {
		if errors.Is(err, errSFTPTimeout) {
			s.writeSFTPError(w, err, "无法访问该路径")
			return
		}
		writeErr(w, http.StatusNotFound, "无法访问该路径："+err.Error())
		return
	}
	if !info.IsDir() {
		writeErr(w, http.StatusBadRequest, "目标不是目录")
		return
	}

	items, err := client.ReadDir(dir)
	if err != nil {
		s.writeSFTPError(w, err, "读取目录失败")
		return
	}

	entries := make([]fileEntry, 0, len(items))
	for _, item := range items {
		entries = append(entries, fileEntry{
			Name:    item.Name(),
			Path:    path.Join(dir, item.Name()),
			IsDir:   item.IsDir(),
			Size:    item.Size(),
			Mode:    item.Mode().String(),
			ModTime: item.ModTime(),
			IsLink:  item.Mode()&os.ModeSymlink != 0,
		})
	}

	// 解析符号链接的真实类型。
	//
	// ReadDir 只给出链接自身的信息，指向目录的链接会被误判成普通文件而无法进入，
	// 因此必须逐个 Stat —— 这是 N 次网络往返。串行执行时，含上百个链接的目录
	// （/usr/lib、/etc/rc*.d 这类）会让一次列目录挂上数秒。
	// 用有界并发把总耗时压到接近一次往返；上限设为 8 是为了不把目标机的
	// sshd 在瞬间打出一大批并发请求。
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < linkResolveWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				resolved, statErr := client.Stat(entries[idx].Path)
				if statErr != nil {
					continue // 断链等情况保持原样：按普通条目展示
				}
				entries[idx].IsDir = resolved.IsDir()
				entries[idx].Size = resolved.Size()
			}
		}()
	}
	for i := range entries {
		if entries[i].IsLink {
			jobs <- i
		}
	}
	close(jobs)
	wg.Wait()

	// 目录优先，其次按名称不区分大小写排序。
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	parent := path.Dir(dir)
	if parent == dir {
		parent = "" // 已处于根目录
	}

	writeJSON(w, http.StatusOK, listResponse{Path: dir, Parent: parent, Entries: entries})
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	target, ok := requireAbsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	info, err := client.Stat(target)
	if err != nil {
		if errors.Is(err, errSFTPTimeout) {
			s.writeSFTPError(w, err, "读取文件信息失败")
			return
		}
		writeErr(w, http.StatusNotFound, "文件不存在："+err.Error())
		return
	}
	if info.IsDir() {
		writeErr(w, http.StatusBadRequest, "不支持直接下载目录")
		return
	}

	file, err := client.Open(target)
	if err != nil {
		s.writeSFTPError(w, err, "打开文件失败")
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", contentDisposition(path.Base(target)))

	// 大文件下载按停滞而非总时长判定失败：只要字节还在流动就一直传，
	// 一旦两侧都没有进展就中断，避免死链路把会话与连接一直占住。
	guard := newStallGuard(sftpStallTimeout, func() { _ = sess.client.Close() })
	defer guard.Release()

	// 任务在拷贝之前登记：响应头已经发出去了，此后的成败只能由记录来体现 ——
	// 传输中断时界面上不会有任何提示，用户事后只能来这里看结论。
	task := s.beginFileTask(hostID, store.TaskKindDownload, "下载 "+target, "")
	size, copyErr := io.Copy(progressWriter{w: w, g: guard}, progressReader{r: file, g: guard})
	if copyErr != nil {
		// 客户端主动关掉与链路停滞在这里是同一个错误，一律记成中断：
		// 报成"下载失败"会把用户自己取消也算成故障。
		task.addItem(target, size, "", copyErr.Error())
		task.abort()
		return
	}
	task.addItem(target, size, "", "")
	task.finish("")
}

// contentDisposition 生成下载头。非 ASCII 文件名需要 RFC 5987 形式，
// 否则中文名会被浏览器丢弃或乱码。
func contentDisposition(name string) string {
	if isASCII(name) {
		return fmt.Sprintf("attachment; filename=%q", name)
	}
	return "attachment; filename*=UTF-8''" + url.PathEscape(name)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (s *Server) handleReadText(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	target, ok := requireAbsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	info, err := client.Stat(target)
	if err != nil {
		if errors.Is(err, errSFTPTimeout) {
			s.writeSFTPError(w, err, "读取文件信息失败")
			return
		}
		writeErr(w, http.StatusNotFound, "文件不存在："+err.Error())
		return
	}
	if info.IsDir() {
		writeErr(w, http.StatusBadRequest, "目标是目录，无法作为文本打开")
		return
	}
	if info.Size() > maxTextEditSize {
		// 预览端会把这个 413 当成"改用分块读取"的信号；在线编辑仍受这个上限约束。
		writeErr(w, http.StatusRequestEntityTooLarge, "文件超过 1 MiB，无法一次性读取")
		return
	}

	file, err := client.Open(target)
	if err != nil {
		s.writeSFTPError(w, err, "打开文件失败")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxTextEditSize))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取文件失败："+err.Error())
		return
	}

	// 统一在服务端识别编码并转成 UTF-8：接口是 JSON，非法 UTF-8 字节在序列化时
	// 就会被替换成 U+FFFD，前端再转码也救不回来。Excel 导出的中文 CSV 通常是
	// GB18030 或 UTF-16，这一步决定了它们能不能正常显示。
	text, encoding, ok := decodeText(data)
	if !ok {
		writeErr(w, http.StatusUnsupportedMediaType, "疑似二进制文件，无法在线编辑")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"path":     target,
		"content":  text,
		"size":     len(data),
		"mode":     info.Mode().String(),
		"modTime":  info.ModTime(),
		"encoding": encoding,
	})
}

func (s *Server) handleWriteText(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}

	var req writeTextRequest
	if err := decodeJSONBody(w, r, &req, maxTextEditSize+4096); err != nil {
		return
	}
	target, ok := requireAbsPath(w, req.Path)
	if !ok {
		return
	}
	if len(req.Content) > maxTextEditSize {
		writeErr(w, http.StatusRequestEntityTooLarge, "内容超过 1 MiB，无法在线保存")
		return
	}

	// 按读取时识别出的编码写回。编码名不合法时 encodeText 会落到 UTF-8，
	// 与改动前一致；这里只处理转码本身失败（GB18030 编不出来的字符等）。
	payload, err := encodeText(req.Content, req.Encoding)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "按原编码 "+req.Encoding+" 保存失败："+err.Error())
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	// 先记住原权限位。SFTP 的 Create 是 O_TRUNC 重写，且没有传 mode 的入口，
	// 结果是把 0600 的配置文件、0755 的脚本一律写成默认权限 ——
	// 用户只是改一行内容，却顺手把 sudoers 的权限改了，这是会出事的。
	var originalMode os.FileMode
	existed := false
	if info, err := client.Stat(target); err == nil {
		if info.IsDir() {
			writeErr(w, http.StatusBadRequest, "目标是目录，无法写入")
			return
		}
		originalMode = info.Mode().Perm()
		existed = true
	}

	file, err := client.Create(target)
	if err != nil {
		if !isPermissionDenied(err) {
			s.writeSFTPError(w, err, "打开远端文件失败")
			return
		}
		// 被权限拒绝（/etc 下的配置多是这样）：改用 root 就地写。
		// 就地截断同样保留属主与权限位，所以不必再补一次 Chmod。
		if rootErr := s.writeFileAsRoot(r.Context(), hostID, target, payload); rootErr != nil {
			s.writeHostError(w, rootErr, "保存失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": target})
		return
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		writeErr(w, http.StatusInternalServerError, "写入失败："+err.Error())
		return
	}
	if err := file.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, "关闭远端文件失败："+err.Error())
		return
	}

	// 还原权限位。放在写完之后：先 Chmod 再写会被后续的 Create 覆盖。
	if existed {
		if err := client.Chmod(target, originalMode); err != nil {
			// 内容已经写成功，此时报错会让用户误以为没保存，因此只提示权限未还原。
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":      true,
				"path":    target,
				"warning": "内容已保存，但权限位未能还原为 " + originalMode.String() + "：" + err.Error(),
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": target})
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req pathRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	target, ok := requireAbsPath(w, req.Path)
	if !ok {
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	// MkdirAll 对已存在的目录不报错，符合"确保存在"的语义。
	if err := client.MkdirAll(target); err != nil {
		if !isPermissionDenied(err) {
			s.writeSFTPError(w, err, "创建目录失败")
			return
		}
		if rootErr := s.mkdirAsRoot(r.Context(), hostID, target); rootErr != nil {
			s.writeHostError(w, rootErr, "创建目录失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": target})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req renameRequest
	if err := decodeJSONBody(w, r, &req, 1<<16); err != nil {
		return
	}
	from, ok := requireAbsPath(w, req.From)
	if !ok {
		return
	}
	to, ok := requireAbsPath(w, req.To)
	if !ok {
		return
	}
	if from == "/" || to == "/" {
		writeErr(w, http.StatusBadRequest, "不允许操作根目录")
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	if _, err := client.Stat(to); err == nil {
		writeErr(w, http.StatusConflict, "目标已存在，请换一个名称")
		return
	}
	if err := client.Rename(from, to); err != nil {
		if !isPermissionDenied(err) {
			s.writeSFTPError(w, err, "重命名失败")
			return
		}
		if rootErr := s.renameAsRoot(r.Context(), hostID, from, to); rootErr != nil {
			s.writeHostError(w, rootErr, "重命名失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": to})
}

func (s *Server) handleDeleteFiles(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req deleteRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要删除的条目")
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	for _, raw := range req.Paths {
		target, ok := requireAbsPath(w, raw)
		if !ok {
			return
		}
		if target == "/" {
			writeErr(w, http.StatusBadRequest, "不允许删除根目录")
			return
		}

		info, err := client.Lstat(target)
		if err != nil {
			if errors.Is(err, errSFTPTimeout) {
				s.writeSFTPError(w, err, "删除失败")
				return
			}
			writeErr(w, http.StatusNotFound, "对象不存在："+target)
			return
		}

		if info.IsDir() {
			// 只删空目录：递归删除是不可逆的破坏性操作，
			// 在缺少回收站机制前不提供。
			if err := client.RemoveDirectory(target); err != nil {
				// 只有权限不足才值得用 root 重做：目录非空也会失败，
				// 但它再提权也不会成功，必须原样保留那句提示。
				if !isPermissionDenied(err) {
					writeErr(w, http.StatusConflict,
						"目录非空或无法删除："+target+"。请先清空目录内容后再删除")
					return
				}
				if rootErr := s.removeAsRoot(r.Context(), hostID, target, true); rootErr != nil {
					s.writeHostError(w, rootErr, "删除失败")
					return
				}
			}
			continue
		}
		if err := client.Remove(target); err != nil {
			if !isPermissionDenied(err) {
				s.writeSFTPError(w, err, "删除失败："+target)
				return
			}
			if rootErr := s.removeAsRoot(r.Context(), hostID, target, false); rootErr != nil {
				s.writeHostError(w, rootErr, "删除失败")
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 权限 / 传输 / 压缩 ----
//
// 下面五个操作改的都是目标机上的真实文件，且多数发生在用户自己的工作目录里，
// 因此统一走 runFileStep：先按登录用户做，确实被权限拒绝才提权。直接以 root
// 执行会让新建的文件变成 root 属主，用户之后自己都改不动。

func (s *Server) handleChmod(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req chmodRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要修改权限的条目")
		return
	}
	mode, modeText, ok := parseOctalMode(req.Mode)
	if !ok {
		writeErr(w, http.StatusBadRequest, "权限值必须是 3-4 位八进制数，例如 755")
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	for _, raw := range req.Paths {
		target, ok := requireAbsPath(w, raw)
		if !ok {
			return
		}
		// 改自己文件的权限不需要 root，SFTP 这一步会成功；只有目标是别人
		// 或系统文件时才会被拒，那时才值得提权重做。
		if err := client.Chmod(target, mode); err != nil {
			if !isPermissionDenied(err) {
				s.writeSFTPError(w, err, "修改权限失败："+target)
				return
			}
			if rootErr := s.chmodAsRoot(r.Context(), hostID, target, modeText); rootErr != nil {
				s.writeHostError(w, rootErr, "修改权限失败")
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCopyFiles(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req transferRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要复制的条目")
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	dir, ok := s.requireTargetDir(w, client, req.Target)
	if !ok {
		return
	}

	for _, raw := range req.Paths {
		src, ok := requireAbsPath(w, raw)
		if !ok {
			return
		}
		if src == "/" {
			writeErr(w, http.StatusBadRequest, "不允许复制根目录")
			return
		}

		dest := path.Join(dir, path.Base(src))
		if dest == src {
			writeErr(w, http.StatusConflict, "源与目标相同："+src)
			return
		}
		if _, err := client.Stat(dest); err == nil {
			writeErr(w, http.StatusConflict, "目标已存在："+dest)
			return
		}

		// cp -a 保留权限、属主与时间戳，-a 自带 -r，目录能整棵拷过去。
		if err := s.runFileStep(r.Context(), hostID,
			"cp -a -- "+shellQuotePath(src)+" "+shellQuotePath(dest), "复制"); err != nil {
			s.writeHostError(w, err, "复制失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMoveFiles(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req transferRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要移动的条目")
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	dir, ok := s.requireTargetDir(w, client, req.Target)
	if !ok {
		return
	}

	for _, raw := range req.Paths {
		src, ok := requireAbsPath(w, raw)
		if !ok {
			return
		}
		if src == "/" {
			writeErr(w, http.StatusBadRequest, "不允许移动根目录")
			return
		}

		dest := path.Join(dir, path.Base(src))
		// 移进它自己所在的目录等于什么都没做，直接跳过而不是报错。
		if dest == src {
			continue
		}
		if _, err := client.Stat(src); err != nil {
			writeErr(w, http.StatusNotFound, "对象不存在："+src)
			return
		}
		if _, err := client.Stat(dest); err == nil {
			writeErr(w, http.StatusConflict, "目标已存在："+dest)
			return
		}

		// 同一文件系统上 Rename 是原子的，也不必起一个 shell；跨设备时它必然失败，
		// 那时才退回 mv —— mv 自己会退化成"拷贝 + 删除"。
		if err := client.Rename(src, dest); err == nil {
			continue
		}
		if err := s.runFileStep(r.Context(), hostID,
			"mv -f -- "+shellQuotePath(src)+" "+shellQuotePath(dest), "移动"); err != nil {
			s.writeHostError(w, err, "移动失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCompress(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req compressRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}

	src, ok := requireAbsPath(w, req.Path)
	if !ok {
		return
	}
	if src == "/" {
		writeErr(w, http.StatusBadRequest, "不允许压缩根目录")
		return
	}
	name, ok := requireSingleName(req.Name)
	if !ok {
		writeErr(w, http.StatusBadRequest, "压缩包名称不能为空、包含斜杠，也不能以 - 开头")
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	if _, err := client.Stat(src); err != nil {
		writeErr(w, http.StatusNotFound, "对象不存在："+src)
		return
	}

	dir := path.Dir(src)
	dest := path.Join(dir, name)
	if _, err := client.Stat(dest); err == nil {
		writeErr(w, http.StatusConflict, "目标已存在："+dest)
		return
	}

	// 压缩包内记相对路径：先 cd 进源目录再打包，解压出来才不会多套一层目录。
	// base 前补 "./" 是防它恰好以 - 开头时被当成命令选项。
	base := "./" + path.Base(src)
	var mainCmd string
	switch req.Format {
	case "zip":
		// 不加 -q：逐条输出的文件名正是进度条的数据来源，静默打包会让面板无从计数。
		// 也不能加 --：Info-ZIP 的 zip 不认这个分隔符，会直接报
		// "can't use -- before archive name" —— 以 - 开头的名字由入口挡住。
		mainCmd = "( cd " + shellQuotePath(dir) + " && zip -r " +
			shellQuotePath(name) + " " + shellQuotePath(base) + " )"
	case "tar.gz":
		// -v 同理：tar 把每个条目名写出来，面板据此推进进度。
		mainCmd = "tar -czf " + shellQuotePath(dest) +
			" -C " + shellQuotePath(dir) + " -v -- " + shellQuotePath(base)
	default:
		writeErr(w, http.StatusBadRequest, "不支持的压缩格式："+req.Format)
		return
	}

	s.runArchiveStream(w, r, hostID,
		archiveScript("find "+shellQuotePath(src), mainCmd),
		archiveOp{what: "压缩", kind: store.TaskKindCompress, target: src, dest: dest})
}

// zipToolMissingMessage 是目标机缺 unzip 时给用户的说明。
//
// 带上主流发行版的安装命令：用户看到"没装 unzip"之后下一步就是去装，
// 让他再自己查一遍怎么装是多余的。
const zipToolMissingMessage = "目标机没有安装 unzip，无法解压 zip 包。请在目标机上安装后重试：" +
	"Debian/Ubuntu 用 apt-get install unzip，RHEL/CentOS 用 yum install unzip，Alpine 用 apk add unzip。"

// missingCommand 判断目标机上是否缺少某个命令。
//
// 用 command -v 而不是 which：前者是 POSIX 内建，最小化安装的机器上 which 自己
// 可能都没装。name 只由调用方写死，不接受用户输入。
//
// 返回的 error 表示"没探出来"（连不上等），调用方应当继续往下走而不是断言命令缺失 ——
// 把连接失败说成"没装 unzip"，用户会去装一个根本不需要装的东西。
func (s *Server) missingCommand(ctx context.Context, hostID int64, name string) (bool, error) {
	res, err := s.execOnHost(ctx, hostID, "command -v "+name, defaultExecTimeout)
	if err != nil {
		return false, err
	}
	return res.ExitCode != 0, nil
}

func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req extractRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}

	src, ok := requireAbsPath(w, req.Path)
	if !ok {
		return
	}

	// 按扩展名选解压器：tar 家族覆盖绝大多数场景，zip 需要目标机装了 unzip。
	//
	// 每种格式都要给出两条命令：一条数出总条目数（countCmd），一条带逐条输出地解压
	// （mainCmd）。tar 用 -t 列表、zip 用 unzip -Z1 只列名字，都是为了拿到
	// "总共要解出多少条"这个分母 —— 没有它，进度条只能是不确定状态。
	lower := strings.ToLower(src)
	dir := path.Dir(src)
	isZip := strings.HasSuffix(lower, ".zip")
	var countCmd, mainCmd string
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		countCmd = "tar -tzf " + shellQuotePath(src)
		mainCmd = "tar -xzvf " + shellQuotePath(src) + " -C " + shellQuotePath(dir)
	case strings.HasSuffix(lower, ".tar"):
		countCmd = "tar -tf " + shellQuotePath(src)
		mainCmd = "tar -xvf " + shellQuotePath(src) + " -C " + shellQuotePath(dir)
	case isZip:
		countCmd = "unzip -Z1 " + shellQuotePath(src)
		// 去掉 -q：与压缩那边同理，逐条输出才是进度的来源。
		mainCmd = "unzip -o " + shellQuotePath(src) + " -d " + shellQuotePath(dir)
	default:
		writeErr(w, http.StatusBadRequest, "只支持解压 .tar.gz / .tgz / .tar / .zip")
		return
	}

	// zip 先探一次 unzip 再执行。远端原本只会回一句 "unzip: command not found"，
	// 用户看不出是面板的问题还是机器缺东西，也不知道下一步该做什么 ——
	// 在真正动手之前把结论和做法一起说清楚，代价只是解压 zip 时多一次短命令。
	if isZip {
		missing, probeErr := s.missingCommand(r.Context(), hostID, "unzip")
		if probeErr != nil {
			s.writeHostError(w, probeErr, "检查目标机是否装了 unzip 失败")
			return
		}
		if missing {
			// 400 而不是 502：这不是连接故障，是目标机缺依赖，换台机器或装完即可。
			writeErr(w, http.StatusBadRequest, zipToolMissingMessage)
			return
		}
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	info, err := client.Stat(src)
	if err != nil {
		writeErr(w, http.StatusNotFound, "文件不存在："+src)
		return
	}
	if info.IsDir() {
		writeErr(w, http.StatusBadRequest, "目录不能解压："+src)
		return
	}

	// done 事件不带路径：解压的产物是"当前目录下多出来的东西"，没有单一目标可言，
	// 界面直接用当前目录来措辞。
	s.runArchiveStream(w, r, hostID, archiveScript(countCmd, mainCmd),
		archiveOp{what: "解压", kind: store.TaskKindExtract, target: src})
}

// archiveEvent 是压缩/解压过程中下发给界面的一帧。
//
// 用 NDJSON 逐帧下发而不是一次性 JSON 响应：这两个操作可能跑上几分钟，用户需要
// 看到进度条在走，而"已经走了多少"只有执行过程中才知道 —— 等命令结束再算出来，
// 进度条也就没有意义了。
//
// 代价是一旦开始下发就没有 HTTP 状态码可用（200 已经写出去了），此后的失败
// 一律用 error 帧表达，前端也必须按这个契约解析。
type archiveEvent struct {
	// Type 取 prepare / progress / escalating / done / error。
	Type string `json:"type"`
	// Total 是总条目数。为 0 表示没数出来（例如目标机的 unzip 不支持 -Z1），
	// 此时界面按"不确定进度"渲染，而不是显示一个假的 0%。
	Total int64 `json:"total,omitempty"`
	// Done 是已处理条目数。
	Done int64 `json:"done,omitempty"`
	// Path 只在 done 帧里有意义，是压缩产物的路径。
	Path string `json:"path,omitempty"`
	// Message 是 error 帧里的失败原因。
	Message string `json:"message,omitempty"`
}

// archiveTotalMarker 是统计段与执行段之间的分隔标记。
//
// 统计脚本把总条目数写在这样一行里，面板据此拿到进度的分母。协议两边必须一致，
// 所以只在这里定义一次。
const archiveTotalMarker = "PANELSSH_TOTAL "

// archiveProgressInterval 是进度帧的最小间隔。
//
// 十万个文件就有十万行输出，逐行下发意味着十万次写响应加 flush —— 带宽和主线程
// 都花在没人看得清的刷新上。150ms 一帧已经足够让进度条看起来是连续的。
const archiveProgressInterval = 150 * time.Millisecond

// archiveScript 把"统计条目数"与"执行"拼成一条脚本。
//
// 合成一条而不是跑两次连接：一次 SSH 会话的开销远大于 find 遍历本身，而两段之间
// 用一行标记分隔，面板就能在同一个输出流里既拿到分母、又拿到逐条进度。
func archiveScript(countCmd, mainCmd string) string {
	return "total=$(" + countCmd + " 2>/dev/null | wc -l)\n" +
		"printf '" + archiveTotalMarker + "%s\\n' \"$total\"\n" +
		"{ " + mainCmd + " ; } 2>&1\n"
}

// parseArchiveTotal 解析统计标记行，取出总条目数。
//
// 按前缀匹配即可：标记行是脚本自己 printf 出来的，固定以它开头；而可能的干扰
// （逐条文件名）在压缩时带 "./" 前缀、解压时以权限位列开头，都撞不上。
func parseArchiveTotal(text string) (int64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), archiveTotalMarker)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// tailOutput 截取输出的末尾用于报错。
//
// 压缩/解压的输出里绝大多数是逐条文件名，而失败原因（Permission denied、
// No space left on device）几乎总在最后几行。把几万行文件名原样抛给用户既读不懂，
// 真正的原因也会被淹没在中间。
func tailOutput(text string) string {
	const limit = 600
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	// 按字节截断可能落在多字节字符中间，先剔除无效字节再交给界面。
	text = strings.ToValidUTF8(text[len(text)-limit:], "")
	// 从第一个换行处切开，避免开头是一个被截了一半的文件名。
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return "……" + text
}

// archiveOp 描述一次压缩/解压。
//
// 打包成一个值传下去，而不是再加三个散参数：runArchiveStream 同时要负责执行、
// 报错措辞与任务记录，参数散着传很容易让某一处的措辞与另一处对不上。
type archiveOp struct {
	// what 是动作名（"压缩"/"解压"），错误信息里用它措辞。
	what string
	// kind 是任务种类，见 store.TaskKind* 常量。
	kind string
	// target 是被操作的对象，进任务记录供事后回看。
	target string
	// dest 是压缩产物的路径，只在 done 帧里有意义；解压没有单一产物，为空。
	dest string
}

// runArchiveStream 执行压缩/解压脚本，并把过程以 NDJSON 逐帧下发给界面。
//
// 与 writeHostError 那套的区别：409/500 这类状态码要求响应头还没发出去，而进度帧
// 从第一秒就开始发了。所以这里的契约是"只要开始下发就一定返回 200，真正的失败
// 放在 error 帧里"。
func (s *Server) runArchiveStream(w http.ResponseWriter, r *http.Request, hostID int64, script string, op archiveOp) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	// 让可能存在的反向代理也别缓冲：这条响应的全部价值就在"边跑边到"。
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	// 任务在动手之前就登记：一次几万文件的打包要跑很久，用户中途关掉窗口时，
	// 记录里至少要留下"有过这么一次操作"。
	task := s.beginFileTask(hostID, op.kind, op.what+" "+op.target, "")
	var failure string
	defer func() {
		switch {
		case r.Context().Err() != nil:
			// 客户端先走了：这次操作对用户已经没有意义，记成中断而不是成功，
			// 与批量执行的口径一致。
			task.abort()
		case failure != "":
			task.finish(failure)
		default:
			task.finish("")
		}
	}()

	progress := &archiveProgress{w: w, flusher: flusher, task: task}
	// 统计阶段本身可能就要几秒（解压 .tar.gz 要先读一遍整个包才知道有多少条目），
	// 期间一条输出都没有。不发这一帧，用户看到的就是进度条停在 0% 不动。
	progress.notify(archiveEvent{Type: "prepare"})

	// fail 一次决定两处的结论：界面上的错误帧与历史记录里的失败原因。
	// 合成一个动作是为了不让某条分支漏记 —— 界面上报了错而历史里显示"已完成"，
	// 会让人不再相信这份记录。
	fail := func(msg string) {
		failure = msg
		progress.notify(archiveEvent{Type: "error", Message: msg})
	}

	res, err := s.execOnHostStream(r.Context(), hostID, script, fileEscalateTimeout, progress.line)
	if err != nil {
		fail(op.what + "失败：" + err.Error())
		return
	}
	if res.ExitCode == 0 {
		progress.notify(archiveEvent{Type: "done", Path: op.dest})
		return
	}

	// 判据用的是未截断的输出：Permission denied 可能出现在开头，截尾之后就看不见了。
	rawDetail := strings.TrimSpace(res.Stderr)
	if rawDetail == "" {
		rawDetail = strings.TrimSpace(res.Stdout)
	}
	if !isPermissionDeniedText(rawDetail) {
		detail := tailOutput(rawDetail)
		if detail == "" {
			detail = fmt.Sprintf("命令以退出码 %d 结束", res.ExitCode)
		}
		fail(op.what + "失败：" + detail)
		return
	}

	// 被权限拒绝才用 root 重做一次，判据与 runFileStep 保持一致。
	// 提权走的是既有的一次性执行通道，拿不到逐条输出，所以进度条会退回不确定状态：
	// 用户看到的是"还在动、但暂时没有百分比"，而不是卡死在某个数字上。
	progress.notify(archiveEvent{Type: "escalating"})
	if err := s.runEscalatedFileStep(r.Context(), hostID, script, fileEscalateTimeout, op.what); err != nil {
		fail(err.Error())
		return
	}
	// 这条路没有逐条输出，明细一定是空的，条目数只能用统计阶段数出来的分母兜底。
	task.setTotal(int(progress.totals()))
	progress.notify(archiveEvent{Type: "done", Path: op.dest})
}

// archiveProgress 把执行过程中的输出折算成进度帧。
//
// 识别到统计标记就记下分母，其余每一行都算"处理完一个条目"：tar -v 与 zip 的
// 逐条输出正好一行一个条目，这就是进度最省事的来源 —— 不必解析行内容，
// 也就不必跟各个版本 tar 的输出格式较劲。
type archiveProgress struct {
	// mu 同时保护计数与响应写入：回调来自两条拷贝协程（stdout / stderr），
	// 并发写同一份响应是错的。
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	last    time.Time
	total   int64
	done    int64
	// task 是这次操作的任务记录：每一行输出同时是明细表里的一条。
	task *fileTask
}

// line 是 execOnHostStream 的输出回调。
func (p *archiveProgress) line(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if n, ok := parseArchiveTotal(text); ok {
		p.total = n
		p.send(archiveEvent{Type: "progress", Total: p.total, Done: 0}, false)
		return
	}
	p.done++
	p.task.addItem(archiveItemPath(text), -1, "", "")
	p.send(p.progressEvent(), true)
}

// archiveItemPath 从压缩/解压的逐条输出里剥出文件名。
//
// 各家工具的格式不同：tar -v 直接给路径（压缩时带 "./" 前缀），zip 给的是
// "adding: x (deflated 12%)"，unzip 给的是 "inflating: x"。剥掉这些固定的
// 前后缀，明细表里剩下的就是用户认得的东西。
func archiveItemPath(line string) string {
	text := strings.TrimSpace(line)
	for _, prefix := range []string{"adding: ", "inflating: ", "extracting: ", "creating: "} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			text = rest
			break
		}
	}
	// zip 会在条目后面补一句压缩率说明。
	if i := strings.Index(text, " ("); i > 0 {
		text = text[:i]
	}
	return strings.TrimPrefix(strings.TrimSpace(text), "./")
}

// totals 返回统计阶段数出来的条目数。
//
// 提权那条路拿不到逐条输出（明细为空），调用方用这个分母把条数补上。
func (p *archiveProgress) totals() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

// notify 下发一帧不该被节流的帧（prepare / escalating / done / error）。
func (p *archiveProgress) notify(ev archiveEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.send(ev, false)
}

// progressEvent 生成当前的进度帧；调用方必须已持锁。
func (p *archiveProgress) progressEvent() archiveEvent {
	done := p.done
	// 分母是个估计值：统计之后源目录里新增的文件、以及命令自己打出的警告行都会
	// 被计入。超出时按已完成显示，绝不让进度条跑过 100% 再倒回来。
	if p.total > 0 && done > p.total {
		done = p.total
	}
	return archiveEvent{Type: "progress", Total: p.total, Done: done}
}

// send 写一帧并立即冲刷；调用方必须已持锁。throttle 只对 progress 帧为真。
func (p *archiveProgress) send(ev archiveEvent, throttle bool) {
	if throttle && time.Since(p.last) < archiveProgressInterval {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	p.last = time.Now()
	_, _ = p.w.Write(append(data, '\n'))
	if p.flusher != nil {
		p.flusher.Flush()
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	dir, ok := requireAbsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	// 使用流式 MultipartReader 而非 ParseMultipartForm：
	// 后者会把整个文件缓冲到内存或临时磁盘，大文件上传会拖垮服务。
	reader, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求不是 multipart 表单："+err.Error())
		return
	}

	sess, client, ok := s.openSFTP(w, hostID)
	if !ok {
		return
	}
	defer sess.Close()

	guard := newStallGuard(sftpStallTimeout, func() { _ = sess.client.Close() })
	defer guard.Release()

	// 任务在收文件之前登记，每个文件另有一条明细（大小取实际写入的字节数）。
	// 失败原因存进 failure、由 defer 统一落库：上传有八条出错路径，每条 return
	// 前都写一遍登记代码，早晚会漏掉一条 —— 而"界面报了错、历史里却是成功"
	// 会让人不再相信这份记录。
	task := s.beginFileTask(hostID, store.TaskKindUpload, "上传到 "+dir, "")
	var failure string
	defer func() {
		if r.Context().Err() != nil {
			task.abort()
			return
		}
		task.finish(failure)
	}()

	saved := make([]string, 0, 1)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			failure = "读取上传内容失败：" + err.Error()
			writeErr(w, http.StatusBadRequest, failure)
			return
		}

		fileName := part.FileName()
		if fileName == "" {
			continue // 非文件字段
		}
		// 只取基础名，避免文件名里携带 ../ 造成目录穿越。
		remote := path.Join(dir, path.Base(fileName))

		file, err := client.Create(remote)
		// 普通用户建不了这个文件（系统目录多是这样）时，改成"先落到 /tmp，
		// 再由 root 搬到目标位置"：上传内容只能顺着 multipart 流读一次，
		// 不能"失败后重新读一遍请求体"。
		var viaRoot string
		if err != nil {
			if !isPermissionDenied(err) {
				failure = "创建远端文件失败：" + err.Error()
				s.writeSFTPError(w, err, "创建远端文件失败")
				return
			}
			if viaRoot, err = uploadTempPath(); err != nil {
				failure = "生成临时文件名失败：" + err.Error()
				writeErr(w, http.StatusInternalServerError, failure)
				return
			}
			if file, err = client.Create(viaRoot); err != nil {
				failure = "创建临时文件失败：" + err.Error()
				s.writeSFTPError(w, err, "创建临时文件失败")
				return
			}
		}

		written, copyErr := io.Copy(progressWriter{w: file, g: guard}, progressReader{r: part, g: guard})
		closeErr := file.Close()

		if copyErr != nil {
			failure = "写入远端文件失败：" + copyErr.Error()
			writeErr(w, http.StatusInternalServerError, failure)
			return
		}
		if closeErr != nil {
			failure = "关闭远端文件失败：" + closeErr.Error()
			writeErr(w, http.StatusInternalServerError, failure)
			return
		}

		if viaRoot != "" {
			if rootErr := s.copyUploadAsRoot(r.Context(), hostID, viaRoot, remote); rootErr != nil {
				// 内容已经落在临时位置了，尽力清掉，别给目标机留垃圾。
				_ = client.Remove(viaRoot)
				failure = "保存上传文件失败：" + rootErr.Error()
				s.writeHostError(w, rootErr, "保存上传文件失败")
				return
			}
		}
		task.addItem(remote, written, "", "")
		saved = append(saved, remote)
	}

	if len(saved) == 0 {
		failure = "没有收到任何文件"
		writeErr(w, http.StatusBadRequest, failure)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": saved})
}
