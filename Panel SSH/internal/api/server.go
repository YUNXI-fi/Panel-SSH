// Package api 提供 HTTP 路由与处理器。
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"panelssh/internal/appstore"
	"panelssh/internal/auth"
	"panelssh/internal/config"
	"panelssh/internal/dsh"
	"panelssh/internal/env"
	"panelssh/internal/plugin"
	"panelssh/internal/secretbox"
	"panelssh/internal/sshclient"
	"panelssh/internal/store"
	"panelssh/internal/sysinfo"
	"panelssh/internal/web"
)

// Server 聚合所有处理器依赖。
type Server struct {
	cfg       *config.Config
	store     *store.Store
	box       *secretbox.Box
	sessions  *auth.Manager
	pool      *sshclient.Pool
	collector *sysinfo.Collector
	// dsh 实例按主机管理：宿主机用本地子进程，其余主机走 SSH 隧道。
	dsh        *dsh.Manager
	knownHosts string
	pluginsDir string
	// runtimes 管理带后端进程的插件。进程只跑在面板所在机器上，
	// 面向哪台目标机是每次调用的参数（见插件设计文档 5.3）。
	runtimes *plugin.RuntimeManager
	// appPackages 管理从 GitHub 拉取的应用包（清单、图标、界面配置）。
	// 与插件不同：应用包不携带可执行文件，只声明动作命令与资源。
	appPackages *appstore.Store
	// pluginTunnels 管理插件申请的端口转发（host:tunnel）。
	pluginTunnels *pluginTunnels

	// 采样器：同一台主机的并发/密集请求只会在目标机上跑一次命令。
	metrics *sampleBuffer[*sysinfo.Metrics]
	procs   *sampleBuffer[processSnapshot]
	network *sampleBuffer[*sysinfo.NetworkSnapshot]
	storage *sampleBuffer[*sysinfo.StorageSnapshot]

	// 指标历史：监控窗口开窗时用它回填曲线，否则每次开窗都要从空图重新爬两分钟。
	history *metricsHistory

	// 指标长驻流：监控窗口的推送通道。按 (主机, 档位) 复用 ——
	// 多个窗口看同一台机器时共享一条流，而不是各开一条 SSH 会话。
	metricsStreams *metricsStreamManager

	// 网络速率的基准值。网络采集与指标采集各自维护自己的历史采样：
	// 两者的调用时机完全独立（监控窗口按 3 秒轮询、网络窗口可能几分钟开一次），
	// 共用一份状态会让"上一次采样"的时间间隔忽长忽短，算出的速率跟着跳。
	netCollector *sysinfo.NetworkCollector

	// OVS 现状。它不并进 network 那个采样器，因为采集代价完全不同：
	// 读 OVS 的状态需要 root（见 network_ovs.go）。
	ovs *ovsSampler

	// 各主机当前未确认的网络变更。变更生效会切断 SSH，因此下发的命令
	// 与"确认/回滚"分两次请求完成，中间这段状态必须留在服务端。
	netChanges *netChangeStore

	// 在线状态：同样是"按主机去重 + 短时缓存"，但探测的是 TCP 端口，
	// 不落任何命令到目标机。
	status *sampleBuffer[hostStatus]

	// 目标机的系统画像：提权方式与「角色」列都读这一份，是唯一会登录每台机器的
	// 列表页采集，缓存窗口比在线状态长得多。
	profiles *sampleBuffer[*env.Profile]

	// 主机上下线告警的判定状态（含防抖），见 hostWatch。
	hostWatch *hostWatch

	// DSH 反代处理器：每台主机一个，构造一次后复用，见 dshProxy。
	dshProxies sync.Map

	// DSH 静态资源缓存：远端链路慢时先把首屏资源顺序抓到面板本地，
	// 避免浏览器并发请求被对端掐断导致白屏，见 dsh_cache.go。
	dshCache *dshAssetCache

	// DSH 原生地址的本机转发：绕开子路径反代完成 DeepSeek 账号登录，见 dshNative。
	dshNative *dshNative

	// 内嵌浏览的镜像反代处理器：同样每台主机一个，见 browserProxy。
	browserProxies sync.Map

	// 内嵌浏览的镜像凭据。iframe 是沙箱化的、带不上会话 Cookie，
	// 因此镜像那条路另用一枚只管读网页的令牌，见 mirrorTokens。
	mirrorTokens *mirrorTokens

	// 常驻 SSH 通道：DSH 反代、内嵌浏览器反代、SOCKS5 代理都要靠它把请求
	// 送到目标机上。按主机持有而不是走连接池，理由见 hostTunnels。
	tunnels *hostTunnels

	// dsh 实例专用的 SSH 连接：远端 dsh 进程挂在承载它的会话上，
	// 不能借 tunnels 那条会被空闲回收的连接（见 dshClients）。
	dshClients *dshClients

	// 按主机开的 SOCKS5 代理（浏览器"用主机访问网页"的系统代理模式）。
	socks *hostSocks

	// 环境管理编排器：快照、执行、补偿、回滚的状态机都在 env 包里。
	envRunner *env.Runner

	// 活跃的 WebSocket 连接（终端 / 批量 / 环境 / AI），供优雅退出时统一关闭。
	sockets *wsRegistry

	// AI 会话的占用表：同一个会话同时只允许一条连接改写它的历史。
	// 零值可用，不需要在 NewServer 里额外初始化。
	aiLeases aiSessionLeases
}

