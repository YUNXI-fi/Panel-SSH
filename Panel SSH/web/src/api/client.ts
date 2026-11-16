// 后端 API 客户端。所有请求同源，凭据由 HttpOnly Cookie 承载，
// 前端不接触 token，避免 XSS 直接窃取会话。

export interface SessionUser {
  id: number;
  username: string;
}

/** 桌面文件夹：把若干应用图标收进一个分组容器。 */
export interface DesktopFolder {
  id: string;
  name: string;
  /** 收纳的应用 id，与桌面图标的 appId 一一对应。 */
  apps: string[];
}

/**
 * 桌面布局。
 *
 * 只记"被移出桌面的图标"和文件夹结构，不记完整可见列表：
 * 插件是动态的，新装的插件默认就该出现在桌面上。
 */
export interface DesktopLayout {
  hidden: string[];
  folders: DesktopFolder[];
}

/**
 * SSH 认证方式。
 *
 * none 不是"还没填凭据"，而是目标机确实不需要凭据（SSH none 认证，
 * 或服务端允许空密码）。localkey 是"用面板所在机器上已有的私钥"，
 * 同样不需要用户输入任何秘密。这两种与"password/key 但凭据留空"必须区分：
 * 后者登不上去，前两者能。
 */
export type AuthType = 'password' | 'key' | 'none' | 'localkey';

export interface Host {
  id: number;
  name: string;
  addr: string;
  port: number;
  username: string;
  authType: AuthType;
  /**
   * 是否已配置登录凭据。
   *
   * 新增主机允许先只填地址、端口、用户名，凭据以后再补。这类主机能看状态、
   * 能测网络连通，但无法登录 —— 界面必须据此把连接类操作挡下并说明原因，
   * 而不是让用户点进去才看到一条"密码为空"。
   */
  hasSecret: boolean;
  group: string;
  remark: string;
  createdAt: string;
  updatedAt: string;
}

/**
 * 面板在目标机上提权的路径，与后端 env.PrivilegeMode 一一对应。
 *
 * 空串表示"还没探到"，与 none（探到了，但这台机器上没法提权）不是一回事。
 */
export type PrivilegeMode = '' | 'root' | 'su' | 'sudo-nopasswd' | 'sudo-password' | 'none';

export interface HostForm {
  name: string;
  addr: string;
  port: number;
  username: string;
  authType: AuthType;
  secret: string;
  /**
   * 提权口令（root 口令），可选。
   * 面板用它回答 su 的提示；留空则退回登录凭据走 sudo。
   * 编辑时留空即保持原值不变。
   */
  escalateSecret: string;
  group: string;
  remark: string;
}

/**
 * 主机的提权方式（列表页「提权」列）。
 *
 * 它答的是"面板能在这台机器上用哪条路径拿到 root"，不是"这个账号属于哪个用户组"。
 * privilegeMode 为空串表示"还没探到"（离线、凭据错误或超出探测预算），
 * 与 none（探到了，但这台机器上没法提权）不是一回事。
 */
export interface HostRole {
  id: number;
  privilegeMode: PrivilegeMode;
}

/**
 * 主机指标摘要（卡片视图用）。
 *
 * 三个百分比为 null 表示这次没采到（离线、超预算、首次采样尚无基准），
 * 与"采到了，值是 0"必须区分 —— 后者才是"机器空闲"。
 */
export interface HostMetricsBrief {
  id: number;
  cpuPercent: number | null;
  memPercent: number | null;
  diskPercent: number | null;
  memUsed: number;
  memTotal: number;
  diskUsed: number;
  diskTotal: number;
  sampledAt: string;
}

/**
 * 主机在线状态。由服务端 TCP 探测得出，与「测试连接」不是一回事：
 * 它只回答"端口通不通"，不校验凭据。
 * latencyMs 仅在 online 为 true 时有意义，且是带小数的毫秒（局域网建连不足 1 毫秒）。
 */
export interface HostStatus {
  id: number;
  online: boolean;
  latencyMs: number;
}

/**
 * 这台主机会怎么提权（需要 root 的操作在动手前用它弹提醒）。
 *
 * label 直接由后端给出可显示的说法，前端不再翻译 mode。
 */
export interface EscalationInfo {
  mode: PrivilegeMode;
  label: string;
  /** 为假表示这台机器上根本提不了权：需要 root 的操作注定失败，得提前告知。 */
  escalatable: boolean;
}

/**
 * 宿主机的提权账号。
 *
 * 宿主机不在主机表里，这份配置单独存：account 为 root 走 su，
 * 为面板运行用户走 sudo -S。口令永远不回传，只回 configured。
 */
export interface LocalPrivilege {
  account: string;
  configured: boolean;
}

export interface TestResult {
  ok: boolean;
  message: string;
  /**
   * SSH 握手耗时（毫秒），覆盖 TCP 连接、密钥交换与认证三段。
   * 失败时为 0 —— 那时用户要看的是原因，不是耗时。
   * 是带小数的毫秒：本机与同网段的握手常在 1 毫秒以内。
   */
  latencyMs: number;
  /**
   * 这次只验证了网络可达，主机尚未配置凭据。
   *
   * 为真时不要再问账号角色：没有凭据必然登录失败，那一步只会多等一个超时，
   * 还会把"网络是通的"这个结论盖成一条错误。
   */
  needSecret?: boolean;
}

/** 提权自检结论：面板能否在这台机器上取得 root。 */
export interface PrivilegeCheckResult {
  ok: boolean;
  /** 走的提权路径：root / su / sudo-* / none。 */
  mode: PrivilegeMode;
  detail: string;
}

export interface ExecResult {
  stdout: string;
  stderr: string;
  exitCode: number;
  durationMs: number;
  stdoutClipped: boolean;
  stderrClipped: boolean;
}

export interface CapabilityInfo {
  name: string;
  label: string;
  /**
   * 风险等级。能力清单由面板按风险升序给出，授权界面直接照这个顺序排版 ——
   * 插件清单里的声明顺序不算数，"什么都想要"的插件不该靠调整顺序把高危项排到前面。
   */
  risk: 'low' | 'medium' | 'high';
}

/** DSH 工作台内置插件的标识；后端与前端都以它判断"是不是这个插件"。 */
export const DSH_WORKSPACE_ORG = 'panelssh';
export const DSH_WORKSPACE_NAME = 'dsh-workspace';

export interface PluginInfo {
  id: number;
  org: string;
  name: string;
  displayName: string;
  description: string;
  version: string;
  entry: string;
  icon: string;
  /**
   * archive 是用户上传的 zip 包，代码来自第三方，必须在沙箱里运行；
   * embedded 是随面板分发的内置插件，属于第一方代码，不套沙箱。
   */
  source: 'archive' | 'embedded';
  /** 清单里申请的能力。 */
  capabilities: string[];
  /** 管理员实际授予的能力。 */
  granted: string[];
  enabled: boolean;
  /**
   * 插件被安装到哪台主机上（0 表示面板所在的本机）。
   *
   * 安装粒度是「每台主机一份」，因此同名的插件可以有多条记录，
   * 每条各自带授权与启停状态；能力桥的越权校验也以它为准。
   */
  hostId: number;
  /** 主机名（宿主机为「宿主机」），用于在界面上分辨同名插件的不同安装。 */
  hostName: string;
  installedAt: string;
}

/** 内置插件：随面板分发，由用户决定是否安装。 */
export interface BuiltinPlugin {
  org: string;
  name: string;
  displayName: string;
  description: string;
  version: string;
  url: string;
  installed: boolean;
}

/** 应用中心里的 AI CLI 目录项在目标机上的状态。 */
export interface AICLIApp {
  id: string;
  name: string;
  pkg: string;
  bins: string[];
  /** 运行所需的最低 Node 大版本；0 表示不限制。 */
  nodeMajor: number;
  /** 工具自身支持的平台；空数组表示不限制（纯 JS 包）。 */
  os: string[];
  arch: string[];
  /** 目录分类：ai 是 AI 编码代理，dev 是通用开发工具。 */
  group: 'ai' | 'dev';
  note?: string;
  installed: boolean;
  /** 目标机是否满足平台条件（含面板安装链路自身的限制）。 */
  supported: boolean;
  version: string;
  /** 实际命中的可执行文件路径；空表示未安装。 */
  path: string;
}

/** 目标机的平台，用于解释某个工具为什么装不了。 */
export interface AICLIPlatform {
  os: string;
  arch: string;
}

export interface PluginList {
  plugins: PluginInfo[];
  builtins: BuiltinPlugin[];
  capabilities: CapabilityInfo[];
}

/**
 * DSH 工作台在某台主机上的运行状态。
 *
 * 每台主机一个实例，因此这个状态是按主机取的，不能跨主机复用。
 */
export interface DshStatus {
  /** 该主机是否配过启动命令。false 表示还没装或还没回填。 */
  available: boolean;
  state: 'disabled' | 'stopped' | 'starting' | 'running' | 'failed';
  message: string;
  /** 就绪时的面板内访问地址（/dsh/{hostId}/），未就绪时为空串。 */
  url: string;
  command: string;
  pid?: number;
  startedAt?: string;
  logs: string[];
}

/**
 * DSH 管理器首屏数据。
 *
 * 与 DshStatus 的区别：这里额外带上了安装形态（版本、Node、数据目录），
 * 那些信息要执行命令才能拿到，因此只在打开管理器或手动刷新时取，
 * 不跟着状态轮询一起每两秒跑一次。
 */
export interface DshOverview extends DshStatus {
  /** 目标机上是否存在可执行的 dsh。 */
  installed: boolean;
  version: string;
  nodeVersion: string;
  /** dsh 的 DSH_HOME（默认 ~/.dsh）。 */
  dataDir: string;
  pid: number;
  startedAt?: string;
}

/**
 * dsh 的原生访问地址。
 *
 * 它绕开面板的 /dsh/{id}/ 子路径反代，让浏览器直接访问 dsh 监听的回环端口。
 * 之所以需要这条路：dsh 的 DeepSeek 账号登录只接受回环 origin，子路径反代下
 * 授权与回调地址都对不上，登录必然失败。
 */
export interface DshNativeAddress {
  /** 形如 http://127.0.0.1:35609/?token=...，含 dsh 的一次性启动令牌。 */
  url: string;
  /** 地址只监听面板机回环：只有浏览器与面板同机时打得开。 */
  loopbackOnly: boolean;
}

/** dsh 工作区注册表里的一行。 */
export interface DshWorkspace {
  id: string;
  title: string;
  path: string;
  /** 未归档的会话数。 */
  sessionCount: number;
  createdAt: string;
  updatedAt: string;
}

/** web profile 里的一个插件（组合包）。 */
export interface DshPlugin {
  name: string;
  spec: string;
  enabled: boolean;
  bundle: boolean;
  description: string;
  homepage: string;
}

/**
 * 浏览器代理在某台主机上的状态。
 *
 * 代理监听的是面板本机的一个回环端口，因此 addr 是"填进浏览器的那一串"，
 * 不是目标机上的地址 —— 目标机只是出网路径的出口。
 */
export interface BrowserProxyStatus {
  running: boolean;
  /** 形如 127.0.0.1:38271；未运行时为空串。 */
  addr: string;
}

/**
 * 内嵌浏览的镜像凭据。
 *
 * 它只解锁"经某台主机读网页"，不含任何面板操作能力 —— 因为镜像路径不能用
 * 会话 Cookie 鉴权：iframe 是沙箱化的（opaque origin），浏览器只会给它带
 * SameSite=None 的 Cookie，而面板的会话 Cookie 是 SameSite=Lax。
 */
export interface BrowserMirrorToken {
  token: string;
  /** 毫秒时间戳；窗口据此在到期前换一枚新的。 */
  expiresAt: number;
}

export interface AIConfig {
  baseUrl: string;
  model: string;
  /** 只告知是否已配置，服务端不会回传明文。 */
  hasApiKey: boolean;
  /** 一次提问内允许的工具调用轮数上限。 */
  maxSteps: number;
}

export interface AITestResult {
  ok: boolean;
  message: string;
}

/**
 * 助手的一个会话：一段彼此独立的对话。
 *
 * 会话就是隔离边界 —— 历史与模型上下文都挂在它下面，切换会话不会带来
 * 另一个会话的任何内容。
 */
export interface AISession {
  id: number;
  /** 服务端取第一句提问生成；空串表示还没聊过，界面显示"新对话"。 */
  title: string;
  createdAt: string;
  updatedAt: string;
}

/** AI 助手会话过程中服务端推送的消息。 */
export interface AIMessage {
  type: 'assistant' | 'tool' | 'toolResult' | 'confirm' | 'confirmExpired' | 'done' | 'error';
  content?: string;
  callId?: string;
  name?: string;
  args?: string;
  reason?: string;
  ok?: boolean;
  output?: string;
  message?: string;
}

/** 模型提出的一次工具调用。字段与服务端的 ai.ToolCall 一一对应。 */
export interface AIChatToolCall {
  id: string;
  type: string;
  function: {
    name: string;
    /** 参数是 JSON 字符串，不是对象 —— 这一点与服务端存储的形态一致。 */
    arguments: string;
  };
}

/**
 * 一条对话历史。字段与服务端 ai.Message 一一对应。
 *
 * 这是**模型看到的上下文本身**，不是界面上的消息流：窗口关掉再打开时要靠它
 * 既恢复显示、又让模型接上之前的话题。
 */
