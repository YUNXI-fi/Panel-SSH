<script setup lang="ts">
import {
  computed,
  h,
  inject,
  nextTick,
  onBeforeUnmount,
  onMounted,
  ref,
  watch,
  type ComponentPublicInstance,
} from 'vue';
import { NDropdown, NPopover, useMessage, type DropdownOption } from 'naive-ui';

import SessionCommands from './SessionCommands.vue';
import SessionFiles from './SessionFiles.vue';
import TerminalSession from './TerminalSession.vue';
import { api, type Host, type HostStatus } from '../api/client';
import { formatLatency } from '../api/format';
import { WindowSubtitleKey } from '../desktop/useWindowManager';

/**
 * 终端窗口的容器：持有会话列表与整窗 UI。
 *
 * 布局取自 XShell / MobaXterm —— 顶部会话标签栏、工具条（会话标识 + 动作按钮）、
 * 左侧 SFTP 文件栏、底部连接参数状态栏；中间才是真正的终端。
 * 单条连接本身（xterm + WebSocket）在 TerminalSession 里，本组件不碰 PTY 数据。
 */
const props = defineProps<{
  hostId?: number;
  hostName?: string;
  /** 打开窗口时要进入的目录，由文件管理的「在终端中打开」带进来。 */
  cwd?: string;
  /** 连上并进入 cwd 后要执行的文件，由文件管理的「运行」带进来。 */
  runPath?: string;
}>();

type SessionStatus = 'connecting' | 'connected' | 'closed' | 'error';

const message = useMessage();

interface Session {
  id: number;
  hostId: number;
  /** 打开窗口时带进来的名字，仅在主机列表还没回来时兜底，不作为显示来源。 */
  hostName: string;
  /** 待发送的工作目录：连上之后发一条 cd 就清掉。 */
  pendingCwd?: string;
  /** 待执行的文件：与 pendingCwd 一起拼成一条命令，发出去就清掉。 */
  pendingRun?: string;
}

interface SessionState {
  status: SessionStatus;
  statusText: string;
  cols: number;
  rows: number;
}

/** 容器向会话要的能力：动作按钮与右键菜单都作用于「当前活动会话」。 */
interface SessionHandle {
  /** 重算网格并把行列数同步给远端：标签切回可见、窗口尺寸变化后都要调。 */
  refit: () => void;
  focus: () => void;
  copy: () => void;
  paste: () => void;
  writeInput: (text: string) => void;
  clear: () => void;
  reconnect: () => void;
  /** 取整段回滚缓冲的纯文本，供「导出会话」落成文件。 */
  exportText: () => string;
  selectAll: () => void;
  hasSelection: () => boolean;
}

const sessions = ref<Session[]>([]);
const activeId = ref<number | null>(null);
/** 每个会话的最新读数，按会话 id 归档 —— 切标签切的就是这份状态。 */
const states = ref<Record<number, SessionState>>({});

const active = computed(() => sessions.value.find((item) => item.id === activeId.value) ?? null);
const activeState = computed(() =>
  activeId.value === null ? null : (states.value[activeId.value] ?? null),
);

let seq = 0;
const handles = new Map<number, SessionHandle>();

function setHandle(id: number, el: Element | ComponentPublicInstance | null): void {
  if (el) handles.set(id, el as unknown as SessionHandle);
  else handles.delete(id);
}

function handleOf(id: number | null): SessionHandle | undefined {
  return id === null ? undefined : handles.get(id);
}

/**
 * 主机列表：工具条的 user@addr:port 用。
 *
 * 选择器内部虽然拉过一份，但只给出 id 与显示名，而这三项正是"这条连接是什么"
 * 的核心信息。拉取失败只让参数区留空，不影响终端本身。
 */
const hosts = ref<Host[]>([]);

onMounted(async () => {
  try {
    hosts.value = await api.hosts.list();
  } catch {
    // 静默：工具条参数区少一段文字，终端本身不受影响。
  }
});

