<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';

import { terminalURL } from '../api/client';
import { currentTheme, currentThemeId } from '../desktop/theme';

/**
 * 单个终端会话：一个 xterm 实例 + 一条 WebSocket。
 *
 * 与容器的分工：这里只管"这一条连接本身" —— PTY 数据、剪贴板、网格尺寸、重连；
 * 会话列表、标签栏、工具条、状态栏、文件侧栏都留在容器里。后者是多个会话共用的
 * 东西，放进会话组件就会变成每个标签各持一份，切换时互相打架。
 */
const props = defineProps<{
  hostId: number;
  hostName: string;
  /** 字号由容器统一持有：切标签时整窗字号保持一致，不该各标签一份。 */
  fontSize: number;
  /** 正文前景色；空串表示跟随主题默认。同样由容器统一持有并持久化。 */
  fgColor: string;
}>();

const emit = defineEmits<{
  (
    e: 'state',
    payload: {
      status: 'connecting' | 'connected' | 'closed' | 'error';
      statusText: string;
      cols: number;
      rows: number;
    },
  ): void;
  /** 右键：坐标交给容器，由它统一弹菜单（菜单里有"关闭会话"这类容器级动作）。 */
  (e: 'menu', payload: { x: number; y: number }): void;
}>();

const container = ref<HTMLDivElement | null>(null);

const status = ref<'connecting' | 'connected' | 'closed' | 'error'>('connecting');
const statusText = ref('正在连接…');
const cols = ref(0);
const rows = ref(0);

/**
 * 状态上报走单一出口。
 *
 * 容器要在工具条的连通灯、底部状态栏、标签上同时表达"这个会话怎么样了"，
 * 逐个状态点去 emit 迟早漏一处；在这里汇总，容器只接一个事件。
 */
watch(
  [status, statusText, cols, rows],
  () => {
    emit('state', {
      status: status.value,
      statusText: statusText.value,
      cols: cols.value,
      rows: rows.value,
    });
  },
  { immediate: true },
);

let term: Terminal | null = null;
let fitAddon: FitAddon | null = null;
let socket: WebSocket | null = null;
let observer: ResizeObserver | null = null;
let resizeTimer: number | null = null;
let hintTimer: number | null = null;

// 远端 shell 是否开启了 bracketed paste（DECSET ?2004）。
// 只有开启时才用 ESC[200~/201~ 包裹粘贴内容；否则在 cat、密码提示等
// raw 输入场景下会留下字面转义序列。状态由 CSI 处理器实时维护。
let bracketedPaste = false;

interface ServerMessage {
  type: string;
  data?: string;
  message?: string;
}

// xterm 的主题色需要具体色值（canvas/DOM 渲染层不接受 CSS 变量），
// 因此这里从主题中心取强调色，其余底色与 ANSI 十六色保持固定 ——
// 终端在所有主题下都保持深底（这是设计约定），只有光标、选区跟着主题走。
function hexToRgba(hex: string, alpha: number): string {
  const value = hex.replace('#', '');
  const full =
    value.length === 3
      ? value
          .split('')
          .map((c) => c + c)
          .join('')
      : value;
  const num = Number.parseInt(full, 16);
  const r = (num >> 16) & 255;
  const g = (num >> 8) & 255;
  const b = num & 255;
  return `rgba(${r}, ${g}, ${b}, ${alpha})`;
}

/*
 * 正文默认前景色。
 *
 * 与原本写死在 termTheme 里的值一致；提成常量是为了让"跟随主题"（fgColor 为空）
 * 这个状态有一个明确的落点，而不是在两处各写一遍字面量。
 */
const DEFAULT_FOREGROUND = '#c3c9d1';

function termTheme() {
  const theme = currentTheme.value;
  // 浅色主题的 accent 在深色终端上偏暗，用 accentHi 保证光标可辨。
  const accent = theme.dark ? theme.vars['--sig'] : theme.vars['--sig-hi'];
  return {
    background: '#08090c',
    foreground: props.fgColor || DEFAULT_FOREGROUND,
    cursor: accent,
    cursorAccent: '#08090c',
    selectionBackground: hexToRgba(accent, 0.28),
    black: '#12151a',
    red: '#ff6b6b',
    green: '#57d98c',
    yellow: '#ffb02e',
    blue: '#5fb8e8',
    magenta: '#cfd95c',
    cyan: '#45c7c0',
    white: '#eef1f5',
    brightBlack: '#565c66',
    brightRed: '#ff8f8f',
    brightGreen: '#7ee2a8',
    brightYellow: '#ffc968',
    brightBlue: '#8fcdf2',
    brightMagenta: '#dfe885',
    brightCyan: '#6fd8d2',
    brightWhite: '#ffffff',
  };
}

