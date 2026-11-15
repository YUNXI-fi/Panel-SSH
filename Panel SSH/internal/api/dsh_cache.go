package api

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"panelssh/internal/dsh"
)

// dshAssetCache 缓存 dsh 前端的静态资源。
//
// 为什么需要它：dsh 0.2.2 的首屏要拉 11.8MB（gzip 后约 3.2MB）的 JS/插件包，
// 而远端主机的 SSH 隧道可能只有几十 KB/s。浏览器会并发开 6 条连接，
// 慢链路上多条并发通道会被对端直接掐断，结果是页面永远白屏。
//
// 这里的做法是：dsh 就绪后在面板侧按顺序把入口 HTML 引用的资源抓一遍并落盘，
// 浏览器随后拿到的都是本地文件，既不再受链路速度影响，也不会触发并发断连。
// 资源名带内容哈希（assets/index-xxxx.js、plugins/...?rev=xxx），天然不可变，
// 因此缓存不需要失效逻辑，dsh 升级后自然换一套新文件名。
const (
	// dshCacheDirName 是缓存根目录在数据目录下的名字。
	dshCacheDirName = "dsh-cache"
	// dshWarmWait 是入口请求等待预抓取完成的上限。
	// 等待超时也要把入口页发出去，否则 dsh 起不来时用户会永远看不到报错。
	dshWarmWait = 5 * time.Minute
	// dshWarmRetry 是单个资源的抓取重试次数。
	dshWarmRetry = 3
	// dshAssetIdleTimeout 是单次读取允许的最长静默时间。
	// 用"空闲超时"而不是总时长：大的插件包在慢链路上可能要读一两分钟，
	// 但只要还在出数据就不该被打断；反过来，完全卡住时必须尽快放弃重试。
	dshAssetIdleTimeout = 45 * time.Second
	// dshWarmMaxAssetBytes 限制单个资源的缓存大小，防止异常响应把磁盘写满。
	dshWarmMaxAssetBytes = 32 << 20
)

// dshAssetMeta 是缓存文件旁的元数据，用于回放响应头。
type dshAssetMeta struct {
	ContentType     string `json:"contentType"`
	ContentEncoding string `json:"contentEncoding"`
}

// dshAssetCache 管理按主机分目录的静态资源缓存与预抓取状态。
type dshAssetCache struct {
	dir string

	mu      sync.Mutex
	warming map[int64]chan struct{}
	warmed  map[int64]time.Time
}

func newDshAssetCache(dataDir string) *dshAssetCache {
	return &dshAssetCache{
		dir:     filepath.Join(dataDir, dshCacheDirName),
		warming: map[int64]chan struct{}{},
		warmed:  map[int64]time.Time{},
	}
}

// cacheableDshPath 判断请求路径是否属于可缓存的静态资源。
//
// 只缓存 /assets/ 与 /plugins/ 下的内容：它们由构建产物生成、带哈希，
// 是首屏体积的全部来源。入口 HTML 与 /api 必须实时转发。
func cacheableDshPath(path string) bool {
	return strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/plugins/")
}

// key 生成缓存文件名。同一主机内 path+query 唯一确定一份内容。
func (c *dshAssetCache) key(hostID int64, path, query string) string {
	sum := sha256.Sum256([]byte(path + "\x00" + query))
	return hex.EncodeToString(sum[:])
}

func (c *dshAssetCache) paths(hostID int64, key string) (bin, meta string) {
	dir := filepath.Join(c.dir, strconv.FormatInt(hostID, 10))
	return filepath.Join(dir, key+".bin"), filepath.Join(dir, key+".json")
}

// has 判断某个资源是否已经在缓存里。
func (c *dshAssetCache) has(hostID int64, path, query string) bool {
	_, metaPath := c.paths(hostID, c.key(hostID, path, query))
	_, err := os.Stat(metaPath)
	return err == nil
}