function addrOf(hostId: number): string {
  const host = hosts.value.find((item) => item.id === hostId);
  return host ? `${host.username}@${host.addr}:${host.port}` : '';
}

/**
 * 标签与工具条上显示的主机名。
 *
 * 名字一律从主机列表实时查，不在会话里存一份：
 *   - 选择器把名字回填回来、与这里建会话之间存在时序差（同一个更新周期里，
 *     父组件的 watch 先于子组件回填执行），直接读回填值会先显示成「主机 #2」；
 *   - 主机在「主机管理」里改了名，已经开着的标签也应当跟着变。
 */
function nameOf(session: Session): string {
  const host = hosts.value.find((item) => item.id === session.hostId);
  return host?.name || session.hostName || `主机 #${session.hostId}`;
}

/**
 * 标签右侧的登录用户。
 *
 * 与主机名同源：同一台机器开了两条不同用户的连接时，只看主机名分不清是谁，
 * 而「这台机器用哪个账号进去的」正是两台同名标签之间唯一有意义的差别。
 * 本地终端没有 SSH 用户，返回空串由模板隐藏这一段。
 */
function userOf(session: Session): string {
  return hosts.value.find((item) => item.id === session.hostId)?.username ?? '';
}

const activeName = computed(() => (active.value === null ? '未选择主机' : nameOf(active.value)));
const activeAddr = computed(() => (active.value === null ? '' : addrOf(active.value.hostId)));

/**
 * 把当前会话的 user@addr:port 写进窗口标题栏。
 *
 * 标题（「终端 - DELL」）只说明是哪台机器，同一台机器开两条连接时看不出区别；
 * 竖栏任务项的悬浮标题读的也是这个后缀。切标签时跟着当前活动会话走。
 * 本地终端没有 SSH 地址，activeAddr 为空，标题保持原样。
 */
const setWindowSubtitle = inject(WindowSubtitleKey, null);

watch(activeAddr, (addr) => setWindowSubtitle?.(addr), { immediate: true });

// ---- 延时读数 ----

/**
 * 面板到目标主机的 TCP 探测延迟。
 *
 * 数据来自服务端的端口探测（internal/api/hoststatus.go），不是本会话的端到端往返：
 * 终端 WebSocket 只认 input / resize，没有可测量的 ping 帧 —— 想显示"这条连接的
 * 真实延迟"得先让后端支持 ping/pong，那是另一件事。它回答的是"这台机器现在有多远、
 * 通不通"，和状态灯配合看足够，但不能当成 ping 的替代品。
 *
 * 15 秒与「主机管理」的轮询同频：服务端按主机缓存探测结果，再密也只是重复问同一份缓存。
 */
const LATENCY_POLL_MS = 15000;
const latencies = ref<Record<number, number>>({});
let latencyTimer: number | undefined;

async function refreshLatency(): Promise<void> {
  try {
    const list = await api.hosts.status();
    const next: Record<number, number> = {};
    for (const item of list) {
      // 只在通的时候记：离线时这个字段没有意义，留着会显示出上一次的旧值。
      if (item.online) next[item.id] = item.latencyMs;
    }
    latencies.value = next;
  } catch {
    // 静默：少一段读数不影响终端使用。
  }
}

/** 当前会话主机的延时；探不到就不显示，而不是编一个数。 */
const activeLatency = computed(() => {
  if (active.value === null) return '';
  const ms = latencies.value[active.value.hostId];
  // 走统一的格式化：不足 1 毫秒要显示成 "<1 ms"，不能是 "0 ms"（见 formatLatency）。
  return ms === undefined ? '' : formatLatency(ms);
});

onMounted(() => {
  void refreshLatency();
  latencyTimer = window.setInterval(() => void refreshLatency(), LATENCY_POLL_MS);
});

onBeforeUnmount(() => {
  if (latencyTimer !== undefined) window.clearInterval(latencyTimer);
});

// ---- 会话生命周期 ----

