<script setup lang="ts">
/**
 * 系统日志窗口：目标机日志与面板自身日志的只读查看。
 *
 * 四个来源的取法完全不同，因此不合成一个"读日志"的动作：
 * journalctl 走 unit/优先级过滤，日志文件按路径 tail，dmesg 是内核环形缓冲，
 * 面板日志则是本地文件 —— 最后这条不依赖主机选择。
 *
 * 只读、不落库：日志是目标机上的既有事实，面板抄一份只会带来留存与隐私问题。
 */
import { computed, h, nextTick, onBeforeUnmount, onMounted, ref, watch, type VNodeChild } from 'vue';
import {
  NAlert,
  NButton,
  NInput,
  NSelect,
  NTabs,
  NTabPane,
  NTag,
  useMessage,
  type SelectOption,
} from 'naive-ui';

import { ApiError, api, copyText, type LogSources, type LogView, type PanelLogView } from '../api/client';
import { formatBytes, formatTime } from '../api/format';
import HostPicker from '../components/HostPicker.vue';
import { useVisiblePoll } from '../desktop/useVisiblePoll';

/** 行数选项。上限 2000 由后端兜底，这里给的是常用档位。 */
const LINE_OPTIONS = [
  { label: '100 行', value: 100 },
  { label: '200 行', value: 200 },
  { label: '500 行', value: 500 },
  { label: '1000 行', value: 1000 },
];

/** journalctl -p 的取值。空串表示不过滤。 */
const PRIORITY_OPTIONS = [
  { label: '全部级别', value: '' },
  { label: 'emerg', value: 'emerg' },
  { label: 'alert', value: 'alert' },
  { label: 'crit', value: 'crit' },
  { label: 'err', value: 'err' },
  { label: 'warning', value: 'warning' },
  { label: 'notice', value: 'notice' },
  { label: 'info', value: 'info' },
  { label: 'debug', value: 'debug' },
];

/** 自动刷新的间隔。日志是持续写入的，5 秒足够看到变化又不至于压垮目标机。 */
const POLL_MS = 5000;

/*
 * 折叠"环境提示"用的白名单。
 *
 * 这些行不是日志，是 journalctl 自己或面板提权探测留在 journal 里的回声：
 * 面板每轮询一次就可能追加一条 sudo 记录，几百行真实日志会被它们淹掉。
 * 只有完全命中下面这些形状才折叠，且随时可以点开 —— 不能拿它当"过滤掉不想看的报错"。
 */
const NOISE_SNIPPETS = [
  'no version information available',
  'you are currently not seeing messages from other users',
  "users in groups 'adm', 'systemd-journal' can see all messages",
  'pass -q to turn off this notice',
];

type Tab = 'journal' | 'file' | 'dmesg' | 'panel';
type Level = 'error' | 'warn' | '';

interface LogRow {
  raw: string;
  /** 时间列只放时分秒：完整日期在 200 行的可视范围里没有信息量。 */
  time: string;
  fullTime: string;
  /** 进程 / systemd 单元。主机名每行都一样，不占列宽。 */
  source: string;
  message: string;
  level: Level;
  noise: boolean;
}

const message = useMessage();

const root = ref<HTMLElement | null>(null);
const linesEl = ref<HTMLElement | null>(null);

const hostId = ref<number | null>(null);
const hostName = ref('');

const tab = ref<Tab>('journal');
const lines = ref(200);
const autoRefresh = ref(true);
const keyword = ref('');
const levelFilter = ref<Level>('');

const journalUnit = ref('');
const journalPriority = ref('');

const sources = ref<LogSources | null>(null);
const sourcesLoading = ref(false);
const selectedFile = ref<string | null>(null);

const log = ref<LogView | null>(null);
const logLoading = ref(false);
const lastError = ref('');

const panelLog = ref<PanelLogView | null>(null);
const panelLoading = ref(false);

/** 暂停时的"画面时刻"：告诉用户眼前这份内容停在什么时候，而不是以为它在实时滚动。 */
const frozenAt = ref<Date | null>(null);
const lastFetchedAt = ref<Date | null>(null);

/** 噪音默认收起；用户点开后就一直展开，直到切换来源。 */
const showNoise = ref(false);
/** 滚动位置是否贴着底部。贴底时刷新后继续跟随，上滚查看历史时绝不抢视线。 */
const atBottom = ref(true);

