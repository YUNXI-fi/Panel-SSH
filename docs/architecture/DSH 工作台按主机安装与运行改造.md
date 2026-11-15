# DSH 工作台按主机安装与运行改造

## 一、背景与问题

「DSH 工作台」是内置插件（`panelssh/dsh-workspace`），让用户通过面板访问 DeepSeek Harness（dsh）的 Web UI。当前实现有三个断点：

1. **「安装」按钮什么都没装**。`installBuiltin()` → `handleInstallBuiltin`（internal/api/plugins.go:115-150）只往数据库 upsert 一条记录，无确认、无预检、无进度。dsh 本体和 Node.js 完全靠用户自己准备。
2. **装到远端主机必然打不开**。dsh 进程由面板在**宿主机本地**用 `exec.Command` 拉起（internal/dsh/supervisor.go:251-301），入口 `dshWorkspaceInstalled()` 只认 `LocalHostID`（internal/api/dsh.go:25-28），但插件中心的安装按钮用的是用户选中的任意主机。结果是：列表显示「已安装」，打开 `/dsh/` 返回 404「插件未安装」。
3. **启动命令要用户手工填**。用户得自己知道 `node /path/to/@deepseek-ai/dsh/lib/bin.js` 这种路径，填错只看到启动失败日志。

**本次目标**：点「安装」→ 确认弹窗（显示目标主机）→ 面板预检环境 → 缺 Node.js / dsh 弹窗询问用户 → 确认后由面板代装（有进度）→ 装完自动回填启动命令 → 写出插件记录，闭环可用。

## 二、已确认的决策（用户拍板）

| 决策 | 选择 |
|---|---|
| 运行架构 | 改造为**每台目标主机一个 dsh 实例**（含宿主机 hostId=0） |
| Node.js 缺失 | 面板**代装系统包**（走现有 env 流水线 + 提权） |
| 启动命令 | 安装完成后**面板自动回填并保存**，保留手工填写入口作兜底 |
| 目标主机范围 | 允许装到任意主机，不限制宿主机 |

## 三、可复用的既有能力（不要新造）

| 能力 | 位置 | 用途 |
|---|---|---|
| env 流水线 | `internal/env/operation.go` `Runner.Plan/Run` | 预检 → 快照 → 步骤执行 → 补偿 → 复核 |
| Node.js 配方 | `internal/env/catalog.go:147-197` `planNode`、slug `nodejs` | 代装 Node，支持 apt/dnf/yum/apk，NodeSource 官方源 |
| 非系统包配方先例 | `internal/env/catalog.go:371-395` `planBun` | 证明「Steps 跑任意 shell、PkgSpecs 只放前置依赖」是既有范式 |
| 目标机执行 | `internal/api/exec.go:235/248` `execOnHost/execOnHostStream` | hostId=0 落本地，其余走 SSH，差异已被吃掉 |
| 提权 | `internal/api/privilege.go:79/115` `execEscalated/runEscalated`、`env.Wrap` | sudo -n / su / sudo -S 三路径 |
| 执行进度弹窗 | `web/src/windows/EnvRunDialog.vue` | props `{show, hostId, hostName, request}`，自带 WebSocket 步骤流，**跨窗口可复用** |
| 预检弹窗 | `web/src/windows/EnvPlanDialog.vue` | props `{show, plan, loading}` |
| 确认弹窗范式 | `web/src/windows/PluginCenter.vue:185-196` `askUninstall` | `dialog.warning` + content 内嵌 hostName |
| 每主机一份的存储范式 | `internal/store/store.go` plugins 表 | `host_id` 列 + 唯一约束，0 表示宿主机 |

## 四、上游事实（已第一手核实，不是推测）

- `@deepseek-ai/dsh`：`bin` 名为 **`dsh`**，入口 `lib/bin.js`，`type: module`，**无 `engines` 约束**，但包元数据 `_nodeVersion: 24.20.0`（上游用 Node 24 构建）。
- `dist-tags.latest` = **`0.1.5-rc.2`**，即上游把预发布版标成了 latest。装 latest 会拿到 rc 版，需在界面文案中如实告知。
- 依赖树 70+ 个 `@deepseek-ai/dsh-*` 包，实际体积与内存开销与现有注释「常驻约 320MB 内存」一致。
- `*ssh.Client` 提供 `DialContext(ctx, n, addr) (net.Conn, error)`（已 `go doc` 验证），可直接作为 `http.Transport.DialContext` 的拨号器 —— 隧道方案成立。
- dsh 启动输出含 `http://127.0.0.1:<port>/?token=<token>`，现有正则 `readyPattern`（internal/dsh/supervisor.go:51）可直接复用于远端。