export interface AIChatMessage {
  role: 'system' | 'user' | 'assistant' | 'tool';
  content?: string;
  tool_calls?: AIChatToolCall[];
  /** 仅 role 为 tool 时出现，指回它对应的是哪次调用。 */
  tool_call_id?: string;
}

/** 服务端保存的助手对话。 */
export interface AIChatHistory {
  history: AIChatMessage[];
  /**
   * tool_call_id → 那次调用成功了没有。
   *
   * 模型历史里没有这个信息（ai.Message 只有 role 和 content），所以由服务端
   * 另外记一份。少了它，回看历史时会把失败的命令当成成功的。
   */
  toolStatus: Record<string, boolean>;
}

export interface FileEntry {
  name: string;
  path: string;
  isDir: boolean;
  size: number;
  mode: string;
  modTime: string;
  isLink: boolean;
}

export interface FileListing {
  path: string;
  /** 上级目录；已在根目录时为空串。 */
  parent: string;
  entries: FileEntry[];
}

export interface TextFile {
  path: string;
  content: string;
  size: number;
  /** 远端文件的权限位，用于在编辑器里展示，并提示保存不会改变它。 */
  mode: string;
  modTime: string;
  /**
   * 服务端识别出的原始编码（utf-8 / utf-8-bom / utf-16le / utf-16be / gb18030）。
   * content 已经转成 UTF-8；保存时要把这个值带回去，按原编码写回。
   */
  encoding: string;
}

/**
 * 大文本的一块。
 *
 * offset / nextOffset 都是文件里的字节位置：下一块必须从上一个 nextOffset 开始，
 * 服务端已经把它对齐到字符边界。eof 为真表示没有下一块了。
 */
export interface TextChunk {
  path: string;
  chunk: string;
  encoding: string;
  offset: number;
  nextOffset: number;
  eof: boolean;
  size: number;
  mode: string;
  modTime: string;
}

/**
 * 一个条目的完整元数据，与服务端 attrResponse 一一对应。
 *
 * 时间戳是绝对时刻的 ISO 串，不含时区歧义：面板与目标机不在同一时区时，
 * 靠字符串传"10:30"会让两边各自理解一次。
 */
export interface FileAttr {
  path: string;
  name: string;
  isDir: boolean;
  isLink: boolean;
  /** 符号链接指向的目标；非链接时为空串。 */
  linkTarget: string;
  size: number;
  /** 实际占用磁盘的字节数，可能大于 size（稀疏文件则相反）。取不到时为 0。 */
  blocks: number;
  /** 形如 -rw-r--r--。 */
  mode: string;
  /** 形如 755 或 4755，属性面板的可编辑区用它回填。 */
  modeOctal: string;
  uid: number;
  gid: number;
  /** 属主名 / 属组名；目标机的 stat 不可用时为空串。 */
  owner: string;
  group: string;
  atime: string;
  mtime: string;
  /** 元数据最后变更时间；取不到时为零值时间。 */
  ctime: string;
  nlink: number;
  inode: number;
}

export interface Disk {
  mountPoint: string;
  filesystem: string;
  total: number;
  used: number;
  available: number;
  usePercent: number;
}

/** GPU/NPU 加速设备。utilPercent / tempC 为 -1 表示工具未给出该值。 */
export interface Accelerator {
  kind: 'gpu' | 'npu';
  /** 厂商工具给出的设备编号。 */
  id: number;
  /** NPU 多核模块内的芯片号；GPU 与单核 NPU 为 0。 */
  chip: number;
  name: string;
  utilPercent: number;
  memUsed: number;
  memTotal: number;
  tempC: number;
}

export interface Metrics {
  hostname: string;
  kernel: string;
  /** 发行版名称（/etc/os-release 的 PRETTY_NAME）；取不到时为空串。 */
  osName: string;
  cpuModel: string;
  cpuCores: number;
  cpuPercent: number;
  load1: number;
  load5: number;
  load15: number;
  uptimeSeconds: number;
  memTotal: number;
  memAvailable: number;
  memUsed: number;
  swapTotal: number;
  swapFree: number;
  netRxBytes: number;
  netTxBytes: number;
  netRxRate: number;
  netTxRate: number;
  diskReadBytes: number;
  diskWriteBytes: number;
  diskReadRate: number;
  diskWriteRate: number;
  disks: Disk[];
  /** GPU/NPU 列表；目标机没有对应厂商工具时为空数组。 */
  accelerators: Accelerator[];
  /** 首次采样没有基准值，速率类指标不可用。 */
  deltaReady: boolean;
  sampledAt: string;
}

/** 历史采样点里的加速设备利用率。 */
export interface MetricsPointAccel {
  kind: 'gpu' | 'npu';
  id: number;
  chip: number;
  /** -1 表示工具未给出该值（未知），不是 0% 空闲。 */
  utilPercent: number;
}

/**
 * 一次历史采样。
 *
 * 服务端只保留曲线需要的字段：完整 Metrics 里的挂载点列表按主机 × 几十个点
 * 存下来是纯浪费。deltaReady 为 false 时速率类字段是不可用的占位 0 ——
 * 前端据此断线，而不是把它画成谷底。
 */
export interface MetricsPoint {
  /** 采样时刻（epoch 毫秒）。 */
  at: number;
  deltaReady: boolean;
  cpuPercent: number;
  memPercent: number;
  netRxRate: number;
  netTxRate: number;
  diskReadRate: number;
  diskWriteRate: number;
  accel: MetricsPointAccel[];
}

export interface Process {
  pid: number;
  ppid: number;
  user: string;
  state: string;
  cpu: number;
  mem: number;
  rssKiB: number;
  elapsed: string;
  command: string;
}

export interface ProcessList {
  processes: Process[];
  total: number;
}

/** 接口上的一个 IP 地址。 */
export interface NetAddress {
  family: 'ipv4' | 'ipv6';
  addr: string;
  prefix: number;
  scope: string;
  /** 来自 DHCP 或 IPv6 自动配置 —— 改静态地址前必须先弄清这一点。 */
  dynamic: boolean;
  /** 地址没有租期（静态配置）。为 false 时 remaining 才有意义。 */
  forever: boolean;
  remaining: string;
}

/** 一个网络接口的当前状态。 */
export interface NetInterface {
  name: string;
  /** ethernet / bridge / bond / vlan / wireless / veth / loopback ... */
  type: string;
  /**
   * 接口归属：physical / container / virtual / kernel。
   *
   * 与 type 是两个维度：docker0 和手建的 br0 类型都是 bridge，
   * 但归属不同 —— 前者是容器网络，后者是管理员建的物理网桥。
   */
  owner: 'physical' | 'container' | 'virtual' | 'kernel';
  mac: string;
  mtu: number;
  /** 管理态（ip link set up/down）。 */
  adminUp: boolean;
  /** 运行态：网线真的通。只插网线不插对端时 adminUp 为 true 而这里是 false。 */
  up: boolean;
  operState: string;
  /** 所属网桥/绑定。 */
  master: string;
  /** 接口本身是网桥时，它的成员列表。 */
  members: string[];
  /** 0 表示读不到速率（虚拟设备），不是零速率。 */
  speedMbps: number;
  addrs: NetAddress[];
  rxBytes: number;
  txBytes: number;
  rxRate: number;
  txRate: number;
  /** NetworkManager 连接名。非空表示该设备由 NM 托管。 */
  connName: string;
  connState: string;
}

export interface NetRoute {
  family: 'ipv4' | 'ipv6';
  dst: string;
  gateway: string;
  dev: string;
  prefsrc: string;
  protocol: string;
  metric: number;
}

/** systemd-resolved 里按链路配置的 DNS。 */
export interface NetLinkDNS {
  link: string;
  servers: string[];
}

export interface NetDNS {
  servers: string[];
  search: string[];
  byLink: NetLinkDNS[];
  /** resolv.conf 指向本机 stub resolver，此时真实上游 DNS 看 byLink。 */
  stub: boolean;
}

export interface NetListener {
  protocol: string;
  address: string;
  port: number;
  process: string;
  /**
   * 占用该端口的进程号，界面据它结束进程。为 0 表示读不到 ——
   * 这一栏来自以 root 重采的那份监听表，目标机提不了权的机器上它整份不存在，
   * 此时端口照旧显示，只是结束不了。
   */
  pid: number;
}

/** 目标机当前的 TCP / UDP 套接字数。 */
export interface NetSocketCounts {
  /** 活动连接：不含 LISTEN、TIME_WAIT 与 CLOSE。 */
  tcp: number;
  /** UDP 没有连接状态，按绑定中的套接字计。 */
  udp: number;
}

/** 整机报文丢弃的累计口径（自开机以来，排除回环）。 */
export interface NetPacketLoss {
  packets: number;
  drops: number;
  percent: number;
}

/** /etc/hosts 里的一条记录。 */
export interface NetHostEntry {
  ip: string;
  /** 主机名与别名，第一个是正名。 */
  names: string[];
  /** 行内 `#` 之后的内容（不含 `#`）；空串表示这一行没有注释。 */
  comment: string;
  /** 它在 /etc/hosts 原文里的行号（1-based，空行与注释行都计入）：改与删靠它定位。 */
  line: number;
}

/**
 * 一次 /etc/hosts 变更。
 *
 * 三种动作用同一份结构：add 用 ip/names/comment；update 与 remove 还要带上
 * line 与 oldNames 作锚点 —— 服务端据此确认"要动的确实是你看到的那一条"，
 * 行号在别人插删过行之后会指向别的记录。
 */
export interface HostsChangeRequest {
  action: 'add' | 'update' | 'remove';
  hostsMd5: string;
  line?: number;
  oldNames?: string[];
  ip?: string;
  names?: string[];
  comment?: string;
}

/** OVS 网桥上的一个端口。 */
export interface OvsPort {
  name: string;
  /** 端口背后是一块真实物理网卡（而不是网桥自己的内部口）。 */
  physical: boolean;
  /** 这台机器当前的默认路由正好从这个网口出去 —— 动它很可能切断面板自己的连接。 */
  carriesDefaultRoute: boolean;
}

/** 一个 OVS 网桥及其端口。 */
export interface OvsBridge {
  name: string;
  ports: OvsPort[];
  /** 默认路由的出口就是这个网桥本身（网口被桥接后名字不再出现在路由表里）。 */
  carriesDefaultRoute: boolean;
  /** 这座网桥是面板建的 —— 只有它才能从面板删掉。 */
  created: boolean;
}

/**
 * 目标机的 Open vSwitch 现状。
 *
 * available 为 false 表示这台机器上根本没有 ovs-vsctl，此时整个 OVS 相关
 * 界面都不展示 —— 这套功能按"有没有装 OVS"探测，而不是按发行版识别。
 */
export interface OvsState {
  available: boolean;
  /**
   * 找得到 ovs-vsctl，但读不到 OVS 的状态。
   *
   * 必须和"没有网桥"分开看：ovs-vsctl 读的是 OVS 自己的数据库，套接字默认
   * 只允许 root 与 openvswitch 组访问。把"读不到"当成"没有"，界面就只能拿
   * 后者去描述前者 —— 用户会对着自己明明建好的桥琢磨是哪里没打开。
   */
  unreadable: boolean;
  /**
   * 读不到的原因：ovs-vsctl 那句报错，或面板提权失败时 su/sudo 的那句话。
   * 空串表示这次没拿到原因。
   *
   * 它直接显示在那条警告里。同一句"读不到"背后的修法完全不同——套接字权限
   * 不足要去加 openvswitch 组，口令不对要去核对主机配置，服务没起要去看 systemd。
   */
  unreadableReason: string;
  bridges: OvsBridge[];
  /** 目标机上装了我们下发的开机自恢复服务。 */
  persistent: boolean;
}

/** 目标主机一次网络信息采集的结果。 */
export interface NetworkSnapshot {
  interfaces: NetInterface[];
  routes: NetRoute[];
  dns: NetDNS;
  listeners: NetListener[];
  connections: NetSocketCounts;
  packetLoss: NetPacketLoss;
  /** /etc/hosts 里的静态域名映射。 */
  hosts: NetHostEntry[];
  /**
   * hosts 原文的摘要，改 /etc/hosts 时原样带回去当乐观锁。
   *
   * 界面改一条记录不可能把整份文件发上来，服务端靠它判断"你看到的那一份"
   * 是否仍是当前文件：对不上就拒绝，而不是拿旧内容把别人的改动盖掉。
   */
  hostsMd5: string;
  /** 实际生效的配置后端：NetworkManager / netplan / systemd-networkd / ifupdown / ifcfg / unknown。 */
  manager: string;
  tools: string[];
  ovs?: OvsState;
  /** 首次采样没有基准值，此时速率不可用。 */
  deltaReady: boolean;
  sampledAt: string;
}

/**
 * 一次已下发、等待确认的网络变更。
 *
 * 生效会切断面板的 SSH，所以下发与确认是两次独立请求；expiresAt 之前
 * 没确认的话，目标机会把配置回滚到变更前：NetworkManager 类由 NM 自己的
 * 检查点负责，OVS 类由面板事前落在目标机上的恢复脚本加看门狗负责。
 */
export interface NetworkChange {
  id: string;
  hostId: number;
  action: string;
  summary: string;
  /** 真正让配置生效的命令，会断开现有连接。 */
  risky: string[];
  createdAt: string;
  expiresAt: string;
}