// 切换主题后立刻换掉终端配色：不必重建会话，xterm 支持运行期改 theme。
watch(currentThemeId, () => {
  if (term) term.options.theme = termTheme();
});

// 字色同理：容器统一下发，改一次所有标签一起变，不需要重连。
watch(
  () => props.fgColor,
  () => {
    if (term) term.options.theme = termTheme();
  },
);

function send(payload: Record<string, unknown>): void {
  if (socket && socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify(payload));
  }
}

function syncSize(): void {
  if (!term) return;
  send({ type: 'resize', cols: term.cols, rows: term.rows });
}

/**
 * 在状态栏短暂提示一句，2.5s 后恢复。剪贴板被浏览器拒绝时必须给反馈，
 * 否则用户会以为按键失灵而反复按。
 */
function flashHint(text: string): void {
  statusText.value = text;
  if (hintTimer !== null) window.clearTimeout(hintTimer);
  hintTimer = window.setTimeout(() => {
    hintTimer = null;
    // 仅在连接仍然正常时恢复；连接状态的变化本身会改写文案，不覆盖它。
    if (status.value === 'connected') statusText.value = '已连接';
  }, 2500);
}

/**
 * 复制终端选区到系统剪贴板。
 *
 * 主路径是 Clipboard API（需安全上下文）；通过局域网 IP 的 http 访问面板时
 * 该 API 不可用，降级到隐藏 textarea + execCommand('copy')。
 * 返回 false 表示当前没有选区（与复制失败区分开，便于给不同提示）。
 */
async function copySelection(): Promise<boolean> {
  const text = term?.getSelection() ?? '';
  if (!text) return false;

  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 用户拒绝授权等情况落到下面的兜底路径。
    }
  }

  const ta = document.createElement('textarea');
  ta.value = text;
  // 移出可视区域但保持可选中：display:none / visibility:hidden 会让 select 失效。
  ta.style.position = 'fixed';
  ta.style.top = '-9999px';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand('copy');
  } catch {
    ok = false;
  }
  ta.remove();
  if (!ok) flashHint('复制失败：浏览器拒绝访问剪贴板');
  return ok;
}

/**
 * 把文本作为"用户输入"发给远端 PTY —— 必须走 input 链路，
 * 不能用 term.write，否则只在浏览器里显示、远端 shell 收不到。
 * shell 开启 bracketed paste 时按协议包裹，多行内容会被当作整块粘贴，
 * 而不是逐行回车执行。
 */
function sendPaste(text: string): void {
  if (!text) return;
  const payload = bracketedPaste ? `\x1b[200~${text}\x1b[201~` : text;
  send({ type: 'input', data: payload });
}

/**
 * 直接往 PTY 写入一段输入。
 *
 * 给容器用：窗口带着工作目录打开时，容器要在这里发一条 cd。
 * 不复用 sendPaste —— 那个会按 shell 的括号粘贴模式包一层转义序列，
 * 而这里发的是命令本身，包了反而会原样出现在命令行里。
 */
function writeInput(text: string): void {
  if (!text) return;
  send({ type: 'input', data: text });
}

/**
 * 读取系统剪贴板并粘贴。非安全上下文或用户拒绝授权时 readText 不可用，
 * 此时明确提示改用 Ctrl+V —— 那条路径走浏览器原生 paste 事件，不需要权限。
 */
async function pasteClipboard(): Promise<void> {
  if (!window.isSecureContext || !navigator.clipboard?.readText) {
    flashHint('当前页面不允许读取剪贴板，请改用 Ctrl+V 粘贴');
    return;
  }
  let text: string;
  try {
    text = await navigator.clipboard.readText();
  } catch {
    flashHint('读取剪贴板被拒绝，请改用 Ctrl+V 粘贴');
    return;
  }
  term?.focus();
  sendPaste(text);
}

/** 右键只把坐标抛给容器 —— 菜单是容器级的（含关闭会话、标签操作）。 */
function onContextMenu(event: MouseEvent): void {
  event.preventDefault();
  emit('menu', { x: event.clientX, y: event.clientY });
}

