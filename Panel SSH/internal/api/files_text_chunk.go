package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// 大文本的按块读取。
//
// 为什么不能把上限直接调大：一份几 MiB 的文本塞进一次 JSON 响应，浏览器还要
// 把它整个铺进 DOM —— 传输和渲染都是一次性开销。CSV / 日志这类文件绝大多数
// 时候只看开头一段，按块下发、滚动到哪儿读到哪儿更合适。
//
// 分块以"行"为界：只有窗口里存在换行时才在最后一个换行处截断，剩下的字节留给
// 下一块。这样每次返回的都是完整行，前端直接拼接不会出现半行。
//
// 编码在第一次请求（offset=0）时识别并返回，后续请求带上它 —— 变长编码下
// 不能简单地按字节切片，必须让服务端算准"这一块消费了多少字节"。

const (
	// textChunkDefault 是一块的大小。256 KiB 对 5 MiB 的文件是 20 块左右，
	// 既不会一次渲染太多，也不至于要滚很多次才看完。
	textChunkDefault = 256 << 10
	textChunkMin     = 16 << 10
	textChunkMax     = 1 << 20
	// textChunkSample 是识别编码时读取的文件头部大小。
	textChunkSample = 64 << 10
)

// textChunkResponse 是一帧分块读取的结果。
type textChunkResponse struct {
	Path       string    `json:"path"`
	Chunk      string    `json:"chunk"`
	Encoding   string    `json:"encoding"`
	Offset     int64     `json:"offset"`
	NextOffset int64     `json:"nextOffset"`
	EOF        bool      `json:"eof"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"`
	ModTime    time.Time `json:"modTime"`
}

func (s *Server) handleTextChunk(w http.ResponseWriter, r *http.Request) {
	hostID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	target, ok := requireAbsPath(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}

	offset := queryNonNegative(r, "offset", 0)
	limit := queryNonNegative(r, "limit", textChunkDefault)
	if limit < textChunkMin {
		limit = textChunkMin
	}
	if limit > textChunkMax {
		limit = textChunkMax
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
	if offset > info.Size() {
		writeErr(w, http.StatusBadRequest, "读取偏移超出文件范围")
		return
	}

	file, err := client.Open(target)
	if err != nil {
		s.writeSFTPError(w, err, "打开文件失败")
		return
	}
	defer file.Close()

	// 分块读取是短促的连续读，用停滞监视兜底即可。返回前就 Release，
	// 不会影响后续请求（每个请求各自一个会话）。
	guard := newStallGuard(sftpStallTimeout, func() { _ = sess.client.Close() })
	defer guard.Release()

	encoding := r.URL.Query().Get("encoding")
	if !knownEncoding(encoding) {
		encoding, err = detectFileEncoding(file, guard)
		if err != nil {
			s.writeHostError(w, err, "识别文件编码失败")
			return
		}
		if encoding == "" {
			writeErr(w, http.StatusUnsupportedMediaType, "疑似二进制文件，无法在线预览")
			return
		}
	}

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		s.writeSFTPError(w, err, "定位文件失败")
		return
	}
	window, err := io.ReadAll(io.LimitReader(progressReader{r: file, g: guard}, limit))
	if err != nil {
		s.writeSFTPError(w, err, "读取文件失败")
		return
	}

	atEOF := offset+int64(len(window)) >= info.Size()

	// 首块要把 BOM 剥掉，别让编辑器里出现一个看不见的字符。
	bom := bomOf(encoding)
	bomLen := 0
	body := window
	if offset == 0 && len(bom) > 0 && bytes.HasPrefix(body, bom) {
		body = body[len(bom):]
		bomLen = len(bom)
	}

	text, valid, err := decodeWindow(body, encoding, atEOF)
	if err != nil {
		writeErr(w, http.StatusUnsupportedMediaType, "无法按 "+encoding+" 解码该文件，请下载后查看")
		return
	}

	used := bomLen + valid
	// 只返回完整的行；最后半行留给下一块，否则前端会出现一行被劈成两段。
	if idx := strings.LastIndex(text, "\n"); idx >= 0 && idx+1 < len(text) {
		cut := text[:idx+1]
		if encoded, encErr := encodeBody(cut, encoding); encErr == nil && bomLen+len(encoded) > 0 {
			text = cut
			used = bomLen + len(encoded)
		}
	}
	// 保底：窗口非空却一个字节都没消费，会让前端在同一偏移上打转。
	if used == 0 && len(window) > 0 {
		used = len(window)
	}

	nextOffset := offset + int64(used)
	if nextOffset > info.Size() {
		nextOffset = info.Size()
	}

	writeJSON(w, http.StatusOK, textChunkResponse{
		Path:       target,
		Chunk:      text,
		Encoding:   encoding,
		Offset:     offset,
		NextOffset: nextOffset,
		// 读到文件末尾、或者这一块什么都没读到，都算结束 —— 后者是防御性的，
		// 正常不会出现，但前端一旦收到就会按"还有下一块"继续请求。
		EOF:     nextOffset >= info.Size() || len(window) == 0,
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime(),
	})
}

// detectFileEncoding 读文件头识别编码；识别不出来时返回空串（视为二进制）。
func detectFileEncoding(file *sftp.File, guard *stallGuard) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	sample, err := io.ReadAll(io.LimitReader(progressReader{r: file, g: guard}, textChunkSample))
	if err != nil {
		return "", err
	}
	encoding, ok := detectEncoding(sample)
	if !ok {
		return "", nil
	}
	return encoding, nil
}

func knownEncoding(encoding string) bool {
	switch encoding {
	case encodingUTF8, encodingUTF8BOM, encodingUTF16LE, encodingUTF16BE, encodingGB18030:
		return true
	default:
		return false
	}
}

func queryNonNegative(r *http.Request, key string, fallback int64) int64 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}
