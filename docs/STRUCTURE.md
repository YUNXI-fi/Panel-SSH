# 项目目录结构

本文件描述 Panel SSH 工作区的目录约定。原则是：**源码、打包内容、运行数据、设计资料、文档各自独立**；会被构建脚本、`go:embed`、fnOS 服务引用的路径保持不动。

## 顶层结构

```
Panel SSH/                       工作区根（fnOS 应用交付根）
├── bin/
│   └── panelssh                 运行时二进制（本地直接启动用，保留）
├── data/                        运行时数据：数据库、密钥、日志、缓存（保留）
│   └── logs/
│       └── panelssh-legacy.log  历史日志归档
├── data.bak/                    历史数据备份（保留，未自动清理）
├── docs/
│   ├── STRUCTURE.md             本文件
│   ├── architecture/            架构与模块设计文档
│   └── design/
│       ├── desktop-redesign-draft.html
│       └── previews/            Frost / Graphite / Midnight / Paper 视觉预览
├── fnos/                        fnOS 应用打包内容
│   ├── app/                     桌面入口与 ui 配置
│   ├── cmd/                     生命周期回调
│   ├── config/                  权限与资源声明
│   ├── wizard/
│   ├── manifest
│   ├── ICON.PNG
│   ├── ICON_256.PNG
│   ├── build.sh                 打包脚本
│   └── .gitignore
└── Panel SSH/                   Go + Vue 源码工程
    ├── cmd/panelssh/            服务端入口
    ├── internal/                Go 内部模块（按职责分包）
    │   ├── ai/                  AI 对话与模型接入
    │   ├── api/                 HTTP / WebSocket 接口层
    │   ├── auth/                登录、会话与口令
    │   ├── config/              服务配置
    │   ├── docker/              Docker 管理
    │   ├── dsh/                 DSH 工作台运行时
    │   ├── env/                 软件环境与安装配方
    │   ├── localterm/           宿主机本地终端
    │   ├── plugin/              插件清单、进程与能力
    │   ├── secretbox/           凭据加密
    │   ├── sshclient/           SSH 连接池
    │   ├── store/               SQLite 存储
    │   ├── sysinfo/             系统信息采集
    │   └── web/dist/            前端构建产物（go:embed 输入，必须保留）
    ├── web/                     前端工程
    │   ├── src/
    │   │   ├── api/             接口客户端与类型
    │   │   ├── components/      通用组件与图标精灵表
    │   │   ├── desktop/         桌面、窗口管理与组合式状态
    │   │   ├── styles/          设计 tokens 与全局样式
    │   │   └── windows/         各业务窗口
    │   ├── scripts/build.sh     前端构建并同步到 internal/web/dist
    │   ├── index.html
    │   ├── package.json
    │   ├── tsconfig.json
    │   └── vite.config.ts
    ├── go.mod
    ├── go.sum
    └── .gitignore
```

## 目录职责

| 目录 | 放什么 | 不放什么 |
|---|---|---|
| `Panel SSH/cmd/` | 进程入口 | 业务逻辑 |
| `Panel SSH/internal/` | 后端业务模块，按领域分包 | 前端资源、设计稿 |
| `Panel SSH/web/src/` | Vue 源码 | 构建产物、依赖 |
| `Panel SSH/internal/web/dist/` | 前端构建产物，供 `go:embed` | 手工维护的源码 |
| `Panel SSH/web/node_modules/` | 前端依赖 | 源码 |
| `fnos/` | 仅打包内容：manifest、生命周期脚本、图标、权限声明 | Go 源码、构建输出 |
| `docs/architecture/` | 模块设计、改造方案 | 运行日志、构建产物 |
| `docs/design/` | 设计稿与视觉预览 | 已实现的前端源码 |
| `data/`、`data.bak/`、`bin/` | 运行时数据、备份、可执行文件 | 源码、文档 |

## 命名约定

- Go 包目录：小写、单词拼接，如 `sshclient`、`localterm`。
- Go 文件：小写 + 下划线，按模块或功能命名，如 `security_apps_api.go`。
- Vue 组件：PascalCase，如 `SecurityAppWindow.vue`、`PluginCenter.vue`。
- TypeScript / CSS / 资源：camelCase 或 kebab-case，与现有文件保持一致。
- 构建输出目录统一用 ASCII：`dist/`，不再使用中文目录名。
- `fnos/ICON.PNG`、`fnos/ICON_256.PNG` 保持大写扩展名，因为打包脚本和 fnOS 约定按该名称校验。

## 构建与运行

```bash
# 前端：构建并同步到 internal/web/dist
cd "Panel SSH/web"
npm run build

# 后端：编译
cd "Panel SSH"
go build ./...

# 本地运行（工作区根目录）
./bin/panelssh -addr :29180 -data ./data

# fnOS 打包：输出到 fnos/dist/panelssh.fpk
bash fnos/build.sh
```

## 生成物与忽略规则

- `Panel SSH/internal/web/dist/` 是 `go:embed` 的输入，**故意不忽略**，修改前端后需要重新构建并提交。
- `Panel SSH/web/node_modules/`、`fnos/app/bin/panelssh`、`fnos/dist/`、`*.fpk`、`bin/`、`data/`、`*.log` 属于依赖或构建/运行产物，不进入源码版本管理。

## 本次整理记录

- 设计稿从根目录 `design-previews/` 归档到 `docs/design/previews/`。
- 设计草案 `desktop-redesign-draft.html` 归档到 `docs/design/`。
- 架构文档从 `Panel SSH/.trae/documents/` 迁移到 `docs/architecture/`。
- fnOS 构建输出目录 `fnos/编译产物/` 更名为 `fnos/dist/`，同步更新 `build.sh` 与 `.gitignore`。
- 删除临时分享包 `.trae-html-share-packages/`（根目录与源码目录各一份）。
- 删除源码目录里重复的构建产物 `Panel SSH/bin/panelssh`。
- 删除前端目录中的空占位文件 `.preview-terminal.html` 与日志 `web/panelssh.log`。
- 删除 fnOS 目录中可重新生成的 `app/bin/panelssh` 与旧 `.fpk`。
- 根目录历史日志 `panelssh.log` 归档到 `data/logs/panelssh-legacy.log`。
- 运行时目录 `bin/`、`data/`、`data.bak/` 保持原位，避免影响正在运行的面板与 fnOS 服务路径。