/**
 * 复制当前选区（快捷键、工具条、右键菜单共用）。
 *
 * 无选区时给一句提示：复制成功本身没有反馈，而终端 buffer 里也不能写
 * 「已复制」之类的字 —— 那会污染远端输出。失败就说清楚，沉默则像是按钮坏了。
 */
function copyFromSelection(): void {
  void copySelection().then((ok) => {
    if (!ok && term?.hasSelection() === false) flashHint('没有选中的文本');
  });
  // 点完按钮接着敲键盘是常规动作，焦点必须留在终端里。
  term?.focus();
}

/**
 * 清屏：只清本地缓冲区与回滚历史，不碰远端会话。
 *
 * 想清远端自己的历史由用户敲 clear —— 这里若代发命令，就变成了"按按钮等于
 * 在那个 shell 里执行命令"，用户不会预期屏幕按钮能写远端 stdin。
 */
function clearScreen(): void {
  term?.clear();
  term?.focus();
}

// CSI 参数允许嵌套（复合参数），拍平后判断是否包含目标私有模式号。
function csiIncludesMode(params: (number | number[])[], target: number): boolean {
  return params.some((p) => (Array.isArray(p) ? p.includes(target) : p === target));
}

/**
 * 绑定复制/粘贴快捷键。
 *
 * - 复制：Ctrl+Shift+C（Win/Linux）、Cmd+C（Mac）、Ctrl+Insert
 * - 粘贴：Ctrl+Shift+V、Shift+Insert
 * - 普通 Ctrl+V / Cmd+V 不拦截：浏览器会派发原生 paste 事件给 xterm 的
 *   隐藏输入框，由 xterm 自己注入 PTY（非安全上下文下的可用粘贴路径）。
 * - 普通 Ctrl+C 永远放行给远端（SIGINT），绝不绑定为复制。
 *
 * 拦截的按键必须显式 preventDefault：customKeyEventHandler 返回 false
 * 只阻止 xterm 处理，不阻止浏览器默认动作，不拦住会同时触发原生 paste
 * 造成内容双发。
 */
function bindClipboardKeys(): void {
  if (!term) return;

  term.attachCustomKeyEventHandler((event) => {
    if (event.type !== 'keydown') return true;

    const isCopy =
      (event.ctrlKey && event.shiftKey && !event.metaKey && event.code === 'KeyC') ||
      (event.metaKey && !event.ctrlKey && event.code === 'KeyC') ||
      (event.ctrlKey && !event.shiftKey && !event.metaKey && event.key === 'Insert');
    if (isCopy) {
      event.preventDefault();
      copyFromSelection();
      return false;
    }

    const isPaste =
      (event.ctrlKey && event.shiftKey && !event.metaKey && event.code === 'KeyV') ||
      (event.shiftKey && !event.ctrlKey && !event.metaKey && event.key === 'Insert');
    if (isPaste) {
      event.preventDefault();
      void pasteClipboard();
      return false;
    }

    return true;
  });
}

/**
 * 心跳。
 *
 * WebSocket 的 onclose 不可靠：网络静默中断（拔网线、目标机断电、中间设备
 * 悄悄回收会话）时，浏览器可能几分钟都不报错，界面就一直停在一个早已死掉的
 * 「已连接」上。应用层心跳是唯一能自己判定"这条链路还在不在"的办法。
 *
 * 每 10 秒发一次 ping，超过 25 秒收不到 pong 就判为断开 —— 容忍连续两个包
 * 丢失，最迟 30 秒内发现真断线；而一条空闲的终端不会因此产生任何可见影响。
 */
const HEARTBEAT_MS = 10_000;
const HEARTBEAT_TIMEOUT_MS = 25_000;
let heartbeatTimer: number | undefined;
let lastPongAt = 0;

function stopHeartbeat(): void {
  if (heartbeatTimer !== undefined) window.clearInterval(heartbeatTimer);
  heartbeatTimer = undefined;
}

function startHeartbeat(ws: WebSocket): void {
  stopHeartbeat();
  lastPongAt = Date.now();

  heartbeatTimer = window.setInterval(() => {
    if (socket !== ws || ws.readyState !== WebSocket.OPEN) {
      stopHeartbeat();
      return;
    }

    if (Date.now() - lastPongAt > HEARTBEAT_TIMEOUT_MS) {
      // 连续两轮没有回音：链路已经死了，只是浏览器还没告诉我们。
      // 必须在这里自己把状态改成断开，否则界面会一直挂着「已连接」骗人。
      stopHeartbeat();
      status.value = 'closed';
      statusText.value = '连接已断开（无响应）';
      term?.write('\r\n\x1b[33m连接已断开：多次心跳无响应\x1b[0m\r\n');
      ws.close();
      return;
    }

    send({ type: 'ping' });
  }, HEARTBEAT_MS);
}