function addSession(
  hostId: number,
  hostName = '',
  cwd?: string,
  runPath?: string,
): void {
  const id = ++seq;
  sessions.value.push({
    id,
    hostId,
    hostName,
    pendingCwd: cwd,
    pendingRun: runPath,
  });
  activeId.value = id;
  nextTick(() => {
    // 先重算网格再聚焦：新会话第一次显示时容器尺寸才是确定的。
    handleOf(id)?.refit();
    handleOf(id)?.focus();
  });
}

/** 按 shell 单引号规则转义路径，供 cd 使用。 */
function shellQuote(text: string): string {
  return `'${text.replaceAll("'", "'\\''")}'`;
}

function closeSession(id: number): void {
  const index = sessions.value.findIndex((item) => item.id === id);
  if (index < 0) return;

  sessions.value.splice(index, 1);
  delete states.value[id];
  handles.delete(id);

  if (activeId.value !== id) return;
  // 关掉当前标签就切到相邻的一个：优先右边，没有则左边。
  const next = sessions.value[index] ?? sessions.value[index - 1];
  activeId.value = next ? next.id : null;
  if (next) {
    nextTick(() => {
      handleOf(next.id)?.refit();
      handleOf(next.id)?.focus();
    });
  }
}

function activate(id: number): void {
  if (activeId.value === id) return;
  activeId.value = id;
  // 隐藏期间窗口可能被拖大拖小：切回来先按当前容器重算网格，
  // 再把新的行列数同步给远端 PTY，否则远端还停在旧宽度上折行。
  nextTick(() => {
    handleOf(id)?.refit();
    handleOf(id)?.focus();
  });
}

/**
 * 窗口级快捷键。
 *
 * 用 Alt 而不是 Ctrl+Shift：Ctrl+Shift+T / Ctrl+W / Ctrl+Tab 是浏览器保留键，
 * 页面根本收不到（会被浏览器抢先执行"重开标签页""关窗口""切标签"）。
 * Alt 组合能稳定送到页面。
 *
 * 监听在捕获阶段并停止冒泡：xterm 会把按键直接送进 PTY，不拦住的话
 * Alt+T 会被 bash 当成 transpose-word 执行两遍。
 */
function onKeydown(event: KeyboardEvent): void {
  // Esc 先关主机下拉：菜单开着时它不该被送进 PTY（远端可能把它当成中断）。
  // 这一步必须在下面的 altKey 判断之前 —— Esc 本身不带修饰键。
  if (event.key === 'Escape' && pickerOpen.value) {
    pickerOpen.value = false;
    event.preventDefault();
    event.stopPropagation();
    return;
  }

  // 带了 Ctrl/Cmd 的组合一律放行：Ctrl+C 必须继续作为 SIGINT。
  if (!event.altKey || event.ctrlKey || event.metaKey) return;

  const handled = (): void => {
    event.preventDefault();
    event.stopPropagation();
  };

  // 同时看 code 与 key：部分环境（远程桌面、自动化注入、非标准键盘布局）
  // 只给出 key 而 code 为空，只认其中一个会让快捷键在这类环境下静默失效。
  const key = event.key;

  if (event.code === 'KeyT' || key === 't' || key === 'T') {
    handled();
    void openPicker();
    return;
  }

  if (event.code === 'KeyW' || key === 'w' || key === 'W') {
    handled();
    if (activeId.value !== null) closeSession(activeId.value);
    return;
  }

  if (event.code === 'Equal' || event.code === 'NumpadAdd' || key === '=' || key === '+') {
    handled();
    stepFontSize(1);
    return;
  }

  if (event.code === 'Minus' || event.code === 'NumpadSubtract' || key === '-' || key === '_') {
    handled();
    stepFontSize(-1);
    return;
  }

  // Alt+1…9：直接切到第 N 个会话。
  const digit = /^(?:Digit)?([1-9])$/.exec(event.code) ?? /^([1-9])$/.exec(key);
  if (digit) {
    const target = sessions.value[Number(digit[1]) - 1];
    if (target) {
      handled();
      activate(target.id);
    }
  }
}

