package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// 文件属性：读取一个条目的完整元数据，并支持就地修改权限 / 属主 / 属组 / 时间戳。
//
// 元数据来自两个地方，缺一不可：
//   - SFTP 的 Lstat：存在性、类型、大小、权限位、UID/GID、mtime/atime、符号链接目标。
//   - 一条 stat 命令：属主名、属组名、ctime、硬链接数、inode、磁盘占用块数。
//
// SFTP 协议本身不携带这些字段（uid/gid 是数字，没有名字；ctime 与 inode 根本没有），
// 所以第二条命令不是冗余，而是唯一来源。它失败时那几项留空，面板仍然可用 ——
// 属性面板的主干信息来自 SFTP，不该因为一条可选命令而整个打不开。

// attrResponse 是属性接口的返回体，字段与前端 FileAttr 一一对应。
type attrResponse struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	IsDir      bool      `json:"isDir"`
	IsLink     bool      `json:"isLink"`
	LinkTarget string    `json:"linkTarget"`
	Size       int64     `json:"size"`
	Blocks     int64     `json:"blocks"`
	Mode       string    `json:"mode"`
	ModeOctal  string    `json:"modeOctal"`
	UID        uint32    `json:"uid"`
	GID        uint32    `json:"gid"`
	Owner      string    `json:"owner"`
	Group      string    `json:"group"`
	Atime      time.Time `json:"atime"`
	Mtime      time.Time `json:"mtime"`
	// Ctime 为零值表示没取到（目标机的 stat 不支持 -c 或不可用）。
	Ctime time.Time `json:"ctime"`
	Nlink int64     `json:"nlink"`
	Inode int64     `json:"inode"`
}

// octalPerm 把权限位写成 3-4 位八进制字符串，供可编辑区回填。
//
// 与前端 modeOf 的口径一致，但这里是唯一权威：4 位用于 setuid/setgid/sticky，
// 少了那一位用户就会在保存时把 setuid 悄悄抹掉。
func octalPerm(mode os.FileMode) string {
	value := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		value |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		value |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		value |= 0o1000
	}
	text := strconv.FormatUint(uint64(value), 8)
	if len(text) == 3 {
		return text
	}
	return text
}

// statCommand 生成补齐元数据用的 stat 命令。
//
// 用 -c 自定义格式而不是解析默认输出：默认输出是为人类排版的，列宽随内容浮动，
// 文件名里有空格时根本无法可靠切分。这里字段之间用制表符分隔，而制表符不可能
// 出现在用户名或组名里，切分因此是确定的。
//
// 不加 -L：属性面板展示的是这个条目本身，与 SFTP 的 Lstat 保持同一口径 ——
// 符号链接的属主/时间戳应当显示链接自己的，而不是它指向的那个文件。
func statCommand(target string) string {
	fields := []string{"%U", "%G", "%Z", "%h", "%i", "%b"}
	return "stat -c " + shellQuotePath(strings.Join(fields, "\t")) + " -- " + shellQuotePath(target)
}

// statFields 是 stat 命令补齐的那几项。
type statFields struct {
	Owner  string
	Group  string
	Ctime  time.Time
	Nlink  int64
	Inode  int64
	Blocks int64
}

// parseStatLine 解析 statCommand 的输出。
//
// 返回的 error 只用于"这行不是我们要的东西"——调用方据此退回空值，
// 而不是把解析失败当成请求失败：目标机的 stat 可能是 busybox 的简化版，
// 缺字段是常态而非异常。
func parseStatLine(text string) (statFields, error) {
	parts := strings.Split(strings.TrimSpace(text), "\t")
	if len(parts) != 6 {
		return statFields{}, errors.New("stat 输出字段数不符")
	}

	fields := statFields{Owner: parts[0], Group: parts[1]}
	// 数值字段解析失败时留 0：0 在界面上会被显示成"—"，比一个错误的数字好。
	if n, err := strconv.ParseInt(parts[2], 10, 64); err == nil && n > 0 {
		fields.Ctime = time.Unix(n, 0)
	}
	fields.Nlink, _ = strconv.ParseInt(parts[3], 10, 64)
	fields.Inode, _ = strconv.ParseInt(parts[4], 10, 64)
	if blocks, err := strconv.ParseInt(parts[5], 10, 64); err == nil {
		// %b 的单位是 512 字节的块，不是字节。直接当字节用会让占用显示小 512 倍。
		fields.Blocks = blocks * 512
	}
	return fields, nil
}

