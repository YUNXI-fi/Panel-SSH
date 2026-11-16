package appstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultIndexURL 是应用仓库的索引地址。
//
// 走 GitHub raw 而不是 API：raw 不走 API 限额，且同一份文件可以直接
// 用浏览器打开核对。需要内网镜像时用 PANELSSH_APP_INDEX_URL 覆盖。
const DefaultIndexURL = "https://raw.githubusercontent.com/YUNXI-fi/Panel-SSH/main/apps/index.json"

// 索引与单个文件的上限。应用包只放声明式资源，超过这个量级基本意味着
// 打包方式错了（例如把二进制塞进了 config/）。
const (
	maxIndexBytes = 4 << 20
	maxFileBytes  = 2 << 20
	indexCacheTTL = 10 * time.Minute
)

var (
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	filePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Store 负责索引拉取、应用包安装与本地读取。
type Store struct {
	dir      string
	indexURL string
	localDir string
	http     *http.Client

	mu       sync.Mutex
	index    *Index
	indexAt  time.Time
	installM sync.Mutex
}

// NewStore 创建应用仓库。dir 是 data/apps；indexURL 为空时用默认地址。
//
// PANELSSH_APP_SOURCE_DIR 非空时直接从本地目录读取（目录结构与 GitHub
// 上的 apps/ 一致），用于离线部署与端到端测试。
func NewStore(dir, indexURL string) *Store {
	if indexURL == "" {
		indexURL = os.Getenv("PANELSSH_APP_INDEX_URL")
	}
	if indexURL == "" {
		indexURL = DefaultIndexURL
	}
	return &Store{
		dir:      dir,
		indexURL: indexURL,
		localDir: os.Getenv("PANELSSH_APP_SOURCE_DIR"),
		http:     &http.Client{Timeout: 20 * time.Second},
	}
}

// Dir 返回本地应用包目录。
func (s *Store) Dir() string { return s.dir }

// IndexURL 返回当前使用的索引地址，供应用中心展示来源。
func (s *Store) IndexURL() string {
	if s.localDir != "" {
		return "file://" + s.localDir
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.indexURL
}

// SetIndexURL 在运行期切换仓库源。
//
// 切换后必须丢掉旧索引：旧源的包清单、版本与摘要都不再适用，
// 继续沿用会把用户引到错误的下载地址。
func (s *Store) SetIndexURL(rawURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexURL = rawURL
	s.index = nil
	s.indexAt = time.Time{}
	_ = os.Remove(s.indexCachePath())
}

// ValidateIndexURL 校验仓库源地址。
//
// 只接受 http/https：file:// 会让面板去读本机任意路径，
// 而这本来该由 PANELSSH_APP_SOURCE_DIR 在部署期显式开启。
func ValidateIndexURL(rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return errors.New("仓库源不是合法地址")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("仓库源必须是 http 或 https 地址")
	}
	if parsed.Host == "" {
		return errors.New("仓库源缺少主机名")
	}
	if !strings.HasSuffix(parsed.Path, ".json") {
		return errors.New("仓库源应指向 index.json")
	}
	return nil
}

// Index 读取应用索引。force 为真时忽略缓存重新拉取；拉取失败但有缓存时
// 返回缓存并把 fromCache 置真，让界面能区分"最新"与"离线可用"。
func (s *Store) Index(ctx context.Context, force bool) (*Index, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !force && s.index != nil && time.Since(s.indexAt) < indexCacheTTL {
		return s.index, true, nil
	}

	idx, err := s.fetchIndex(ctx)
	if err == nil {
		s.index = idx
		s.indexAt = time.Now()
		_ = s.writeIndexCache(idx)
		return idx, false, nil
	}

	if s.index != nil {
		return s.index, true, nil
	}
	if cached := s.readIndexCache(); cached != nil {
		s.index = cached
		s.indexAt = time.Now()
		return cached, true, nil
	}
	return nil, false, err
}

func (s *Store) fetchIndex(ctx context.Context) (*Index, error) {
	var raw []byte
	var err error
	if s.localDir != "" {
		raw, err = os.ReadFile(filepath.Join(s.localDir, "index.json"))
	} else {
		raw, err = s.get(ctx, s.indexURL, maxIndexBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("拉取应用索引失败：%w", err)
	}
	var idx Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("应用索引不是合法 JSON：%w", err)
	}
	if err := validateIndex(&idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

func (s *Store) indexCachePath() string { return filepath.Join(s.dir, "index.json") }

func (s *Store) readIndexCache() *Index {
	raw, err := os.ReadFile(s.indexCachePath())
	if err != nil {
		return nil
	}
	var idx Index
	if json.Unmarshal(raw, &idx) != nil || validateIndex(&idx) != nil {
		return nil
	}
	return &idx
}

func (s *Store) writeIndexCache(idx *Index) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.indexCachePath(), raw, 0o644)
}

// Entry 在索引里按 slug 查找。
func (s *Store) Entry(slug string) (*Entry, error) {
	idx, _, err := s.Index(context.Background(), false)
	if err != nil {
		return nil, err
	}
	for i := range idx.Apps {
		if idx.Apps[i].Slug == slug {
			return &idx.Apps[i], nil
		}
	}
	return nil, fmt.Errorf("应用仓库里没有 %s", slug)
}

// Installed 读取一个已安装应用包的清单与界面配置。
func (s *Store) Installed(slug string) (*Manifest, *UI, error) {
	if !slugPattern.MatchString(slug) {
		return nil, nil, errors.New("应用标识不合法")
	}
	dir := filepath.Join(s.dir, slug)
	manifest, err := readManifest(filepath.Join(dir, "app.json"))
	if err != nil {
		return nil, nil, err
	}
	ui, err := readUI(filepath.Join(dir, "ui.json"))
	if err != nil {
		// ui.json 缺失时退回通用分组，不把整个应用判成损坏。
		ui = nil
	}
	return manifest, ui, nil
}

// InstalledVersion 返回已安装版本；未安装时返回空串。
func (s *Store) InstalledVersion(slug string) string {
	manifest, _, err := s.Installed(slug)
	if err != nil {
		return ""
	}
	return manifest.Version
}

// ListInstalled 列出本地已安装的应用包。
func (s *Store) ListInstalled() []*Manifest {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	out := make([]*Manifest, 0)
	for _, entry := range entries {
		// "." 开头是缓存/临时目录，"_" 开头是模板与草稿，都不算已安装应用。
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		manifest, err := readManifest(filepath.Join(s.dir, entry.Name(), "app.json"))
		if err != nil {
			continue
		}
		out = append(out, manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// Install 下载并安装（或更新）一个应用包。
//
// 先写到临时目录、逐个文件校验 SHA-256、再整体替换旧目录：
// 校验失败或中途断网时，上一版仍然可用。
func (s *Store) Install(ctx context.Context, slug string) (*Manifest, *UI, error) {
	if !slugPattern.MatchString(slug) {
		return nil, nil, errors.New("应用标识不合法")
	}
	entry, err := s.Entry(slug)
	if err != nil {
		return nil, nil, err
	}
	if len(entry.Files) == 0 {
		return nil, nil, fmt.Errorf("应用 %s 的索引没有声明任何文件", slug)
	}

	s.installM.Lock()
	defer s.installM.Unlock()

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, nil, err
	}
	tmp, err := os.MkdirTemp(s.dir, ".install-"+slug+"-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)

	for _, file := range entry.Files {
		if err := validateFileRef(file); err != nil {
			return nil, nil, fmt.Errorf("%s/%s：%w", slug, file.Path, err)
		}
		raw, err := s.fetchFile(ctx, slug, file.Path)
		if err != nil {
			return nil, nil, err
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != strings.ToLower(file.SHA256) {
			return nil, nil, fmt.Errorf("%s/%s 校验失败：内容与索引不一致", slug, file.Path)
		}
		dest := filepath.Join(tmp, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(dest, raw, 0o644); err != nil {
			return nil, nil, err
		}
	}

	manifest, err := readManifest(filepath.Join(tmp, "app.json"))
	if err != nil {
		return nil, nil, err
	}
	if manifest.Slug != slug {
		return nil, nil, fmt.Errorf("应用包标识不一致：索引=%s，清单=%s", slug, manifest.Slug)
	}
	ui, _ := readUI(filepath.Join(tmp, "ui.json"))

	target := filepath.Join(s.dir, slug)
	// exFAT 上不能把目录 rename 到已存在的目录上，先移除旧版本。
	// 旧版本已被内存中的清单使用完毕；这里失败时调用方会提示重试。
	if err := os.RemoveAll(target); err != nil {
		return nil, nil, err
	}
	if err := os.Rename(tmp, target); err != nil {
		return nil, nil, err
	}
	return manifest, ui, nil
}

// Uninstall 删除本地应用包。目标机上的软件与配置不会被动到。
func (s *Store) Uninstall(slug string) error {
	if !slugPattern.MatchString(slug) {
		return errors.New("应用标识不合法")
	}
	return os.RemoveAll(filepath.Join(s.dir, slug))
}

// Icon 读取应用图标。
//
// 已安装时直接用包内文件；未安装时按索引拉取一次并缓存，
// 让应用中心在安装前也能正常展示图标。
func (s *Store) Icon(ctx context.Context, slug string) ([]byte, string, error) {
	if !slugPattern.MatchString(slug) {
		return nil, "", errors.New("应用标识不合法")
	}
	cacheDir := filepath.Join(s.dir, ".cache", "icons")
	if raw, err := os.ReadFile(filepath.Join(s.dir, slug, "icon.svg")); err == nil {
		return raw, "image/svg+xml", nil
	}
	if raw, err := os.ReadFile(filepath.Join(s.dir, slug, "icon.png")); err == nil {
		return raw, "image/png", nil
	}
	if raw, err := os.ReadFile(filepath.Join(cacheDir, slug+".svg")); err == nil {
		return raw, "image/svg+xml", nil
	}

	entry, err := s.Entry(slug)
	if err != nil {
		return nil, "", err
	}
	iconPath := entry.Icon
	if iconPath == "" {
		iconPath = "icon.svg"
	}
	raw, err := s.fetchFile(ctx, slug, iconPath)
	if err != nil {
		return nil, "", err
	}
	for _, file := range entry.Files {
		if file.Path == iconPath {
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != strings.ToLower(file.SHA256) {
				return nil, "", errors.New("图标校验失败")
			}
			break
		}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err == nil {
		_ = writeFileAtomic(filepath.Join(cacheDir, slug+".svg"), raw, 0o644)
	}
	return raw, "image/svg+xml", nil
}

func (s *Store) fetchFile(ctx context.Context, slug, path string) ([]byte, error) {
	if s.localDir != "" {
		raw, err := os.ReadFile(filepath.Join(s.localDir, slug, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		if len(raw) > maxFileBytes {
			return nil, fmt.Errorf("%s/%s 超过单文件上限", slug, path)
		}
		return raw, nil
	}
	return s.get(ctx, joinSourceURL(s.indexURL, slug, path), maxFileBytes)
}

// joinSourceURL 把索引地址的目录与 slug/相对路径拼成下载地址。
func joinSourceURL(indexURL, slug, path string) string {
	base := indexURL
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[:idx]
	}
	return strings.TrimSuffix(base, "/") + "/" + url.PathEscape(slug) + "/" +
		strings.Join(escapeSegments(path), "/")
}

func escapeSegments(path string) []string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return parts
}

func (s *Store) get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "PanelSSH/1.0")
	res, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("响应超过 %d 字节上限", limit)
	}
	return raw, nil
}

// ValidateManifest 供生成器与校验脚本在发布前检查 app.json。
func ValidateManifest(m *Manifest) error { return validateManifest(m) }

// ValidateIndex 供生成器与校验脚本在发布前检查 apps/index.json。
func ValidateIndex(idx *Index) error { return validateIndex(idx) }

func readManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("app.json 不是合法 JSON：%w", err)
	}
	if err := validateManifest(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func readUI(path string) (*UI, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ui UI
	if err := json.Unmarshal(raw, &ui); err != nil {
		return nil, err
	}
	if ui.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("ui.json 版本不支持：%d", ui.SchemaVersion)
	}
	return &ui, nil
}

func writeFileAtomic(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ---------------- 校验 ----------------

func validateIndex(idx *Index) error {
	if idx.SchemaVersion != SchemaVersion {
		return fmt.Errorf("应用索引版本不支持：%d（面板支持 %d）", idx.SchemaVersion, SchemaVersion)
	}
	if len(idx.Apps) == 0 {
		return errors.New("应用索引为空")
	}
	seen := map[string]bool{}
	for i := range idx.Apps {
		entry := &idx.Apps[i]
		if !slugPattern.MatchString(entry.Slug) {
			return fmt.Errorf("应用标识不合法：%q", entry.Slug)
		}
		if seen[entry.Slug] {
			return fmt.Errorf("应用索引里 %s 重复", entry.Slug)
		}
		seen[entry.Slug] = true
		if strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.Version) == "" {
			return fmt.Errorf("%s 缺少名称或版本", entry.Slug)
		}
		if entry.Manifest == "" || entry.Icon == "" {
			return fmt.Errorf("%s 缺少 app.json 或图标声明", entry.Slug)
		}
		hasManifest := false
		hasIcon := false
		hasUI := entry.UI == ""
		for _, file := range entry.Files {
			if err := validateFileRef(file); err != nil {
				return fmt.Errorf("%s：%w", entry.Slug, err)
			}
			switch file.Path {
			case entry.Manifest:
				hasManifest = true
			case entry.Icon:
				hasIcon = true
			case entry.UI:
				hasUI = true
			}
		}
		if !hasManifest {
			return fmt.Errorf("%s 的文件列表里没有 %s", entry.Slug, entry.Manifest)
		}
		if !hasIcon {
			return fmt.Errorf("%s 的文件列表里没有图标 %s", entry.Slug, entry.Icon)
		}
		if !hasUI {
			return fmt.Errorf("%s 的文件列表里没有界面配置 %s", entry.Slug, entry.UI)
		}
	}
	return nil
}

func validateFileRef(file FileRef) error {
	if !filePattern.MatchString(file.Path) || strings.Contains(file.Path, "..") {
		return fmt.Errorf("文件路径不合法：%q", file.Path)
	}
	if !hashPattern.MatchString(strings.ToLower(file.SHA256)) {
		return fmt.Errorf("%s 的 SHA-256 不合法", file.Path)
	}
	return nil
}

func validateManifest(m *Manifest) error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%s 的 app.json 版本不支持：%d", m.Slug, m.SchemaVersion)
	}
	if !slugPattern.MatchString(m.Slug) {
		return fmt.Errorf("应用标识不合法：%q", m.Slug)
	}
	if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Version) == "" {
		return errors.New("app.json 缺少名称或版本")
	}
	if m.Icon == "" {
		return fmt.Errorf("%s 没有声明图标", m.Slug)
	}
	if len(m.Actions) == 0 {
		return fmt.Errorf("%s 没有声明任何动作", m.Slug)
	}
	ids := map[string]bool{}
	for i := range m.Actions {
		action := &m.Actions[i]
		if !slugPattern.MatchString(action.ID) {
			return fmt.Errorf("%s 的动作标识不合法：%q", m.Slug, action.ID)
		}
		if ids[action.ID] {
			return fmt.Errorf("%s 的动作 %s 重复", m.Slug, action.ID)
		}
		ids[action.ID] = true
		if strings.TrimSpace(action.Label) == "" {
			return fmt.Errorf("%s/%s 缺少名称", m.Slug, action.ID)
		}
		if action.Command == "" && !action.Builtin {
			return fmt.Errorf("%s/%s 没有命令模板", m.Slug, action.ID)
		}
		if action.TimeoutSec < 0 || action.TimeoutSec > 1800 {
			return fmt.Errorf("%s/%s 的 timeoutSec 超出范围", m.Slug, action.ID)
		}
		keys := map[string]bool{}
		for _, field := range action.Fields {
			if !slugPattern.MatchString(field.Key) {
				return fmt.Errorf("%s/%s 的字段标识不合法：%q", m.Slug, action.ID, field.Key)
			}
			if keys[field.Key] {
				return fmt.Errorf("%s/%s 的字段 %s 重复", m.Slug, action.ID, field.Key)
			}
			keys[field.Key] = true
			if field.Pattern != "" {
				if _, err := regexp.Compile(field.Pattern); err != nil {
					return fmt.Errorf("%s/%s 的字段 %s 正则不合法：%v", m.Slug, action.ID, field.Key, err)
				}
			}
		}
		if action.Command != "" {
			if err := validateTemplate(action.Command, keys); err != nil {
				return fmt.Errorf("%s/%s：%w", m.Slug, action.ID, err)
			}
		}
	}
	if m.DefaultAction != "" && !ids[m.DefaultAction] {
		return fmt.Errorf("%s 的默认动作 %s 不存在", m.Slug, m.DefaultAction)
	}
	for _, config := range m.Config {
		if !strings.HasPrefix(config.Path, "/") && !strings.HasPrefix(config.Path, "~/") {
			return fmt.Errorf("%s 的配置路径必须是绝对路径：%s", m.Slug, config.Path)
		}
	}
	return nil
}

