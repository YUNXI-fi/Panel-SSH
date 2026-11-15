package api

import (
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// 在线预览：图片、音视频、PDF。
//
// 文本类（代码 / Markdown / HTML）不走这里 —— 它们要经过内容校验、编码处理与
// 前端高亮，走已有的 file/text 拿正文更直接；这里只负责"原样把字节吐回去"。
//
// 为什么必须单独开一个接口而不是复用 file/download：
//
//  1. download 带 Content-Disposition: attachment，浏览器只会存盘，《img》《video》
//     拿不到可渲染的响应。
//  2. 音视频拖动进度条依赖 HTTP Range（206）。download 是一次性全量输出，
//     播放器无法跳转。
//
// http.ServeContent 负责 Range / If-Modified-Since / HEAD 这些细节，
// 我们只需要给它一个能 Seek 的远端文件。

// previewContentTypes 是允许预览的扩展名与对应类型。
//
// 只收浏览器能直接渲染的格式：放进列表里的类型都是"点了就能看"的，
// 不在表里的扩展名一律让用户下载 —— 猜一个 Content-Type 只会让浏览器
// 把文件当成下载或渲染成乱码，两种结果都不如说清楚"不支持预览"。
var previewContentTypes = map[string]string{
	// 图片
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",
	".avif": "image/avif",
	".svg":  "image/svg+xml",
	// 音频
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/ogg",
	".flac": "audio/flac",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	// 视频
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".webm": "video/webm",
	".ogv":  "video/ogg",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	// PDF
	".pdf": "application/pdf",
}

// previewContentType 按扩展名给出预览用的 Content-Type。
func previewContentType(name string) (string, bool) {
	contentType, ok := previewContentTypes[strings.ToLower(path.Ext(name))]
	return contentType, ok
}

func (s *Server) handlePreviewFile(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	target, ok := requireAbsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	contentType, ok := previewContentType(target)
	if !ok {
		writeErr(w, http.StatusBadRequest, "该类型不支持在线预览，请下载后查看")
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
		writeErr(w, http.StatusBadRequest, "目标是目录，无法预览")
		return
	}

	file, err := client.Open(target)
	if err != nil {
		s.writeSFTPError(w, err, "打开文件失败")
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", contentType)
	// inline 是这里与 download 的关键差别：浏览器据此就地渲染而不是存盘。
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 预览内容随文件变化，不能让中间层或浏览器拿旧副本。
	w.Header().Set("Cache-Control", "no-store")

	// SVG 既是图片也是"可执行脚本的文档"：直接点开这个预览地址时，浏览器会把它
	// 当作 HTML 文档渲染，脚本就在面板同源下执行了。sandbox 让它即使被直接打开
	// 也拿不到面板的凭据；作为 <img> 加载时这条头不参与渲染。
	if contentType == "image/svg+xml" {
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	}

	// 媒体播放是长时间连续读取，既不能设总超时，也不能不加观光：
	// 一旦链路停滞，监视器会关掉本次会话，阻塞中的读立刻返回。
	guard := newStallGuard(sftpStallTimeout, func() { _ = sess.client.Close() })
	defer guard.Release()

	http.ServeContent(w, r, path.Base(target), info.ModTime(), guardedReadSeeker{file: file, guard: guard})
}

// guardedReadSeeker 把远端文件的读取接到停滞监视器上。
//
// ServeContent 需要 io.ReadSeeker 才能处理 Range（它先 Seek 到末尾拿长度，
// 再 Seek 回请求的起点）。这里只代理这两个方法，不做缓冲 —— 媒体文件可能
// 有几个 GB，任何整体载入都会把面板拖垮。
type guardedReadSeeker struct {
	file  *sftp.File
	guard *stallGuard
}

func (g guardedReadSeeker) Read(p []byte) (int, error) {
	n, err := g.file.Read(p)
	if n > 0 {
		g.guard.touch()
	}
	return n, err
}

func (g guardedReadSeeker) Seek(offset int64, whence int) (int64, error) {
	// SeekEnd 会向目标机要一次文件大小（pkg/sftp 内部 stat）。链路黑洞时
	// 这一步会永久阻塞，而 ServeContent 是在发响应头之前调用它的 ——
	// 没有超时的话，这个请求会一直挂在那里，连接池的引用也永远不归零。
	type outcome struct {
		pos int64
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		pos, err := g.file.Seek(offset, whence)
		done <- outcome{pos: pos, err: err}
	}()

	select {
	case result := <-done:
		return result.pos, result.err
	case <-time.After(sftpOpTimeout):
		_ = g.file.Close()
		return 0, errSFTPTimeout
	}
}

// 编译期确认接口实现，避免将来替换底层类型时悄悄退化成全量缓冲。
var _ io.ReadSeeker = guardedReadSeeker{}