const isPanel = computed(() => tab.value === 'panel');
const busy = computed(() => logLoading.value || panelLoading.value);
const filtering = computed(() => keyword.value.trim() !== '' || levelFilter.value !== '');

/** 当前来源能不能取：面板日志与主机无关，其余必须先选主机（文件还要选中一个文件）。 */
const canFetch = computed(() => {
  if (isPanel.value) return true;
  if (hostId.value === null) return false;
  if (tab.value === 'file') return selectedFile.value !== null;
  return true;
});

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

/**
 * 拉这台机器上有什么日志可看。
 *
 * 只在切到依赖主机的页签时做：这一步要起一次 SSH 会话（含提权探测），
 * 而「面板日志」根本不需要它。
 */
async function loadSources(): Promise<void> {
  if (isPanel.value || hostId.value === null) return;
  sourcesLoading.value = true;
  try {
    sources.value = await api.logs.sources(hostId.value);
    lastError.value = '';
    // 换主机后原来选中的文件多半不存在了，退回清单里最近被写过的那个。
    const available = sources.value.files.some((f) => f.path === selectedFile.value);
    if (!available) selectedFile.value = sources.value.files[0]?.path ?? null;
  } catch (err) {
    sources.value = null;
    lastError.value = describe(err, '读取日志来源失败');
  } finally {
    sourcesLoading.value = false;
  }
}

/** 取一次当前页签的内容。自动刷新与手动刷新走的是同一条路径。 */
async function fetchLog(): Promise<void> {
  if (tab.value === 'panel') {
    panelLoading.value = true;
    try {
      panelLog.value = await api.logs.panel(lines.value);
      lastError.value = '';
      markFetched();
    } catch (err) {
      panelLog.value = null;
      lastError.value = describe(err, '读取面板日志失败');
    } finally {
      panelLoading.value = false;
    }
    await followLatest();
    return;
  }

  if (!canFetch.value || hostId.value === null) return;
  logLoading.value = true;
  try {
    const params =
      tab.value === 'journal'
        ? { source: 'journal' as const, unit: journalUnit.value.trim(), priority: journalPriority.value }
        : tab.value === 'file'
          ? { source: 'file' as const, file: selectedFile.value ?? '' }
          : { source: 'dmesg' as const };
    log.value = await api.logs.tail(hostId.value, { ...params, lines: lines.value });
    lastError.value = '';
    markFetched();
  } catch (err) {
    log.value = null;
    lastError.value = describe(err, '读取日志失败');
  } finally {
    logLoading.value = false;
  }
  await followLatest();
}

function markFetched(): void {
  lastFetchedAt.value = new Date();
  // 暂停状态下手动刷新了一次：画面时刻跟着走，否则会显示一个比内容更早的时间。
  if (!autoRefresh.value) frozenAt.value = lastFetchedAt.value;
}

/* ---------------- 日志行的解析与着色 ---------------- */

const ISO_TIME = /^(\d{4}-\d{2}-\d{2})T(\d{2}:\d{2}:\d{2})(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})\s+/;
const SYSLOG_TIME = /^([A-Z][a-z]{2})\s+(\d{1,2})\s+(\d{2}:\d{2}:\d{2})\s+/;
const PANEL_TIME = /^(\d{4})\/(\d{2})\/(\d{2})\s+(\d{2}:\d{2}:\d{2})\s+/;
const DMESG_TIME = /^\[\s*(\d+\.\d+)\]\s+/;
/** journalctl / syslog：`主机名 进程[pid]: 消息`。 */
const SRC_WITH_HOST = /^(\S+)\s+([\w.@+-]+(?:\[\d+\])?):\s*/;
/** 没有主机名的来源（面板日志、journalctl 自己输出的提示）：`进程: 消息`。 */
const SRC_PLAIN = /^([\w.@+-]+(?::\d+)?(?:\[\d+\])?):\s*/;

const ERROR_RE = /\b(error|errno|failed|failure|fatal|panic|crit(?:ical)?|emerg(?:ency)?|denied|refused|timeout|timed out|killed)\b/i;
const WARN_RE = /\b(warn(?:ing)?|deprecated|retry(?:ing)?)\b/i;

function levelOf(text: string): Level {
  if (ERROR_RE.test(text) || text.includes('错误') || text.includes('失败')) return 'error';
  if (WARN_RE.test(text) || text.includes('警告')) return 'warn';
  return '';
}