/**
 * 建立（或重建）终端连接。
 *
 * 抽成独立函数是为了支持重连：远端重启、网络抖动、或初始握手失败之后，
 * 用户不必关掉窗口再重新从主机列表打开一次 —— 会话历史还能留在原地。
 */
function connect(): void {
  if (!term) return;

  status.value = 'connecting';
  statusText.value = '正在连接…';

  const ws = new WebSocket(terminalURL(props.hostId));
  socket = ws;

  /**
   * 旧连接的回调必须整体失效。
   *
   * 重连是"先 close 再新建"，而 close 事件要到下一轮事件循环才派发 ——
   * 那时 socket 已指向新连接，旧连接的 onclose 会把新连接的「已连接」
   * 改写成「连接已断开」，状态栏随后一直挂在错误状态上。
   */
  const isCurrent = (): boolean => socket === ws;

  ws.onopen = () => {
    if (!isCurrent()) return;
    status.value = 'connected';
    statusText.value = '已连接';
    // 后端 PTY 初值为 30x100，连上后立刻同步真实尺寸。
    fitAddon?.fit();
    syncSize();
    term?.focus();
    startHeartbeat(ws);
  };

  ws.onmessage = (event: MessageEvent<string>) => {
    if (!isCurrent()) return;

    let msg: ServerMessage;
    try {
      msg = JSON.parse(event.data) as ServerMessage;
    } catch {
      return;
    }

    switch (msg.type) {
      case 'pong':
        lastPongAt = Date.now();
        break;
      case 'output':
        if (msg.data) term?.write(msg.data);
        break;
      case 'error':
        status.value = 'error';
        statusText.value = msg.message ?? '连接出错';
        term?.write(`\r\n\x1b[31m${msg.message ?? '连接出错'}\x1b[0m\r\n`);
        break;
      case 'exit':
        status.value = 'closed';
        statusText.value = msg.message ?? '会话已结束';
        term?.write(`\r\n\x1b[33m${msg.message ?? '会话已结束'}\x1b[0m\r\n`);
        break;
      default:
        break;
    }
  };

  ws.onerror = () => {
    if (!isCurrent()) return;
    status.value = 'error';
    statusText.value = 'WebSocket 连接失败';
  };

  ws.onclose = () => {
    if (!isCurrent()) return;
    stopHeartbeat();
    // 仅在未主动进入终态时改写，避免覆盖更具体的错误信息。
    if (status.value === 'connected' || status.value === 'connecting') {
      status.value = 'closed';
      statusText.value = '连接已断开';
    }
  };
}

function reconnect(): void {
  if (socket && socket.readyState !== WebSocket.CLOSED) {
    socket.close();
  }
  term?.write('\r\n\x1b[90m—— 重新连接 ——\x1b[0m\r\n');
  connect();
}

/**
 * 字号变了要重算网格。
 *
 * 字号只影响本地渲染，但行列数会因此改变：这次 fit 触发 onResize，
 * 新的尺寸由那里统一同步给远端 PTY。
 */
watch(
  () => props.fontSize,
  (size) => {
    if (!term) return;
    term.options.fontSize = size;
    fitAddon?.fit();
  },
);