// statFieldsOf 跑一次 stat 命令补齐元数据。
//
// 失败一律返回零值：这几项是锦上添花，取不到就让界面显示"—"。
// 但 context 取消要原样透出 —— 那说明客户端已经走了，继续往下写响应没有意义。
func (s *Server) statFieldsOf(ctx context.Context, hostID int64, target string) statFields {
	res, err := s.execOnHost(ctx, hostID, statCommand(target), defaultExecTimeout)
	if err != nil || res.ExitCode != 0 {
		return statFields{}
	}
	fields, err := parseStatLine(res.Stdout)
	if err != nil {
		return statFields{}
	}
	return fields
}

func (s *Server) handleFileAttr(w http.ResponseWriter, r *http.Request) {
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

	// 用 Lstat 而不是 Stat：属性面板要看的是这个条目本身，符号链接不能被解析掉，
	// 否则"这是个链接、它指向哪里"这两条最该看到的信息就消失了。
	info, err := client.Lstat(target)
	if err != nil {
		if errors.Is(err, errSFTPTimeout) {
			s.writeSFTPError(w, err, "读取文件信息失败")
			return
		}
		writeErr(w, http.StatusNotFound, "对象不存在："+target)
		return
	}

	resp := attrResponse{
		Path:      target,
		Name:      path.Base(target),
		IsDir:     info.IsDir(),
		IsLink:    info.Mode()&os.ModeSymlink != 0,
		Size:      info.Size(),
		Mode:      info.Mode().String(),
		ModeOctal: octalPerm(info.Mode()),
		Mtime:     info.ModTime(),
	}

	// Atime 与 UID/GID 在 os.FileInfo 接口上看不到：pkg/sftp 把它们放在
	// FileStat 里，通过 Sys() 取出来。取不到就留零值，界面显示"—"。
	if stat, ok := info.Sys().(*sftp.FileStat); ok {
		resp.UID = stat.UID
		resp.GID = stat.GID
		resp.Atime = time.Unix(int64(stat.Atime), 0)
	}

	if resp.IsLink {
		if dest, err := client.ReadLink(target); err == nil {
			resp.LinkTarget = dest
		}
	}

	// 补齐 SFTP 给不出的字段。注意顺序：先 Lstat 确认存在，再跑命令 ——
	// 反过来的话，路径不存在时用户拿到的是一句 stat 的原始报错。
	fields := s.statFieldsOf(r.Context(), hostID, target)
	resp.Owner = fields.Owner
	resp.Group = fields.Group
	resp.Ctime = fields.Ctime
	resp.Nlink = fields.Nlink
	resp.Inode = fields.Inode
	resp.Blocks = fields.Blocks

	writeJSON(w, http.StatusOK, resp)
}

// chownRequest 是改属主/属组的入参。
//
// 两个字段都可选，但至少要有一个：只改属主、只改属组都是常见诉求，
// 而"都不给"是个空操作，说明调用方搞错了。
type chownRequest struct {
	Paths []string `json:"paths"`
	Owner string   `json:"owner"`
	Group string   `json:"group"`
}

// touchRequest 是改时间戳的入参。
//
// 时间是 epoch 秒而不是格式化字符串：日期选择器给的是绝对时刻，
// 换成字符串就要在两边约定时区，而"面板所在时区"和"目标机时区"往往不是一回事。
// 0 表示这一项不改。
type touchRequest struct {
	Paths []string `json:"paths"`
	Mtime int64    `json:"mtime"`
	Atime int64    `json:"atime"`
}

