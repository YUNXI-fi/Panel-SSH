package plugin

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// maxArchiveFiles 限制安装包内条目数，挡住 zip 炸弹里的文件数量维度。
	maxArchiveFiles = 2000
	// maxFileSize 限制单文件解压体积。
	maxFileSize = 32 << 20
	// maxTotalSize 限制解压后总体积。
	maxTotalSize = 256 << 20
	// maxManifestSize 限制 plugin.json 体积。
	maxManifestSize = 1 << 20
	// stagingDir 是解压暂存区的目录名，位于插件目录内以保证最终 rename 是同设备操作。
	stagingDir = ".staging"
)

// Staged 是一次已解压完成、尚未生效的安装。
//
// 为什么把"解压"和"落位"拆成两步：旧实现先删掉同插件的其它版本目录再解压，
// 一旦后续写库失败，磁盘上旧版本已被删除、库里却仍指向旧版本，
// 结果是插件彻底打不开。解压放到暂存区后，未 Commit 之前不动任何已安装版本，
// 任何一步失败都不会破坏当前正在工作的那份。
type Staged struct {
	Manifest *Manifest

	// dir 是内容所在的暂存目录，commit 时整体改名为正式版本目录。
	dir  string
	root string
}

// Stage 解压安装包到暂存区并完成全部校验，不触碰任何已安装版本。
// 调用方必须在结束后执行 Commit 或 Discard 之一。
func Stage(pluginsDir string, archive io.ReaderAt, size int64) (*Staged, error) {
	reader, err := zip.NewReader(archive, size)
	if err != nil {
		return nil, fmt.Errorf("安装包不是有效的 zip：%w", err)
	}
	if len(reader.File) > maxArchiveFiles {
		return nil, fmt.Errorf("安装包内条目数超过 %d", maxArchiveFiles)
	}

	// 有些打包工具会多包一层顶层目录，这里自动剥掉，
	// 不强迫插件作者按某种固定结构打包。
	prefix := detectTopLevelPrefix(reader.File)

	manifest, err := readManifest(reader, prefix)
	if err != nil {
		return nil, err
	}

	// 每个插件固定一个暂存目录：崩溃留下的残留在下次安装时被直接覆盖，
	// 不会像随机临时目录那样越积越多。
	destRoot := filepath.Join(pluginsDir, stagingDir, manifest.Org, manifest.Name)
	if err := os.RemoveAll(destRoot); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return nil, err
	}

	staged := &Staged{Manifest: manifest, dir: destRoot, root: pluginsDir}
	if err := extractAll(reader, prefix, destRoot); err != nil {
		staged.Discard()
		return nil, err
	}

	// 声明的入口必须真实存在，否则装上就是一个打不开的空壳。
	entryPath := filepath.Join(destRoot, filepath.FromSlash(manifest.Entry))
	if !withinDir(destRoot, entryPath) {
		staged.Discard()
		return nil, errors.New("entry 指向安装目录之外")
	}
	if info, err := os.Stat(entryPath); err != nil || info.IsDir() {
		staged.Discard()
		return nil, fmt.Errorf("安装包内缺少入口文件：%s", manifest.Entry)
	}

	return staged, nil
}

// Commit 把暂存内容落位为正式版本，并清掉同插件的其它版本目录。
// 插件同时只有一个生效版本，旧版本目录不被任何记录引用，留着只会永久占用磁盘。
func (s *Staged) Commit() error {
	dest := filepath.Join(s.root, s.Manifest.Org, s.Manifest.Name, s.Manifest.Version)
	pluginRoot := filepath.Dir(dest)
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		return err
	}

	// 同版本重装：先把旧目录挪走，落位成功后再删。中途失败要挪回来，
	// 否则用户会得到一个"目录名还在但内容是空的"插件。
	backup := dest + ".previous"
	hasBackup := false
	if _, err := os.Stat(dest); err == nil {
		_ = os.RemoveAll(backup)
		if err := os.Rename(dest, backup); err != nil {
			return err
		}
		hasBackup = true
	}

	if err := os.Rename(s.dir, dest); err != nil {
		if hasBackup {
			_ = os.Rename(backup, dest)
		}
		return err
	}
	if hasBackup {
		_ = os.RemoveAll(backup)
	}

	if siblings, err := os.ReadDir(pluginRoot); err == nil {
		for _, sibling := range siblings {
			if sibling.IsDir() && sibling.Name() != s.Manifest.Version {
				_ = os.RemoveAll(filepath.Join(pluginRoot, sibling.Name()))
			}
		}
	}
	return nil
}

// Discard 放弃本次安装，清掉暂存内容。
func (s *Staged) Discard() {
	if s == nil || s.dir == "" {
		return
	}
	_ = os.RemoveAll(s.dir)
}