/** 变更请求。字段含义随 action 变化，与服务端的 NetChangeRequest 一一对应。 */
export interface NetChangeRequest {
  action:
    | 'address'
    | 'mtu'
    | 'link-state'
    | 'bridge-create'
    | 'bridge-delete'
    | 'bridge-attach'
    | 'bridge-detach'
    | 'ovs-attach'
    | 'ovs-detach'
    | 'ovs-bridge-create'
    | 'ovs-bridge-delete';
  interface?: string;
  connection?: string;
  bridge?: string;
  members?: string[];
  method?: 'manual' | 'auto';
  addresses?: string[];
  gateway?: string;
  dns?: string[];
  mtu?: number;
  up?: boolean;
}

/** 一个块设备：整盘、分区、逻辑卷或软 RAID 成员。 */
export interface BlockDevice {
  name: string;
  path: string;
  /** disk / part / lvm / raid / rom */
  type: string;
  size: number;
  /** 为空表示设备上还没有文件系统（尚未格式化）。 */
  fstype: string;
  label: string;
  uuid: string;
  mountPoints: string[];
  readOnly: boolean;
  /** 外接设备（U 盘、移动硬盘）。 */
  removable: boolean;
  model: string;
  serial: string;
  transport: string;
  rotational: boolean;
  /** 分区或逻辑卷。 */
  children: BlockDevice[];
  /** 承载着操作系统，不能被格式化或分区。 */
  system: boolean;
  /** 正在被挂载或用作 swap。 */
  busy: boolean;
  /** 所在磁盘的分区表类型：gpt / msdos。为空表示这块盘还没初始化过。 */
  partitionTable: string;
  /** 分区号；整盘与逻辑卷为 0。 */
  partNumber: number;
  /** 分区在磁盘上的起始偏移（字节）。 */
  start: number;
}

/** 一个已挂载的文件系统。 */
export interface StorageMount {
  target: string;
  source: string;
  fstype: string;
  options: string;
  total: number;
  used: number;
  avail: number;
  /** 为 -1 表示没有容量概念（网络存储、伪文件系统）。 */
  usePercent: number;
  uuid: string;
  label: string;
  virtual: boolean;
  /** /etc/fstab 里有对应条目，重启后仍会挂载。 */
  persistent: boolean;
  kind: 'disk' | 'network' | 'virtual';
}

export interface FstabEntry {
  source: string;
  target: string;
  fstype: string;
  options: string;
  raw: string;
}

/** 一个软 RAID 阵列（来自 /proc/mdstat）。 */
export interface RaidArray {
  name: string;
  /** raid0 / raid1 / raid5 / raid6 / raid10 / linear。 */
  level: string;
  /** active / inactive / auto-read-only。 */
  state: string;
  members: string[];
  /** 应有成员数 / 当前在线成员数。 */
  total: number;
  working: number;
  /** 阵列容量，单位与 /proc/mdstat 一致（1K 块）。 */
  blocks: number;
  healthy: boolean;
  /** 成员不全但仍在提供服务 —— 最容易被忽略的那种状态。 */
  degraded: boolean;
}

/** 目标主机一次存储信息采集的结果。 */
export interface StorageSnapshot {
  devices: BlockDevice[];
  mounts: StorageMount[];
  fstab: FstabEntry[];
  /** 存在的工具；缺 nfs / cifs 时网络存储挂不上，缺 mkfs.* / parted 时不能格式化或分区。 */
  tools: string[];
  /** 支持的文件系统类型 -> 对应的 mkfs 命令名。下拉框据此生成，缺工具的项由 tools 判定禁用。 */
  formatTargets: Record<string, string>;
  raid: RaidArray[];
  sampledAt: string;
}

/** 一条 SMART 属性。 */
export interface SmartAttribute {
  id: number;
  name: string;
  value: number;
  worst: number;
  /** 厂商给的失效阈值。 */
  threshold: number;
  raw: number;
  /** raw 的单位：扇区 / 次 / %。 */
  unit: string;
  /** 已经跌破阈值，该准备换盘了。 */
  failed: boolean;
  /** 与数据完整性直接相关，值得一直盯着。 */
  critical: boolean;
}

/** 一块磁盘的 SMART 健康报告。 */
export interface SmartReport {
  device: string;
  /** 为假时 note 说明原因（设备不支持、权限不足、没装 smartctl）。 */
  available: boolean;
  note: string;
  model: string;
  serial: string;
  passed: boolean;
  /** -1 表示设备没有报告这一项。 */
  temperature: number;
  powerOnHours: number;
  /** 只包含关键项与已经异常的项，不是全集。 */
  attributes: SmartAttribute[];
}

/** 目录用量的一个子项。 */
export interface DiskUsageEntry {
  path: string;
  name: string;
  bytes: number;
}

/** 一次目录用量统计的结果。 */
export interface DiskUsage {
  target: string;
  total: number;
  entries: DiskUsageEntry[];
  /** 还有更小的子项没列出来。 */
  truncated: boolean;
  /** du 报告的问题，多数是"有子目录读不到"。非空表示上面的数字只是能读到的部分。 */
  warning: string;
}

/** 存储操作请求，与服务端的 StorageRequest 一一对应。 */
export interface StorageRequest {
  action:
    | 'mount-local'
    | 'mount-nfs'
    | 'mount-cifs'
    | 'unmount'
    | 'format'
    | 'part-label'
    | 'part-create'
    | 'part-delete'
    | 'fstab-remove';
  device?: string;
  /** 挂载点目录。格式化与分区操作不以挂载点为目标，因此可留空。 */
  target?: string;
  fstype?: string;
  options?: string[];
  server?: string;
  share?: string;
  username?: string;
  password?: string;
  /** 写入 /etc/fstab，重启后自动挂载。 */
  persist?: boolean;
  /** 格式化时写入的卷标。 */
  label?: string;
  /** 破坏性操作的二次确认，必须与 device 完全一致。 */
  confirm?: string;
  /** 要创建的分区表类型：gpt / msdos。 */
  partTable?: string;
  /** 新建分区的起止位置，如 0% / 100% / 512MiB。 */
  partStart?: string;
  partEnd?: string;
  /** 要删除的分区号。 */
  partNumber?: number;
}

/**
 * 存储操作的结果。
 *
 * verified 为假表示命令跑完了但状态复核不通过（verifyNote 说明原因）——
 * 这不算失败，但也绝不能当成成功。
 */
export interface StorageResult {
  ok: boolean;
  summary: string;
  verified: boolean;
  verifyNote?: string;
  snapshot?: StorageSnapshot;
}

// ---------- 环境管理 ----------

export type EnvFamily = 'apt' | 'dnf' | 'yum' | 'apk';

export interface EnvProfile {
  family: EnvFamily;
  os: string;
  prettyName: string;
  version: string;
  codename: string;
  arch: string;
  pkgArch: string;
  root: boolean;
  /**
   * 可用 su 提权：目标机有 su，且主机上单独配了「提权口令」（root 口令）——
   * su 认的就是这一份，两者缺一不可。
   * 只是"这条路走得通"，不代表口令一定对 —— 口令不对会在执行首个变更步骤时暴露。
   */
  hasSu: boolean;
  /**
   * 这个账号在这台机器上能免密 sudo（由探针实测得出，与口令无关）。
   */
  hasSudoNoPass: boolean;
  /**
   * 可用 sudo 提权：目标机有 sudo，且执行通道拿得出登录凭据 ——
   * sudo 认的是执行者自己的口令，正好是登录口令。
   */
  hasSudo: boolean;
  kernel: string;
}

export interface EnvPackage {
  name: string;
  version: string;
  arch: string;
}

export interface EnvUpgrade {
  name: string;
  arch: string;
  current: string;
  candidate: string;
  repo: string;
}

export interface EnvCandidate {
  version: string;
  repo: string;
}

export interface EnvVersionOption {
  value: string;
  label: string;
}

export interface EnvCatalogItem {
  slug: string;
  name: string;
  /** 分类标识（runtime / toolchain / service / database / monitor / …），显示名见 envCatalog.ts。 */
  category: string;
  description: string;
  /** 适用场景说明；目前网络安全分类会给出，其余分类可能为空。 */
  useCase?: string;
  /** 非空表示安装后可以在面板里打开；为空表示只提供安装/重装。 */
  launch?: LaunchSpec;
  supported: boolean;
  installed: boolean;
  version: string;
  versions?: EnvVersionOption[];
  versionError?: string;
  note?: string;
}

/** 目录条目的启动方式：打开时在目标机终端执行一条命令。 */
export interface LaunchSpec {
  command: string;
  configPaths?: string[];
}

/** 已安装且可打开的应用：桌面图标与「打开」入口的数据。 */
export interface LaunchApp {
  slug: string;
  name: string;
  hostId: number;
  hostName: string;
  command: string;
  configPaths?: string[];
  /** 应用仓库安装的包带自己的图标与版本；内置安全应用为空。 */
  iconUrl?: string;
  version?: string;
  category?: string;
}

/** 安全应用原生 UI 的表单字段。 */
export interface SecurityField {
  key: string;
  label: string;
  type: 'text' | 'number' | 'select' | 'password' | 'textarea';
  placeholder?: string;
  default?: string;
  options?: string[];
  required?: boolean;
  help?: string;
}

export interface SecurityAction {
  id: string;
  label: string;
  description?: string;
  /** inspect：读取/诊断；manage：会改变系统状态。 */
  group?: 'inspect' | 'manage';
  fields?: SecurityField[];
  confirm?: string;
  danger?: boolean;
}

export interface SecurityConfigEntry {
  path: string;
  isDir: boolean;
}

/** 应用包 ui.json：通用窗口的分组与提示。 */
export interface AppUIView {
  schemaVersion: number;
  layout: string;
  overview?: string[];
  sections?: { title: string; actions: string[] }[];
  hint?: string;
}

/** 应用仓库索引里随包下发的界面结构预览。 */
export interface AppUIPreviewAction {
  id: string;
  label: string;
}

export interface AppUIPreviewSection {
  title: string;
  actions: AppUIPreviewAction[];
}

export interface AppUIPreview {
  layout: string;
  overview?: AppUIPreviewAction[];
  sections?: AppUIPreviewSection[];
  hint?: string;
}

/** 应用仓库索引里的一个文件及其摘要。 */
export interface StoreFileRef {
  path: string;
  sha256: string;
}

/** 应用仓库里的一个应用条目。 */
export interface StoreAppEntry {
  slug: string;
  name: string;
  version: string;
  category: string;
  description: string;
  icon: string;
  manifest: string;
  ui: string;
  files: StoreFileRef[];
  /** 界面结构预览：安装之前就能看到应用窗口的构成。 */
  uiPreview?: AppUIPreview;
  installed: boolean;
  installedVersion?: string;
  updateAvailable: boolean;
  /** 当前选中的主机桌面上是否已有入口。 */
  onHost: boolean;
}

export interface StoreCatalog {
  source: string;
  fetchedAt: string;
  /** 拉取失败时退回本地缓存，界面据此提示"离线可用"。 */
  cached: boolean;
  apps: StoreAppEntry[];
}

export interface SecurityAppView {
  slug: string;
  name: string;
  description?: string;
  defaultAction?: string;
  actions: SecurityAction[];
  config: SecurityConfigEntry[];
  /** 以下字段只对应用仓库安装的包有值。 */
  version?: string;
  category?: string;
  iconUrl?: string;
  ui?: AppUIView;
}

export interface SecurityRunResult {
  ok: boolean;
  exitCode: number;
  output: string;
  stdout?: string;
  stderr?: string;
  durationMs: number;
}

/** Fail2Ban 单个 Jail 的结构化状态。 */
export interface Fail2BanJail {
  name: string;
  currentlyFailed: number;
  totalFailed: number;
  currentlyBanned: number;
  totalBanned: number;
  bannedIps: string[];
  /** 只有这个 Jail 读取失败时才有值，其余 Jail 仍可正常展示。 */
  error?: string;
}

export interface Fail2BanOverview {
  available: boolean;
  running: boolean;
  error?: string;
  jails: Fail2BanJail[];
  checkedAt: string;
}

export interface GnuPgpSubkey {
  keyId: string;
  fingerprint: string;
  algorithm: string;
  bits: number;
  created: number;
  expires: number;
  capabilities: string[];
}

/** GnuPG 钥匙环里的一把密钥（--with-colons 解析结果）。 */
export interface GnuPgpKey {
  fingerprint: string;
  keyId: string;
  uid: string;
  name: string;
  email?: string;
  algorithm: string;
  bits: number;
  created: number;
  expires: number;
  /** GnuPG 的有效性标记：u=有效、r=吊销、e=过期、d=停用。 */
  validity: string;
  capabilities: string[];
  subkeys: GnuPgpSubkey[];
}

export interface GnuPgpKeysView {
  available: boolean;
  error?: string;
  keys: GnuPgpKey[];
  checkedAt: string;
}

export interface EnvOverview {
  profile: EnvProfile;
  catalog: EnvCatalogItem[];
  /**
   * 后端选定的提权方式（root → su → 免密 sudo → 登录口令 sudo → none）。
   * 优先级是后端的行为契约，界面不再按 profile 里的几个布尔自己拼。
   */
  privilegeMode: PrivilegeMode;
}

export interface EnvChangeItem {
  name: string;
  arch?: string;
  from?: string;
  to?: string;
}

export interface EnvSimReport {
  ok: boolean;
  install: EnvChangeItem[];
  upgrade: EnvChangeItem[];
  downgrade: EnvChangeItem[];
  remove: EnvChangeItem[];
  conflicts: string[];
  downloadKB: number;
  note?: string;
}

