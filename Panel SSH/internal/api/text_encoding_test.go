package api

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("测试样本不是合法十六进制：%v", err)
	}
	return b
}

// 这组样本是 "姓名,城市\n张三,北京\n" 各编码下的真实字节，
// 用来锁定"识别 → 转 UTF-8 → 原编码写回"这条链路。
const (
	sampleText    = "姓名,城市\n张三,北京\n"
	sampleGB18030 = "d0d5c3fb2cb3c7cad00ad5c5c8fd2cb1b1bea90a"
	sampleUTF16LE = "d3590d542c00ce57025e0a00205f094e2c001753ac4e0a00"
	sampleUTF16BE = "59d3540d002c57ce5e02000a5f204e09002c53174eac000a"
	sampleUTF8BOM = "efbbbfe5a793e5908d2ce59f8ee5b8820ae5bca0e4b8892ce58c97e4baac0a"
)

func TestDecodeTextEncodings(t *testing.T) {
	cases := []struct {
		name     string
		data     []byte
		wantText string
		wantEnc  string
	}{
		{"UTF-8", []byte(sampleText), sampleText, encodingUTF8},
		{"UTF-8 BOM", mustHex(t, sampleUTF8BOM), sampleText, encodingUTF8BOM},
		{"GB18030", mustHex(t, sampleGB18030), sampleText, encodingGB18030},
		{"UTF-16LE", append(utf16LEBOM, mustHex(t, sampleUTF16LE)...), sampleText, encodingUTF16LE},
		{"UTF-16BE", append(utf16BEBOM, mustHex(t, sampleUTF16BE)...), sampleText, encodingUTF16BE},
		{"空文件", nil, "", encodingUTF8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, encoding, ok := decodeText(tc.data)
			if !ok {
				t.Fatalf("应当识别为文本")
			}
			if text != tc.wantText {
				t.Fatalf("正文 = %q，期望 %q", text, tc.wantText)
			}
			if encoding != tc.wantEnc {
				t.Fatalf("编码 = %q，期望 %q", encoding, tc.wantEnc)
			}
		})
	}
}

// 二进制文件必须被挡住：GB18030 的映射范围很宽，只看"能否解码"会把
// 不少二进制误判成文本。含 NUL 是最可靠的否定信号。
func TestDecodeTextRejectsBinary(t *testing.T) {
	cases := map[string][]byte{
		"PNG 头":  {0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00},
		"NUL 填充": {0x41, 0x00, 0x42},
		"截断的双字节": {0x81},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := decodeText(data); ok {
				t.Fatalf("二进制被误判成了文本：%x", data)
			}
		})
	}
}

// 保存必须按原编码写回：把 GBK 的 CSV 改一行再保存，文件不能变成 UTF-8。
func TestEncodeTextRoundTrip(t *testing.T) {
	gp := mustHex(t, sampleGB18030)
	out, err := encodeText(sampleText, encodingGB18030)
	if err != nil {
		t.Fatalf("GB18030 编码失败：%v", err)
	}
	if !bytes.Equal(out, gp) {
		t.Fatalf("GB18030 写回字节不一致：%x，期望 %x", out, gp)
	}

	// UTF-8 带 BOM：读取时去掉了 BOM，写回时要补上。
	out, err = encodeText(sampleText, encodingUTF8BOM)
	if err != nil {
		t.Fatalf("UTF-8 BOM 编码失败：%v", err)
	}
	if !bytes.Equal(out, mustHex(t, sampleUTF8BOM)) {
		t.Fatalf("UTF-8 BOM 写回字节不一致：%x", out)
	}

	// UTF-16LE / BE：BOM 与字节序都要原样还原。
	for _, tc := range []struct {
		encoding string
		bom      []byte
		body     string
	}{
		{encodingUTF16LE, utf16LEBOM, sampleUTF16LE},
		{encodingUTF16BE, utf16BEBOM, sampleUTF16BE},
	} {
		out, err := encodeText(sampleText, tc.encoding)
		if err != nil {
			t.Fatalf("%s 编码失败：%v", tc.encoding, err)
		}
		want := append(append([]byte{}, tc.bom...), mustHex(t, tc.body)...)
		if !bytes.Equal(out, want) {
			t.Fatalf("%s 写回字节不一致：%x，期望 %x", tc.encoding, out, want)
		}
	}

	// 空编码按 UTF-8：老客户端不传这个字段。
	out, err = encodeText(sampleText, "")
	if err != nil {
		t.Fatalf("默认编码失败：%v", err)
	}
	if string(out) != sampleText {
		t.Fatalf("默认编码应当原样写出 UTF-8：%q", string(out))
	}
}

// 分块读取必然会在窗口末尾切断字符：解出来的完整字符数必须往回让，
// 否则 nextOffset 会落在半个字符上，下一块开头就是乱码。
func TestDecodeWindowTrimsPartialCharacter(t *testing.T) {
	utf8Text := []byte("你好世界")
	cases := []struct {
		name     string
		data     []byte
		encoding string
		wantText string
		wantUsed int
	}{
		{"UTF-8 切断三字节字符", utf8Text[:5], encodingUTF8, "你", 3},
		{"UTF-8 完整", utf8Text, encodingUTF8, "你好世界", 12},
		{"GB18030 切断双字节字符", mustHex(t, "c4e3ba"), encodingGB18030, "你", 2},
		{"UTF-16LE 奇数字节", mustHex(t, "604f7d59")[:3], encodingUTF16LE, "你", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, used, err := decodeWindow(tc.data, tc.encoding, false)
			if err != nil {
				t.Fatalf("解码失败：%v", err)
			}
			if text != tc.wantText || used != tc.wantUsed {
				t.Fatalf("解码结果 (%q,%d)，期望 (%q,%d)", text, used, tc.wantText, tc.wantUsed)
			}
		})
	}
}

// 编码识别拿到的样本通常也被截断；逐次去掉尾部字节后仍要能认出来。
func TestDetectEncodingWithTruncatedSample(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"UTF-8 截断", []byte("你好世界")[:5], encodingUTF8},
		{"GB18030 截断", mustHex(t, "c4e3bac3cac0bde7")[:5], encodingGB18030},
		{"UTF-16LE BOM + 截断", append(append([]byte{}, utf16LEBOM...), mustHex(t, "604f7d59")[:3]...), encodingUTF16LE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := detectEncoding(tc.data)
			if !ok || got != tc.want {
				t.Fatalf("识别结果 (%q,%v)，期望 %q", got, ok, tc.want)
			}
		})
	}
}
