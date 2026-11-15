package api

import (
	"bytes"
	"errors"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
)

// 文本文件的编码识别与转换。
//
// 为什么必须在服务端做：接口返回的是 JSON，Go 在序列化时会把非法 UTF-8 字节
// 替换成 U+FFFD —— 到前端时原始字节已经不可恢复，前端再转码也救不回来。
//
// 覆盖范围刻意收在"中文环境里真实会遇到的三种"：
//
//	UTF-8（带不带 BOM）、UTF-16（Excel 的「Unicode 文本」）、GB18030（Excel /
//	Windows 导出的 CSV 最常见）。
//
// 保存时按读取时识别出的编码写回：把 GBK 的 CSV 打开改一行再保存，文件编码
// 不能悄悄变成 UTF-8 —— 用户的其他系统可能一直按 GBK 读它。

const (
	encodingUTF8    = "utf-8"
	encodingUTF8BOM = "utf-8-bom"
	encodingUTF16LE = "utf-16le"
	encodingUTF16BE = "utf-16be"
	encodingGB18030 = "gb18030"
)

var (
	utf8BOM    = []byte{0xef, 0xbb, 0xbf}
	utf16LEBOM = []byte{0xff, 0xfe}
	utf16BEBOM = []byte{0xfe, 0xff}
)

// decodeText 把文件字节转成 UTF-8 文本，并返回识别出的编码。
//
// 第二个返回值是保存时要用的编码名；第三个返回 false 表示"这不是文本"
// （二进制文件、无法解码的编码），调用方应当拒绝在编辑器里打开它。
func decodeText(data []byte) (string, string, bool) {
	switch {
	case bytes.HasPrefix(data, utf8BOM):
		body := data[len(utf8BOM):]
		if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
			return "", "", false
		}
		return string(body), encodingUTF8BOM, true

	case bytes.HasPrefix(data, utf16LEBOM):
		return decodeUTF16(data[len(utf16LEBOM):], unicode.LittleEndian, encodingUTF16LE)

	case bytes.HasPrefix(data, utf16BEBOM):
		return decodeUTF16(data[len(utf16BEBOM):], unicode.BigEndian, encodingUTF16BE)
	}

	if utf8.Valid(data) {
		// UTF-8 文本里出现 NUL 基本可以断定是二进制（图片、压缩包、可执行文件）。
		if bytes.IndexByte(data, 0) >= 0 {
			return "", "", false
		}
		return string(data), encodingUTF8, true
	}

	// 非 UTF-8：中文环境里最可能是 GBK 系，用它的超集 GB18030 解码。
	// 先看 NUL：二进制文件里通常有，先挡掉可以少一次解码。
	if bytes.IndexByte(data, 0) >= 0 {
		return "", "", false
	}
	decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
	if err != nil {
		return "", "", false
	}
	// 解码器遇到非法字节不会报错，而是塞一个 U+FFFD 继续走。出现替换字符
	// 就说明这份数据不是合法的 GBK 系文本（多半是二进制），不能在编辑器里打开。
	if bytes.Contains(decoded, []byte(string(utf8.RuneError))) {
		return "", "", false
	}
	return string(decoded), encodingGB18030, true
}

// decodeUTF16 解码 BOM 之后的 UTF-16 内容。
func decodeUTF16(body []byte, order unicode.Endianness, encoding string) (string, string, bool) {
	if len(body)%2 != 0 {
		return "", "", false
	}
	decoded, err := unicode.UTF16(order, unicode.IgnoreBOM).NewDecoder().Bytes(body)
	if err != nil {
		return "", "", false
	}
	// 解码出来的正文里不该有 NUL；有的话说明这份数据不是文本。
	if bytes.IndexRune(decoded, 0) >= 0 {
		return "", "", false
	}
	// UTF-16 解码器同样用 U+FFFD 顶替非法代理对，出现它就说明不是合法文本。
	if bytes.Contains(decoded, []byte(string(utf8.RuneError))) {
		return "", "", false
	}
	return string(decoded), encoding, true
}

// encodeText 按指定编码把编辑器里的文本写回。
//
// 空编码按 UTF-8 处理：老客户端不会传这个字段，行为与改动前一致。
func encodeText(text, encoding string) ([]byte, error) {
	switch encoding {
	case encodingUTF8BOM:
		out := make([]byte, 0, len(utf8BOM)+len(text))
		out = append(out, utf8BOM...)
		return append(out, text...), nil

	case encodingUTF16LE:
		return encodeUTF16(text, unicode.LittleEndian, utf16LEBOM)

	case encodingUTF16BE:
		return encodeUTF16(text, unicode.BigEndian, utf16BEBOM)

	case encodingGB18030:
		return simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(text))

	default:
		return []byte(text), nil
	}
}

func encodeUTF16(text string, order unicode.Endianness, bom []byte) ([]byte, error) {
	encoded, err := unicode.UTF16(order, unicode.IgnoreBOM).NewEncoder().Bytes([]byte(text))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(bom)+len(encoded))
	out = append(out, bom...)
	return append(out, encoded...), nil
}