function onState(id: number, payload: SessionState): void {
  states.value[id] = payload;

  // 工作目录与运行命令只能等会话真的连上再发：PTY 还没建立时写进去会被丢掉。
  const session = sessions.value.find((item) => item.id === id);
  if (payload.status !== 'connected' || !session) return;

  // cd 与运行拼成一条命令：运行依赖工作目录，拆成两条发送就得自己保证先后。
  const parts: string[] = [];
  if (session.pendingCwd) parts.push(`cd ${shellQuote(session.pendingCwd)}`);
  if (session.pendingRun) parts.push(shellQuote(session.pendingRun));
  if (parts.length === 0) return;

  session.pendingCwd = undefined;
  session.pendingRun = undefined;
  handleOf(id)?.writeInput(`${parts.join(' && ')}\r`);
}

/** 工具条与右键菜单能触发的动作；带参数的能力（writeInput）不在此列。 */
type SessionAction = 'copy' | 'paste' | 'clear' | 'reconnect' | 'selectAll';

function callActive(action: SessionAction): void {
  handleOf(activeId.value)?.[action]();
}

/**
 * 连通灯。
 *
 * 只给会话条上的标签用：当前会话的状态由底部状态栏的文字承担，
 * 会话条上不再单独放一个灯（那会与标签自身的灯重复）。
 */
function ledClassOf(status?: SessionStatus): Record<string, boolean> {
  return {
    'led--ok': status === 'connected',
    'led--danger': status === 'error',
    'led--warn': status === 'closed',
    'led--sig': status === 'connecting',
    'led--live': status === 'connecting',
  };
}

/** 行列数读数；还没测出网格时不显示 0×0 这种不存在的尺寸。 */
const gridText = computed(() => {
  const state = activeState.value;
  return state && state.cols > 0 && state.rows > 0 ? `${state.cols}×${state.rows}` : '—';
});

// ---- 新建会话：默认本机，新建时从下拉列表选主机 ----

const pickerOpen = ref(false);
const hostList = ref<Host[]>([]);
const hostStatuses = ref<Map<number, HostStatus>>(new Map());

/**
 * 拉取可选主机与在线状态。
 *
 * 只在列表打开时取一次：新建会话是低频动作，没必要在终端窗口里常驻一套轮询 ——
 * 那会为了一个偶尔打开一次的列表，持续给目标机扇出探测。
 *
 * 失败不报错：列表里仍有「宿主机」这一项，用户依旧能新建会话。
 */
async function loadHosts(): Promise<void> {
  try {
    const [list, status] = await Promise.all([api.hosts.list(), api.hosts.status()]);
    hostList.value = list;
    hostStatuses.value = new Map(status.map((item) => [item.id, item]));
  } catch {
    // 静默：至少还能连宿主机。
  }
}

// 列表由「+」的下拉浮层承载，打开时才取数据；键盘 Alt+T 也走这里。
watch(pickerOpen, (open) => {
  if (open) void loadHosts();
});

function openPicker(): void {
  pickerOpen.value = true;
}

function startSession(hostId: number, hostName: string): void {
  addSession(hostId, hostName);
  pickerOpen.value = false;
}

/** 列表里的连通灯：没有状态时给黄色，表示"还没探测"。 */
function hostLed(row: Host): Record<string, boolean> {
  const status = hostStatuses.value.get(row.id);
  if (!status) return { 'led--warn': true };
  return status.online ? { 'led--ok': true } : { 'led--danger': true };
}

onMounted(() => {
  // 从「主机管理」那一行的「连接」进来时带了目标，直接开会话；
  // 从桌面图标进来时默认落在宿主机 —— 打开终端还要先选一台主机，
  // 等于把最常用的"本机运维"平白多推一步。
  if (props.hostId !== undefined) {
    addSession(props.hostId, props.hostName ?? '', props.cwd, props.runPath);
    return;
  }
  addSession(0, '宿主机');
});

// ---- 侧栏 ----