## 五、实现方案

### 阶段一：地基（行为不变的重构，可独立验证）

#### 1. 存储：每主机一份启动命令

新表（不加进 `migrate` 的基础 schema，走版本迁移）：

```sql
CREATE TABLE IF NOT EXISTS dsh_hosts (
    host_id    INTEGER PRIMARY KEY,   -- 一台主机只有一条命令，主键即约束
    command    TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);
```

**为什么不上 meta 表拼键**：meta 是 `key TEXT PRIMARY KEY`（store.go:90-93），无 host 列。拼键把 ID 编进字符串，按主机枚举要 LIKE 扫；更重要的是语义错位 —— meta 存的是面板自身的全局配置（`schema.version`、`session_epoch`），而「每台主机的启动命令」是与 hosts 同源的目标机数据，混在一起必然留下孤儿键。

方法（`internal/store/store.go`，与 plugins 段相邻）：
- `GetDshCommand(hostID int64) (string, error)`：不存在返回 `""`、`nil`（沿用 `GetMeta` 的约定）
- `SetDshCommand(hostID int64, command string) error`
- `DeleteDshCommand(hostID int64) error`：卸载插件时清理

**迁移**（追加到 `schemaMigrations` 末尾，store.go:309，范式照 `migratePluginsPerHost`，必须带 tx）：
`migrateDshCommandsPerHost`（版本 2）建表，并把旧的全局 meta 键 `dsh.command` 搬到宿主机（`host_id = 0`）名下后**删除该键**。旧模型下 dsh 就装在宿主机上，语义完全一致。

**关于 `DeleteMeta`**：不新增。迁移里一句 `DELETE FROM meta WHERE key = ?` 就够，为一个一次性动作给 store 加长期方法不划算；卸载清理走新表的 `DeleteDshCommand`。

`cmd/panelssh/main.go:90-94` 改为：宿主机默认值取 `cfg.DshCommand`，其余主机无部署级默认（未配置即 disabled）。

#### 2. dsh 包：启动方式抽象

新增两个接口，把「怎么在目标机上起一个进程」从 `Supervisor` 剥离：

```go
// Launcher 抽象"在一台主机上把 dsh 跑起来"的两种方式。
type Launcher interface { Start(argv []string, workDir string) (Process, error) }

// Process 是已启动的 dsh 进程句柄。
type Process interface {
    Stdout() io.Reader
    Stderr() io.Reader
    Wait() error      // 唯一回收点
    Terminate()       // 优雅终止整个进程组
    Kill()            // 强杀兜底
}
```

- `launcher_local.go` / `localProcess`：现有 `os/exec` 逻辑**原样搬入**，保留 `Setpgid` + `Pdeathsig` + `syscall.Kill(-pid)`。
- `launcher_remote.go` / `remoteProcess`：持有 `*ssh.Client` + `*ssh.Session`；stdout/stderr 直接取 session 的两个管道；`Wait()` 即 `session.Wait()`。

`Supervisor` 只改四处：字段 `proc *exec.Cmd` → `proc Process`；`spawn()` 构造 argv 后交 `launcher.Start`（补齐 `web --no-open --host 127.0.0.1 --port 0` 的逻辑不动）；`watch(cmd)` → `watch(p)`；`signalGroup` → `Terminate/Kill`。

**`consume`（readyPattern 解析）、`closeReadyLocked`（防重复 close）、`ready`/`watchDone` 通道语义一行不改** —— 它们本来就不碰 `exec.Cmd`，这是本次改造成本可控的根本原因。

#### 3. Manager：每主机一个实例

`Server.dsh` 从 `*dsh.Supervisor` 改为 `*dsh.Manager`：

