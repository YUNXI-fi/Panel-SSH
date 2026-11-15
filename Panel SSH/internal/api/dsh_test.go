package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// dshTestPrefix 是测试里用的主机前缀。
//
// 刻意写成字面量而不是 dshPrefixFor(7)：断言不该跟着实现一起漂 ——
// 如果实现把主机号拼错了，用同一个函数算期望值的测试是发现不了的。
const dshTestPrefix = "/dsh/7"

func dshHTMLResponse(body string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Body:          io.NopCloser(strings.NewReader(body)),
		Header:        http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		ContentLength: int64(len(body)),
	}
}

// 正常体量的入口文档应当被改写：base 路径与 /plugins/ 都要挂到这台主机的前缀之下。
func TestRewriteDshHTMLRewritesSmallEntry(t *testing.T) {
	resp := dshHTMLResponse(`<!doctype html><html><head><base href="/"><title>dsh</title></head>` +
		`<body><script src="/plugins/x.js"></script></body></html>`)

	if err := rewriteDshHTML(resp, dshTestPrefix); err != nil {
		t.Fatalf("改写失败：%v", err)
	}

	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取改写结果失败：%v", err)
	}
	text := string(page)

	if !strings.Contains(text, `<base href="`+dshTestPrefix+`/">`) {
		t.Error("base 路径未被改写")
	}
	if !strings.Contains(text, `"`+dshTestPrefix+`/plugins/`) {
		t.Error("插件资源路径未被改写")
	}
	if !strings.Contains(text, "location.origin") {
		t.Error("未注入同源地址修正脚本")
	}
	// 注入脚本里的前缀常量也必须跟着换。它决定 dsh 运行时拼出的 /api 地址挂到哪，
	// 只断言"脚本存在"是抓不到这个的 —— 一段仍然指向旧前缀的脚本同样"存在"。
	if !strings.Contains(text, "`"+dshTestPrefix+"`") {
		t.Error("同源地址修正脚本里的前缀常量未被替换")
	}
	// Content-Length 必须跟着新正文走，否则浏览器会截断或挂住。
	if resp.ContentLength != int64(len(page)) {
		t.Fatalf("ContentLength 未同步：声明 %d，实际 %d", resp.ContentLength, len(page))
	}
	headerLen := resp.Header.Get("Content-Length")
	if headerLen == "" {
		t.Fatal("响应头缺少 Content-Length")
	}
	if headerLen != strconv.Itoa(len(page)) {
		t.Fatalf("响应头 Content-Length 与实际正文不符：%s vs %d", headerLen, len(page))
	}
}

// 超过上限时必须报错，而不是把截断后的半截 HTML 当成改写结果发出去。
// 旧实现静默截断，用户会看到一个结构被砍断的页面且无从判断原因。
func TestRewriteDshHTMLRejectsOversizedEntry(t *testing.T) {
	oversized := strings.Repeat("a", maxDshEntryHTML+64)
	resp := dshHTMLResponse(oversized)

	err := rewriteDshHTML(resp, dshTestPrefix)
	if err == nil {
		t.Fatal("超过上限的入口文档本应报错，而不是静默截断")
	}
	if !strings.Contains(err.Error(), "超过") {
		t.Fatalf("错误信息应说明原因，实际：%v", err)
	}
}

// 恰好等于上限的文档仍应被正常处理（边界不能误伤）。
func TestRewriteDshHTMLAcceptsExactlyAtLimit(t *testing.T) {
	filler := strings.Repeat("a", maxDshEntryHTML-len("<head></head>"))
	resp := dshHTMLResponse("<head></head>" + filler)

	if err := rewriteDshHTML(resp, dshTestPrefix); err != nil {
		t.Fatalf("恰好达到上限的文档不该被拒绝：%v", err)
	}
}

// 前缀由主机号决定：宿主机是 0，其余主机用自己的 ID。
// 两侧必须能共存，否则两台机器会抢同一个路径。
func TestDshPrefixFor(t *testing.T) {
	if got := dshPrefixFor(0); got != "/dsh/0" {
		t.Fatalf("宿主机前缀应为 /dsh/0，实际 %s", got)
	}
	if got := dshPrefixFor(42); got != "/dsh/42" {
		t.Fatalf("主机 42 的前缀应为 /dsh/42，实际 %s", got)
	}
}

// 重定向改写只该给"本站根路径、且尚未带前缀"的地址补前缀：
// 补错就是一次死循环（补给一个已经带前缀的地址，dsh 会再跳回来）。
func TestRewriteDshLocation(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"根路径补前缀", "/", "/dsh/7/"},
		{"子路径补前缀", "/login", "/dsh/7/login"},
		{"已带前缀不动", "/dsh/7/foo", "/dsh/7/foo"},
		{"绝对地址不动", "https://example.com/", "https://example.com/"},
		{"空值不动", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteDshLocation(c.in, dshTestPrefix); got != c.want {
				t.Fatalf("rewriteDshLocation(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}
