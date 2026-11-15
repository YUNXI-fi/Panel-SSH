// Package web 负责把前端构建产物内嵌进二进制并提供访问。
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// dist 由 `npm run build` 产出（见 web/vite.config.ts 的 outDir）。
//
//go:embed all:dist
var distFS embed.FS

// asset 是预处理好的一份静态资源。
//
// 为什么在启动时就把 gzip 结果算出来：dist 是内嵌的、运行期不可变，
// 资源数量固定在一二十个，全部预压缩后常驻内存的代价可以忽略，
// 换来的是每个请求都不必再做压缩与磁盘读取。
//
// 之前的实现直接把文件明文写出去 —— 实测首屏传输 1461 KiB，
// 而其中主包就有 935 KiB 未压缩，这是这个面板最大的一笔性能开销。
type asset struct {
	raw         []byte
	gz          []byte // 不可压缩类型为 nil
	contentType string
	etag        string
}

type assetServer struct {
	files map[string]*asset
}

// compressible 判断该类型是否值得压缩。
// 图片/字体/压缩包本身已压缩，再压一次只会白耗 CPU 并略微变大。
func compressible(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".js", ".css", ".json", ".svg", ".map", ".txt", ".webmanifest":
		return true
	default:
		return false
	}
}

func contentTypeOf(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		// 文本类型补上 charset，避免中文界面在部分场景下乱码。
		if strings.HasPrefix(ct, "text/") || ct == "application/javascript" || ct == "image/svg+xml" {
			if !strings.Contains(ct, "charset") {
				ct += "; charset=utf-8"
			}
		}
		return ct
	}
	return "application/octet-stream"
}

func newAssetServer() (*assetServer, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}

	files := make(map[string]*asset)
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		item := &asset{
			raw:         raw,
			contentType: contentTypeOf(p),
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
		}
		if compressible(p) {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			if _, err := zw.Write(raw); err == nil && zw.Close() == nil {
				// 小于 1 KiB 的资源压完可能更大，这种情况直接不提供压缩版本。
				if buf.Len() < len(raw) {
					item.gz = buf.Bytes()
				}
			}
		}
		files[p] = item
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &assetServer{files: files}, nil
}

// Handler 返回静态资源处理器；未命中的路径回退到 index.html，
// 由前端路由接管（SPA fallback）。
func Handler() http.Handler {
	srv, err := newAssetServer()
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(srv.serve)
}

func (s *assetServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	item := s.files[name]
	spaFallback := false
	if item == nil {
		// 未命中的路径交给前端路由。
		item = s.files["index.html"]
		spaFallback = true
	}
	if item == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", item.contentType)
	w.Header().Set("Vary", "Accept-Encoding")
	if name == "index.html" || spaFallback {
		// 入口文档必须每次协商，否则发版后用户会一直拿到旧页面。
		// 用 no-store 而不是 no-cache：某些中间缓存会忽略 no-cache 仍然复用旧副本。
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
	} else if strings.HasPrefix(name, "assets/") {
		// 构建产物文件名带内容哈希，内容变了文件名必然变，可以长期缓存。
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("ETag", item.etag)

	// 协商缓存：命中则直接 304，连压缩结果都不用写。
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, item.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := item.raw
	if item.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		body = item.gz
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))

	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
