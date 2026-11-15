# Panel SSH 统一命令入口
#
# 应用包生成与校验都从这里走，避免"生成器在哪个目录、参数怎么传"
# 靠口头约定。Node 相关命令在 exFAT 分区上需要清掉宿主机的动态库变量，
# 脚本里已经处理。
.RECIPEPREFIX = >

.PHONY: apps validate-apps test build-web build

apps:
> cd "Panel SSH" && go run ./cmd/appgen -out ../apps

validate-apps:
> ./scripts/validate-apps.sh

test:
> cd "Panel SSH" && go test ./...
> cd "Panel SSH/web" && env -u LD_LIBRARY_PATH -u LD_PRELOAD node ./node_modules/vitest/vitest.mjs run

build-web:
> cd "Panel SSH/web" && env -u LD_LIBRARY_PATH -u LD_PRELOAD bash scripts/build.sh

build: validate-apps build-web
> cd "Panel SSH" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
>   go build -trimpath -ldflags='-s -w' -o ../bin/panelssh ./cmd/panelssh