/** 环境操作请求；不同动作只使用其中部分字段。 */
export interface EnvRunRequest {
  action: 'install' | 'upgrade' | 'remove' | 'upgrade-all' | 'mirror' | 'rollback';
  specs?: { name: string; version?: string }[];
  recipeSlug?: string;
  recipeVersion?: string;
  mirror?: string;
  originOpId?: number;
}

export interface EnvPlan {
  action: string;
  kind: 'packages' | 'recipe' | 'mirror' | 'rollback' | 'upgrade-all';
  target: string;
  recipeSlug?: string;
  recipeVersion?: string;
  steps: string[];
  sim?: EnvSimReport;
  preview?: string;
  note?: string;
}

export interface EnvSourceFile {
  path: string;
  content: string;
}

export interface EnvOpStep {
  seq: number;
  name: string;
  status: 'running' | 'success' | 'failed' | 'skipped';
  exitCode: number;
  stdout?: string;
  stderr?: string;
  durationMs: number;
}

export interface EnvOp {
  id: number;
  hostId: number;
  hostName: string;
  action: string;
  target: string;
  plan: EnvPlan | null;
  status: 'running' | 'success' | 'failed' | 'interrupted';
  compensated: boolean;
  verified: boolean;
  verifyNote: string;
  originOpId: number;
  summary: string;
  createdAt: string;
  finishedAt: string;
  steps?: EnvOpStep[];
}

/** 一个候选配置文件。read 为假表示文件存在但内容没取回来（体积超限或本轮配额用尽）。 */
export interface EnvVarFile {
  path: string;
  scope: 'system' | 'user';
  exists: boolean;
  read: boolean;
  reason?: string;
  /** 这个文件里定义了多少个变量。 */
  defines: number;
}

/**
 * 一个环境变量。
 *
 * value 是登录 shell 里实际生效的值；effective 为假表示这个变量只写在配置文件里、
 * 当前会话中并不存在（条件分支没走到或被覆盖），此时 value 是文件里写的字面值。
 * scope 为 session 表示它是会话建立时注入的（SSH_CONNECTION、PWD 这类），没有配置文件来源。
 */
export interface EnvVar {
  name: string;
  value: string;
  effective: boolean;
  scope: 'system' | 'user' | 'session';
  file?: string;
  line?: number;
  raw?: string;
  note?: string;
}

/** 一次环境变量采集的结果。 */
export interface EnvVarsView {
  shell: string;
  home: string;
  files: EnvVarFile[];
  vars: EnvVar[];
  /** 有文件没读到内容，来源推断可能不完整。 */
  truncated: boolean;
  /** 用户级变量可选的落地文件（家目录下的相对名）。 */
  userFiles: string[];
  /** 系统级变量的落地文件。 */
  systemFile: string;
}

/** 环境变量变更请求。 */
export interface EnvVarRequest {
  name: string;
  value?: string;
  scope: 'user' | 'system';
  /** 仅用户级有意义：留空则落到后端给的默认文件。 */
  file?: string;
}

/** 环境变量变更结论。 */
export interface EnvVarResult {
  opId: number;
  status: 'success' | 'failed';
  /** 实际改动的文件。界面要如实显示它。 */
  file: string;
  /** 复核通过：新起的登录会话里该变量的状态与预期一致。 */
  verified: boolean;
  sessionValue?: string;
  summary: string;
  note?: string;
}

/** 扫描到的一个可执行文件。 */
export interface ScanCommand {
  name: string;
  /** 它在当前登录 PATH 下解析到的绝对路径；空串表示现在根本用不到它。 */
  resolved?: string;
}

/** 扫描到的一个环境目录。 */
export interface ScanDir {
  path: string;
  /** 来源标识与展示名，由后端给（frontend 不翻译）。 */
  source: string;
  label: string;
  commands: ScanCommand[];
  /** 目录里可执行文件的实际总数（commands 可能被上限截断）。 */
  total: number;
  /** 目录存在但没有读权限，列不出内容 —— 与"目录不存在"不是一回事。 */
  denied?: boolean;
  /** 这个目录已经在登录 shell 的 PATH 里了。 */
  inPath: boolean;
  /** 把它加进 PATH 会被遮住的现有命令名（当前解析到别处）。 */
  shadows?: string[];
}

/** 一次环境扫描的结果。 */
export interface ScanView {
  home: string;
  /** 当前登录 shell 的 PATH 目录列表。 */
  pathDirs: string[];
  dirs: ScanDir[];
}

/** 把目录加入 PATH 的请求。 */
export interface EnvPathAddRequest {
  dir: string;
  scope: 'user' | 'system';
  /** 仅用户级有意义：留空则落到后端给的默认文件。 */
  file?: string;
}

/** 环境操作 WebSocket 回传消息。 */
export interface EnvWSMsg {
  type: 'begin' | 'hostStart' | 'stepStart' | 'stepLog' | 'stepEnd' | 'hostEnd' | 'done' | 'error';
  total?: number;
  hostId?: number;
  hostName?: string;
  seq?: number;
  name?: string;
  stream?: 'stdout' | 'stderr';
  data?: string;
  status?: string;
  exitCode?: number;
  durationMs?: number;
  opId?: number;
  compensated?: boolean;
  verified?: boolean;
  verifyNote?: string;
  summary?: string;
  error?: string;
}

/** 批量执行过程中服务端回传的消息。 */
export interface BatchMessage {
  type: 'begin' | 'hostStart' | 'output' | 'hostEnd' | 'done' | 'error';
  hostId?: number;
  hostName?: string;
  stream?: 'stdout' | 'stderr';
  data?: string;
  exitCode?: number;
  error?: string;
  note?: string;
  durationMs?: number;
  message?: string;
  total?: number;
}

/**
 * 一次任务的历史记录：批量执行，或单台主机上的一次文件操作。
 *
 * 三个计数由服务端聚合返回，列表页不必为每行再去拉一次结果表；
 * 命令输出正文刻意不落库，历史记录只保留结果性信息。
 */
export interface Task {
  id: number;
  /** 任务种类：批量执行，或文件操作（含跨主机传输）。 */
  kind: 'batch' | 'hash' | 'compress' | 'extract' | 'upload' | 'download' | 'transfer';
  command: string;
  timeoutSec: number;
  status: 'running' | 'done' | 'interrupted';
  total: number;
  successCount: number;
  failedCount: number;
  errorCount: number;
  /** 文件类任务的目标主机名；批量执行为空。 */
  hostName: string;
  /** 哈希任务的算法名（sha256 等）；其余种类为空。 */
  algo: string;
  createdAt: string;
  /** 未结束时服务端返回零值时间；判断是否结束请看 status，不要用这个字段。 */
  finishedAt: string;
}

/** 单台主机在某次任务中的结果。不含命令输出。 */
export interface TaskResult {
  hostId: number;
  hostName: string;
  status: 'success' | 'failed' | 'error';
  exitCode: number;
  error: string;
  note: string;
  durationMs: number;
}

/** 文件类任务的一条明细：一个被处理的文件。 */
export interface TaskItem {
  path: string;
  /** 字节数；-1 表示这条路拿不到大小（目标机命令算哈希时不输出大小）。 */
  size: number;
  hash: string;
  error: string;
}

/** 任务详情：元数据加全部主机结果与文件明细。 */
export interface TaskDetail {
  task: Task;
  results: TaskResult[];
  /** 文件类任务的明细；批量执行恒为空数组。 */
  items: TaskItem[];
}

/**
 * 一条通知。
 *
 * category 决定前端如何呈现与点击行为，level 只决定配色：
 * 同为 host 来源，"恢复上线"与"离线"是两个相反的 level。
 */
export interface Notification {
  id: number;
  category: 'task' | 'host' | 'security';
  level: 'info' | 'success' | 'warning' | 'error';
  title: string;
  body: string;
  /** 关联对象 ID（目前只有任务通知用它指向任务），无关联时为 0。 */
  ref: number;
  read: boolean;
  createdAt: string;
}

/** 通知列表与未读数。 */
export interface NotificationList {
  items: Notification[];
  unread: number;
}

// ---------- 操作快照 ----------

/**
 * 跨主机操作列表里的一条：环境操作本体，加上它的快照落盘情况。
 *
 * 快照本身不在任何数据表里 —— 它只是环境变更落在数据目录下的一组文件。
 * 因此这几个字段是服务端现算的：列表页要能一眼看出"这次变更有没有留下快照"。
 */
export interface SnapshotOp extends EnvOp {
  hasSnapshot: boolean;
  /**
   * 快照采集过、后来被保留策略清理了。
   *
   * 与 hasSnapshot 都为假、且这是采集阶段就失败的操作不是一回事：
   * 前者要在界面上说清楚，否则用户会以为面板弄丢了数据。
   */
  pruned: boolean;
  snapshotBytes: number;
  packageCount: number;
}

/** 同名包的版本变化。 */
export interface SnapshotPackageChange {
  name: string;
  from: string;
  to: string;
}

/** 与同主机上一条快照相比的包差异。 */
export interface SnapshotDiff {
  /** 作为对比基准的那次操作。 */
  baseOpId: number;
  baseAt: string;
  added: EnvPackage[];
  removed: EnvPackage[];
  changed: SnapshotPackageChange[];
}

/** 一条操作的快照内容。 */
export interface SnapshotDetail {
  op: EnvOp;
  available: boolean;
  /** 快照读不到时的原因（已被保留策略清理）。 */
  reason?: string;
  /** 附注：解析降级、读取失败等情况的说明。 */
  note?: string;
  /** 包清单是否按目标机的包管理器解析成功；为假时看 raw。 */
  parsed: boolean;
  family?: EnvFamily;
  packages: EnvPackage[];
  /** 快照里的包总数（packages 可能被上限截断）。 */
  packageTotal: number;
  truncated: boolean;
  /** 解析不了时给出的包清单原文。 */
  raw?: string;
  /** 同时留存下来的安装源配置快照体积。 */
  sourcesBytes: number;
  diff?: SnapshotDiff;
}

// ---------- 系统日志 ----------

/** 日志来源。三者的取法完全不同。 */
export type LogSource = 'journal' | 'file' | 'dmesg';

export interface LogFileInfo {
  path: string;
  bytes: number;
  /** 最后写入时刻（epoch 秒）。 */
  mtime: number;
}

/** 这台机器上有什么日志可看。 */
export interface LogSources {
  journal: boolean;
  dmesg: boolean;
  /** 按最后写入时间倒序：排障要看的多半是当前正在写的那个。 */
  files: LogFileInfo[];
}

/** 一次日志读取的结果。 */
export interface LogView {
  source: string;
  file?: string;
  unit?: string;
  priority?: string;
  lines: string[];
  /** 没读到内容时的原因（权限不足等）。 */
  note?: string;
  /** 输出被体积上限截断，拿到的是开头一段。 */
  truncated: boolean;
}

/** 面板自身落盘日志的尾部。 */
export interface PanelLogView {
  path: string;
  bytes: number;
  lines: string[];
  note?: string;
}

// ---------- Docker ----------

/**
 * 一台主机上 Docker 与 Compose 的可用性。
 *
 * available 只表示 docker 命令存在；error 非空表示守护进程现在用不了
 * （没装、没权限、socket 没起），此时列表为空不是"没有容器"，界面要分别说清楚。
 */
export interface DockerProbe {
  available: boolean;
  version: string;
  /** 空串表示目标机上没有 compose（v2 插件与 v1 都探过）。 */
  composeVersion: string;
  error: string;
}

export interface DockerContainer {
  id: string;
  name: string;
  image: string;
  command: string;
  createdAt: string;
  runningFor: string;
  /** running / exited / paused / created / restarting / dead。 */
  state: string;
  status: string;
  ports: string;
  mounts: string;
  networks: string;
  /** 属于哪个 compose 项目；空串表示不是 compose 起的。 */
  project: string;
  workingDir: string;
  configFiles: string[];
}

export interface DockerImage {
  id: string;
  repo: string;
  tag: string;
  /** 可直接用于 docker pull/rmi 的引用（悬空镜像退化为镜像 ID）。 */
  ref: string;
  digest: string;
  size: string;
  created: string;
  containers: string;
  dangling: boolean;
}

export interface DockerNetwork {
  id: string;
  name: string;
  driver: string;
  scope: string;
  ipv6: boolean;
  internal: boolean;
  createdAt: string;
}

/** compose 项目由容器标签聚合而来，不依赖 compose CLI 是否存在。 */
export interface DockerProject {
  name: string;
  configFiles: string[];
  workingDir: string;
  total: number;
  running: number;
  containers: string[];
}

/** 窗口一次轮询拿到的全部状态。errors 按块给出局部失败原因。 */
export interface DockerSummary {
  probe: DockerProbe;
  containers: DockerContainer[];
  images: DockerImage[];
  networks: DockerNetwork[];
  projects: DockerProject[];
  errors?: Record<string, string>;
}

export interface DockerNetworkAttachment {
  containerId: string;
  name: string;
  ipv4: string;
  mac: string;
}

export interface DockerNetworkDetail {
  id: string;
  name: string;
  driver: string;
  scope: string;
  internal: boolean;
  ipv6: boolean;
  subnets: { subnet: string; gateway: string }[];
  containers: DockerNetworkAttachment[];
}

export interface DockerSearchHit {
  name: string;
  description: string;
  stars: number;
  official: boolean;
  automated: boolean;
}

