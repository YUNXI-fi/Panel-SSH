package env

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Snapshot 是一次变更前的系统状态快照（保存在面板数据目录，不在目标机上）。
//
// 三份材料各自服务于回滚：
//   - Packages：包级版本回退的依据；
//   - sources.tgz：安装源与密钥的完整还原；
//   - Facts：包管理器管不到的安装事实（/usr/local/go 这类 tarball）。
type Snapshot struct {
	Dir      string
	Packages []Package
	Facts    map[string]string
}

// snapshotScript 在目标机上采集快照。
//
// 源文件在脚本内先按存在性收集，再从根目录打相对路径 tar：
// busybox tar（Alpine）不支持 --ignore-failed-read，成员路径带不带前导
// 斜杠的行为也和 GNU tar 不一致，cd / 后传相对路径是两边唯一都稳的写法。
func snapshotScript(mgr PackageManager) string {
	paths := mgr.SourcePaths()
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, "'"+p+"'")
	}
	return `
export LC_ALL=C
echo "===PACKAGES==="
` + mgr.PackageListCommand() + `
echo "===FACTS==="
if [ -f /usr/local/go/VERSION ]; then
  echo "GO_VERSION=$(cat /usr/local/go/VERSION)"
else
  echo "GO_VERSION="
fi
echo "GO_SYMLINK=$(readlink /usr/local/bin/go 2>/dev/null || true)"
echo "===SOURCES_B64==="
FILES=""
for p in ` + strings.Join(quoted, " ") + `; do
  [ -e "$p" ] && FILES="$FILES $p"
done
if [ -n "$FILES" ]; then
  (cd / && tar czf - $FILES 2>/dev/null) | base64 | tr -d '\r\n'
  echo
fi
echo "===END==="
`
}

// TakeSnapshot 在目标机上采集快照并落盘到 dir。
func TakeSnapshot(ctx context.Context, ex Executor, p *Profile, mgr PackageManager, dir string) (*Snapshot, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	out, err := runQuery(ctx, ex, snapshotScript(mgr), snapshotTimeout)
	if err != nil {
		return nil, err
	}
	sections := splitSections(out)

	snap := &Snapshot{Dir: dir, Facts: map[string]string{}}

	pkgRaw := sections["PACKAGES"]
	if err := os.WriteFile(filepath.Join(dir, "packages.tsv"), []byte(pkgRaw), 0o600); err != nil {
		return nil, err
	}
	snap.Packages = mgr.ParsePackages(pkgRaw)

	for _, line := range strings.Split(sections["FACTS"], "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k != "" {
			snap.Facts[k] = v
		}
	}

	if b64 := strings.TrimSpace(sections["SOURCES_B64"]); b64 != "" {
		data, derr := base64.StdEncoding.DecodeString(b64)
		if derr == nil && len(data) > 0 {
			if err := os.WriteFile(filepath.Join(dir, "sources.tgz"), data, 0o600); err != nil {
				return nil, err
			}
		}
	}
	return snap, nil
}

// snapshotTimeout 覆盖大包清单（几万行）加源文件打包的耗时。
const snapshotTimeout = 60 * time.Second

// LoadSnapshot 从磁盘读取一次历史快照。
func LoadSnapshot(dir string, mgr PackageManager) (*Snapshot, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "packages.tsv"))
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{Dir: dir, Packages: mgr.ParsePackages(string(raw)), Facts: map[string]string{}}
	return snap, nil
}

// packageVersion 查快照片里某包的版本，不存在返回空串。
func (s *Snapshot) packageVersion(name string) string {
	for _, p := range s.Packages {
		if p.Name == name {
			return p.Version
		}
	}
	return ""
}

// restoreSourcesScript 生成"把快照源文件解包回目标机"的脚本。
//
// 先落临时文件再解包：几万个包的系统源 tar 很小（百 KiB 内），
// 但经管道直接解包时一旦传输被截断会得到半个 /etc/yum.repos.d，
// 落盘后解包是原子性更好的姿势。
func restoreSourcesScript(tgzBase64 string) string {
	return `
set -e
TMP=$(mktemp)
cat > "$TMP" <<'PANELSSH_B64'
` + tgzBase64 + `
PANELSSH_B64
base64 -d "$TMP" | (cd / && tar xzf -)
rm -f "$TMP"
`
}

// readSnapshotSourcesB64 读取快照里的源 tar 并编码为单行 base64。
func readSnapshotSourcesB64(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "sources.tgz"))
	if err != nil || len(data) == 0 {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(data), true
}