```go
type Manager struct { /* dataRoot, store, launcherFor(hostID) Launcher, 实例 map */ }
func (m *Manager) Supervisor(hostID int64) *Supervisor          // 懒建；命令为空即 StateDisabled
func (m *Manager) SetCommand(hostID int64, cmd string) error    // 写库 + 停旧换新
func (m *Manager) Stop(hostID int64)
func (m *Manager) StopAll()
```

`launcherFor` 由 api 包注入（只有它知道 SSH 凭据与 known_hosts 路径），**dsh 包不引 store/sshclient**，保持可单测。

**远端孤儿防护 —— 推荐「SSH session 常驻 + pid 文件兜底」**：
- 只靠「会话断开带走进程」不够：dsh 必须活得比任何一次 HTTP 请求长，网络抖动会让进程消失，所以必须支持**主动重拉**，把它当可恢复事件而非终态。
- 只靠 pid 文件不够：远端没有 `Pdeathsig` 的等价物，面板被 SIGKILL 时会留一个占几百 MB 的 node。
- 落地：远端启动脚本用 `sh -c 'cd <workDir> && echo $$ > dsh.pid && exec <argv…>'`（`exec` 让 pid 就是 dsh 本体，便于按进程组 kill）；`Start()` 前先读该 pid 文件，`kill -0` 判活则先清掉上次残留（`--port 0` 不会撞端口，但会白占内存）；`Terminate/Kill` 走**另一条** SSH session 执行。

**远端连接的归属**：**不能**从 `sshclient.Pool` 借常驻连接 —— `refs` 永不归零会让 3 分钟 idleTTL 回收失效，且 `Invalidate`（改主机凭据）时因 `refs > 0` 只标 retired 不关闭，形成泄漏。改由 Manager 按 hostID 持有专用连接，`Stop()` 时关闭。

#### 4. api：路径前缀参数化 + 隧道反代

```go
func dshPrefixFor(hostID int64) string  // "/dsh/7"，宿主机 "/dsh/0"
```

路由（替换 server.go:198-203）：

```go
mux.Handle("GET  /api/hosts/{id}/dsh/status",  s.requireAuth(s.handleDshStatus))
mux.Handle("PUT  /api/hosts/{id}/dsh/command", s.requireAuth(s.handleDshSetCommand))
mux.Handle("/dsh/{id}/", s.requireAuth(s.handleDshEntry))
mux.Handle("/dsh/{id}",  s.requireAuth(...307 → /dsh/{id}/))
mux.Handle("/dsh/",      s.requireAuth(...307 → /dsh/0/))   // 旧入口，等价于宿主机
mux.Handle(dshPrefix,    ... 同上)
```

用路径参数 `{id}` 而不是手工切字符串：`pathHostID`（internal/api/plugins.go:192）已实现「0 合法 + 主机必须存在」，手工切会多出第二套解析与校验。CSRF 豁免仍走 `/dsh` 前缀（withCSRF，server.go:362），不需改。

**反代**：去掉 `dshProxyOnce/dshProxyHandler` 单例（已不成立），改为 `dshProxies sync.Map`（hostID → handler）。`newDshProxy(hostID)` 的 `Rewrite` 每次重新取 `s.dsh.Supervisor(hostID).Endpoint()`（保留「重启后不转发到旧地址」这一性质）；剥前缀改用 `strings.TrimPrefix(path, dshPrefixFor(hostID))`。

`Rewrite` 里 `r.Out.Host = target.Host` 与 Origin/Referer 改写**照旧不动**：隧道下目标仍是回环，dsh 的 browser-trust 栅栏依然满足。

**Transport 切换**：`dshTransport(hostID) http.RoundTripper`
- hostID=0 → `http.DefaultTransport`（复用连接池）
- hostID>0 → `http.Transport{DialContext: …sshClient.DialContext…}`，连接由 Manager 持有，失效则重建并关旧。

**前缀参数化的三处改写点**（都在 internal/api/dsh.go）：
1. `rewriteDshHTML(resp *http.Response, prefix string)`：`<base href="/">`、`"/plugins/` 两处替换与 `dshOriginShim` 注入都用 prefix（shim 里 `P` 常量随之变成 `/dsh/7`，其「已带前缀就不再拼」的判定天然继续成立）
2. Location 重定向：抽成纯函数 `rewriteDshLocation(loc, prefix) string`（现在内联在闭包里无法单测，抽出来本身就是收益）
3. 401 自愈：`redirectToTokenExchange(resp, token, prefix)`，**token 必须取对应 hostID 的 `Endpoint()`**（最容易漏的一处）