// NewServer 构造 API 服务。
func NewServer(
	cfg *config.Config,
	st *store.Store,
	box *secretbox.Box,
	sessions *auth.Manager,
	pool *sshclient.Pool,
	collector *sysinfo.Collector,
) *Server {
	s := &Server{
		cfg:        cfg,
		store:      st,
		box:        box,
		sessions:   sessions,
		pool:       pool,
		collector:  collector,
		knownHosts: filepath.Join(cfg.DataDir, "known_hosts"),
		pluginsDir: filepath.Join(cfg.DataDir, "plugins"),
		envRunner:  env.NewRunner(st, filepath.Join(cfg.DataDir, "env")),
		metrics:    newSampleBuffer[*sysinfo.Metrics](metricsTTL),
		history:    newMetricsHistory(),
		procs:      newSampleBuffer[processSnapshot](processTTL),
		network:    newSampleBuffer[*sysinfo.NetworkSnapshot](netTTL),
		storage:    newSampleBuffer[*sysinfo.StorageSnapshot](storageTTL),
		status:     newSampleBuffer[hostStatus](statusTTL),
		profiles:   newSampleBuffer[*env.Profile](profileTTL),
		hostWatch:  newHostWatch(),
		sockets:    newWSRegistry(),

		netCollector: sysinfo.NewNetworkCollector(),
		ovs:          newOvsSampler(),
		netChanges:   newNetChangeStore(),
	}
	// 流管理器要回指 Server：流里解析出的点得写进它的采样缓存与指标历史，
	// 否则流式消费者与非流式消费者（AI 助手的 get_metrics）会各说各话。
	// 只能在字面量之后再挂这一条。
	s.metricsStreams = newMetricsStreamManager(s)

	// dsh 实例同样按主机管理，且"在哪台机器上、用哪种方式起进程"只有
	// api 包知道（凭据、known_hosts 位置、目标机的工作目录），因此由这里
	// 注入，dsh 包只拿接口。同样只能在字面量之后再挂。
	s.tunnels = newHostTunnels(s)
	s.dshClients = newDshClients(s)
	s.dsh = dsh.NewManager(st, s.dshTarget)
	s.dshNative = newDshNative(s)
	s.dshCache = newDshAssetCache(cfg.DataDir)
	s.socks = newHostSocks(s)
	s.mirrorTokens = newMirrorTokens()
	// 插件后端进程：安装包目录是只读资源，数据目录是插件自己可写的地盘
	// （socket、pid、日志、它自己的数据库）。两者分开，卸载时删数据目录
	// 不会碰到安装包，反之亦然。
	s.runtimes = plugin.NewRuntimeManager(s.pluginsDir, filepath.Join(cfg.DataDir, "plugins-runtime"))
	s.pluginTunnels = newPluginTunnels(s)
	// 应用包与插件包分开放：应用包是声明式资源，卸载只删这一份目录。
	// 仓库源可能被管理员在应用中心改过，读一次落库的地址；读不到就用内置官方源。
	storedSource, _ := st.GetMeta(storeSourceMetaKey)
	s.appPackages = appstore.NewStore(filepath.Join(cfg.DataDir, "apps"), storedSource)
	return s
}

// 会话签名密钥轮换所依赖的元数据键。
const metaSessionEpoch = "session_epoch"

// sessionKey 用主密钥与 epoch 派生会话签名密钥。
// epoch 改变等价于作废全部历史会话。
func sessionKey(box *secretbox.Box, epoch string) []byte {
	return box.Derive("session:" + epoch)
}

// NewSessionManager 依据持久化的 epoch 构造会话管理器。
// epoch 落库保证了服务重启后已签发的会话依然有效。
func NewSessionManager(st *store.Store, box *secretbox.Box, ttl time.Duration) (*auth.Manager, error) {
	epoch, err := st.GetMeta(metaSessionEpoch)
	if err != nil {
		return nil, err
	}
	if epoch == "" {
		if epoch, err = secretbox.RandomToken(24); err != nil {
			return nil, err
		}
		if err := st.SetMeta(metaSessionEpoch, epoch); err != nil {
			return nil, err
		}
	}
	return auth.NewManager(sessionKey(box, epoch), ttl), nil
}