/**
 * 侧栏当前展示什么：文件、命令，或收起。
 *
 * 用三态而不是两个布尔：文件与命令占的是同一块位置，两个独立开关会
 * 产生"两个都开"这种画不出来的状态。
 *
 * 默认展开命令面板：进终端最先要做的事通常是敲命令；文件侧栏一打开就要读目标
 * 目录，没必要默认占掉 240px。宿主机与远端主机现在都支持文件侧栏。
 */
const sideMode = ref<'files' | 'commands' | null>('commands');

/**
 * 执行命令面板里点的命令。
 *
 * 走会话自己的写入通道：与键盘输入是同一条 WebSocket，PTY 会照常回显，
 * 用户看到的就是"命令真的输进去了"，而不是面板演的一个假象。
 */
function runCommand(command: string): void {
  const handle = handleOf(activeId.value);
  if (!handle) return;
  handle.writeInput(`${command}\r`);
  handle.focus();
}

// ---- 字号与字色 ----

const MIN_FONT_SIZE = 10;
const MAX_FONT_SIZE = 20;

/*
 * 这两项是"用户偏好"而不是"会话状态"：调过一次就该在下次打开窗口时还在，
 * 所以落 localStorage。字号在窗口内对所有标签共用一份 —— 切标签时字号
 * 跟着变会让人以为切到了另一个程序。
 */
const FONT_SIZE_KEY = 'panelssh.term.fontSize';
const FG_COLOR_KEY = 'panelssh.term.fg';

function loadFontSize(): number {
  const raw = Number(window.localStorage.getItem(FONT_SIZE_KEY));
  if (!Number.isFinite(raw) || raw <= 0) return 13;
  return Math.min(MAX_FONT_SIZE, Math.max(MIN_FONT_SIZE, Math.round(raw)));
}

const fontSize = ref(loadFontSize());

/**
 * 终端前景色；空串表示"跟随主题"。
 *
 * 只影响正文前景，ANSI 十六色保持不变 —— 那套颜色是程序（ls、git、日志）
 * 用来表达语义的，允许用户改会把语义改乱；用户真正想调的是"字看起来累不累"。
 */
const termFg = ref(window.localStorage.getItem(FG_COLOR_KEY) ?? '');

/*
 * 默认前景色。
 *
 * 与 TerminalSession 里的 DEFAULT_FOREGROUND 是同一个值：终端底色固定为
 * #08090c，默认前景也就固定。这里只用来给「默认」那一项画一个预览圆点。
 */
const DEFAULT_FG_COLOR = '#c3c9d1';

const TERM_COLORS = [
  { label: '默认（跟随主题）', value: '' },
  { label: '柔灰', value: '#9aa7b6' },
  { label: '高对比白', value: '#e9eef4' },
  { label: '琥珀', value: '#ffb02e' },
  { label: '青绿', value: '#57d98c' },
  { label: '天蓝', value: '#5fb8e8' },
];

watch(termFg, (value) => {
  // 选「默认」时删掉键，而不是存空串：主题改了要能跟着变回去。
  if (value === '') window.localStorage.removeItem(FG_COLOR_KEY);
  else window.localStorage.setItem(FG_COLOR_KEY, value);
});

function stepFontSize(delta: number): void {
  const next = Math.min(MAX_FONT_SIZE, Math.max(MIN_FONT_SIZE, fontSize.value + delta));
  if (next === fontSize.value) return;
  fontSize.value = next;
  window.localStorage.setItem(FONT_SIZE_KEY, String(next));
  // 字号由容器下发给各会话，它们自己重算网格并把新尺寸同步给远端 PTY。
  nextTick(() => handleOf(activeId.value)?.focus());
}

// ---- 导出会话 ----

/** 文件名里的时间戳：20261113-143005。 */
function stampOf(date: Date): string {
  const pad = (value: number): string => String(value).padStart(2, '0');
  return `${date.getFullYear()}${pad(date.getMonth() + 1)}${pad(date.getDate())}-${pad(date.getHours())}${pad(date.getMinutes())}${pad(date.getSeconds())}`;
}