`dshStatusViewValue(hostID)`、`renderDshStatusPage(w, hostID, hint)` 同步参数化；状态页里内联的 `fetch("/api/dsh/command")` 也要改成带主机的路径。

### 阶段二：环境代装与安装流程（用户可见的价值）

#### 5. env 配方：`dsh`

在 `internal/env/catalog.go` 的 `catalog` 新增一项：

```go
{Slug: "dsh", Name: "DeepSeek Harness", Category: "toolchain",
 Description: "DeepSeek Harness（dsh）命令行与 Web 工作台，通过 npm 全局安装",
 DetectCommand: detectDsh, CheckCommand: checkDsh, Plan: planDsh}
```

- `detectDsh`：用 npm 自己认包最可靠，**不用 `dsh --version`**（其存在性未验证，且 npm 全局 bin 目录未必在非登录 shell 的 PATH 上）：
  `npm ls -g --depth=0 @deepseek-ai/dsh 2>/dev/null | sed -n 's/.*@deepseek-ai\/dsh@\(.*\)/\1/p'`
- `checkDsh`（比照 `checkBun`）：`command -v dsh` 判可执行，输出 `OK` 或问题描述。
- `planDsh`：**复用 `planNode`，把 Node 与 dsh 合并成一次操作**。

```go
func planDsh(ctx context.Context, p *Profile, _ string) (*RecipePlan, error) {
    node, err := planNode(ctx, p, "24")   // 上游用 Node 24 构建
    if err != nil { return nil, err }
    return &RecipePlan{
        SimScript:      node.SimScript,     // apt/dnf 系有；apk 无 → 走 PkgSpecs
        PkgSpecs:       node.PkgSpecs,      // apk 家族靠它模拟 nodejs/npm
        RollbackRemove: node.RollbackRemove // 只含 nodejs；dsh 是 npm 包，包管理器卸载不了
        Steps:          append(append([]Step{}, node.Steps...), dshNpmStep()),
        Verify:         detectDsh, VerifyLabel: "dsh",
        Note: "会先确保 Node.js 24.x（NodeSource 官方源）就位，再全局安装 @deepseek-ai/dsh。" +
              "Node 步骤是幂等的；dsh 是 npm 全局包，包管理器不认识它，回滚不会自动卸载。" +
              "上游当前 latest 为预发布版，属于 developer preview。",
    }, nil
}
```

`dshNpmStep` = `set -e` + `export LC_ALL=C` + `npm install -g @deepseek-ai/dsh@latest` + `hash -r`。

**为什么合并成一次操作而不是串两次**：① `Runner.hostLock` 在两次之间会释放，中间态（Node 装了、dsh 没装）的补偿语义说不清；② 用户视角是一个动作，一次确认、一次进度、一次补偿；③ 「前置依赖作为非关键步骤」在配方内已有先例（`planBun` 的 curl/unzip）。`buildRecipe` 里 `PkgSpecs` 为空时返回 `SimReport{OK:true}`，不报错；`Supports(Family)` 缺省全家族。

#### 6. 启动命令回填

新增 `POST /api/hosts/{id}/dsh/command/detect`：执行探测脚本拿到 shim 绝对路径，保存后返回新状态。

```
p=$(command -v dsh 2>/dev/null); [ -n "$p" ] && { echo "$p"; exit 0; }
for d in /usr/local/bin /usr/bin; do [ -x "$d/dsh" ] && { echo "$d/dsh"; exit 0; }; done
```

**为什么取绝对路径**：与 `detectBun`/`detectCUDA` 用绝对路径同理 —— 面板跑命令是非登录 shell，不读 `/etc/profile.d`，PATH 不可信。**为什么不用 `npx`**：每次启动都要走 npx 解析，慢且可能联网。**为什么不塞进 env 的 Verify**：那套契约是「OK 或问题描述」，前端按 OK 判定，混入路径会污染语义。

执行走 `execOnHost`（主机差异已被它吃掉），用 `execEscalated(..., escalatePreferred)`：npm 全局 bin 目录通常 root 可写，能提就提。