// Router 组装路由表。
func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()

	// 认证
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.Handle("POST /api/auth/password", s.requireAuth(s.handleChangePassword))
	mux.HandleFunc("GET /api/auth/me", s.handleMe)

	// 桌面布局（按用户保存）
	mux.Handle("GET /api/desktop/layout", s.requireAuth(s.handleGetDesktopLayout))
	mux.Handle("PUT /api/desktop/layout", s.requireAuth(s.handleSetDesktopLayout))

	// 主机
	mux.Handle("GET /api/hosts", s.requireAuth(s.handleListHosts))
	mux.Handle("GET /api/hosts/status", s.requireAuth(s.handleHostStatus))
	mux.Handle("POST /api/hosts", s.requireAuth(s.handleCreateHost))
	mux.Handle("PUT /api/hosts/{id}", s.requireAuth(s.handleUpdateHost))
	mux.Handle("DELETE /api/hosts/{id}", s.requireAuth(s.handleDeleteHost))
	mux.Handle("POST /api/hosts/{id}/test", s.requireAuth(s.handleTestHost))
	// 提权自检：实测面板能否在这台机器上拿到 root（装/升级系统环境的前提）。
	mux.Handle("POST /api/hosts/{id}/privilege-check", s.requireAuth(s.handlePrivilegeCheck))
	// 全部主机的账号角色。会真的登录每台机器，因此带缓存（见 handleHostRoles）。
	mux.Handle("GET /api/hosts/roles", s.requireAuth(s.handleHostRoles))
	// 批量指标：卡片视图一屏十几台，逐台请求会让并发与超时都失控，
	// 因此并发上限与整批预算都收在服务端一处（见 hostmetrics.go）。
	// 段数比 /api/hosts/{id}/metrics 少一段，ServeMux 不会与它相撞。
	mux.Handle("GET /api/hosts/metrics", s.requireAuth(s.handleHostMetrics))
	// 单台主机的提权方式：需要 root 的操作在动手前用它弹提醒。
	mux.Handle("GET /api/hosts/{id}/escalation", s.requireAuth(s.handleHostPrivilege))
	// 宿主机的提权账号：宿主不在主机表里，配置入口只能单独给。
	mux.Handle("GET /api/local/privilege", s.requireAuth(s.handleLocalPrivilege))
	mux.Handle("PUT /api/local/privilege", s.requireAuth(s.handleLocalPrivilege))
	mux.Handle("DELETE /api/local/privilege", s.requireAuth(s.handleLocalPrivilege))
	// 测试尚未保存的表单配置。比 {id}/test 少一段，两者不会互相匹配。
	mux.Handle("POST /api/hosts/test", s.requireAuth(s.handleTestHostDraft))

	// 终端（WebSocket）
	mux.Handle("GET /api/ws/terminal", s.requireAuth(s.handleTerminal))

	// 批量命令执行（WebSocket 流式回传）
	mux.Handle("GET /api/ws/batch", s.requireAuth(s.handleBatchExec))

	// AI 助手：模型配置与会话（会话走 WebSocket，因为要推送工具调用过程）
	mux.Handle("GET /api/ai/config", s.requireAuth(s.handleGetAIConfig))
	mux.Handle("PUT /api/ai/config", s.requireAuth(s.handleSetAIConfig))
	mux.Handle("POST /api/ai/test", s.requireAuth(s.handleTestAIConfig))
	mux.Handle("GET /api/ws/ai", s.requireAuth(s.handleAIChat))

	// 助手会话列表：一个会话就是一段独立对话，切换会话只换数据，不共享上下文。
	mux.Handle("GET /api/ai/sessions", s.requireAuth(s.handleAIListSessions))
	mux.Handle("POST /api/ai/sessions", s.requireAuth(s.handleAICreateSession))
	mux.Handle("DELETE /api/ai/sessions/{id}", s.requireAuth(s.handleAIDeleteSession))

	// 助手对话记录。窗口一打开就要显示历史，而 WebSocket 要等用户发出第一条
	// 消息才连，因此历史单走 HTTP；清空同样走 HTTP，它是持久化数据上的操作。
	// 两者都按会话（?session=）操作，不提供"默认会话"—— 那会让调用方在
	// 没指定会话时被动读到另一段对话。
	mux.Handle("GET /api/ai/history", s.requireAuth(s.handleAIGetHistory))
	mux.Handle("DELETE /api/ai/history", s.requireAuth(s.handleAIClearHistory))

	// DSH 工作台：面板把各台主机上的 dsh Web UI 反代到 /dsh/{id}/ 子路径下。
	// 入口要求登录（会话 Cookie 在 iframe 里照常携带），但不禁 CSRF —— 原因见 withCSRF。
	mux.Handle("GET /api/hosts/{id}/dsh/status", s.requireAuth(s.handleDshStatus))
	// 应用中心 · AI CLI：按主机探测与一键安装（用户级，不需要 root）。
	mux.Handle("GET /api/hosts/{id}/ai-cli", s.requireAuth(s.handleAICLIList))
	mux.Handle("POST /api/hosts/{id}/ai-cli/install", s.requireAuth(s.handleAICLIInstall))
	mux.Handle("POST /api/hosts/{id}/ai-cli/uninstall", s.requireAuth(s.handleAICLIUninstall))
	mux.Handle("GET /api/hosts/{id}/dsh/overview", s.requireAuth(s.handleDshOverview))
	// 原生地址：绕开 /dsh/{id}/ 子路径反代，供 dsh 完成 DeepSeek 账号登录。
	mux.Handle("GET /api/hosts/{id}/dsh/native", s.requireAuth(s.handleDshNative))
	mux.Handle("PUT /api/hosts/{id}/dsh/command", s.requireAuth(s.handleDshSetCommand))
	mux.Handle("POST /api/hosts/{id}/dsh/command/detect", s.requireAuth(s.handleDshDetectCommand))
	mux.Handle("POST /api/hosts/{id}/dsh/provision", s.requireAuth(s.handleDshProvision))
	mux.Handle("POST /api/hosts/{id}/dsh/start", s.requireAuth(s.handleDshStart))
	mux.Handle("POST /api/hosts/{id}/dsh/stop", s.requireAuth(s.handleDshStop))
	mux.Handle("POST /api/hosts/{id}/dsh/restart", s.requireAuth(s.handleDshRestart))
	mux.Handle("POST /api/hosts/{id}/dsh/update", s.requireAuth(s.handleDshUpdate))
	mux.Handle("POST /api/hosts/{id}/dsh/reinstall", s.requireAuth(s.handleDshReinstall))
	mux.Handle("GET /api/hosts/{id}/dsh/logs", s.requireAuth(s.handleDshLogs))
	mux.Handle("DELETE /api/hosts/{id}/dsh/logs", s.requireAuth(s.handleDshClearLogs))
	mux.Handle("GET /api/hosts/{id}/dsh/workspaces", s.requireAuth(s.handleDshWorkspaces))
	mux.Handle("GET /api/hosts/{id}/dsh/plugins", s.requireAuth(s.handleDshPlugins))
	mux.Handle("POST /api/hosts/{id}/dsh/plugins/install", s.requireAuth(s.handleDshPluginInstall))
	mux.Handle("POST /api/hosts/{id}/dsh/plugins/update", s.requireAuth(s.handleDshPluginUpdate))
	mux.Handle("POST /api/hosts/{id}/dsh/plugins/remove", s.requireAuth(s.handleDshPluginRemove))
	mux.Handle("POST /api/hosts/{id}/dsh/plugins/enabled", s.requireAuth(s.handleDshPluginEnabled))
	mux.Handle("/dsh/{id}/", s.requireAuth(s.handleDshEntry))
	mux.Handle("/dsh/{id}", s.requireAuth(s.handleDshRedirect))
	// 不带主机号的入口沿用改造前的语义："面板自己所在的那台机器"。
	// 老书签与旧链接因此照常可用，而不是撞上 404。
	mux.Handle("/dsh/", s.requireAuth(s.handleDshRootRedirect))
	mux.Handle(dshPrefix, s.requireAuth(s.handleDshRootRedirect))

	// 浏览器代理：把面板本机的一个 SOCKS5 端口变成"从这台主机出网"的入口，
	// 供用户配在自己浏览器上。按主机启停，因为出网路径就是这台主机的网络。
	mux.Handle("GET /api/hosts/{id}/browser/socks", s.requireAuth(s.handleBrowserSocksStatus))
	mux.Handle("POST /api/hosts/{id}/browser/socks/start", s.requireAuth(s.handleBrowserSocksStart))
	mux.Handle("POST /api/hosts/{id}/browser/socks/stop", s.requireAuth(s.handleBrowserSocksStop))

	// 内嵌浏览：把用户填的网址经目标机取回来，改写成本地镜像后放进窗口里的 iframe。
	//
	// 镜像路径不走会话鉴权，而是路径里自带一枚"只能读网页"的短时令牌（见 mirrorTokens）：
	// iframe 是沙箱化的，浏览器不会给它带会话 Cookie，用会话鉴权就等于每个请求都 401。
	// 令牌由下面那条接口签发，它才要求会话身份。两条路径都不禁 CSRF —— 原因见 withCSRF。
	mux.Handle("POST /api/hosts/{id}/browser/mirror", s.requireAuth(s.handleBrowserMirrorIssue))
	mux.Handle(browserPrefix+"/", http.HandlerFunc(s.handleBrowserProxy))
	mux.Handle(browserPrefix+"/{id}", s.requireAuth(s.handleBrowserRootRedirect))
	mux.Handle(browserPrefix+"/{id}/", http.HandlerFunc(s.handleBrowserProxy))

	// 文件管理（SFTP）
	mux.Handle("GET /api/hosts/{id}/files", s.requireAuth(s.handleListFiles))
	mux.Handle("GET /api/hosts/{id}/file/download", s.requireAuth(s.handleDownloadFile))
	// 图片 / 音视频 / PDF 的原位预览：inline + Range，供 img / video / iframe 使用。
	mux.Handle("GET /api/hosts/{id}/file/preview", s.requireAuth(s.handlePreviewFile))
	mux.Handle("GET /api/hosts/{id}/file/text", s.requireAuth(s.handleReadText))
	// 大文本的按块读取：超过单次读取上限时前端改用这个接口流式加载。
	mux.Handle("GET /api/hosts/{id}/file/text/chunk", s.requireAuth(s.handleTextChunk))
	mux.Handle("PUT /api/hosts/{id}/file/text", s.requireAuth(s.handleWriteText))
	mux.Handle("POST /api/hosts/{id}/files/mkdir", s.requireAuth(s.handleMkdir))
	mux.Handle("POST /api/hosts/{id}/files/rename", s.requireAuth(s.handleRename))
	mux.Handle("POST /api/hosts/{id}/files/delete", s.requireAuth(s.handleDeleteFiles))
	mux.Handle("POST /api/hosts/{id}/files/upload", s.requireAuth(s.handleUpload))
	mux.Handle("POST /api/hosts/{id}/files/chmod", s.requireAuth(s.handleChmod))
	mux.Handle("POST /api/hosts/{id}/files/copy", s.requireAuth(s.handleCopyFiles))
	mux.Handle("POST /api/hosts/{id}/files/move", s.requireAuth(s.handleMoveFiles))
	// 跨主机复制 / 移动：{id} 是源主机，目标主机在请求体里。
	mux.Handle("POST /api/hosts/{id}/files/transfer", s.requireAuth(s.handleTransferFiles))
	mux.Handle("POST /api/hosts/{id}/files/compress", s.requireAuth(s.handleCompress))
	mux.Handle("POST /api/hosts/{id}/files/extract", s.requireAuth(s.handleExtract))
	mux.Handle("GET /api/hosts/{id}/file/attr", s.requireAuth(s.handleFileAttr))
	mux.Handle("POST /api/hosts/{id}/files/chown", s.requireAuth(s.handleChown))
	mux.Handle("POST /api/hosts/{id}/files/touch", s.requireAuth(s.handleTouch))
	mux.Handle("POST /api/hosts/{id}/files/hash", s.requireAuth(s.handleHash))
	mux.Handle("POST /api/hosts/{id}/files/scan", s.requireAuth(s.handleFileScan))

	// 资源监控
	mux.Handle("GET /api/hosts/{id}/metrics", s.requireAuth(s.handleMetrics))
	// 开窗回填用：纯内存的历史采样点，不触发目标机操作。
	mux.Handle("GET /api/hosts/{id}/metrics/history", s.requireAuth(s.handleMetricsHistory))
	// 指标长驻流（WebSocket）：监控窗口按档位订阅，服务端持续推送采样点。
	// 与上面那条的分工是"持续给我"对"要一个当前值"，两者读同一份缓存与历史。
	mux.Handle("GET /api/ws/metrics", s.requireAuth(s.handleMetricsStream))
	mux.Handle("GET /api/hosts/{id}/processes", s.requireAuth(s.handleProcesses))
	mux.Handle("POST /api/hosts/{id}/processes/kill", s.requireAuth(s.handleKillProcess))
	mux.Handle("POST /api/hosts/{id}/exec", s.requireAuth(s.handleExec))

	// 网络现状（只读）：接口、地址、路由、DNS、监听端口。
	mux.Handle("GET /api/hosts/{id}/network", s.requireAuth(s.handleNetwork))

	// 修改目标机的 /etc/hosts。它不并入下面那套网络变更：改 hosts 不会切断
	// 面板的 SSH，写下去的又是面板算好的整份文本，不需要确认与回滚机制。
	mux.Handle("POST /api/hosts/{id}/network/hosts", s.requireAuth(s.handleHostsChange))

	// 网络变更。变更会切断面板赖以操作的 SSH，因此拆成"下发"与"确认/回滚"
	// 两步，中间由 NetworkManager 的检查点兜底：窗口内没等到确认就自动还原。
	mux.Handle("POST /api/hosts/{id}/network/apply", s.requireAuth(s.handleNetworkApply))
	mux.Handle("POST /api/hosts/{id}/network/commit", s.requireAuth(s.handleNetworkCommit))
	mux.Handle("POST /api/hosts/{id}/network/rollback", s.requireAuth(s.handleNetworkRollback))
	mux.Handle("GET /api/hosts/{id}/network/change", s.requireAuth(s.handleNetworkChange))

	// 卸掉 OVS 的开机自恢复服务。装它是有副作用的（目标机上多一个 systemd
	// 单元），因此必须有一个对称的卸载入口 —— 不能只装不卸。
	mux.Handle("POST /api/hosts/{id}/network/ovs/unpersist", s.requireAuth(s.handleOvsUnpersist))

	// 存储：现状查看、挂载/卸载（含 fstab 持久化），以及按需的
	// SMART 健康与目录用量探测（两项都重，不跟着轮询走，单独成接口）。
	mux.Handle("GET /api/hosts/{id}/storage", s.requireAuth(s.handleStorage))
	mux.Handle("GET /api/hosts/{id}/storage/smart", s.requireAuth(s.handleStorageSmart))
	mux.Handle("GET /api/hosts/{id}/storage/usage", s.requireAuth(s.handleStorageUsage))
	mux.Handle("POST /api/hosts/{id}/storage/apply", s.requireAuth(s.handleStorageApply))

	// Docker：容器、compose、镜像与网络。与其他按主机寻址的模块同一约定，
	// {id} 允许 0（宿主机）—— 面板自己所在机器的 docker 往往正是最常管的那一个。
	// 一次 summary 取齐状态/容器/镜像/网络：轮询多一条 SSH 会话，就多一次目标机进程创建。
	mux.Handle("GET /api/hosts/{id}/docker/summary", s.requireAuth(s.handleDockerSummary))
	mux.Handle("POST /api/hosts/{id}/docker/containers", s.requireAuth(s.handleDockerCreate))
	mux.Handle("POST /api/hosts/{id}/docker/containers/action", s.requireAuth(s.handleDockerContainerAction))
	mux.Handle("GET /api/hosts/{id}/docker/containers/logs", s.requireAuth(s.handleDockerContainerLogs))
	// 容器内文件（只读）：浏览目录、查看文本文件。与宿主机文件管理分开，
	// 因为它走的是 docker exec，不是 SFTP。
	mux.Handle("GET /api/hosts/{id}/docker/containers/files", s.requireAuth(s.handleDockerContainerFiles))
	mux.Handle("GET /api/hosts/{id}/docker/containers/file", s.requireAuth(s.handleDockerContainerFile))
	mux.Handle("POST /api/hosts/{id}/docker/images/pull", s.requireAuth(s.handleDockerImagePull))
	mux.Handle("POST /api/hosts/{id}/docker/images/remove", s.requireAuth(s.handleDockerImageRemove))
	mux.Handle("GET /api/hosts/{id}/docker/images/search", s.requireAuth(s.handleDockerImageSearch))
	// 镜像加速与代理：直接读写目标机的 /etc/docker/daemon.json，保存会重启 Docker。
	mux.Handle("GET /api/hosts/{id}/docker/config", s.requireAuth(s.handleDockerConfigRead))
	mux.Handle("PUT /api/hosts/{id}/docker/config", s.requireAuth(s.handleDockerConfigSave))
	mux.Handle("GET /api/hosts/{id}/docker/networks/detail", s.requireAuth(s.handleDockerNetworkDetail))
	mux.Handle("POST /api/hosts/{id}/docker/networks/create", s.requireAuth(s.handleDockerNetworkCreate))
	mux.Handle("POST /api/hosts/{id}/docker/networks/remove", s.requireAuth(s.handleDockerNetworkRemove))
	mux.Handle("POST /api/hosts/{id}/docker/networks/connect", s.requireAuth(s.handleDockerNetworkConnect))
	mux.Handle("GET /api/hosts/{id}/docker/compose/file", s.requireAuth(s.handleDockerComposeRead))
	mux.Handle("PUT /api/hosts/{id}/docker/compose/file", s.requireAuth(s.handleDockerComposeSave))
	mux.Handle("POST /api/hosts/{id}/docker/compose/action", s.requireAuth(s.handleDockerComposeAction))

	// 插件。静态资源与 SDK 刻意免鉴权：插件运行在 opaque origin 的沙箱里，
	// 其子资源请求不携带会话 Cookie；若加鉴权，插件页面将无法加载自身资源。
	// 这些资源不含用户数据，所有敏感操作都要经过能力桥由主应用代办。
	mux.HandleFunc("GET /api/plugins/sdk.js", s.handlePluginSDK)
	mux.HandleFunc("GET /api/plugins/{org}/{name}/app/{path...}", s.handlePluginAsset)

	mux.Handle("GET /api/plugins", s.requireAuth(s.handleListPlugins))
	// 安装接口把主机放在子路径而非查询串：主机是这个动作的主语
	// （"往哪台机器上装"），放路径里才能和下面那些按主机寻址的接口保持同一种读法，
	// 也避免在 handler 里对查询参数做一遍额外校验。{id} 允许 0，表示面板本机。
	//
	// 主机要放在 {org}/{name} 前面：两条模式都以 hosts/{id} 起头，
	// 段数不同（install 那条多一段 builtin），ServeMux 才能明确挑出更具体的那个。
	// 反过来把 {org} 放前面时，内置插件的安装路径会与通用安装路径在
	// 同一段上互斥，注册时就 panic。
	mux.Handle("POST /api/plugins/hosts/{id}/install", s.requireAuth(s.handleInstallPlugin))
	mux.Handle("POST /api/plugins/hosts/{id}/builtin/{org}/{name}/install", s.requireAuth(s.handleInstallBuiltin))
	mux.Handle("DELETE /api/plugins/hosts/{id}/{org}/{name}", s.requireAuth(s.handleUninstallPlugin))
	mux.Handle("POST /api/plugins/hosts/{id}/{org}/{name}/enabled", s.requireAuth(s.handleSetPluginEnabled))
	mux.Handle("PUT /api/plugins/hosts/{id}/{org}/{name}/capabilities", s.requireAuth(s.handleSetPluginCapabilities))

	// 插件前端经主应用转发到插件的后端进程；method 是插件的私有命名空间，面板不解释它。
	// 这里要求会话身份：插件前端在沙箱里发的请求不带 Cookie，由主应用代办。
	mux.Handle("POST /api/plugins/{org}/{name}/rpc/{method}", s.requireAuth(s.handlePluginRPC))

	// 插件后端回调面板。免会话鉴权——插件进程不是浏览器，没有会话 Cookie，
	// 它的身份由 X-Plugin-Token 请求头给出（令牌文件 0600，与进程同生命周期）。
	mux.HandleFunc("POST "+pluginAPIPrefix+"/{method}", s.handlePluginAPI)

	// 环境管理：系统画像/软件目录、包查询、预检、安装源、操作记录；
	// 真正的变更走 WebSocket（/api/ws/env）流式回传每个步骤。
	// 路径里的 {id} 允许 0：0 表示面板所在本机，与监控接口同一约定。
	mux.Handle("GET /api/hosts/{id}/env/overview", s.requireAuth(s.handleEnvOverview))
	mux.Handle("GET /api/hosts/{id}/env/packages", s.requireAuth(s.handleEnvPackages))
	mux.Handle("GET /api/hosts/{id}/env/candidates", s.requireAuth(s.handleEnvCandidates))
	mux.Handle("POST /api/hosts/{id}/env/plan", s.requireAuth(s.handleEnvPlan))
	mux.Handle("GET /api/hosts/{id}/env/sources", s.requireAuth(s.handleEnvSources))
	// 环境变量：读一次列出全部（生效值 + 来源），写走同步 POST（见 env_vars.go 的说明）。
	mux.Handle("GET /api/hosts/{id}/env/vars", s.requireAuth(s.handleEnvVars))
	mux.Handle("POST /api/hosts/{id}/env/vars/set", s.requireAuth(s.handleEnvVarSet))
	mux.Handle("POST /api/hosts/{id}/env/vars/unset", s.requireAuth(s.handleEnvVarUnset))
	mux.Handle("POST /api/hosts/{id}/env/vars/restore", s.requireAuth(s.handleEnvVarRestore))
	mux.Handle("POST /api/hosts/{id}/env/vars/path-add", s.requireAuth(s.handleEnvVarPathAdd))
	// 扫描约定位置里"装好了但用不到"的命令行环境。只读，见 env_vars.go。
	mux.Handle("GET /api/hosts/{id}/env/scan", s.requireAuth(s.handleEnvScan))
	mux.Handle("GET /api/env/ops", s.requireAuth(s.handleListEnvOps))
	mux.Handle("GET /api/env/ops/{opId}", s.requireAuth(s.handleGetEnvOp))
	// 已安装且支持在面板里打开的应用：桌面图标与「打开」入口的数据源。
	mux.Handle("GET /api/launch-apps", s.requireAuth(s.handleLaunchApps))
	// 安全应用的原生 UI：元数据、白名单操作、配置文件读写。
	mux.Handle("GET /api/hosts/{id}/security-apps/{slug}", s.requireAuth(s.handleSecurityApp))
	mux.Handle("POST /api/hosts/{id}/security-apps/{slug}/actions/{action}", s.requireAuth(s.handleSecurityAction))
	mux.Handle("GET /api/hosts/{id}/security-apps/{slug}/config", s.requireAuth(s.handleSecurityConfigRead))
	mux.Handle("PUT /api/hosts/{id}/security-apps/{slug}/config", s.requireAuth(s.handleSecurityConfigSave))
	mux.Handle("GET /api/hosts/{id}/security-apps/{slug}/browse", s.requireAuth(s.handleSecurityBrowse))
	// 结构化视图：Fail2Ban 与 GnuPG 的输出在这里解析成卡片/表格所需的数据，
	// 前端不再自己拆命令文本。变更操作仍走上面的白名单 actions。
	mux.Handle("GET /api/hosts/{id}/security-apps/{slug}/overview", s.requireAuth(s.handleFail2BanOverview))
	mux.Handle("GET /api/hosts/{id}/security-apps/{slug}/keys", s.requireAuth(s.handleGnuPGKeys))
	// 应用仓库：从 GitHub 拉取的应用包清单、图标与安装/卸载。
	mux.Handle("GET /api/store/apps", s.requireAuth(s.handleStoreApps))
	mux.Handle("POST /api/store/apps/{slug}/install", s.requireAuth(s.handleStoreInstall))
	mux.Handle("DELETE /api/store/apps/{slug}", s.requireAuth(s.handleStoreUninstall))
	mux.Handle("GET /api/store/apps/{slug}/icon", s.requireAuth(s.handleStoreIcon))
	// 仓库源：读取与切换（默认官方仓库，管理员可改）。
	mux.Handle("GET /api/store/source", s.requireAuth(s.handleStoreSourceGet))
	mux.Handle("PUT /api/store/source", s.requireAuth(s.handleStoreSourceSave))
	mux.Handle("GET /api/ws/env", s.requireAuth(s.handleEnvRun))

	// 操作快照：把上面那批变更操作留下的快照汇总成跨主机视图。
	// 只有读取 —— 快照由环境变更自己产生，回滚仍走 /api/ws/env。
	mux.Handle("GET /api/snapshots", s.requireAuth(s.handleSnapshotList))
	mux.Handle("GET /api/snapshots/{opId}", s.requireAuth(s.handleSnapshotDetail))

	// 系统日志（只读）：目标机的 journalctl / 日志文件 / dmesg，以及面板自身
	// 落盘的那份。路径里的 {id} 同样允许 0（本机）。
	// /api/panel/logs 不与 /api/hosts/{id}/logs 冲突：前者不在 /api/hosts/ 下。
	mux.Handle("GET /api/hosts/{id}/logs/sources", s.requireAuth(s.handleLogSources))
	mux.Handle("GET /api/hosts/{id}/logs", s.requireAuth(s.handleHostLogs))
	mux.Handle("GET /api/panel/logs", s.requireAuth(s.handlePanelLogs))

	// 批量任务记录。只有读取：记录的写入由批量执行本身在 /api/ws/batch
	// 的连接生命周期里完成，不对外开放。
	mux.Handle("GET /api/tasks", s.requireAuth(s.handleListTasks))
	mux.Handle("GET /api/tasks/{id}", s.requireAuth(s.handleGetTask))

	// 通知中心。同样只有读取与"标记已读"：通知由服务端在各事件点上产生，
	// 不开放给前端写入 —— 否则通知就失去了"系统记录"的可信度。
	mux.Handle("GET /api/notifications", s.requireAuth(s.handleListNotifications))
	mux.Handle("POST /api/notifications/read", s.requireAuth(s.handleMarkNotificationsRead))
	mux.Handle("POST /api/notifications/read-all", s.requireAuth(s.handleMarkAllNotificationsRead))

	// 前端静态资源（含 SPA 回退），放在最后作为兜底
	//
	// 但 /api/ 下的未注册路径绝不能落到 SPA 兜底：那样前端调错接口时会拿到
	// 200 + index.html，把 HTML 当 JSON 用，报错现场离真正的原因很远。
	// 这里显式把未匹配的 /api/* 变成 404 JSON。
	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "接口不存在："+r.Method+" "+r.URL.Path)
	}))

	if !s.cfg.DevMode {
		mux.Handle("/", web.Handler())
	}

	return withRecover(withCSRF(mux))
}

