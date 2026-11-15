#!/usr/bin/env bash
#
# 校验 apps/ 下的应用包：索引、app.json、ui.json、图标规格与文件摘要。
#
# 这个脚本是合并前置检查：生成器写出的索引与包文件必须同时提交，
# 任何一处漂移都会在这里失败。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODULE="${HERE}/../Panel SSH"
APPS="${HERE}/../apps"

if [ ! -f "${APPS}/index.json" ]; then
  echo "缺少 ${APPS}/index.json，请先执行 make apps" >&2
  exit 1
fi

cd "${MODULE}"
go run ./cmd/appgen -check -out "${APPS}"