// serve 尝试用缓存响应请求，命中时返回 true。
func (c *dshAssetCache) serve(w http.ResponseWriter, r *http.Request, hostID int64, path string) bool {
	key := c.key(hostID, path, r.URL.RawQuery)
	bin, metaPath := c.paths(hostID, key)

	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var meta dshAssetMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	f, err := os.Open(bin)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}

	if meta.ContentEncoding == "gzip" && !acceptsGzip(r) {
		// 浏览器都接受 gzip；只有 curl 这类客户端才会走到解压分支。
		zr, err := gzip.NewReader(f)
		if err != nil {
			return false
		}
		defer zr.Close()
		if meta.ContentType != "" {
			w.Header().Set("Content-Type", meta.ContentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, zr)
		return true
	}

	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	if meta.ContentEncoding != "" {
		w.Header().Set("Content-Encoding", meta.ContentEncoding)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// 内容寻址的资源可以放心让浏览器长期缓存，减少重复下载。
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
	return true
}

// store 把一次完整响应写入缓存。data 必须已经是完整响应体。
func (c *dshAssetCache) store(hostID int64, path, query string, header http.Header, data []byte) error {
	key := c.key(hostID, path, query)
	bin, metaPath := c.paths(hostID, key)
	dir := filepath.Dir(bin)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	meta := dshAssetMeta{
		ContentType:     header.Get("Content-Type"),
		ContentEncoding: header.Get("Content-Encoding"),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// 先写元数据再换正式文件：反过来会出现"有 bin 没 meta"的半截状态。
	metaTmp := metaPath + ".tmp"
	if err := os.WriteFile(metaTmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, bin); err != nil {
		_ = os.Remove(metaTmp)
		return err
	}
	return os.Rename(metaTmp, metaPath)
}

// capture 在把响应流向浏览器的同时旁路写一份缓存。
//
// 顺序抓取（warm）是主路径；这里兜住"预抓取还没跑完、浏览器已经在请求"的情况，
// 让即便慢也算成功的那一次加载直接变成后续的缓存。
func (c *dshAssetCache) capture(hostID int64, path, query string, header http.Header, body io.ReadCloser) io.ReadCloser {
	dir := filepath.Join(c.dir, strconv.FormatInt(hostID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return body
	}
	tmp, err := os.CreateTemp(dir, "tmp-*")
	if err != nil {
		return body
	}
	return &cacheWriter{
		cache: c, hostID: hostID, path: path, query: query, header: header,
		body: body, tmp: tmp,
	}
}

type cacheWriter struct {
	cache  *dshAssetCache
	hostID int64
	path   string
	query  string
	header http.Header
	body   io.ReadCloser
	tmp    *os.File

	size int64
	done bool
}

func (w *cacheWriter) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	if n > 0 {
		w.size += int64(n)
		if w.size <= dshWarmMaxAssetBytes {
			_, _ = w.tmp.Write(p[:n])
		}
	}
	if err == io.EOF {
		w.finish()
	}
	return n, err
}

func (w *cacheWriter) Close() error {
	if !w.done {
		_ = w.tmp.Close()
		_ = os.Remove(w.tmp.Name())
	}
	return w.body.Close()
}

// finish 在读满整个响应后原子落盘。
func (w *cacheWriter) finish() {
	if w.done {
		return
	}
	w.done = true
	if w.size > dshWarmMaxAssetBytes {
		_ = w.tmp.Close()
		_ = os.Remove(w.tmp.Name())
		return
	}
	if err := w.tmp.Close(); err != nil {
		_ = os.Remove(w.tmp.Name())
		return
	}
	bin, metaPath := w.cache.paths(w.hostID, w.cache.key(w.hostID, w.path, w.query))
	meta := dshAssetMeta{
		ContentType:     w.header.Get("Content-Type"),
		ContentEncoding: w.header.Get("Content-Encoding"),
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		_ = os.Remove(w.tmp.Name())
		return
	}
	if err := os.WriteFile(metaPath+".tmp", raw, 0o600); err != nil {
		_ = os.Remove(w.tmp.Name())
		return
	}
	if err := os.Rename(w.tmp.Name(), bin); err != nil {
		_ = os.Remove(w.tmp.Name())
		_ = os.Remove(metaPath + ".tmp")
		return
	}
	_ = os.Rename(metaPath+".tmp", metaPath)
}

// startWarm 启动该主机的预抓取；force 为 true 时即使刚抓过也重跑
// （dsh 重启或升级后资源哈希会变）。
func (c *dshAssetCache) startWarm(s *Server, hostID int64, force bool) <-chan struct{} {
	c.mu.Lock()
	if ch, ok := c.warming[hostID]; ok {
		c.mu.Unlock()
		return ch
	}
	if !force {
		if at, ok := c.warmed[hostID]; ok && time.Since(at) < 10*time.Minute {
			done := make(chan struct{})
			close(done)
			c.mu.Unlock()
			return done
		}
	}
	done := make(chan struct{})
	c.warming[hostID] = done
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.warming, hostID)
			c.warmed[hostID] = time.Now()
			c.mu.Unlock()
			close(done)
		}()
		c.warm(s, hostID)
	}()
	return done
}