// 状态变更请求必须携带的自定义请求头。
//
// 跨站页面无法在不触发 CORS 预检的前提下添加自定义头，而本服务不返回任何
// CORS 放行头，因此「存在该头」这一条件即可挡住 CSRF。与会话 Cookie 的
// SameSite=Lax 构成双重防护。
const csrfHeader = "X-PanelSSH-Request"

// withCSRF 拒绝缺少自定义头的状态变更请求。
func withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// DSH 工作台是面板反代的第三方应用，它的前端不可能带上本面板的自定义请求头。
		// 跳过这道检查是可接受的：该路径仍然要求会话 Cookie，而跨站发起的非 GET 请求
		// 在 SameSite=Lax 下不会携带 Cookie，因此照样会被鉴权挡在外面。
		// 精确路径 /dsh（无斜杠）也要一并豁免：它会 307 跳到 /dsh/，
		// 不豁免的话这种跳转请求会先被 CSRF 检查拒成 403。
		if r.URL.Path == dshPrefix || strings.HasPrefix(r.URL.Path, dshPrefix+"/") {
			next.ServeHTTP(w, r)
			return
		}
		// 内嵌浏览与被代理的页面同理：镜像里的表单会以页面自己的形态提交，
		// 不可能带上本面板的自定义头。跳过这道检查是可接受的：该路径仍然要求
		// 会话 Cookie，而跨站发起的非 GET 请求在 SameSite=Lax 下不携带 Cookie，
		// 因此照样会被鉴权挡在外面。
		if r.URL.Path == browserPrefix || strings.HasPrefix(r.URL.Path, browserPrefix+"/") {
			next.ServeHTTP(w, r)
			return
		}
		// 插件后端的回调接口用令牌鉴权，全程不讲 Cookie。CSRF 的前提是
		// "浏览器会自动带上凭据"，而这枚令牌来自插件自己去读的 0600 文件，
		// 跨站页面拿不到它，也就无从伪造 —— 这道检查对它是纯噪音。
		if strings.HasPrefix(r.URL.Path, pluginAPIPrefix+"/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(csrfHeader) == "" {
			writeErr(w, http.StatusForbidden, "缺少 "+csrfHeader+" 请求头，请求被拒绝")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth 校验会话，并把用户 ID 注入上下文。
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, ok := s.sessions.Current(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r.WithContext(auth.WithUser(r.Context(), userID)))
	})
}