// bomOf 返回该编码对应的 BOM；没有 BOM 的编码返回 nil。
func bomOf(encoding string) []byte {
	switch encoding {
	case encodingUTF8BOM:
		return utf8BOM
	case encodingUTF16LE:
		return utf16LEBOM
	case encodingUTF16BE:
		return utf16BEBOM
	default:
		return nil
	}
}

// encodeBody 只编码正文，不写 BOM。分块读取要靠它算"这一块实际消费了多少字节"。
func encodeBody(text, encoding string) ([]byte, error) {
	switch encoding {
	case encodingGB18030:
		return simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(text))
	case encodingUTF16LE:
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder().Bytes([]byte(text))
	case encodingUTF16BE:
		return unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewEncoder().Bytes([]byte(text))
	default:
		// UTF-8 / UTF-8 BOM 的正文都是 UTF-8。
		return []byte(text), nil
	}
}

// detectEncoding 从一段样本里识别编码。
//
// 样本通常是从文件头读的一小段，末尾很可能正好切断一个多字节字符，所以每种
// 编码都要按"逐次去掉末尾 1-3 字节"重试一次。
//
// 顺序很关键：必须把所有截断长度下的 UTF-8 都试完，再试 GB18030。否则一份
// 被截断的 UTF-8 样本可能在"去掉 1 字节"时恰好能按 GBK 解码成功，被误判成
// GB18030 —— 中文字符用 UTF-8 编码后，其字节序列常常也是合法的 GBK 双字节。
func detectEncoding(sample []byte) (string, bool) {
	// 1) 有 BOM 时结论最确定，BOM 本身不会被截断。
	for cut := 0; cut <= 3; cut++ {
		if cut > 0 && cut >= len(sample) {
			break
		}
		data := sample[:len(sample)-cut]
		if bytes.HasPrefix(data, utf16LEBOM) ||
			bytes.HasPrefix(data, utf16BEBOM) ||
			bytes.HasPrefix(data, utf8BOM) {
			if _, encoding, ok := decodeText(data); ok {
				return encoding, true
			}
		}
	}

	// 2) 无 BOM：先把 UTF-8 的所有可能截断长度都排除掉。
	for cut := 0; cut <= 3; cut++ {
		if cut > 0 && cut >= len(sample) {
			break
		}
		data := sample[:len(sample)-cut]
		if bytes.IndexByte(data, 0) >= 0 {
			return "", false
		}
		if utf8.Valid(data) {
			return encodingUTF8, true
		}
	}

	// 3) 最后才认为是 GBK 系。
	for cut := 0; cut <= 3; cut++ {
		if cut > 0 && cut >= len(sample) {
			break
		}
		data := sample[:len(sample)-cut]
		if bytes.IndexByte(data, 0) >= 0 {
			return "", false
		}
		decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
		if err == nil && !bytes.Contains(decoded, utf8Replacement()) {
			return encodingGB18030, true
		}
	}
	return "", false
}

// decodeWindow 解码一个字节窗口，并返回其中完整字符占用的字节数。
//
// 窗口末尾可能切断一个多字节字符（分块读取必然遇到），这里把它让回去：
// 调用方据此把 nextOffset 定在真正的字符边界上，下一块才不会从半个字符开始。
// atEOF 为真时窗口就是文件末尾，没有"下一块"，允许按替换字符处理非法序列。
func decodeWindow(data []byte, encoding string, atEOF bool) (string, int, error) {
	switch encoding {
	case encodingUTF16LE, encodingUTF16BE:
		order := unicode.LittleEndian
		if encoding == encodingUTF16BE {
			order = unicode.BigEndian
		}
		n := len(data) - len(data)%2
		for n > 0 {
			text, err := unicode.UTF16(order, unicode.IgnoreBOM).NewDecoder().Bytes(data[:n])
			if err == nil && (atEOF || !bytes.HasSuffix(text, utf8Replacement())) {
				return string(text), n, nil
			}
			n -= 2
		}
		return "", 0, errInvalidTextWindow

	case encodingGB18030:
		// 回退最多 3 字节即可覆盖 GB18030 的最长字符（4 字节）。
		for cut := 0; cut <= 3 && cut < len(data); cut++ {
			decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data[:len(data)-cut])
			if err == nil && (atEOF || !bytes.HasSuffix(decoded, utf8Replacement())) {
				return string(decoded), len(data) - cut, nil
			}
		}
		return "", 0, errInvalidTextWindow

	default:
		for cut := 0; cut <= 3 && cut < len(data); cut++ {
			if utf8.Valid(data[:len(data)-cut]) {
				return string(data[:len(data)-cut]), len(data) - cut, nil
			}
		}
		return "", 0, errInvalidTextWindow
	}
}

func utf8Replacement() []byte {
	return []byte(string(utf8.RuneError))
}

var errInvalidTextWindow = errors.New("无法解码文件内容")