var placeholderPattern = regexp.MustCompile(`\{\{([a-z0-9-]{1,32})\}\}`)

// validateTemplate 确认模板里的占位符都来自已声明字段，并拒绝残留的 {{ }}。
func validateTemplate(template string, fields map[string]bool) error {
	seen := map[string]bool{}
	for _, match := range placeholderPattern.FindAllStringSubmatch(template, -1) {
		key := match[1]
		if !fields[key] {
			return fmt.Errorf("命令模板引用了未声明的字段 %s", key)
		}
		seen[key] = true
	}
	rest := placeholderPattern.ReplaceAllString(template, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return errors.New("命令模板存在无法解析的占位符")
	}
	return nil
}

// Render 把命令模板里的 {{key}} 替换成 shell 引号包裹后的值。
//
// 参数只来自表单，且已按字段声明做过类型/正则/长度校验；这里仍然统一
// 加引号，避免"某个包忘了写引号"变成命令注入。
func Render(template string, values map[string]string) string {
	return placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		key := placeholderPattern.FindStringSubmatch(match)[1]
		return ShellQuote(values[key])
	})
}

// ShellQuote 把值包成单引号字符串。
func ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// VersionGreater 判断 a 是否比 b 新。只解析数字段；解析失败时按字符串比较，
// 保证任何格式都能给出一个确定结果。
func VersionGreater(a, b string) bool {
	parse := func(v string) ([]int, bool) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		parts := strings.Split(v, ".")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, false
			}
			out = append(out, n)
		}
		return out, true
	}
	av, aok := parse(a)
	bv, bok := parse(b)
	if !aok || !bok {
		return strings.TrimSpace(a) > strings.TrimSpace(b)
	}
	for i := 0; i < len(av) || i < len(bv); i++ {
		var x, y int
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}
