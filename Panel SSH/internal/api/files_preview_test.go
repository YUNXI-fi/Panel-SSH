package api

import "testing"

// 预览类型表决定"哪些文件点开能看"：漏一个只是退化成下载，
// 多一个（把不支持的格式放进来）会让浏览器渲染成乱码或直接下载，
// 用户看到的是"点了没反应"。
func TestPreviewContentType(t *testing.T) {
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"a.PNG", "image/png", true},
		{"photo.JPEG", "image/jpeg", true},
		{"icon.svg", "image/svg+xml", true},
		{"clip.mp4", "video/mp4", true},
		{"song.flac", "audio/flac", true},
		{"manual.pdf", "application/pdf", true},
		// 文本走 file/text，不该被这个接口接住。
		{"main.go", "", false},
		{"README.md", "", false},
		{"page.html", "", false},
		{"archive.tar.gz", "", false},
		{"noext", "", false},
	}
	for _, tc := range cases {
		got, ok := previewContentType(tc.name)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("previewContentType(%q) = (%q, %v)，期望 (%q, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