#### 7. 前端编排

`PluginCenter.vue` 的 `installBuiltin` 重写为编排函数，UI 结构不变：

```
确认弹窗(dialog.warning，content 含 hostName + 将发生的事)
  → api.env.overview(hostId) 预检
      · privilegeMode === 'none' → dialog.error 中止（无法提权就不能装系统包）
  → const missing = ['nodejs','dsh'].filter(slug => !catalog 中该项 installed)
  → missing 非空 → dialog.warning 询问（文案列出将安装的项 + 提示可能数分钟 + rc 版提示）
  → EnvRunDialog 执行 { action:'install', recipeSlug:'dsh' }   ← 复用现有组件
  → 成功：api.dsh.detectCommand(hostId)（服务端探测 + 保存）
  → api.plugins.installBuiltin(hostId, org, name)
  → message.success
```

关键点：
- **询问弹窗只出一个**。dsh 配方本来就把 Node 一起装，问两次会让用户以为要做两件事；且「同意装 Node、不同意装 dsh」这个中间态没有可交付的结果。文案按预检结果生成，清单列全，仍是显式确认。
- **顺序：先回填命令、再写插件记录**。记录是 `/dsh/` 入口的开关，先写会出现「入口存在但命令未配」的窗口期；反过来失败无副作用（多一条命令记录，卸载时一并清）。
- **`busyBuiltin` 覆盖整段编排**（预检→弹窗→执行→回填），并给按钮加 `:disabled="busyBuiltin !== ''"` —— naive-ui 的 `:loading` 不阻止点击。跨窗口并发天然安全（组件内 ref）；同一主机并发由后端 `Runner.hostLock` 拒绝并给出可读错误。
- **entry 归一化在后端做**。`handleListPlugins` 返回前，对 embedded 的 dsh 插件把 `Entry` 现算为 `/dsh/{hostId}/`；`handleInstallBuiltin` 写入时也写这个值。这样「插件入口形态」只在后端 `internal/plugin/manifest.go` 常量处出现一次，前端不必硬编码 org/name，且历史记录里的 `/dsh/` 不必做数据迁移（迁移漏掉一台就是一个坏链接）。
- **`PluginFrame.vue`、桌面图标、`windowBelongsTo`（已比 hostId）都不用改**：`frameSrc` 走 `pluginEntryURL` 自动拿到带主机的地址；config 变体是 `/dsh/7/?config=1`，后端 `Query().Get("config")` 照旧生效。

## 六、接口清单

| 变更 | 路径 | 说明 |
|---|---|---|
| 改 | `GET /api/hosts/{id}/dsh/status` | 按主机的状态 |
| 改 | `PUT /api/hosts/{id}/dsh/command` | body `{command}` |
| 新 | `POST /api/hosts/{id}/dsh/command/detect` | 探测 + 保存，返回 `{command, status}` |
| 改 | `/dsh/{id}/`、`/dsh/{id}`、`/dsh`、`/dsh/` | 入口与 307 |
| 删 | 旧 `/api/dsh/*` | 路径无法表达主机；单二进制前后端同版本发布，不做兼容层 |

store：`GetDshCommand` / `SetDshCommand` / `DeleteDshCommand`
env：`detectDsh` / `checkDsh` / `planDsh` / `dshNpmStep` + catalog 新增 `dsh` 项
dsh 包：`Launcher` / `Process` / `localProcess` / `remoteProcess` / `Manager`
api：`dshPrefixFor` / `dshTransport` / `rewriteDshLocation`
client.ts：`api.dsh.{status, setCommand, detectCommand}`

## 七、测试

沿用现有约定（不用 `NewServer`，用裸 `&Server{}` 字面量；helper `newTestStore(t)`、`newTestSessionManager(t)`、`newSampleBuffer[T](ttl)`）。