/**
 * 目标机 /etc/docker/daemon.json 里与镜像拉取相关的配置。
 *
 * 这不是面板自己的设置：docker pull 由目标机的 dockerd 执行，
 * 面板进程里的代理环境变量传不过去，只能改这里再重启 Docker。
 */
export interface DockerDaemonConfig {
  /** registry-mirrors，按顺序尝试。 */
  mirrors: string[];
  /** 同时用于 HTTP 与 HTTPS 的代理地址；空串表示没有配置。 */
  proxy: string;
  noProxy: string;
  /** 目标机上是否已有 daemon.json。 */
  hasFile: boolean;
  path: string;
}

export interface DockerDaemonConfigUpdate {
  mirrors: string[];
  proxy: string;
  noProxy: string;
}

/** 新建容器的表单，与服务端 docker.RunSpec 一一对应。 */
export interface DockerRunSpec {
  image: string;
  name?: string;
  command?: string;
  ports?: string[];
  env?: string[];
  volumes?: string[];
  restart?: string;
  network?: string;
}

/** 容器内目录里的一个条目。 */
export interface DockerContainerFile {
  name: string;
  path: string;
  dir: boolean;
  link: boolean;
  size: number;
}

/** 一次容器内目录列表的结果。 */
export interface DockerContainerFiles {
  path: string;
  /** 上级目录；已在根目录时为空串。 */
  parent: string;
  entries: DockerContainerFile[];
  /** 条目超过上限，只返回了前一部分。 */
  truncated: boolean;
}

/** 容器内文本文件的读取结果。 */
export interface DockerContainerText {
  path: string;
  /** binary 为真时这里为空串。 */
  content: string;
  truncated: boolean;
  binary: boolean;
}

export type DockerContainerAction = 'start' | 'stop' | 'restart' | 'pause' | 'unpause' | 'remove';
export type DockerComposeAction = 'up' | 'down' | 'stop' | 'start' | 'restart' | 'pull' | 'logs';

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/** 状态变更请求携带的 CSRF 防护头，需与服务端 csrfHeader 保持一致。 */
const CSRF_HEADER = 'X-PanelSSH-Request';