/**
 * 把当前会话的完整回滚缓冲导出成 .txt。
 *
 * 文件名取「主机名-时间戳」：同一台机器导多次不会互相覆盖，
 * 而主机名让文件从名字就能看出是哪台机器。
 *
 * 内容加 BOM：Windows 记事本打开无 BOM 的 UTF-8 会把中文显示成乱码，
 * 而这份文件最常见的去向就是发给别人用记事本/Excel 打开。
 */
function exportSession(): void {
  const handle = handleOf(activeId.value);
  const session = active.value;
  if (!handle || !session) return;

  const text = handle.exportText();
  if (text.trim() === '') {
    message.warning('当前会话还没有输出，没有可导出的内容');
    return;
  }

  // 主机名是用户自己起的，可能带 / 空格 冒号 —— 直接用会让下载文件名非法。
  const safeName = nameOf(session).replace(/[\\/:*?"<>|\s]+/g, '_');
  const filename = `${safeName}-${stampOf(new Date())}.txt`;

  const blob = new Blob(['\uFEFF', text], { type: 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = filename;
  document.body.appendChild(link);
  link.click();
  link.remove();
  URL.revokeObjectURL(url);

  message.success(`已导出 ${filename}（${text.split('\n').length} 行）`);
}

// ---- 右键菜单 ----

const menu = ref({ show: false, x: 0, y: 0 });
const menuHasSelection = ref(false);

const menuOptions = computed<DropdownOption[]>(() => [
  { label: '新建会话（Alt+T）', key: 'new' },
  { key: 'd0', type: 'divider' },
  { label: '复制（Ctrl+Shift+C）', key: 'copy', disabled: !menuHasSelection.value },
  { label: '粘贴（Ctrl+Shift+V）', key: 'paste' },
  { label: '全选', key: 'selectAll' },
  { key: 'd1', type: 'divider' },
  { label: '清屏', key: 'clear' },
  { label: '重连', key: 'reconnect' },
  { label: '导出会话（.txt）', key: 'export', disabled: active.value === null },
  { key: 'd2', type: 'divider' },
  /*
   * 字号与字色收进「更多」的二级菜单。
   *
   * 会话条上不再有单独的「Aa」按钮：那里每个按钮都占横向空间，
   * 而这两项调一次能用很久，属于"设置"而不是"操作"。
   */
  {
    label: `字号 · 当前 ${fontSize.value}`,
    key: 'font',
    children: [
      { label: '放大（Alt+=）', key: 'fontUp', disabled: fontSize.value >= MAX_FONT_SIZE },
      { label: '缩小（Alt+-）', key: 'fontDown', disabled: fontSize.value <= MIN_FONT_SIZE },
    ],
  },
  {
    label: '字色',
    key: 'fg',
    children: TERM_COLORS.map((color) => ({
      // 当前项带「· 当前」，不用勾选图标也能一眼看出选了哪个。
      label: color.value === termFg.value ? `${color.label} · 当前` : color.label,
      key: `fg:${color.value}`,
      icon: () =>
        h('span', {
          class: 'termfg-dot',
          style: { background: color.value || DEFAULT_FG_COLOR },
        }),
    })),
  },
  { key: 'd3', type: 'divider' },
  { label: '关闭当前会话（Alt+W）', key: 'close' },
]);

function onMenu(payload: { x: number; y: number }): void {
  // 有选区才点亮「复制」：菜单项灰着比点了没反应清楚。
  menuHasSelection.value = handleOf(activeId.value)?.hasSelection() ?? false;
  menu.value = { show: true, x: payload.x, y: payload.y };
}

function onMenuSelect(key: string): void {
  menu.value = { ...menu.value, show: false };
  if (key === 'new') {
    void openPicker();
    return;
  }
  if (key === 'close') {
    if (activeId.value !== null) closeSession(activeId.value);
    return;
  }
  if (key === 'fontUp') {
    stepFontSize(1);
    return;
  }
  if (key === 'fontDown') {
    stepFontSize(-1);
    return;
  }
  if (key === 'export') {
    exportSession();
    return;
  }
  // 字色项的键形如 fg:#ffb02e；空值那项是 fg:（跟随主题）。
  if (key.startsWith('fg:')) {
    termFg.value = key.slice(3);
    return;
  }
  callActive(key as SessionAction);
}
</script>

<template>
  <div class="term" @keydown.capture="onKeydown">
    <!--
      会话条：一条横带同时承担"有哪几个会话"与"对当前会话能做什么"。

      tab 自己已经带着状态灯与主机名，这里不再重复一遍；地址只在右侧露一段，
      悬停看全文。动作收进「更多」菜单 —— 复制/粘贴/清屏/重连/字号都不是
      每秒要点的东西，常驻一排按钮要多占一行（36px + 14px 间距），
      而那行高度本来应该留给终端。
    -->
    <div class="term__tabs ui-strip">
      <div class="term__tablist">
        <button
          v-for="session in sessions"
          :key="session.id"
          class="term__tab"
          :class="{ 'is-active': session.id === activeId }"
          :title="nameOf(session)"
          @click="activate(session.id)"
        >
          <span class="led" :class="ledClassOf(states[session.id]?.status)" aria-hidden="true" />
          <span class="term__tab-name">{{ nameOf(session) }}</span>
          <span v-if="userOf(session)" class="term__tab-meta">{{ userOf(session) }}</span>
          <span
            class="term__tab-close"
            role="button"
            aria-label="关闭会话"
            :title="`关闭 ${nameOf(session)}（Alt+W）`"
            @click.stop="closeSession(session.id)"
          >
            <svg width="8" height="8" viewBox="0 0 10 10" aria-hidden="true">
              <path d="M2 2l6 6M8 2l-6 6" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
            </svg>
          </span>
        </button>
        <!--
          新建会话：点「+」在按钮下方展开主机下拉列表。
          不再用居中弹窗 —— 它只是"挑一台机器"，弹窗会把视线和鼠标都拽到
          屏幕中央，选完还要再回到终端，多两次位移。
          「+」不随会话数消失：关掉最后一个会话后，它是唯一的新建入口。
        -->
        <n-popover
          v-model:show="pickerOpen"
          trigger="click"
          placement="bottom-start"
          :show-arrow="false"
          @clickoutside="pickerOpen = false"
        >
          <template #trigger>
            <button class="term__tab-add" title="新建会话（Alt+T）" aria-label="新建会话">
              <svg width="9" height="9" viewBox="0 0 10 10" aria-hidden="true">
                <path d="M5 1.5v7M1.5 5h7" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
              </svg>
            </button>
          </template>
          <div class="termpick">
            <button class="termpick__item" @click="startSession(0, '宿主机')">
              <span class="led led--ok" aria-hidden="true" />
              <span class="termpick__name">宿主机</span>
              <span class="termpick__addr">本机</span>
            </button>
            <button
              v-for="row in hostList"
              :key="row.id"
              class="termpick__item"
              @click="startSession(row.id, row.name)"
            >
              <span class="led" :class="hostLed(row)" aria-hidden="true" />
              <span class="termpick__name">{{ row.name }}</span>
              <span class="termpick__addr num">{{ row.addr }}:{{ row.port }}</span>
            </button>
            <p v-if="hostList.length === 0" class="termpick__empty">
              还没有添加其它主机。可以在「主机管理」里新增，或先连接宿主机。
            </p>
          </div>
        </n-popover>
      </div>

      <span v-if="activeAddr" class="term__addr" :title="activeAddr">{{ activeAddr }}</span>

      <span class="term__bar-spacer" />

      <div class="term__tools">
        <button
          class="term__tool"
          :class="{ 'is-on': sideMode === 'files' }"
          :disabled="active === null"
          :title="sideMode === 'files' ? '收起文件侧栏' : '展开文件侧栏'"
          @click="sideMode = sideMode === 'files' ? null : 'files'"
        >
          文件
        </button>
        <!--
          命令与文件共用侧栏位置：两块内容一模一样宽，切换时终端视口不跳。
          本地终端的 PTY 一样能跑命令，所以这个按钮对宿主机会话也可用。
        -->
        <button
          class="term__tool"
          :class="{ 'is-on': sideMode === 'commands' }"
          :disabled="active === null"
          :title="sideMode === 'commands' ? '收起命令面板' : '展开命令面板'"
          @click="sideMode = sideMode === 'commands' ? null : 'commands'"
        >
          命令
        </button>
        <!--
          字号与字色都在「更多」的二级菜单里：它们是"调一次用很久"的设置，
          不该在会话条上各占一个按钮位。
        -->
        <n-dropdown trigger="click" placement="bottom-end" :options="menuOptions" @select="onMenuSelect">
          <button class="term__tool term__tool--more" title="更多操作" aria-label="更多操作">⋯</button>
        </n-dropdown>
      </div>
    </div>

    <div class="term__body">
      <SessionFiles
        v-if="sideMode === 'files' && active"
        :host-id="active.hostId"
        :host-name="activeName"
      />
      <SessionCommands v-else-if="sideMode === 'commands' && active" @run="runCommand" />

      <div class="term__main">
        <!--
          每个会话各自一个实例，靠 visibility 切换而不是销毁重建：
          隐藏时保留布局尺寸，xterm 不必在每次切标签时重算网格、重绘画布。
        -->
        <div
          v-for="session in sessions"
          :key="session.id"
          class="term__pane"
          :class="{ 'is-active': session.id === activeId }"
        >
          <TerminalSession
            :ref="(el) => setHandle(session.id, el)"
            :host-id="session.hostId"
            :host-name="session.hostName"
            :font-size="fontSize"
            :fg-color="termFg"
            @state="onState(session.id, $event)"
            @menu="onMenu"
          />
        </div>

        <p v-if="sessions.length === 0" class="term__empty">选好主机后这里会出现终端</p>
      </div>
    </div>

    <!--
      底部状态栏：与 XShell 一样只放「这条连接是什么」的事实。
      SSH 与 xterm-256color 不是装饰 —— 前者是后端建会话的方式，后者是
      RequestPty 的实参（见 internal/api/terminal.go），改后端要同步改这里。
      编码那一栏刻意留空：远端 LANG/LC_* 由目标机决定，面板拿不到真实值，
      写死一个「UTF-8」就是编造。
    -->
    <div class="term__foot">
      <span>SSH</span>
      <span>xterm-256color</span>
      <span class="num">{{ gridText }}</span>

      <span class="term__foot-spacer" />

      <!-- 快捷键只在这里提示一行：按钮上的 title 要悬停才看得到，
           而"窗口里还有这些键"本身值得被看见。 -->
      <span class="term__foot-keys">Alt+T 新建 · Alt+W 关闭 · Alt+1…9 切换</span>

      <!--
        状态与延时成一组贴在右下角：延时答"这台机器现在有多远"，状态答"这条会话
        通不通"，两个读数放在一起看才有意义，所以用分隔点相连而不是分置两端。
      -->
      <div class="term__foot-metrics">
        <span
          class="term__foot-status"
          :class="{ 'is-bad': activeState?.status === 'error' || activeState?.status === 'closed' }"
          :title="activeState?.statusText ?? ''"
        >
          {{ activeState?.statusText ?? '等待选择主机' }}
        </span>
        <template v-if="activeLatency">
          <span aria-hidden="true">·</span>
          <!-- 只剩数字，含义靠这个 title 撑住：它说的是探测延迟，不是本会话往返时延 -->
          <span class="num" :title="`面板到 ${activeName} 的 TCP 探测延迟，不是本会话的往返时延`">
            {{ activeLatency }}
          </span>
        </template>
      </div>
    </div>

    <n-dropdown
      :show="menu.show"
      :x="menu.x"
      :y="menu.y"
      :options="menuOptions"
      trigger="manual"
      placement="bottom-start"
      @select="onMenuSelect"
      @clickoutside="menu.show = false"
    />
  </div>
</template>