var dshHTMLRefPattern = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// warm 顺序抓取入口页引用的全部静态资源。
//
// 顺序而不是并发：目标链路对并发通道很敏感，实测 4 条以上就会断连。
// 反正这是一次性的后台动作，慢一点没有关系。
func (c *dshAssetCache) warm(s *Server, hostID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), dshWarmWait)
	defer cancel()

	dsh.AppendLog(s.dshLogPath(hostID), "INFO", "开始预抓取 dsh 静态资源")

	transport := s.dshWarmTransport(hostID)
	// 总时长交给 warm 的 ctx 和读取层的空闲超时管，这里不设整体上限。
	client := &http.Client{Transport: transport}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return
	}
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	endpoint, token, ok := s.waitDshEndpoint(ctx, hostID)
	if !ok {
		dsh.AppendLog(s.dshLogPath(hostID), "WARN", "预抓取跳过：dsh 未就绪")
		return
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return
	}

	// 入口页需要先拿 dsh 的浏览器会话：token 兑换一次，后续请求带上 Cookie。
	if token != "" {
		exchange := endpoint + "/?token=" + url.QueryEscape(token)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, exchange, nil)
		req.Host = base.Host
		if resp, err := client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
		}
	}

	htmlBody, _, ok := c.fetch(ctx, client, base.Host, "/", false)
	if !ok {
		dsh.AppendLog(s.dshLogPath(hostID), "WARN", "预抓取失败：无法读取 dsh 入口页")
		return
	}
	refs := extractDshRefs(string(htmlBody))
	stored := 0
	for _, ref := range refs {
		u, _ := url.Parse(ref)
		// 已经有缓存就跳过：重复启动 dsh 时不该把几 MB 资源再传一遍。
		if u != nil && c.has(hostID, u.Path, u.RawQuery) {
			stored++
			continue
		}
		got := false
		for attempt := 0; attempt < dshWarmRetry; attempt++ {
			body, header, ok := c.fetch(ctx, client, base.Host, ref, true)
			if ok {
				if u != nil {
					if err := c.store(hostID, u.Path, u.RawQuery, header, body); err == nil {
						stored++
					}
				}
				got = true
				break
			}
			select {
			case <-ctx.Done():
				dsh.AppendLog(s.dshLogPath(hostID), "WARN",
					fmt.Sprintf("预抓取中断：%d/%d 个资源已缓存", stored, len(refs)))
				return
			case <-time.After(2 * time.Second):
			}
		}
		if !got {
			dsh.AppendLog(s.dshLogPath(hostID), "WARN", "预抓取资源失败："+ref)
		}
	}
	dsh.AppendLog(s.dshLogPath(hostID), "INFO",
		fmt.Sprintf("预抓取完成：%d/%d 个资源已缓存", stored, len(refs)))
}

// fetch 取一个上游资源；gzip 为 true 时要求压缩，减小链路传输量。
func (c *dshAssetCache) fetch(ctx context.Context, client *http.Client, host, path string, gzip bool) ([]byte, http.Header, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return nil, nil, false
	}
	req.Host = host
	if gzip {
		req.Header.Set("Accept-Encoding", "gzip")
	} else {
		req.Header.Set("Accept-Encoding", "identity")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, nil, false
	}
	idle := &idleBody{rc: resp.Body, timeout: dshAssetIdleTimeout}
	idle.timer = time.AfterFunc(dshAssetIdleTimeout, func() { _ = resp.Body.Close() })
	defer idle.Close()
	body, err := io.ReadAll(io.LimitReader(idle, dshWarmMaxAssetBytes+1))
	if err != nil || len(body) > dshWarmMaxAssetBytes {
		return nil, nil, false
	}
	return body, resp.Header, true
}

// idleBody 在每次成功读取后重置计时器；静默超过上限时关闭底层连接，
// 让阻塞中的 Read 立刻返回错误。
type idleBody struct {
	rc      io.ReadCloser
	timer   *time.Timer
	timeout time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	if b.timer != nil {
		b.timer.Reset(b.timeout)
	}
	return b.rc.Read(p)
}

func (b *idleBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	return b.rc.Close()
}

// extractDshRefs 从入口 HTML 里取出静态资源地址。
func extractDshRefs(page string) []string {
	var refs []string
	seen := map[string]bool{}
	for _, m := range dshHTMLRefPattern.FindAllStringSubmatch(page, -1) {
		ref := html.UnescapeString(m[1])
		// 只关心静态资源；./ 前缀是相对路径的写法，统一去掉。
		ref = strings.TrimPrefix(ref, "./")
		if !cacheableDshPath("/" + ref) {
			continue
		}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, "/"+ref)
	}
	return refs
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// waitDshEndpoint 等 dsh 就绪并返回它的回环地址与一次性令牌。
func (s *Server) waitDshEndpoint(ctx context.Context, hostID int64) (string, string, bool) {
	sup := s.dsh.Supervisor(hostID)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if endpoint, token, ok := sup.Endpoint(); ok {
			return endpoint, token, true
		}
		select {
		case <-ctx.Done():
			return "", "", false
		case <-ticker.C:
		}
	}
}

// dshWarmTransport 给预抓取用的传输层：与反代一样走常驻 SSH 隧道。
func (s *Server) dshWarmTransport(hostID int64) http.RoundTripper {
	if hostID == LocalHostID {
		return http.DefaultTransport
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return s.tunnels.dialContext(ctx, hostID, network, addr)
		},
		IdleConnTimeout: 30 * time.Second,
		// 顺序抓取，一条连接足够；这里显式限流，避免预抓取本身触发并发断连。
		MaxConnsPerHost:     1,
		MaxIdleConnsPerHost: 1,
	}
}