// withRecover 兜底 panic，避免单个请求打挂整个进程。
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler 是标准库约定的"安静中止"信号，不是故障：
			// 反代在拷贝响应体失败时会抛它（最典型的场景就是浏览器提前断开 ——
			// 切换页面、取消加载），net/http 收到它会直接掐掉连接、不做任何记录。
			// 这里必须原样放行，否则它会被当成服务端错误：日志里刷出一段带完整栈的
			// 记录，并且还会往一个已经断开的连接上再写一次 500（表现为
			// "superfluous response.WriteHeader"）。
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			log.Printf("[panic] %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
			writeErr(w, http.StatusInternalServerError, "服务内部错误")
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[warn] 写出 JSON 失败: %v", err)
	}
}

// decodeJSONBody 解析 JSON 请求体，失败时已写好 400 响应。
// limit 用于限制请求体大小，避免超大请求耗尽内存。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体格式错误或过大")
		return err
	}
	return nil
}

// errorBody 是所有错误响应的统一结构，前端只需判断 error 字段。
//
// code 是可选的机器可读分类：只给一句中文提示，前端分不清"该重试"还是
// "该让用户去授权"，而这两种情况要给出完全不同的界面反馈。
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// writeErrCode 在错误体里附带一个机器可读的分类。
func writeErrCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}