/** 解析响应体：JSON 优先，解析不了就按纯文本。 */
function parseBody(text: string): unknown {
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** 从响应体里取出可读的失败原因。fallback 用于这类请求有自己的动词时。 */
function errorText(data: unknown, status: number, fallback?: string): string {
  if (data && typeof data === 'object' && 'error' in data) {
    return String((data as { error: unknown }).error);
  }
  return fallback ?? `请求失败（HTTP ${status}）`;
}

function parseResult<T>(status: number, text: string): T {
  const data = parseBody(text);
  if (status < 200 || status >= 300) {
    throw new ApiError(status, errorText(data, status));
  }
  return data as T;
}

async function toResult<T>(res: Response): Promise<T> {
  return parseResult<T>(res.status, await res.text());
}

/**
 * 从 XHR 读结果。
 *
 * 与 toResult 共用 parseResult，是为了让 401 / 422 这类状态在两处得到
 * 完全一样的处理 —— 各写一遍的话，迟早有一处会漏掉某个分支，
 * 而用户看到的就是两种截然不同的提示。
 */
async function xhrResult<T>(xhr: XMLHttpRequest): Promise<T> {
  return parseResult<T>(xhr.status, xhr.responseText ?? '');
}

/**
 * 传输进度回调。
 *
 * total 为 0 表示拿不到总长度（服务端没给 Content-Length，或浏览器未报告），
 * 此时只能显示"已传多少"，算不出百分比。
 */
export type ProgressFn = (loaded: number, total: number) => void;

/**
 * 压缩 / 解压过程中的一帧，与服务端的 archiveEvent 一一对应。
 *
 * 之所以有 prepare 与 escalating 两种"没有数字"的帧：统计条目数、以及权限不足时
 * 改用 root 重做，这两段都拿不到逐条输出，界面只能显示不确定进度 ——
 * 没有这两帧的话，用户会在那两段时间里看到进度条停着不动。
 */
export type ArchiveEvent =
  | { type: 'prepare' }
  | { type: 'progress'; total: number; done: number }
  | { type: 'escalating' }
  | { type: 'done'; path?: string }
  | { type: 'error'; message: string };

/** 支持的哈希算法，与服务端 hashAlgos 一一对应。 */
export type HashAlgo = 'sha256' | 'sha512' | 'sha1' | 'md5' | 'blake3';

/**
 * 跨主机搬运过程中的一帧，与服务端的 transferEvent 一一对应。
 *
 * mode 帧说明这一趟走的是哪条路：direct 是源机直连目标机（tar | ssh，拿不到
 * 字节进度，只有根条目数），relay 是面板中转（有字节进度与逐文件明细）。
 * 直连失败后服务端会自动清掉残留再改走中转，因此 mode 可能中途再发一次 relay。
 *
 * done 帧的 failed 是没能搬过去的文件数：整体不算失败，但界面要说出来。
 */
export type TransferEvent =
  | { type: 'prepare' }
  | { type: 'mode'; mode: 'direct' | 'relay'; message?: string }
  | { type: 'progress'; done?: number; total?: number; bytes?: number; bytesTotal?: number; path?: string }
  | { type: 'done'; done?: number; total?: number; failed?: number }
  | { type: 'error'; message: string };

/**
 * 哈希过程中的一帧，与服务端的 hashEvent 一一对应。
 *
 * result 是"这一个文件算完了"，逐条下发；size 缺省表示这条路拿不到大小
 * （走目标机命令时 sha256sum 不输出大小），界面显示"—"而不是一个假的 0。
 * 单行失败放在 result 的 error 里，整批失败才是 error 帧。
 *
 * item / bytes 是"这个文件读到哪儿了"：摘要工具要读完整个文件才给结果，几十 GB 的
 * 文件会让结果帧长时间不来。item 宣告开始处理哪个文件，bytes 报告已读字节数
 * （bytesTotal 是这个文件的总字节数，用它算百分比）。
 */
export type HashEvent =
  | { type: 'prepare' }
  | { type: 'item'; path: string; size?: number }
  | { type: 'bytes'; path?: string; bytes?: number; bytesTotal?: number }
  | { type: 'progress'; total?: number; done?: number }
  | { type: 'result'; path: string; size?: number; hash?: string; error?: string }
  | { type: 'escalating' }
  | { type: 'done'; total?: number; done?: number }
  | { type: 'error'; message: string };

/**
 * 文件扫描过程中的一帧，与服务端的 scanEvent 一一对应。
 *
 * entries 逐批下发而不是逐条：几万文件的目录逐条推会把界面淹掉。
 * sizes 与这一批的 paths 一一对应，-1 表示这条路拿不到大小（界面显示"—"）；
 * 它随批次一起下来，所以"表格里的第几行"要靠批次序数累加，不能直接用批内下标。
 */
export type ScanEvent =
  | { type: 'entries'; paths: string[]; sizes?: number[]; total?: number }
  | { type: 'done'; total?: number }
  | { type: 'error'; message: string };

/**
 * 发起一次会逐帧回传进度的请求。
 *
 * 不能用 request()：那个要等整个响应体读完才解析，而这条响应的意义就在于"边跑边到"。
 * 也不能用 XHR —— 它拿不到增量文本，onprogress 只报字节数，对"算到第几个文件"没有帮助。
 *
 * 契约：请求在流开始前被拒（鉴权、参数、目标已存在）时仍是非 2xx，响应体是普通 JSON 错误；
 * 一旦开始下发就恒为 200，真正的失败放在 error 帧里。
 *
 * signal 只在调用方需要"放弃这趟"时传（如关掉弹窗）：中断会让 fetch 抛 AbortError，
 * 调用方自行识别。不带 signal 的调用点行为不变。
 */
async function streamNDJSON<T>(
  path: string,
  body: unknown,
  onEvent: (event: T) => void,
  signal?: AbortSignal,
): Promise<void> {
  const res = await fetch(path, {
    method: 'POST',
    headers: { [CSRF_HEADER]: '1', 'content-type': 'application/json' },
    body: JSON.stringify(body),
    signal,
  });
  if (!res.ok) {
    throw new ApiError(res.status, errorText(parseBody(await res.text()), res.status));
  }
  if (!res.body) {
    throw new ApiError(res.status, '响应没有可读的流，无法获取进度');
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffered = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffered += decoder.decode(value, { stream: true });

    // NDJSON：一行一帧。末尾可能是一个还没收全的半帧，留在缓冲里等下一块。
    for (;;) {
      const index = buffered.indexOf('\n');
      if (index < 0) break;
      const line = buffered.slice(0, index).trim();
      buffered = buffered.slice(index + 1);
      if (!line) continue;
      dispatchFrame(line, onEvent);
    }
  }

  const rest = buffered.trim();
  if (rest) {
    dispatchFrame(rest, onEvent);
  }
}

/**
 * 把一帧交出去。
 *
 * 解析失败与处理失败必须区别对待：前者只是这一帧没收全或对不上协议，丢掉不影响后面的帧；
 * 后者说明调用方的处理逻辑出了错 —— 那种错误一旦被无声吞掉，用户看到的就是"服务端明明
 * 算完了，界面一片空白"，而且没有任何线索可查。所以这里把它记到控制台。
 */
function dispatchFrame<T>(line: string, onEvent: (event: T) => void): void {
  let event: T;
  try {
    event = JSON.parse(line) as T;
  } catch {
    return;
  }
  try {
    onEvent(event);
  } catch (err) {
    console.error('[streamNDJSON] 处理进度帧时出错', err, event);
  }
}

/** 压缩 / 解压。事件类型见 ArchiveEvent。 */
function streamArchive(
  path: string,
  body: unknown,
  onEvent: (event: ArchiveEvent) => void,
): Promise<void> {
  return streamNDJSON<ArchiveEvent>(path, body, onEvent);
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  // 状态变更请求必须携带该自定义头：跨站页面无法在不触发 CORS 预检的情况下
  // 添加自定义头，因此它构成一层 CSRF 防护，与会话 Cookie 的 SameSite 互补。
  if (method !== 'GET' && method !== 'HEAD') {
    headers[CSRF_HEADER] = '1';
  }

  const init: RequestInit = { method, headers };
  if (body !== undefined) {
    headers['content-type'] = 'application/json';
    init.body = JSON.stringify(body);
  }

  return toResult<T>(await fetch(path, init));
}

function query(params: Record<string, string>): string {
  return new URLSearchParams(params).toString();
}

export const api = {
  auth: {
    login: (username: string, password: string) =>
      request<SessionUser>('POST', '/api/auth/login', { username, password }),
    logout: () => request<{ ok: boolean }>('POST', '/api/auth/logout'),
    changePassword: (oldPassword: string, newPassword: string) =>
      request<{ ok: boolean }>('POST', '/api/auth/password', { oldPassword, newPassword }),
    me: () => request<SessionUser>('GET', '/api/auth/me'),
  },
  desktop: {
    getLayout: () => request<DesktopLayout>('GET', '/api/desktop/layout'),
    saveLayout: (layout: DesktopLayout) =>
      request<DesktopLayout>('PUT', '/api/desktop/layout', layout),
  },
  hosts: {
    list: () => request<Host[]>('GET', '/api/hosts'),
    /** 全部主机的在线状态。服务端按主机缓存探测结果，可以放心轮询。 */
    status: () => request<HostStatus[]>('GET', '/api/hosts/status'),
    /**
     * 全部主机的提权方式。
     *
     * 服务端会真的登录每一台机器（结果按主机缓存 5 分钟），所以别把它放进轮询里；
     * 打开列表用缓存值，用户点刷新时传 fresh 强制重探。
     */
    roles: (fresh = false) => request<HostRole[]>('GET', `/api/hosts/roles${fresh ? '?fresh=1' : ''}`),
    /**
     * 批量取主机指标（卡片视图用）。
     *
     * 按并发上限扇出采集，因此只把「在线」的主机 id 传进来 ——
     * 离线的主机连不上，采它们只会白白耗到超时。宿主机的保留 ID 0 也是
     * 合法目标：它不走网络，直接本地采集。
     * 不传 ids 表示全部主机（含宿主机）。
     */
    metrics: (ids?: number[]) =>
      request<HostMetricsBrief[]>(
        'GET',
        ids && ids.length > 0
          ? `/api/hosts/metrics?ids=${ids.join(',')}`
          : '/api/hosts/metrics',
      ),
    /**
     * 单台主机的提权方式。
     *
     * 服务端走系统画像缓存（5 分钟），所以窗口里选主机时顺手取一次即可，
     * 不必担心打开一个窗口就连两次目标机。
     */
    escalation: (id: number, fresh = false) =>
      request<EscalationInfo>('GET', `/api/hosts/${id}/escalation${fresh ? '?fresh=1' : ''}`),
    create: (payload: HostForm) => request<Host>('POST', '/api/hosts', payload),
    update: (id: number, payload: HostForm) => request<Host>('PUT', `/api/hosts/${id}`, payload),
    remove: (id: number) => request<{ ok: boolean }>('DELETE', `/api/hosts/${id}`),
    test: (id: number) => request<TestResult>('POST', `/api/hosts/${id}/test`),
    /**
     * 提权自检：实测面板能否在这台机器上取得 root。
     *
     * 与探测不同，它是真的提一次权（只跑 id -u），因此能回答"口令对不对"这个
     * 探测阶段答不了的问题 —— 环境管理写操作能不能用，看这个结论。
     */
    checkPrivilege: (id: number) => request<PrivilegeCheckResult>('POST', `/api/hosts/${id}/privilege-check`),
    /**
     * 用表单里尚未保存的配置试连一次。
     *
     * id 是编辑态下已保存主机的 ID：表单不会回填凭据，secret 为空即表示
     * "沿用原凭据"，后端需要这个 ID 才能把它取回来测。新增主机时传 null。
     */
    testDraft: (payload: {
      id: number | null;
      addr: string;
      port: number;
      username: string;
      authType: AuthType;
      secret: string;
    }) => request<TestResult>('POST', '/api/hosts/test', payload),
    exec: (id: number, command: string, timeoutSec = 0) =>
      request<ExecResult>('POST', `/api/hosts/${id}/exec`, { command, timeoutSec }),
  },
  /** 宿主机的提权账号：宿主机不在主机表里，配置入口单独给。 */
  localPrivilege: {
    get: () => request<LocalPrivilege>('GET', '/api/local/privilege'),
    save: (account: string, secret: string) =>
      request<LocalPrivilege>('PUT', '/api/local/privilege', { account, secret }),
    clear: () => request<LocalPrivilege>('DELETE', '/api/local/privilege'),
  },
  files: {
    list: (hostId: number, dir: string) =>
      request<FileListing>('GET', `/api/hosts/${hostId}/files?${query({ path: dir })}`),
    readText: (hostId: number, file: string) =>
      request<TextFile>('GET', `/api/hosts/${hostId}/file/text?${query({ path: file })}`),
    /**
     * 按块读取大文本。
     *
     * 第一次调用不传 encoding，由服务端识别并随响应返回；后续必须带上它，
     * 否则变长编码下没法对齐字符边界。limit 由服务端夹到 16 KiB–1 MiB。
     */
    textChunk: (
      hostId: number,
      file: string,
      offset: number,
      encoding?: string,
      limit?: number,
    ) =>
      request<TextChunk>(
        'GET',
        `/api/hosts/${hostId}/file/text/chunk?${query({
          path: file,
          offset: String(offset),
          ...(encoding ? { encoding } : {}),
          ...(limit ? { limit: String(limit) } : {}),
        })}`,
      ),
    writeText: (hostId: number, file: string, content: string, encoding = 'utf-8') =>
      request<{ ok: boolean; warning?: string }>('PUT', `/api/hosts/${hostId}/file/text`, {
        path: file,
        content,
        encoding,
      }),
    mkdir: (hostId: number, dir: string) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/files/mkdir`, { path: dir }),
    rename: (hostId: number, from: string, to: string) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/files/rename`, { from, to }),
    remove: (hostId: number, paths: string[]) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/files/delete`, { paths }),
    /** 改权限。mode 传八进制字符串（如 '755'），避免走数字被当成十进制。 */
    chmod: (hostId: number, paths: string[], mode: string) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/files/chmod`, { paths, mode }),
    copy: (hostId: number, paths: string[], target: string) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/files/copy`, { paths, target }),
    move: (hostId: number, paths: string[], target: string) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/files/move`, { paths, target }),
    /**
     * 跨主机复制 / 移动。源主机在路径上，目标主机在请求体里。
     *
     * 过程逐帧回传：直连时只有根条目数，中转时有字节数。服务端会自己决定
     * 走哪条路（直连探测不通过就用中转），调用方只需按帧更新界面。
     * signal 用来放弃这一趟 —— 断开连接会同时终止目标机上的命令。
     */
    transfer: (
      fromHostId: number,
      toHostId: number,
      paths: string[],
      target: string,
      move: boolean,
      onEvent: (event: TransferEvent) => void,
      signal?: AbortSignal,
    ) =>
      streamNDJSON<TransferEvent>(
        `/api/hosts/${fromHostId}/files/transfer`,
        { toHostId, paths, target, move },
        onEvent,
        signal,
      ),
    /**
     * 压缩。过程逐帧回传，调用方据此画进度条。
     *
     * 返回值只表示"这一趟结束了"，结论要看事件流里的 done / error 帧 ——
     * 详见 streamArchive 的契约说明。
     */
    compress: (
      hostId: number,
      path: string,
      name: string,
      format: 'tar.gz' | 'zip',
      onEvent: (event: ArchiveEvent) => void,
    ) => streamArchive(`/api/hosts/${hostId}/files/compress`, { path, name, format }, onEvent),
    /** 解压。同 compress，过程走事件流。 */
    extract: (hostId: number, path: string, onEvent: (event: ArchiveEvent) => void) =>
      streamArchive(`/api/hosts/${hostId}/files/extract`, { path }, onEvent),
    /** 单个条目的完整属性。 */
    attr: (hostId: number, file: string) =>
      request<FileAttr>('GET', `/api/hosts/${hostId}/file/attr?${query({ path: file })}`),
    /**
     * 改属主 / 属组。
     *
     * 两个字段都可选，但至少要给一个 —— 都不给是空操作，服务端会拒绝。
     * 只改属组时 owner 传空串，服务端会拼成 ":group"。
     */
    chown: (hostId: number, paths: string[], owner: string, group: string) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/files/chown`, { paths, owner, group }),
    /**
     * 改修改时间 / 访问时间。
     *
     * 传 epoch 秒而不是格式化字符串：面板与目标机可能不在同一时区，
     * 用字符串传"10:30"会让两边各自理解一次。0 表示这一项不改。
     */
    touch: (hostId: number, paths: string[], mtime: number, atime: number) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/files/touch`, { paths, mtime, atime }),
    /**
     * 计算哈希。目录会递归展开，结果逐帧回传，调用方据此边收边显示。
     *
     * 返回值只表示"这一趟结束了"，结论要看事件流里的 done / error 帧 ——
     * 详见 streamNDJSON 的契约说明。
     *
     * signal 用来中止这一趟（用户点「中止计算」）：断开连接会同时终止目标机上的命令，
     * 不会留下一个还在读盘的进程。已经收到的结果仍然有效。
     */
    hash: (
      hostId: number,
      paths: string[],
      algo: HashAlgo,
      onEvent: (event: HashEvent) => void,
      signal?: AbortSignal,
    ) => streamNDJSON<HashEvent>(`/api/hosts/${hostId}/files/hash`, { paths, algo }, onEvent, signal),
    /**
     * 列出选中的文件 / 目录里会被逐个处理的文件（目录递归展开）。
     *
     * 与 hash 分成两个请求：打开弹窗时先看清范围，用户确认算法后再动手算。
     * signal 用来放弃这趟（关掉弹窗时），避免目标机继续白扫一遍目录。
     */
    scan: (
      hostId: number,
      paths: string[],
      onEvent: (event: ScanEvent) => void,
      signal?: AbortSignal,
    ) => streamNDJSON<ScanEvent>(`/api/hosts/${hostId}/files/scan`, { paths }, onEvent, signal),
    /**
     * 图片 / 音视频 / PDF 的原位预览地址。
     *
     * 直接交给 <img> / <video> / <iframe>：它是同源 GET，浏览器会自动带上会话
     * Cookie；后端用 inline + Range 响应，播放器才能拖动进度条。
     * 不要在这个 URL 上再套 fetch —— 那会把媒体整体读进内存。
     */
    previewUrl: (hostId: number, file: string) =>
      `/api/hosts/${hostId}/file/preview?${query({ path: file })}`,
    /**
     * 下载文件，返回字节流。
     *
     * 不把 URL 交给浏览器原生下载：那样页面既不知道已经收到多少、也不知道总量，
     * 任何进度都无从谈起。代价是文件会先在内存里过一遍 —— 单文件几百 MB 还能
     * 接受，再大就不该走这里了。
     */
    download: (hostId: number, file: string, onProgress?: ProgressFn): Promise<Blob> =>
      new Promise<Blob>((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('GET', `/api/hosts/${hostId}/file/download?${query({ path: file })}`);
        xhr.responseType = 'blob';

        if (onProgress) {
          xhr.onprogress = (event) => onProgress(event.loaded, event.total);
        }
        xhr.onload = () => {
          if (xhr.status >= 200 && xhr.status < 300) {
            resolve(xhr.response as Blob);
            return;
          }
          const fallback = `下载失败（HTTP ${xhr.status}）`;
          // responseType 是 blob，失败时的 JSON 错误体也变成了 Blob ——
          // 要读回文本才拿得到后端真正的原因，否则用户只看到一句状态码。
          void (xhr.response as Blob)
            .text()
            .then((text) =>
              reject(new ApiError(xhr.status, errorText(parseBody(text), xhr.status, fallback))),
            )
            .catch(() => reject(new ApiError(xhr.status, fallback)));
        };
        xhr.onerror = () => reject(new ApiError(0, '网络错误，下载中断'));
        xhr.send();
      }),
    /**
     * multipart 上传。
     *
     * 用 XMLHttpRequest 而不是 fetch：fetch 至今没有上传进度事件，要拿到
     * "已经发出多少字节"只能用 XHR，或者用 Request 的 duplex 流式 body
     * （浏览器支持面还很窄）。boundary 由浏览器补，不能手动设置 Content-Type。
     */
    upload: (
      hostId: number,
      dir: string,
      files: File[],
      onProgress?: ProgressFn,
    ): Promise<{ saved: string[] }> =>
      new Promise<{ saved: string[] }>((resolve, reject) => {
        const form = new FormData();
        for (const file of files) {
          form.append('file', file, file.name);
        }

        const xhr = new XMLHttpRequest();
        xhr.open('POST', `/api/hosts/${hostId}/files/upload?${query({ path: dir })}`);
        xhr.setRequestHeader(CSRF_HEADER, '1');

        if (onProgress) {
          // 这是"字节已发往服务器"的进度，不是"服务器已经写完"：大文件在最后
          // 一字节发出之后，进度条会停在 100% 等响应回来。
          xhr.upload.onprogress = (event) => onProgress(event.loaded, event.total);
        }
        xhr.onload = () => {
          xhrResult<{ saved: string[] }>(xhr).then(resolve, reject);
        };
        xhr.onerror = () => reject(new ApiError(0, '网络错误，上传中断'));
        xhr.send(form);
      }),
  },
  metrics: {
    /**
     * fresh 表示"用户主动点的立即采样"：跳过服务端 2 秒的采样缓存窗口。
     * 轮询走默认值即可，多开监控窗口时服务端会把请求合并成一次采集。
     */
    get: (hostId: number, fresh = false) =>
      request<Metrics>('GET', `/api/hosts/${hostId}/metrics${fresh ? '?fresh=1' : ''}`),
    /**
     * 最近若干个采样点，用于打开窗口时回填曲线。
     *
     * 纯内存数据、不触发目标机操作：服务端最多保留 60 个点（约 3 分钟），
     * 前端只取最近的若干个，因此"关了窗口几分钟再打开"仍是满图。
     */
    history: (hostId: number) =>
      request<{ points: MetricsPoint[] }>('GET', `/api/hosts/${hostId}/metrics/history`),
    processes: (hostId: number, fresh = false) =>
      request<ProcessList>('GET', `/api/hosts/${hostId}/processes${fresh ? '?fresh=1' : ''}`),
    kill: (hostId: number, pid: number, signal: 'TERM' | 'KILL') =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/processes/kill`, { pid, signal }),
  },
  network: {
    /**
     * 网络现状（只读）。
     *
     * fresh 与指标采集同义：用户点「刷新」时跳过服务端 3 秒的采样缓存。
     */
    snapshot: (hostId: number, fresh = false) =>
      request<NetworkSnapshot>('GET', `/api/hosts/${hostId}/network${fresh ? '?fresh=1' : ''}`),
    /**
     * 修改 /etc/hosts（增 / 改 / 删一条记录）。
     *
     * verified 为 false 表示改动已经写入、但复核没通过（内容与预期不一致），
     * 这时要如实告诉用户去看一眼，而不是当作成功。
     */
    hostsChange: (hostId: number, payload: HostsChangeRequest) =>
      request<{ ok: boolean; verified: boolean; verifyNote?: string }>(
        'POST',
        `/api/hosts/${hostId}/network/hosts`,
        payload,
      ),
    /** 当前未确认的变更，以及变更脚本在目标机上的执行日志。 */
    change: (hostId: number) =>
      request<{ change: NetworkChange | null; log?: string }>('GET', `/api/hosts/${hostId}/network/change`),
    /** 下发变更。返回后变更命令才会在目标机上延迟执行。 */
    apply: (hostId: number, payload: NetChangeRequest) =>
      request<{ change: NetworkChange; timeoutSec: number }>('POST', `/api/hosts/${hostId}/network/apply`, payload),
    /**
     * 确认变更，撤掉回滚保护。
     *
     * 当前状态与预期不符时服务端会返回 409（除非 force 为真）——
     * 那说明变更多半已被自动回滚，此时不应该把它当成成功。
     *
     * persistNote 非空表示变更已生效、只是开机自恢复没更新成功：那是警告，不是失败。
     */
    commit: (hostId: number, force = false) =>
      request<{ ok: boolean; verified: boolean; verifyNote: string; persistNote: string }>(
        'POST',
        `/api/hosts/${hostId}/network/commit`,
        { force },
      ),
    /** 立即回滚到变更前的配置。 */
    rollback: (hostId: number) =>
      request<{ ok: boolean; output: string }>('POST', `/api/hosts/${hostId}/network/rollback`),
    /**
     * 卸掉目标机上的 OVS 开机自恢复服务。
     *
     * 装它是在用户机器上增加一个 systemd 单元，所以必须有一个对称的卸载入口。
     */
    ovsUnpersist: (hostId: number) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/network/ovs/unpersist`),
  },
  storage: {
    /** 存储现状（只读）：块设备、挂载点、fstab。 */
    snapshot: (hostId: number, fresh = false) =>
      request<StorageSnapshot>('GET', `/api/hosts/${hostId}/storage${fresh ? '?fresh=1' : ''}`),
    /** 挂载、卸载，以及随之而来的 fstab 变更。 */
    apply: (hostId: number, payload: StorageRequest) =>
      request<StorageResult>('POST', `/api/hosts/${hostId}/storage/apply`, payload),
    /**
     * 一块磁盘的 SMART 健康。
     *
     * 按需调用而不是跟着轮询走：smartctl 会唤醒休眠中的盘，
     * 也有明显耗时，不该拿目标机的硬盘寿命换一个没人看的数字。
     */
    smart: (hostId: number, device: string) =>
      request<SmartReport>(
        'GET',
        `/api/hosts/${hostId}/storage/smart?device=${encodeURIComponent(device)}`,
      ),
    /**
     * 某个已挂载目录下各子目录的占用。
     *
     * du 是在真的遍历目录树，大目录可能要跑很久，所以同样按需触发。
     */
    usage: (hostId: number, target: string, depth = 1) =>
      request<DiskUsage>(
        'GET',
        `/api/hosts/${hostId}/storage/usage?target=${encodeURIComponent(target)}&depth=${depth}`,
      ),
  },
  docker: {
    /** 一次取齐探测、容器、镜像、网络与 compose 项目：轮询只跑一条 SSH 命令。 */
    summary: (hostId: number) =>
      request<DockerSummary>('GET', `/api/hosts/${hostId}/docker/summary`),
    /** 新建并启动容器。镜像不存在时 docker run 自己会先拉取，可能耗时几分钟。 */
    createContainer: (hostId: number, spec: DockerRunSpec) =>
      request<{ ok: boolean; output: string }>('POST', `/api/hosts/${hostId}/docker/containers`, spec),
    containerAction: (
      hostId: number,
      id: string,
      action: DockerContainerAction,
      force = false,
    ) =>
      request<{ ok: boolean; output: string }>('POST', `/api/hosts/${hostId}/docker/containers/action`, {
        id,
        action,
        force,
      }),
    containerLogs: (hostId: number, id: string, tail = 300) =>
      request<{ output: string; tail: number }>(
        'GET',
        `/api/hosts/${hostId}/docker/containers/logs?${query({ id, tail: String(tail) })}`,
      ),
    /** 列出容器内的目录（只读）。容器没有 sh 或没在运行时，服务端会给出原因。 */
    containerFiles: (hostId: number, container: string, path: string) =>
      request<DockerContainerFiles>(
        'GET',
        `/api/hosts/${hostId}/docker/containers/files?${query({ container, path })}`,
      ),
    /** 读取容器内的文本文件；二进制文件返回 binary=true。 */
    readContainerFile: (hostId: number, container: string, path: string) =>
      request<DockerContainerText>(
        'GET',
        `/api/hosts/${hostId}/docker/containers/file?${query({ container, path })}`,
      ),
    /** 拉取镜像。目标机上没有外网时，失败原因会由 docker 自己给出。 */
    pullImage: (hostId: number, reference: string) =>
      request<{ ok: boolean; output: string }>('POST', `/api/hosts/${hostId}/docker/images/pull`, {
        reference,
      }),
    removeImages: (hostId: number, references: string[], force = false) =>
      request<{ ok: boolean; output: string }>('POST', `/api/hosts/${hostId}/docker/images/remove`, {
        references,
        force,
      }),
    searchImages: (hostId: number, q: string) =>
      request<{ hits: DockerSearchHit[]; note: string }>(
        'GET',
        `/api/hosts/${hostId}/docker/images/search?${query({ q })}`,
      ),
    getConfig: (hostId: number) =>
      request<DockerDaemonConfig>('GET', `/api/hosts/${hostId}/docker/config`),
    setConfig: (hostId: number, payload: DockerDaemonConfigUpdate) =>
      request<{ ok: boolean; output: string }>(
        'PUT',
        `/api/hosts/${hostId}/docker/config`,
        payload,
      ),
    networkDetail: (hostId: number, name: string) =>
      request<DockerNetworkDetail>(
        'GET',
        `/api/hosts/${hostId}/docker/networks/detail?${query({ name })}`,
      ),
    createNetwork: (
      hostId: number,
      payload: { name: string; driver: string; subnet: string; gateway: string; internal: boolean },
    ) =>
      request<{ ok: boolean; id: string }>(
        'POST',
        `/api/hosts/${hostId}/docker/networks/create`,
        payload,
      ),
    removeNetwork: (hostId: number, name: string) =>
      request<{ ok: boolean; output: string }>(
        'POST',
        `/api/hosts/${hostId}/docker/networks/remove`,
        { name },
      ),
    connectNetwork: (hostId: number, name: string, container: string, disconnect = false) =>
      request<{ ok: boolean }>('POST', `/api/hosts/${hostId}/docker/networks/connect`, {
        name,
        container,
        disconnect,
      }),
    readCompose: (hostId: number, path: string) =>
      request<{ path: string; content: string }>(
        'GET',
        `/api/hosts/${hostId}/docker/compose/file?${query({ path })}`,
      ),
    /** 保存 compose 文件；deploy 为真时紧接着 compose up -d。 */
    saveCompose: (
      hostId: number,
      payload: { path: string; content: string; project: string; deploy: boolean },
    ) =>
      request<{ ok: boolean; path: string; output: string }>(
        'PUT',
        `/api/hosts/${hostId}/docker/compose/file`,
        payload,
      ),
    composeAction: (
      hostId: number,
      project: string,
      files: string[],
      action: DockerComposeAction,
    ) =>
      request<{ ok: boolean; output: string }>(
        'POST',
        `/api/hosts/${hostId}/docker/compose/action`,
        { project, files, action },
      ),
  },
  plugins: {
    list: () => request<PluginList>('GET', '/api/plugins'),
    /**
     * 安装插件到指定主机。granted 传入则按该列表授权；不传则默认不授予任何能力，
     * 由管理员随后在「授权」里逐项确认（默认拒绝）。
     */
    install: async (hostId: number, file: File, granted: string[] = []): Promise<PluginInfo> => {
      const query = granted.length > 0 ? `?capabilities=${encodeURIComponent(granted.join(','))}` : '';
      const res = await fetch(`/api/plugins/hosts/${hostId}/install${query}`, {
        method: 'POST',
        headers: { [CSRF_HEADER]: '1', 'content-type': 'application/zip' },
        body: file,
      });
      return toResult<PluginInfo>(res);
    },
    uninstall: (hostId: number, org: string, name: string) =>
      request<{ ok: boolean }>('DELETE', `/api/plugins/hosts/${hostId}/${org}/${name}`),
    /** 安装内置插件：只写一条记录，没有安装包可上传。 */
    installBuiltin: (hostId: number, org: string, name: string) =>
      request<PluginInfo>('POST', `/api/plugins/hosts/${hostId}/builtin/${org}/${name}/install`),
    setEnabled: (hostId: number, org: string, name: string, enabled: boolean) =>
      request<{ ok: boolean }>('POST', `/api/plugins/hosts/${hostId}/${org}/${name}/enabled`, { enabled }),
    setCapabilities: (hostId: number, org: string, name: string, granted: string[]) =>
      request<PluginInfo>('PUT', `/api/plugins/hosts/${hostId}/${org}/${name}/capabilities`, { granted }),
    /**
     * 调用插件后端进程的方法。
     *
     * 方法名与参数都是插件自己的命名空间，面板只负责转发。hostId 由面板核对：
     * 插件只能操作自己被安装到的那台主机。
     */
    rpc: (org: string, name: string, method: string, hostId: number, params: unknown) =>
      request<{ result: unknown }>(
        'POST',
        `/api/plugins/${org}/${name}/rpc/${encodeURIComponent(method)}`,
        { hostId, params },
      ),
  },
  /** 应用中心 · AI CLI：按主机探测与用户级一键安装。 */
  aiCLI: {
    list: (hostId: number) =>
      request<{ apps: AICLIApp[]; platform: AICLIPlatform }>(
        'GET',
        `/api/hosts/${hostId}/ai-cli`,
      ),
    install: (hostId: number, id: string) =>
      request<{ ok: boolean; output: string }>(
        'POST',
        `/api/hosts/${hostId}/ai-cli/install`,
        { id },
      ),
    uninstall: (hostId: number, id: string, elevated = false, path = '') =>
      request<{ ok: boolean; output: string }>(
        'POST',
        `/api/hosts/${hostId}/ai-cli/uninstall`,
        { id, elevated, path },
      ),
  },
  dsh: {
    /** 某台主机上的 DSH 工作台状态。 */
    status: (hostId: number) => request<DshStatus>('GET', `/api/hosts/${hostId}/dsh/status`),
    overview: (hostId: number) =>
      request<DshOverview>('GET', `/api/hosts/${hostId}/dsh/overview`),
    /** 原生地址：绕开面板反代，用于完成 dsh 的 DeepSeek 账号登录。 */
    native: (hostId: number) =>
      request<DshNativeAddress>('GET', `/api/hosts/${hostId}/dsh/native`),
    setCommand: (hostId: number, command: string) =>
      request<DshStatus>('PUT', `/api/hosts/${hostId}/dsh/command`, { command }),
    /** 在目标机上探测 dsh 的安装位置并保存为该主机的启动命令。 */
    detectCommand: (hostId: number) =>
      request<DshStatus>('POST', `/api/hosts/${hostId}/dsh/command/detect`),
    /**
     * 用户级安装：下载 Node 24 到 ~/.panelssh/node，把 dsh 装到
     * ~/.panelssh/dsh-runtime，全程不需要 root。
     */
    provision: (hostId: number) =>
      request<{ ok: boolean; message: string; command: string }>(
        'POST',
        `/api/hosts/${hostId}/dsh/provision`,
      ),
    start: (hostId: number) => request<DshStatus>('POST', `/api/hosts/${hostId}/dsh/start`),
    stop: (hostId: number) => request<DshStatus>('POST', `/api/hosts/${hostId}/dsh/stop`),
    restart: (hostId: number) => request<DshStatus>('POST', `/api/hosts/${hostId}/dsh/restart`),
    update: (hostId: number) =>
      request<{ ok: boolean; message: string; output: string }>(
        'POST',
        `/api/hosts/${hostId}/dsh/update`,
      ),
    reinstall: (hostId: number) =>
      request<{ ok: boolean; message: string; output: string }>(
        'POST',
        `/api/hosts/${hostId}/dsh/reinstall`,
      ),
    logs: (hostId: number, limit = 500) =>
      request<{ lines: string[] }>('GET', `/api/hosts/${hostId}/dsh/logs?limit=${limit}`),
    clearLogs: (hostId: number) =>
      request<{ ok: boolean }>('DELETE', `/api/hosts/${hostId}/dsh/logs`),
    workspaces: (hostId: number) =>
      request<{ workspaces: DshWorkspace[] }>(
        'GET',
        `/api/hosts/${hostId}/dsh/workspaces`,
      ),
    plugins: (hostId: number) =>
      request<{ plugins: DshPlugin[] }>('GET', `/api/hosts/${hostId}/dsh/plugins`),
    installPlugin: (hostId: number, spec: string) =>
      request<{ ok: boolean; message: string }>(
        'POST',
        `/api/hosts/${hostId}/dsh/plugins/install`,
        { spec },
      ),
    updatePlugin: (hostId: number, name: string) =>
      request<{ ok: boolean; message: string }>(
        'POST',
        `/api/hosts/${hostId}/dsh/plugins/update`,
        { name },
      ),
    removePlugin: (hostId: number, name: string) =>
      request<{ ok: boolean; message: string }>(
        'POST',
        `/api/hosts/${hostId}/dsh/plugins/remove`,
        { name },
      ),
    setPluginEnabled: (hostId: number, name: string, enabled: boolean) =>
      request<{ ok: boolean; message: string }>(
        'POST',
        `/api/hosts/${hostId}/dsh/plugins/enabled`,
        { name, enabled },
      ),
  },
  browser: {
    /** 这台主机当前的浏览器代理状态。 */
    socksStatus: (hostId: number) =>
      request<BrowserProxyStatus>('GET', `/api/hosts/${hostId}/browser/socks`),
    /**
     * 启动浏览器代理，返回分配到的本机端口。
     *
     * 宿主机会被服务端明确挡下（400 + 原因）：它就在面板本机，浏览器与它
     * 同一个网络，经它出网不改变任何东西。
     */
    startSocks: (hostId: number) =>
      request<BrowserProxyStatus>('POST', `/api/hosts/${hostId}/browser/socks/start`),
    stopSocks: (hostId: number) =>
      request<BrowserProxyStatus>('POST', `/api/hosts/${hostId}/browser/socks/stop`),
    /**
     * 领一枚镜像凭据，用于在窗口里的 iframe 中打开网页。
     *
     * 每次开窗或换主机都要先来这里领：镜像路径不认会话 Cookie（见
     * BrowserMirrorToken 的说明），令牌是与主机绑定的。
     */
    mirrorToken: (hostId: number) =>
      request<BrowserMirrorToken>('POST', `/api/hosts/${hostId}/browser/mirror`),
  },
  ai: {
    getConfig: () => request<AIConfig>('GET', '/api/ai/config'),
    setConfig: (payload: { baseUrl: string; model: string; apiKey: string; maxSteps: number }) =>
      request<AIConfig>('PUT', '/api/ai/config', payload),
    test: () => request<AITestResult>('POST', '/api/ai/test'),
    /** 全部会话，按最近活动倒序。 */
    sessions: () => request<{ sessions: AISession[] }>('GET', '/api/ai/sessions'),
    /** 新建空会话。前端只在用户发出第一句话时才调用，避免留下空会话。 */
    createSession: () => request<AISession>('POST', '/api/ai/sessions'),
    /** 删除会话及其全部对话记录。 */
    deleteSession: (sessionId: number) =>
      request<{ ok: boolean }>('DELETE', `/api/ai/sessions/${sessionId}`),
    /** 某个会话已保存的对话记录，窗口打开或切换会话时用来重建消息流。 */
    history: (sessionId: number) =>
      request<AIChatHistory>('GET', `/api/ai/history?session=${sessionId}`),
    /**
     * 清空某个会话的对话记录。
     *
     * 清掉的不只是界面上的，也包括模型下次读到的上下文；其他会话不受影响。
     */
    clearHistory: (sessionId: number) =>
      request<{ ok: boolean }>('DELETE', `/api/ai/history?session=${sessionId}`),
  },
  tasks: {
    /** 最近的批量执行记录，按时间倒序。 */
    list: () => request<Task[]>('GET', '/api/tasks'),
    get: (id: number) => request<TaskDetail>('GET', `/api/tasks/${id}`),
  },
  /** 安全应用的原生 UI：元数据、白名单操作、配置文件读写。 */
  securityApps: {
    get: (hostId: number, slug: string) =>
      request<SecurityAppView>(
        'GET',
        `/api/hosts/${hostId}/security-apps/${encodeURIComponent(slug)}`,
      ),
    run: (hostId: number, slug: string, action: string, params: Record<string, string>) =>
      request<SecurityRunResult>(
        'POST',
        `/api/hosts/${hostId}/security-apps/${encodeURIComponent(slug)}/actions/${encodeURIComponent(action)}`,
        { params },
      ),
    /** Fail2Ban 的 Jail 与封禁名单（结构化，前端不解析命令输出）。 */
    fail2banOverview: (hostId: number) =>
      request<Fail2BanOverview>('GET', `/api/hosts/${hostId}/security-apps/fail2ban/overview`),
    /** GnuPG 钥匙环；kind=secret 读私钥列表。 */
    gnupgKeys: (hostId: number, kind: 'public' | 'secret') =>
      request<GnuPgpKeysView>(
        'GET',
        `/api/hosts/${hostId}/security-apps/gnupg/keys?${query({ kind })}`,
      ),
    readConfig: (hostId: number, slug: string, path: string) =>
      request<{ path: string; content: string; exists: boolean; backup: string }>(
        'GET',
        `/api/hosts/${hostId}/security-apps/${encodeURIComponent(slug)}/config?${query({ path })}`,
      ),
    saveConfig: (hostId: number, slug: string, path: string, content: string) =>
      request<{ ok: boolean; path: string; backup: string }>(
        'PUT',
        `/api/hosts/${hostId}/security-apps/${encodeURIComponent(slug)}/config`,
        { path, content },
      ),
    browse: (hostId: number, slug: string, path: string) =>
      request<{ path: string; output: string }>(
        'GET',
        `/api/hosts/${hostId}/security-apps/${encodeURIComponent(slug)}/browse?${query({ path })}`,
      ),
  },
  /** 应用仓库：从 GitHub（或本地镜像）拉取的应用包。 */
  store: {
    list: (hostId: number, refresh = false) =>
      request<StoreCatalog>(
        'GET',
        `/api/store/apps?hostId=${hostId}${refresh ? '&refresh=1' : ''}`,
      ),
    install: (slug: string, hostId: number) =>
      request<{ ok: boolean; slug: string; version: string; hostId: number }>(
        'POST',
        `/api/store/apps/${encodeURIComponent(slug)}/install`,
        { hostId },
      ),
    uninstall: (slug: string) =>
      request<{ ok: boolean; slug: string }>(
        'DELETE',
        `/api/store/apps/${encodeURIComponent(slug)}`,
      ),
    /** 图标走 <img src>，带会话 Cookie，因此这里只提供路径。 */
    iconUrl: (slug: string) => `/api/store/apps/${encodeURIComponent(slug)}/icon`,
    /** 仓库源：当前地址与内置默认值。 */
    source: () =>
      request<{ source: string; default: string }>('GET', '/api/store/source'),
    /** 切换仓库源；后端会先试拉一次索引，失败则保留原地址。 */
    saveSource: (source: string) =>
      request<{ ok: boolean; source: string; apps: number }>('PUT', '/api/store/source', {
        source,
      }),
  },
  env: {
    overview: (hostId: number) =>
      request<EnvOverview>('GET', `/api/hosts/${hostId}/env/overview`),
    /** 已安装且支持在面板里打开的应用（桌面图标数据源）。 */
    launchApps: () => request<{ apps: LaunchApp[] }>('GET', '/api/launch-apps'),
    packages: (hostId: number) =>
      request<{ packages: EnvPackage[]; family: EnvFamily; truncated: boolean }>(
        'GET',
        `/api/hosts/${hostId}/env/packages?filter=installed`,
      ),
    upgrades: (hostId: number) =>
      request<{ upgrades: EnvUpgrade[]; family: EnvFamily }>(
        'GET',
        `/api/hosts/${hostId}/env/packages?filter=upgrades`,
      ),
    candidates: (hostId: number, pkg: string) =>
      request<{ candidates: EnvCandidate[]; family: EnvFamily }>(
        'GET',
        `/api/hosts/${hostId}/env/candidates?pkg=${encodeURIComponent(pkg)}`,
      ),
    plan: (hostId: number, payload: EnvRunRequest) =>
      request<EnvPlan>('POST', `/api/hosts/${hostId}/env/plan`, payload),
    sources: (hostId: number) =>
      request<{ files: EnvSourceFile[]; family: EnvFamily; mirrors: { id: string; name: string }[] }>(
        'GET',
        `/api/hosts/${hostId}/env/sources`,
      ),
    ops: (hostId?: number) =>
      request<{ ops: EnvOp[] }>(
        'GET',
        hostId === undefined ? '/api/env/ops' : `/api/env/ops?hostId=${hostId}`,
      ),
    op: (id: number) => request<EnvOp>('GET', `/api/env/ops/${id}`),
    /** 环境变量：一次拉全（生效值 + 来源），采集要起目标机的登录 shell，所以要有加载态。 */
    vars: (hostId: number) => request<EnvVarsView>('GET', `/api/hosts/${hostId}/env/vars`),
    setVar: (hostId: number, payload: EnvVarRequest) =>
      request<EnvVarResult>('POST', `/api/hosts/${hostId}/env/vars/set`, payload),
    unsetVar: (hostId: number, payload: EnvVarRequest) =>
      request<EnvVarResult>('POST', `/api/hosts/${hostId}/env/vars/unset`, payload),
    /** 把某个变量文件还原成某次操作之前的内容（环境变量的回滚入口）。 */
    restoreVars: (hostId: number, originOpId: number) =>
      request<EnvVarResult>('POST', `/api/hosts/${hostId}/env/vars/restore`, { originOpId }),
    // 扫描只读且不缓存：用户点它的动机就是"我怀疑刚才装的东西没进 PATH"，
    // 给一份几分钟前的缓存正好答错这个问题。
    scanEnv: (hostId: number) => request<ScanView>('GET', `/api/hosts/${hostId}/env/scan`),
    addPath: (hostId: number, payload: EnvPathAddRequest) =>
      request<EnvVarResult>('POST', `/api/hosts/${hostId}/env/vars/path-add`, payload),
  },
  notifications: {
    /** 最近的通知与未读数，两者总是一起用，因此合并成一个请求。 */
    list: () => request<NotificationList>('GET', '/api/notifications'),
    markRead: (ids: number[]) =>
      request<{ unread: number }>('POST', '/api/notifications/read', { ids }),
    markAllRead: () => request<{ unread: number }>('POST', '/api/notifications/read-all'),
  },
  snapshots: {
    /** 全部主机的环境变更记录及其快照状态，按时间倒序。 */
    list: () => request<{ ops: SnapshotOp[] }>('GET', '/api/snapshots'),
    /** 一条操作的快照内容：包清单、与上一条快照的差异。 */
    get: (opId: number) => request<SnapshotDetail>('GET', `/api/snapshots/${opId}`),
  },
  logs: {
    /** 这台机器上有哪些日志可看（journalctl 是否可用、/var/log 下有什么）。 */
    sources: (hostId: number) => request<LogSources>('GET', `/api/hosts/${hostId}/logs/sources`),
    /**
     * 读一次目标机日志（尾部 N 行）。
     *
     * 参数只传非空的：file 仅对 source=file 有意义，unit/priority 仅对
     * journal 有意义，混着传会被服务端按来源忽略。
     */
    tail: (
      hostId: number,
      params: { source: LogSource; file?: string; unit?: string; priority?: string; lines?: number },
    ) => {
      const q: Record<string, string> = { source: params.source };
      if (params.file) q.file = params.file;
      if (params.unit) q.unit = params.unit;
      if (params.priority) q.priority = params.priority;
      if (params.lines) q.lines = String(params.lines);
      return request<LogView>('GET', `/api/hosts/${hostId}/logs?${query(q)}`);
    },
    /** 面板自身日志的尾部。纯本地文件读，不连任何目标机。 */
    panel: (lines?: number) =>
      request<PanelLogView>('GET', `/api/panel/logs${lines ? `?lines=${lines}` : ''}`),
  },
};