onMounted(() => {
  const host = container.value;
  if (!host) return;

  term = new Terminal({
    cursorBlink: true,
    fontSize: props.fontSize,
    fontFamily: '"JetBrains Mono", "Cascadia Mono", Consolas, "DejaVu Sans Mono", monospace',
    theme: termTheme(),
    scrollback: 5000,
  });
  fitAddon = new FitAddon();
  term.loadAddon(fitAddon);
  term.open(host);
  fitAddon.fit();

  // open + fit 之后 xterm 已有确定的网格尺寸，先把状态栏的读数填上；
  // 之后窗口缩放、字号调整引起的行列变化都由 onResize 维护。
  cols.value = term.cols;
  rows.value = term.rows;

  /**
   * 尺寸变化的唯一出口：既更新底部状态栏的读数，也把新行列数同步给远端 PTY。
   *
   * 窗口拖拽缩放（ResizeObserver → fit）与字号调整（fontSize watch → fit）
   * 最终都汇到这里，不必在每处 fit 之后各写一遍同步。
   */
  term.onResize((size) => {
    cols.value = size.cols;
    rows.value = size.rows;
    syncSize();
  });

  term.onData((data) => send({ type: 'input', data }));

  bindClipboardKeys();
  term.element?.addEventListener('contextmenu', onContextMenu);
  // 跟踪远端 bracketed paste 模式（ESC[?2004h / ESC[?2004l）。
  term.parser.registerCsiHandler({ prefix: '?', final: 'h' }, (params) => {
    if (csiIncludesMode(params, 2004)) bracketedPaste = true;
    // 返回 false 表示不吞掉该序列，行编辑器仍需正常处理它。
    return false;
  });
  term.parser.registerCsiHandler({ prefix: '?', final: 'l' }, (params) => {
    if (csiIncludesMode(params, 2004)) bracketedPaste = false;
    return false;
  });

  // 这里不写"正在连接"之类的提示：缓冲区只放远端 PTY 的输出。
  // 连接进度是临时状态，由窗口的状态栏承担 —— 它实时、可清除，
  // 而写进终端的东西会永久留在 scrollback 里（回滚、复制、清屏都会撞见它）。
  connect();

  // 窗口被拖拽缩放时重算终端网格；合并短时间内的连续触发，避免刷屏 resize。
  // 这次 fit 引起的行列变化由上面的 onResize 一并同步给远端。
  observer = new ResizeObserver(() => {
    if (resizeTimer !== null) window.clearTimeout(resizeTimer);
    resizeTimer = window.setTimeout(() => fitAddon?.fit(), 60);
  });
  observer.observe(host);
});

onBeforeUnmount(() => {
  if (resizeTimer !== null) window.clearTimeout(resizeTimer);
  if (hintTimer !== null) window.clearTimeout(hintTimer);
  stopHeartbeat();
  observer?.disconnect();
  observer = null;
  socket?.close();
  socket = null;
  // CSI 处理器注册在 term 上，随 dispose 失效；contextmenu 挂在 element 上，
  // dispose 前显式摘掉，避免热更新/重开窗口期间残留监听。
  term?.element?.removeEventListener('contextmenu', onContextMenu);
  term?.dispose();
  term = null;
  fitAddon = null;
});

/**
 * 把整段回滚缓冲导出成纯文本。
 *
 * 取的是 xterm 的 normal buffer：包含屏幕上的内容与 scrollback 里已经滚出去的行，
 * 也就是用户往上翻能看到多少，导出就有多少。
 *
 * 只给文本、不带 ANSI 转义：导出这件事的用途是存档、贴给同事、grep 关键字，
 * 带转义序列的文件在编辑器里全是 `ESC[...m`，反而不能用。
 *
 * 末尾连续空行会剪掉 —— 终端底部本来就有一段空白，留着只是让文件多几行。
 */
function exportText(): string {
  const buffer = term?.buffer.active;
  if (!buffer) return '';

  const lines: string[] = [];
  for (let i = 0; i < buffer.length; i += 1) {
    const line = buffer.getLine(i);
    lines.push(line ? line.translateToString(true) : '');
  }
  while (lines.length > 0 && lines[lines.length - 1].trim() === '') lines.pop();
  return lines.join('\n');
}

/**
 * 对容器开出的能力。
 *
 * 工具条按钮、右键菜单都作用于"当前活动会话"，但那些控件住在容器里，
 * 所以只能由容器拿着这些方法去调 —— 会话自己不持有任何窗口级 UI。
 */
defineExpose({
  /** 重算网格：标签从隐藏切回可见时容器要调它（隐藏期间容器宽高为 0）。 */
  refit: () => fitAddon?.fit(),
  focus: () => term?.focus(),
  copy: copyFromSelection,
  paste: () => void pasteClipboard(),
  writeInput,
  clear: clearScreen,
  reconnect,
  exportText,
  selectAll: () => {
    term?.selectAll();
    term?.focus();
  },
  hasSelection: () => term?.hasSelection() ?? false,
});
</script>

<template>
  <div
    ref="container"
    class="term__viewport"
    title="复制：选中后 Ctrl+Shift+C（Mac 为 Cmd+C）或右键｜粘贴：Ctrl+V / Ctrl+Shift+V 或右键"
  />
</template>
