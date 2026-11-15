#!/bin/bash
#
# 构建 Panel SSH 的 fnOS 应用包。
#
# 目录约定：
#   fnos/          本脚本所在目录，只放打包内容（manifest/app/cmd/config/wizard/图标）
#   ../Panel SSH/  源码
#   dist/          构建输出，不参与打包
#
# 流程：校验 → 编译 → 临时移走重复资源 → fnpack build → 收集产物
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="${HERE}/../Panel SSH"
DIST="${HERE}/dist"
BIN="${HERE}/app/bin/panelssh"
FPK="${DIST}/panelssh.fpk"

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

# ---------- 1. 校验 ----------
[ -f "${SRC}/go.mod" ] || fail "找不到源码目录：${SRC}（源码应与 fnos 同级）"
[ -f "${SRC}/internal/web/dist/index.html" ] || \
  fail "前端产物缺失：${SRC}/internal/web/dist/index.html，请先在 web/ 下执行 npm run build"

for required in manifest config/privilege config/resource ICON.PNG ICON_256.PNG app/ui/config cmd/main; do
  [ -e "${HERE}/${required}" ] || fail "缺少打包必需文件：${required}"
done

# 端口必须双处一致：manifest 决定服务监听哪个端口，app/ui/config 决定桌面入口打开哪个端口。
# 两者写错一处，应用能装能启动，但点开就是空白页。
manifest_port="$(sed -n 's/^service_port=//p' "${HERE}/manifest" | tr -d '[:space:]')"
ui_port="$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*"\([0-9][0-9]*\)".*/\1/p' "${HERE}/app/ui/config" | head -n 1)"
[ -n "${manifest_port}" ] || fail "manifest 缺少 service_port"
[ -n "${ui_port}" ] || fail "app/ui/config 缺少 port"
[ "${manifest_port}" = "${ui_port}" ] || \
  fail "端口不一致：manifest.service_port=${manifest_port}，app/ui/config.port=${ui_port}"

# ---------- 2. 编译 ----------
# manifest 的 platform=x86，这里固定编译 amd64。
# 要出 arm64 包，需把 manifest 的 platform 改成 arm，再把这里的 GOARCH 改成 arm64。
mkdir -p "${DIST}" "$(dirname "${BIN}")"
(
  cd "${SRC}"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o "${BIN}" ./cmd/panelssh
)
chmod +x "${BIN}"
echo "已生成二进制：${BIN}"

if ! command -v fnpack >/dev/null 2>&1; then
  echo "未找到 fnpack，跳过 .fpk 打包。" >&2
  echo "下载：https://static2.fnnas.com/fnpack/fnpack-1.2.3-linux-amd64" >&2
  exit 0
fi

# ---------- 3. 临时移走重复资源 ----------
# fnpack 会把根目录的 config/cmd/wizard 收进 app.tgz；若 app/ 下也存在同名内容，
# 包里就会有两份。这里只在打包期间把它们挪开，结束后原样放回。
STASH="$(mktemp -d -t panelssh-fpk-XXXXXX)"
STASHED=""
restore_stash() {
  local name
  for name in ${STASHED}; do
    if [ -e "${STASH}/${name}" ]; then
      mv "${STASH}/${name}" "${HERE}/app/${name}"
    fi
  done
  rm -rf "${STASH}"
}
trap restore_stash EXIT

for name in config cmd wizard manifest; do
  if [ -e "${HERE}/app/${name}" ]; then
    mv "${HERE}/app/${name}" "${STASH}/${name}"
    STASHED="${STASHED} ${name}"
  fi
done

# ---------- 4. 打包 ----------
rm -f "${FPK}"
(
  cd "${DIST}"
  fnpack build --directory "${HERE}"
)
[ -f "${FPK}" ] || fail "打包结束但未找到 ${FPK}"

echo "打包完成：${FPK}（$(du -h "${FPK}" | cut -f1)）"
echo "安装：把 fpk 拷到 fnOS 设备，用应用中心手动安装，或执行 appcenter-cli install-fpk。"