/**
 * AI 助手会话的 WebSocket 地址。
 *
 * 会话 ID 必须带上：服务端据此只加载那一个会话的历史，也只把结果写回它。
 * 不带 ID 的地址会被服务端直接拒绝 —— 那意味着一条连接不知道该读写哪段对话。
 */
export function aiChatURL(sessionId: number): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${window.location.host}/api/ws/ai?session=${sessionId}`;
}

/**
 * 插件入口地址。
 *
 * archive 插件从面板托管的静态资源里加载自身页面；embedded 插件的 entry
 * 存的就是面板内部地址（如 /dsh/），直接用它。
 */
export function pluginEntryURL(item: PluginInfo): string {
  if (item.source === 'embedded') return item.entry;
  return `/api/plugins/${item.org}/${item.name}/app/${item.entry || 'index.html'}`;
}

/** 插件图标地址。 */
export function pluginIconURL(item: PluginInfo): string {
  return `/api/plugins/${item.org}/${item.name}/app/${item.icon}`;
}

/**
 * 让浏览器保存一个 URL。
 *
 * 用隐藏链接触发而不是 window.open：后者会被弹窗拦截规则吃掉。
 *
 * filename 可选：走服务端 URL 时文件名由 Content-Disposition 决定，
 * 而 Blob URL 没有响应头，必须显式给名字，否则会存成一串随机字符。
 */
export function triggerDownload(url: string, filename?: string): void {
  const link = document.createElement('a');
  link.href = url;
  link.rel = 'noopener';
  if (filename) link.download = filename;
  document.body.appendChild(link);
  link.click();
  link.remove();
}

/**
 * 把一段文本写进本地剪贴板，返回是否成功。
 *
 * 主路径是 Clipboard API，但它要求安全上下文；面板常被用局域网 IP 的 http 打开，
 * 那时该 API 根本不存在，必须降级到隐藏 textarea + execCommand。
 *
 * 不在这里弹提示：只有调用方知道复制出去的是什么（一条路径、一份摘要清单）。
 */
export async function copyText(text: string): Promise<boolean> {
  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 用户拒绝授权等情况落到下面的兜底路径。
    }
  }

  const holder = document.createElement('textarea');
  holder.value = text;
  holder.setAttribute('readonly', '');
  holder.style.position = 'fixed';
  holder.style.top = '-9999px';
  holder.style.opacity = '0';
  document.body.appendChild(holder);
  holder.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch {
    ok = false;
  }
  holder.remove();
  return ok;
}

/** 构造终端 WebSocket 地址；协议跟随页面协议，同源以便携带会话 Cookie。 */
export function terminalURL(hostId: number): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${window.location.host}/api/ws/terminal?hostId=${hostId}`;
}

/** 批量执行的 WebSocket 地址。命令通过首条消息下发，不放在 URL 里。 */
export function batchURL(): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${window.location.host}/api/ws/batch`;
}

/** 环境变更执行的 WebSocket 地址。动作清单通过首条消息下发。 */
export function envRunURL(): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${window.location.host}/api/ws/env`;
}

/** 指标长驻流的 WebSocket 地址。主机与采样档位通过首条消息下发。 */
export function metricsStreamURL(): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${window.location.host}/api/ws/metrics`;
}

/**
 * 指标长驻流推来的一条事件。
 *
 * type 为 error 时只表示"这条流当前不通"：服务端会自己重连目标机，
 * 前端据此把窗口标成重连中即可，不该打断用户。
 */
export interface MetricsStreamEvent {
  type: 'sample' | 'error';
  hostId: number;
  metrics?: Metrics;
  error?: string;
}