function isNoiseLine(raw: string): boolean {
  const lower = raw.toLowerCase();
  // 面板提权探测的特征是三件事同时成立；用户自己的 sudo 失败不会被误收。
  if (
    lower.includes('a password is required') &&
    lower.includes('command=/usr/bin/true')
  ) {
    return true;
  }
  return NOISE_SNIPPETS.some((snippet) => lower.includes(snippet));
}

/**
 * 把一行拆成「时间 / 来源 / 消息」三段。
 *
 * 解析失败不丢信息：拆不出来就整行当消息，视觉上仍是原文。
 * 这样 nginx access log、缩进的堆栈续行这类非标准格式也能正常看。
 */
function parseLine(raw: string): LogRow {
  let rest = raw;
  let time = '';
  let fullTime = '';
  let match: RegExpExecArray | null;

  if ((match = ISO_TIME.exec(rest))) {
    time = match[2];
    fullTime = `${match[1]} ${match[2]}`;
    rest = rest.slice(match[0].length);
  } else if ((match = SYSLOG_TIME.exec(rest))) {
    time = match[3];
    fullTime = `${match[1]} ${match[2]} ${match[3]}`;
    rest = rest.slice(match[0].length);
  } else if ((match = PANEL_TIME.exec(rest))) {
    time = match[4];
    fullTime = `${match[1]}-${match[2]}-${match[3]} ${match[4]}`;
    rest = rest.slice(match[0].length);
  } else if ((match = DMESG_TIME.exec(rest))) {
    time = `[${match[1]}]`;
    fullTime = time;
    rest = rest.slice(match[0].length);
  }

  let source = '';
  if ((match = SRC_WITH_HOST.exec(rest))) {
    source = match[2];
    rest = rest.slice(match[0].length);
  } else if ((match = SRC_PLAIN.exec(rest))) {
    source = match[1];
    rest = rest.slice(match[0].length);
  }

  const noise = isNoiseLine(raw);
  return {
    raw,
    time,
    fullTime: fullTime || time,
    source,
    message: rest,
    level: noise ? '' : levelOf(rest),
    noise,
  };
}

/**
 * 内容里的行，以及关键字过滤与高亮。
 *
 * 高亮必须"先转义再包 mark"：日志内容来自目标机上的任意程序，直接塞进
 * v-html 等于把远端输出当 HTML 渲染。
 */
