package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// http.ErrAbortHandler 是标准库约定的"安静中止"信号，不是故障：反代在拷贝响应体
// 失败时抛它，而最常见的触发场景就是浏览器提前断开（切换页面、取消加载）。
// net/http 收到它会直接掐掉连接、不做任何记录，因此这里也必须原样放行 ——
// 曾被兜底逻辑当成服务端错误，结果只是正常翻个页面就会在日志里刷出一段
// 带完整栈的 panic 记录，还附一条 superfluous WriteHeader 警告。
func TestWithRecoverLetsAbortHandlerThrough(t *testing.T) {
	handler := withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("中止信号应当原样向上抛，实际拿到 %v", rec)
		}
	}()

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	t.Fatal("panic 应当继续向上抛，不该被兜住")
}

// 其余 panic 仍要兜住：单条请求出错不该把整个进程带走。
func TestWithRecoverTurnsPanicIntoServerError(t *testing.T) {
	handler := withRecover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "服务内部错误") {
		t.Errorf("响应体 = %q，期望统一错误文案", rec.Body.String())
	}
}
