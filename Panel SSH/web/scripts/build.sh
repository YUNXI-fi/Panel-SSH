#!/bin/bash
#
# 构建前端并同步到 Go 的 embed 目录。
#
# 为什么不是直接 `vite build`：本仓库位于 exFAT 分区（/media 下的外挂盘）。
# exFAT 不支持 Unix 的原子 rename，而 Vite 清空/替换产物目录依赖这个语义 ——
# 结果是 Vite 报 "built in 2s"、退出码 0，而磁盘上的产物纹丝不动。
# 这个失败是静默的，靠肉眼几乎发现不了，所以这里先构建到 ext4 的临时目录，
# 再逐个文件拷回来（cp 是普通写入，不依赖 rename）。
#
# 构建完会校验产物时间戳，确认这次的确写进去了。Vite 的退出码不足以信任。

set -euo pipefail

WEB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="$WEB_DIR/../internal/web/dist"
STAGE_DIR="$(mktemp -d -t panelssh-dist-XXXXXX)"

cleanup() { rm -rf "$STAGE_DIR"; }
trap cleanup EXIT

echo "==> 构建到 $STAGE_DIR（ext4，绕开 exFAT 的 rename 限制）"
cd "$WEB_DIR"
node ./node_modules/vite/bin/vite.js build --outDir "$STAGE_DIR" --emptyOutDir

# Vite 声称成功不代表产物真的存在：先确认它至少产出了入口文件。
if [ ! -f "$STAGE_DIR/index.html" ]; then
    echo "构建失败：暂存目录里没有 index.html" >&2
    exit 1
fi

echo "==> 同步到 $DIST_DIR"
# 先清空再拷：否则上一次构建的文件会残留在目录里，
# 而 index.html 只引用本次的哈希文件名，残留文件永远不会被加载，只会越积越多。
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"
cp -r "$STAGE_DIR/." "$DIST_DIR/"

# 校验产物确实落地了。不能只看 cp 的退出码：如果目标目录因为挂载问题
# 变成只读或写到别处，cp 也可能"成功"而文件不在预期位置。
if [ ! -f "$DIST_DIR/index.html" ]; then
    echo "同步失败：$DIST_DIR 里没有 index.html" >&2
    exit 1
fi

# 比对两边的大小，任何截断/丢失都会在这里暴露。
if ! diff -q \
    <(cd "$STAGE_DIR" && find . -type f | sort | xargs stat -c '%s %n' | sort) \
    <(cd "$DIST_DIR" && find . -type f | sort | xargs stat -c '%s %n' | sort) >/dev/null; then
    echo "同步失败：产物与暂存目录不一致" >&2
    exit 1
fi

ENTRY=$(grep -o 'assets/index-[A-Za-z0-9_.-]*\.js' "$DIST_DIR/index.html" | head -1)
echo "==> 完成：$ENTRY（$(find "$DIST_DIR" -type f | wc -l) 个文件）"
echo "    记得重新编译后端，go:embed 才会把新产物打进二进制"