function escapeHtml(text: string): string {
  return text.replace(
    /[&<>"']/g,
    (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[ch] ?? ch,
  );
}

function highlight(text: string, kw: string): string {
  const escaped = escapeHtml(text);
  const needle = escapeHtml(kw).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  if (!needle) return escaped;
  return escaped.replace(new RegExp(needle, 'gi'), (match) => `<mark>${match}</mark>`);
}

/** 当前展示的原始行（面板日志与目标机日志共用一个出口）。 */
const sourceLines = computed(() => (isPanel.value ? (panelLog.value?.lines ?? []) : (log.value?.lines ?? [])));

const parsedRows = computed(() => sourceLines.value.map(parseLine));
const noiseRows = computed(() => parsedRows.value.filter((row) => row.noise));
const contentRows = computed(() => parsedRows.value.filter((row) => !row.noise));
const errorCount = computed(() => contentRows.value.filter((row) => row.level === 'error').length);
const warnCount = computed(() => contentRows.value.filter((row) => row.level === 'warn').length);

/**
 * 最终要渲染的行。
 *
 * 噪音行默认不出现，但有两种情况必须露出来：用户点开了折叠条，
 * 或者关键字正好命中它 —— 搜索是"我要找这串字"，不能用折叠把它藏起来。
 */
const visibleRows = computed(() => {
  const kw = keyword.value.trim().toLowerCase();
  return parsedRows.value.filter((row) => {
    if (levelFilter.value && row.level !== levelFilter.value) return false;
    if (kw && !row.raw.toLowerCase().includes(kw)) return false;
    if (row.noise && !showNoise.value && !kw) return false;
    return true;
  });
});

const renderedRows = computed(() => {
  const kw = keyword.value.trim();
  return visibleRows.value.map((row) => ({ ...row, html: highlight(row.message, kw) }));
});

/** 过滤生效或折叠了噪音时给出总数，避免"204 行怎么只剩 48 行"的疑问。 */
const countWithTotal = computed(
  () => filtering.value || (noiseRows.value.length > 0 && !showNoise.value),
);

/** 权限/空内容提示：两条来源各自带着自己的 note。 */
const note = computed(() => (isPanel.value ? (panelLog.value?.note ?? '') : (log.value?.note ?? '')));

const truncated = computed(() => !isPanel.value && (log.value?.truncated ?? false));

/* ---------------- 展示辅助 ---------------- */

const frozenLabel = computed(() => clock(frozenAt.value ?? lastFetchedAt.value));

const fetchedLabel = computed(() => (lastFetchedAt.value ? clock(lastFetchedAt.value) : '尚未读取'));

function clock(date: Date | null): string {
  if (!date) return '--:--:--';
  return date.toLocaleTimeString('zh-CN', { hour12: false });
}

const fileOptions = computed(() =>
  (sources.value?.files ?? []).map((file) => ({
    label: file.path,
    value: file.path,
    meta: `${formatBytes(file.bytes)} · ${formatTime(new Date(file.mtime * 1000).toISOString())}`,
  })),
);

/** 下拉选项两行：路径在上、大小与时间在下；选中态只留路径，避免把 28px 的控件撑高。 */
function renderFileLabel(option: SelectOption): VNodeChild {
  return h('span', { class: 'logs__fileopt-path num' }, String(option.label ?? ''));
}

function renderFileOption(info: { option: SelectOption }): VNodeChild {
  const option = info.option;
  const meta = (option as SelectOption & { meta?: string }).meta ?? '';
  return h('div', { class: 'logs__fileopt' }, [
    h('span', { class: 'logs__fileopt-path num' }, String(option.label ?? '')),
    h('span', { class: 'logs__fileopt-meta num' }, meta),
  ]);
}

const fileSummary = computed(() => {
  if (sourcesLoading.value) return '正在扫描日志文件…';
  const total = sources.value?.files.length ?? 0;
  if (total === 0) return '没有找到常见日志文件，或当前账号读不到 /var/log。';
  return `共 ${total} 个文件，按最近写入排序`;
});

const dmesgHint =
  '内核环形缓冲。部分发行版把 dmesg 限制为 root 可读（kernel.dmesg_restrict），提不了权的机器会给出明确提示。';
const panelHint =
  '面板自身进程的日志（与所选主机无关）。面板不是 systemd 服务，这份文件是回看上次启动与崩溃现场的唯一入口；每次启动时若超过 5 MB 会先轮转为 panelssh.log.1。';

const emptyHint = computed(() => {
  if (!isPanel.value && hostId.value === null) return '先选择一台主机。';
  if (tab.value === 'file' && selectedFile.value === null) return '先选择一个日志文件。';
  if (filtering.value) return '没有匹配的日志行。';
  if (noiseRows.value.length > 0) return '这个来源只返回了环境提示，已折叠在上方。';
  if (isPanel.value) return '面板日志当前没有返回内容。';
  return '这个来源当前没有返回内容。';
});

/* ---------------- 交互 ---------------- */

function toggleAuto(): void {
  autoRefresh.value = !autoRefresh.value;
  if (autoRefresh.value) {
    frozenAt.value = null;
    void fetchLog();
    return;
  }
  frozenAt.value = lastFetchedAt.value ?? new Date();
}

/** 阈值留一点余量：底部的最后一行往往不到 20px 高，严格等于零会让人贴不住底。 */
const BOTTOM_SLACK = 24;

function onLinesScroll(): void {
  const el = linesEl.value;
  if (!el) return;
  atBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight <= BOTTOM_SLACK;
}

function scrollToBottom(): void {
  const el = linesEl.value;
  if (!el) return;
  el.scrollTop = el.scrollHeight;
  atBottom.value = true;
}

/** 贴底才跟随：用户正在回看上面的内容时，刷新不应该把他拽回底部。 */
async function followLatest(): Promise<void> {
  if (!atBottom.value) return;
  await nextTick();
  scrollToBottom();
}

async function copyVisible(): Promise<void> {
  if (visibleRows.value.length === 0) return;
  const text = visibleRows.value.map((row) => row.raw).join('\n');
  if (await copyText(text)) {
    message.success(`已复制 ${visibleRows.value.length} 行日志`);
  } else {
    message.error('复制失败：浏览器拒绝了剪贴板访问');
  }
}

// 用户改了过滤条件就回到顶部：要看的是"第一条命中"，而不是上次停留的位置。
// 不用 watch：切页签时程序性地清空关键字不应该把新来源的滚动位置也拽到顶部。
async function resetScrollToTop(): Promise<void> {
  await nextTick();
  if (linesEl.value) linesEl.value.scrollTop = 0;
  onLinesScroll();
}

function setKeyword(value: string): void {
  keyword.value = value;
  void resetScrollToTop();
}

function selectLevel(level: Level): void {
  levelFilter.value = level;
  void resetScrollToTop();
}

onMounted(() => {
  void loadSources();
  void fetchLog();
});

// 换主机：来源清单、具体内容、原来选中的文件全部作废。
watch(hostId, () => {
  log.value = null;
  sources.value = null;
  selectedFile.value = null;
  showNoise.value = false;
  atBottom.value = true;
  void loadSources();
  void fetchLog();
});

// 切页签：面板日志不依赖主机，切过去时只拉面板那份。
watch(tab, () => {
  keyword.value = '';
  levelFilter.value = '';
  showNoise.value = false;
  atBottom.value = true;
  void loadSources();
  void fetchLog();
});

watch([lines, journalPriority], () => {
  atBottom.value = true;
  void fetchLog();
});

/*
 * 文件选择单独一个 watch。
 *
 * 换主机时 loadSources 会重写 selectedFile；若它和行数共用一个 watch，
 * 那两个改动会各发一次请求。只有停在「日志文件」页签时才值得重取。
 */
watch(selectedFile, () => {
  if (tab.value !== 'file') return;
  atBottom.value = true;
  void fetchLog();
});

/*
 * 单元名是手输的，必须防抖。
 *
 * 直接 watch 会在每敲一个字符时发一次请求 —— 而这里的"一次请求"是一整条
 * SSH 会话（还带提权探测），敲完 nginx.service 就是十几次远端命令，
 * 目标机上会看到一串 journalctl -u n、-u ng… 的垃圾调用。
 */
const UNIT_DEBOUNCE_MS = 400;
let unitTimer: number | null = null;

watch(journalUnit, () => {
  if (unitTimer !== null) window.clearTimeout(unitTimer);
  unitTimer = window.setTimeout(() => {
    unitTimer = null;
    atBottom.value = true;
    void fetchLog();
  }, UNIT_DEBOUNCE_MS);
});

// 窗口关掉后防抖计时器还可能挂着，不清掉会在组件销毁后再发一次请求。
onBeforeUnmount(() => {
  if (unitTimer !== null) window.clearTimeout(unitTimer);
});

useVisiblePoll({
  root,
  intervalMs: POLL_MS,
  task: fetchLog,
  enabled: () => autoRefresh.value && canFetch.value,
});
</script>

<template>
  <div ref="root" class="logs">
    <div class="logs__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">LOGS</p>
        <span class="page-title">运行日志</span>
      </div>
      <span class="logs__spacer" />
      <div class="logs__actions">
        <n-select v-model:value="lines" class="logs__size" size="small" :options="LINE_OPTIONS" />
        <button
          type="button"
          class="logs__live"
          :class="{ 'is-paused': !autoRefresh }"
          :title="autoRefresh ? '暂停自动刷新' : '继续自动刷新'"
          @click="toggleAuto"
        >
          <span class="logs__live-dot" aria-hidden="true" />
          <span>{{ autoRefresh ? '实时' : '已暂停' }}</span>
          <span class="num logs__live-meta">{{ autoRefresh ? `${POLL_MS / 1000}s` : frozenLabel }}</span>
        </button>
        <n-button size="small" :loading="busy" @click="fetchLog">刷新</n-button>
      </div>
      <!-- 主机胶囊放页头右端：它和标题回答的是同一个问题（现在看的是哪台机器）。 -->
      <HostPicker v-model:host-id="hostId" v-model:host-name="hostName" include-local />
    </div>

    <n-tabs v-model:value="tab" type="line" size="small" class="logs__tabs">
      <n-tab-pane name="journal" tab="journalctl" :disabled="sources !== null && !sources.journal" />
      <n-tab-pane name="file" tab="日志文件" />
      <n-tab-pane name="dmesg" tab="dmesg" :disabled="sources !== null && !sources.dmesg" />
      <n-tab-pane name="panel" tab="面板日志" />
    </n-tabs>

    <div class="logs__source">
      <template v-if="tab === 'journal'">
        <n-input
          v-model:value="journalUnit"
          class="logs__unit"
          size="small"
          clearable
          placeholder="systemd 单元（可选），如 nginx.service"
          aria-label="systemd 单元"
        />
        <n-select
          v-model:value="journalPriority"
          class="logs__prio"
          size="small"
          :options="PRIORITY_OPTIONS"
          aria-label="日志级别"
        />
        <n-tag v-if="sources && !sources.journal" size="small" type="warning" :bordered="false">
          这台机器没有 journalctl
        </n-tag>
      </template>

      <template v-else-if="tab === 'file'">
        <n-select
          v-model:value="selectedFile"
          class="logs__filepick"
          size="small"
          :options="fileOptions"
          :loading="sourcesLoading"
          :disabled="(sources?.files.length ?? 0) === 0"
          :render-label="renderFileLabel"
          :render-option="renderFileOption"
          placeholder="选择日志文件"
          aria-label="选择日志文件"
        />
        <span class="logs__source-note">{{ fileSummary }}</span>
      </template>

      <p v-else class="logs__hint" :title="tab === 'dmesg' ? dmesgHint : panelHint">
        {{ tab === 'dmesg' ? dmesgHint : panelHint }}
      </p>

      <span v-if="tab === 'journal' || tab === 'file'" class="logs__spacer" />
      <span class="num logs__updated">更新 {{ fetchedLabel }}</span>
    </div>

    <p v-if="lastError" class="logs__error">{{ lastError }}</p>

    <n-alert v-if="note" type="warning" class="logs__alert">{{ note }}</n-alert>

    <div class="logs__content">
      <div class="logs__content-bar">
        <span class="silk">{{ isPanel ? '面板日志' : '日志内容' }}</span>
        <span
          v-if="!autoRefresh"
          class="logs__frozen"
          title="自动刷新已暂停，点上方「已暂停」可继续"
        >已冻结</span>
        <span v-if="truncated" class="logs__clip">输出过长，只取到开头一段</span>

        <span class="logs__spacer" />

        <button
          v-if="noiseRows.length > 0"
          type="button"
          class="logs__noise"
          :class="{ 'is-open': showNoise }"
          :title="showNoise ? '收起环境提示' : '展开环境提示'"
          @click="showNoise = !showNoise"
        >
          环境提示 {{ noiseRows.length }} 行
        </button>

        <div class="logs__levels" role="group" aria-label="按级别过滤">
          <button
            type="button"
            class="logs__level"
            :class="{ 'is-active': levelFilter === '' }"
            :aria-pressed="levelFilter === ''"
            @click="selectLevel('')"
          >
            全部
          </button>
          <button
            type="button"
            class="logs__level is-error"
            :class="{ 'is-active': levelFilter === 'error' }"
            :aria-pressed="levelFilter === 'error'"
            :disabled="errorCount === 0"
            @click="selectLevel('error')"
          >
            错误 <span class="num">{{ errorCount }}</span>
          </button>
          <button
            type="button"
            class="logs__level is-warn"
            :class="{ 'is-active': levelFilter === 'warn' }"
            :aria-pressed="levelFilter === 'warn'"
            :disabled="warnCount === 0"
            @click="selectLevel('warn')"
          >
            警告 <span class="num">{{ warnCount }}</span>
          </button>
        </div>

        <n-input
          :value="keyword"
          class="logs__search"
          size="small"
          clearable
          placeholder="过滤并高亮关键字"
          aria-label="过滤日志关键字"
          @update:value="setKeyword"
        />
        <span class="num logs__count">
          {{ renderedRows.length }} 行<template v-if="countWithTotal"> / {{ sourceLines.length }}</template>
        </span>
        <n-button size="tiny" quaternary :disabled="renderedRows.length === 0" @click="copyVisible">
          复制
        </n-button>
      </div>

      <div class="logs__body">
        <div
          ref="linesEl"
          class="logs__lines"
          :class="{ 'is-loading': busy }"
          @scroll.passive="onLinesScroll"
        >
          <div
            v-for="(row, i) in renderedRows"
            :key="i"
            class="logs__line"
            :class="row.level ? `is-${row.level}` : undefined"
          >
            <span class="logs__time num" :title="row.fullTime">{{ row.time }}</span>
            <span class="logs__src" :title="row.source">{{ row.source }}</span>
            <span class="logs__msg" v-html="row.html" />
          </div>
          <p v-if="renderedRows.length === 0" class="logs__empty">{{ emptyHint }}</p>
        </div>
        <button v-if="!atBottom" type="button" class="logs__jump" @click="scrollToBottom">
          回到最新
        </button>
      </div>
    </div>
  </div>
</template>