// extractAll 把包内文件解压到 destRoot，并执行体积与路径校验。
func extractAll(reader *zip.Reader, prefix, destRoot string) error {
	var total int64

	for _, file := range reader.File {
		clean := path.Clean(strings.TrimPrefix(file.Name, "./"))
		if clean == "" || clean == "." {
			continue
		}
		if clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return fmt.Errorf("安装包内含非法路径：%s", file.Name)
		}
		if prefix != "" && !strings.HasPrefix(clean, prefix) {
			// 剥前缀模式下，落在顶层前缀之外的文件不予安装。
			continue
		}

		rel := strings.TrimPrefix(clean, prefix)
		if rel == "" || rel == "." {
			continue
		}

		target := filepath.Join(destRoot, filepath.FromSlash(rel))
		// 双保险：前面的判断即使被绕过，也要确认最终落点仍在目标目录内。
		if !withinDir(destRoot, target) {
			return fmt.Errorf("安装包内含越界路径：%s", file.Name)
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}

		// 符号链接可以指向包外，直接拒绝而不是尝试解析。
		if file.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("安装包内不允许符号链接：%s", file.Name)
		}
		// 头部声明的体积先做一次快速拒绝，真正的把关在写完之后按实际字节数做。
		if file.UncompressedSize64 > maxFileSize {
			return fmt.Errorf("文件超过单文件体积上限：%s", file.Name)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		written, err := extractFile(file, target)
		if err != nil {
			return err
		}

		// 总量按"实际写入的字节数"累加。
		//
		// 旧实现累加的是 zip 头里的 UncompressedSize64，而那个字段由打包方填写、
		// 可以撒谎：声称每个文件只有 1 KiB，实际写出 32 MiB，总量上限就被绕过了。
		total += written
		if total > maxTotalSize {
			return errors.New("解压后总体积超过上限")
		}
	}
	return nil
}

// extractFile 解压单个条目，返回实际写入的字节数。
func extractFile(file *zip.File, target string) (int64, error) {
	rc, err := file.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}

	// 多读 1 字节用于判断是否越界；越界时删掉半成品，避免留下超大文件。
	written, err := io.Copy(out, io.LimitReader(rc, maxFileSize+1))
	closeErr := out.Close()
	if err != nil {
		_ = os.Remove(target)
		return written, err
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return written, closeErr
	}
	if written > maxFileSize {
		_ = os.Remove(target)
		return written, fmt.Errorf("文件实际解压体积超过上限：%s", file.Name)
	}
	return written, nil
}

// Uninstall 删除插件在磁盘上的全部版本目录。
func Uninstall(pluginsDir, org, name string) error {
	if !identifierPattern.MatchString(org) || !identifierPattern.MatchString(name) {
		return errors.New("插件标识非法")
	}
	return os.RemoveAll(filepath.Join(pluginsDir, org, name))
}

// detectTopLevelPrefix 识别"多包了一层目录"的安装包。
// 只有当所有条目都在同一个顶层目录下时才剥前缀，否则保持原样。
func detectTopLevelPrefix(files []*zip.File) string {
	prefix := ""

	for _, file := range files {
		clean := path.Clean(strings.TrimPrefix(file.Name, "./"))
		if clean == "" || clean == "." {
			continue
		}

		segment, _, found := strings.Cut(clean, "/")
		if !found {
			// 根目录下直接有文件，说明不需要剥前缀。
			return ""
		}
		if prefix == "" {
			prefix = segment
			continue
		}
		if prefix != segment {
			return ""
		}
	}

	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

func readManifest(reader *zip.Reader, prefix string) (*Manifest, error) {
	target := prefix + "plugin.json"

	for _, file := range reader.File {
		if path.Clean(strings.TrimPrefix(file.Name, "./")) != target {
			continue
		}

		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("读取 plugin.json 失败：%w", err)
		}
		defer rc.Close()

		var manifest Manifest
		if err := json.NewDecoder(io.LimitReader(rc, maxManifestSize)).Decode(&manifest); err != nil {
			return nil, fmt.Errorf("plugin.json 解析失败：%w", err)
		}
		return manifest.Normalize()
	}

	return nil, errors.New("安装包根目录缺少 plugin.json")
}

// ReadInstalledManifest 读取磁盘上某个已落位插件目录里的清单。
//
// 运行时用它拿 runtime 段。清单以磁盘上那份为准，不在数据库里再存一份副本：
// 两份拷贝一旦不一致，出问题的永远是真正生效的那一份。
func ReadInstalledManifest(root string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(root, "plugin.json"))
	if err != nil {
		return nil, err
	}

	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("plugin.json 解析失败：%w", err)
	}
	return manifest.Normalize()
}

// ResolveAsset 在插件目录内解析一个包内相对路径，越界时返回错误。
//
// 静态资源托管是唯一一处"用户可控路径直接映射到磁盘"的地方，
// 因此校验逻辑集中放在这里，而不是散落在 HTTP 处理函数中。
func ResolveAsset(root, rel string) (string, error) {
	clean := path.Clean(strings.TrimPrefix(rel, "/"))
	if clean == "" || clean == "." || clean == ".." ||
		strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return "", errors.New("路径非法")
	}

	target := filepath.Join(root, filepath.FromSlash(clean))
	if !withinDir(root, target) {
		return "", errors.New("路径越界")
	}
	return target, nil
}

// withinDir 判断 target 是否位于 root 目录内。
func withinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