- **`internal/api/dsh_test.go`**：三个现有用例改调 `rewriteDshHTML(resp, "/dsh/7")`，断言用字面量 `/dsh/7` 而非常量（否则测试会跟着实现一起漂）；新增断言 shim 里的 `P` 常量确为 `/dsh/7`（现用例只断言 `location.origin` 出现，抓不到前缀没换）。新增 `TestDshPrefixFor`、`TestRewriteDshLocation`。
- **`Supervisor`**：用 fake `Launcher`/`Process`（内部一对 `io.Pipe`，`Wait()` 阻塞到 `Terminate()`），纯内存、无进程无 SSH。测四件事：喂入 readyPattern 后 `Endpoint()` 就绪；启动中 `Stop()` 能唤醒等待者；进程提前退出 → Failed 且 ready 被唤醒；并发 `EnsureRunning` 只 `Start` 一次。
- **`Manager`**：fake store，测 `SetCommand` 停旧换新、`Stop` 后 Endpoint 为空、`StopAll` 覆盖全部实例。
- **store**：`TestDshCommandRoundTrip`（不存在返回 `""`+`nil`）；`TestMigrateDshCommandsPerHost`（造只有 meta 表的旧库并塞 `dsh.command` → 跑迁移 → 断言新表存在、宿主机命令已搬、meta 键已删）。
- **env**：`TestPlanDshIncludesNodeFirst`（纯函数、不联网）断言首步名含 Node、末步含 `npm install -g @deepseek-ai/dsh` —— 这条把「Node 一起装」的决策钉死。
- **api**：`TestDshWorkspaceInstalledPerHost`（装了主机 7、没装 8 → 只有 7 为真）。

## 八、验证方法

```bash
# 后端
go vet ./... && go test ./...
go build -o bin/panelssh ./cmd/panelssh

# 前端（类型检查 + 构建，产物嵌入 internal/web/dist）
cd web && npm run typecheck && npm run build
```

端到端验收（真正的验收点）：**在一台远端主机上点安装 → 自动装 Node 24 + dsh → 回填命令 → iframe 打开 dsh 界面 → 会话可交互**。

分阶段验证：
- 阶段一完成后，宿主机（hostId=0）行为必须与改造前**完全一致**（只是路径变成 `/dsh/0/`），这是「行为不变的重构」是否做对的判据。
- 阶段二完成后走上面的端到端闭环。

## 九、风险与待验证

**实现顺序**（1–4 是行为不变的重构，可独立提交与验证）：
1. 存储 + 迁移 + 三方法 + main.go 改读
2. `Launcher` 抽象 + `Supervisor` 改造（本地逻辑原样搬）
3. `Manager` + `Server.dsh` 换型，仍只服务 hostId=0
4. 路由/前缀参数化 + Transport 切换
5. 远端 `remoteProcess` + 隧道（**风险集中，单独提交**）
6. env 配方 `dsh` + 探测回填接口
7. 前端编排 + entry 归一化

**必须在真机上验证的假设**：
- **R1（最关键）**：SSH session/client 关闭时，远端进程是否真被 sshd 终止。真机起一个 `sleep 300`，`client.Close()` 后 `ps` 看它是否还在 —— 这决定 pid 文件是「兜底」还是「必需」。
- **R2**：WebSocket 经 SSH 隧道反代是否可用。dsh 的实时通道走 WebSocket，而自定义 Transport 下 `ReverseProxy` 的 101 升级与双向拷贝需要实测（现有注释明确说 fetch/XHR/WebSocket/EventSource 四个出口都要走通）。
- **R3**：当前 dsh 版本是否仍支持 `--port 0` 与 `--host`（现有 supervisor 依赖它），以及是否接受 `--no-open`。
- **R4**：Node 24 是否必需（上游无 `engines` 约束）。若 Node 22 也可用，`planNode` 的版本参数可放宽。
- **R5**：`dial` 出的 SSH 隧道连接 `LocalAddr()` 为零值，需确认 `ReverseProxy` 在 upgrade 场景下不依赖它。

**其他风险**：
- 401 自愈依赖 `resp.Request.URL.Path == "/"`（出站请求已剥前缀）。这条现在能用，改造时**原样保留**，且 token 必须取对应 hostID 的 `Endpoint()`。
- 远端 dsh 连接一断进程即失，用户需重拉（十几秒 + 新 token）。这在 `watch → Failed → EnsureRunning` 链路下是**可表达、可自愈**的，不要试图做成「断线后进程还活着」。
- 上游 latest 是 rc 版，用户可能装到预发布版本 —— 在询问弹窗与设置页文案中如实告知，不隐藏。