// validUserName 校验一个用户名或组名。
//
// 只挡掉会让命令语义变掉的东西：冒号是 chown 的分隔符，控制字符与空白在
// 用户名里不合法且会让命令难以阅读。其余交给 shellQuotePath —— 真正的注入
// 防护在那里，这里不做多余的白名单（用户名规则各发行版并不一致）。
func validUserName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	return !strings.ContainsAny(name, ":\x00 \t\n\r")
}

func (s *Server) handleChown(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req chownRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要修改属主的条目")
		return
	}
	owner := strings.TrimSpace(req.Owner)
	group := strings.TrimSpace(req.Group)
	if owner == "" && group == "" {
		writeErr(w, http.StatusBadRequest, "请至少填写属主或属组")
		return
	}
	if owner != "" && !validUserName(owner) {
		writeErr(w, http.StatusBadRequest, "属主名不合法")
		return
	}
	if group != "" && !validUserName(group) {
		writeErr(w, http.StatusBadRequest, "属组名不合法")
		return
	}

	// spec 的三种形态：user:group / user / :group。第三种只改组，
	// 写成 "user:" 也可以，但显式的 ":group" 更不容易被误读成"连属主一起改"。
	spec := owner
	if group != "" {
		spec = owner + ":" + group
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
		if _, err := client.Lstat(target); err != nil {
			writeErr(w, http.StatusNotFound, "对象不存在："+target)
			return
		}
		// 走 runFileStep 而不是无条件提权：改自己目录里文件的属组不需要 root，
		// 无脑提权会把属主换成 root，用户之后自己都改不动。
		// -h 作用于链接本身，与 Lstat 的口径一致。
		if err := s.runFileStep(r.Context(), hostID,
			"chown -h "+shellQuotePath(spec)+" -- "+shellQuotePath(target), "修改属主"); err != nil {
			s.writeHostError(w, err, "修改属主失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// touchStamp 把 epoch 秒写成 touch -t 认的 UTC 形式。
//
// 用 POSIX 的 -t 而不是 GNU 的 -d @epoch：-t 在 GNU / busybox / BSD 上写法一致，
// 而 "@" 前缀是 GNU 扩展。配合调用处的 TZ=UTC，两边的时区差异就被彻底消掉了 ——
// 否则目标机时区与面板不同时，用户设的 10:00 会落成另一个时刻。
func touchStamp(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("200601021504.05")
}

func (s *Server) handleTouch(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req touchRequest
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		return
	}
	if len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "未选择要修改时间的条目")
		return
	}
	if req.Mtime == 0 && req.Atime == 0 {
		writeErr(w, http.StatusBadRequest, "请至少指定修改时间或访问时间")
		return
	}

	// -a 与 -m 各管一个时间戳，两者互不覆盖，所以要分别执行。
	// 合成一个脚本一次跑完：多条路径 × 两个时间戳会变成很多次 SSH 往返。
	//
	// 开头的 set -e 不能省：默认情况下 sh 会跑完剩下的命令并以最后一条的退出码
	// 收场，于是"第一条路径就失败了"会被后面成功的命令掩盖成成功。
	var script strings.Builder
	script.WriteString("set -e\n")
	appendTouch := func(pathArg string) {
		if req.Mtime != 0 {
			script.WriteString("TZ=UTC touch -h -m -t " + touchStamp(req.Mtime) + " -- " + pathArg + "\n")
		}
		if req.Atime != 0 {
			script.WriteString("TZ=UTC touch -h -a -t " + touchStamp(req.Atime) + " -- " + pathArg + "\n")
		}
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
		if _, err := client.Lstat(target); err != nil {
			writeErr(w, http.StatusNotFound, "对象不存在："+target)
			return
		}
		appendTouch(shellQuotePath(target))
	}

	if err := s.runFileStep(r.Context(), hostID, script.String(), "修改时间"); err != nil {
		s.writeHostError(w, err, "修改时间失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
