<script setup lang="ts">
/**
 * 资源监控窗口：CPU / 内存 / 磁盘读写 / 网络的指标卡与挂载点列表。
 *
 * 进程表已拆到 ProcessWindow —— 一台忙机器上进程表有几百行九列，
 * 和指标卡挤在同一个窗口里时，两边都只剩半截可视区域。
 *
 * 两种打开方式：
 *   - 主机管理那一行点「监控」——带 hostId，直接采集；
 *   - 桌面「资源监控」图标——没有主机上下文，先让用户选一台。
 */
import { computed, onMounted, onUnmounted, ref, watch } from 'vue';
import { NProgress, NSelect, NSpace, NTag } from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import Sparkline from '../components/Sparkline.vue';
import { watchedHost } from '../desktop/watchedHost';
import {
  ApiError,
  api,
  metricsStreamURL,
  type Accelerator,
  type Metrics,
  type MetricsPoint,
  type MetricsStreamEvent,
} from '../api/client';
import { formatBytes, formatRate, formatUptime } from '../api/format';

const props = defineProps<{ hostId?: number; hostName?: string }>();

/**
 * 采样频率档位。
 *
 * 它现在不是"前端多久问一次"，而是"远端多久采一次"：档位随订阅消息发给服务端，
 * 由目标机上那条长驻采集循环按这个节奏输出。好处是采样间隔不再被 SSH 往返拉长 ——
 * 公网链路上 200ms 的档位在轮询模式下物理上不可能达成。
 *
 * 但 200ms 一轮仍是真实的采集开销（面板只把 df / nvidia-smi 那几条降到 30 秒一次），
 * 因此默认停在标准档，需要盯瞬时抖动时再手动切上去。
 */
interface SampleRate {
  value: string;
  label: string;
  intervalMs: number;
}

const RATES: SampleRate[] = [
  { value: 'fast', label: '高速（200ms）', intervalMs: 200 },
  { value: 'standard', label: '标准（1 秒）', intervalMs: 1000 },
  { value: 'slow', label: '低速（5 秒）', intervalMs: 5000 },
  { value: 'paused', label: '暂停', intervalMs: 0 },
];

/** 曲线窗口的点数；换算成时间随档位变化：高速档约 8 秒，标准档约 40 秒。 */
const HISTORY_SIZE = 40;

const sampleRate = ref('standard');
const rate = computed(() => RATES.find((item) => item.value === sampleRate.value) ?? RATES[1]);
const paused = computed(() => rate.value.intervalMs === 0);
/** 当前档位的采样间隔。暂停档不订阅流，这里沿用标准档的值供曲线判缺口。 */
const pollMs = computed(() => rate.value.intervalMs || 1000);

/**
 * 缺口判定阈值：当前采样间隔的 2.5 倍。
 *
 * 不取更紧的值：远端虽然按固定节奏输出，但网络传输与处理仍有抖动，
 * 实际相邻两条采样的间隔总会略大于标称周期，阈值太紧会把正常采样误判成缺口。
 * 阈值必须跟着档位走，否则高速档下几乎每个点都会被断开。
 */
const gapMs = computed(() => pollMs.value * 2.5);
// CPU% 上限固定 100，内存上限固定 100，网络与磁盘上限按当前窗口内的峰值自适应。
const PERCENT_MAX = 100;

/** 曲线配色：CPU 橙、内存青，与网络/磁盘卡片用的是同一组色值。 */
const CPU_COLOR = '#ffab2e';
const MEM_COLOR = '#45c7c0';

const root = ref<HTMLElement | null>(null);

// 窗口自己持有目标主机：带 hostId 打开时就是它，从桌面图标打开时由选择器填充。
const target = ref<number | null>(props.hostId ?? null);
/** 目标的显示名，由选择器回填；胶囊上显示的就是它。 */
const targetName = ref(props.hostName ?? '');

/**
 * 登记「当前被监控主机」，右上角状态胶囊据此决定显示谁的数据。
 *
 * 还没选目标时不动它：胶囊上留着的上一台机器的数字依然有效，清空只会让它
 * 退回"未选择"—— 而用户只是刚打开一个还没选主机的窗口。
 */
function claim(): void {
  const id = target.value;
  if (id === null) return;
  watchedHost.value = { id, name: targetName.value || `#${id}` };
}

// 名字可能比 id 晚一拍到位（选择器先改目标、再回填名字），所以两者都要盯着。
watch([target, targetName], () => claim(), { immediate: true });

watch(
  () => props.hostId,
  (id) => {
    if (id !== undefined) {
      target.value = id;
      targetName.value = props.hostName ?? '';
    }
  },
);

// 这里刻意不清理 watchedHost：胶囊是"用户选定的监控目标"，关掉窗口不代表
// 不该再盯着那台机器 —— 它的读数本来就是常驻着才有用的。

const metrics = ref<Metrics | null>(null);
const lastError = ref('');

/**
 * 一次采样：时刻 + 各指标的值。
 *
 * 用"采样点"而不是并行的数值数组，是因为几条曲线必须严格对齐同一时刻：
 * 速率类指标（CPU / 网络 / 磁盘）在首次采样时没有基准，旧实现靠"不入曲线"规避，
 * 代价是它们比内存、加速卡少一个点 —— 分开看卡片没事，叠到同一张图上就是整体错位。
 * 现在无可用值记 NaN，曲线在那里断开：既对齐，也不会画出不存在的谷底。
 */
interface Sample {
  at: number;
  cpu: number;
  mem: number;
  netRx: number;
  netTx: number;
  diskRead: number;
  diskWrite: number;
  /** 设备键 → 利用率；该时刻采集结果里没有这台设备时为 undefined（曲线断开）。 */
  accel: Record<string, number | undefined>;
}

/** 曲线窗口：40 点 × 3 秒 ≈ 2 分钟。 */
const samples = ref<Sample[]>([]);

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 504) return `${err.message}。请检查目标机负载与网络`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

function memPercentOf(data: Metrics): number {
  return data.memTotal > 0 ? (data.memUsed / data.memTotal) * 100 : 0;
}

/** 同一块加速设备在采样记录与曲线里的稳定键：种类 + 厂商编号 + 芯片号。 */
function accelKeyOf(kind: string, id: number, chip: number): string {
  return `${kind}-${id}-${chip}`;
}

/** 没有可用值一律记 NaN：曲线会在那里断开，而不是画成 0。 */
function usable(ready: boolean, value: number): number {
  return ready ? value : NaN;
}

/** 把一次实时采样转成采样点。 */
function toSample(data: Metrics): Sample {
  const ready = data.deltaReady;
  const accel: Record<string, number | undefined> = {};
  for (const item of data.accelerators) {
    // 工具没给利用率时是 -1（未知），不能当成 0% 空闲。
    accel[accelKeyOf(item.kind, item.id, item.chip)] =
      item.utilPercent >= 0 ? item.utilPercent : NaN;
  }

  return {
    at: Date.parse(data.sampledAt) || Date.now(),
    cpu: usable(ready, data.cpuPercent),
    mem: memPercentOf(data),
    netRx: usable(ready, data.netRxRate),
    netTx: usable(ready, data.netTxRate),
    diskRead: usable(ready, data.diskReadRate),
    diskWrite: usable(ready, data.diskWriteRate),
    accel,
  };
}

/** 把服务端历史点转成采样点，口径与实时采样一致。 */
function fromPoint(point: MetricsPoint): Sample {
  const accel: Record<string, number | undefined> = {};
  for (const item of point.accel) {
    accel[accelKeyOf(item.kind, item.id, item.chip)] =
      item.utilPercent >= 0 ? item.utilPercent : NaN;
  }

  return {
    at: point.at,
    cpu: usable(point.deltaReady, point.cpuPercent),
    mem: point.memPercent,
    netRx: usable(point.deltaReady, point.netRxRate),
    netTx: usable(point.deltaReady, point.netTxRate),
    diskRead: usable(point.deltaReady, point.diskReadRate),
    diskWrite: usable(point.deltaReady, point.diskWriteRate),
    accel,
  };
}

/**
 * 用服务端保留的历史采样回填曲线。
 *
 * 窗口打开的那一刻如果从空图开始，要等满两分钟才有趋势可看；而服务端本来就
 * 留着最近几十个点（谁在采集不重要，看的是同一台机器）。
 *
 * 失败静默：回填只是锦上添花，拿不到就从空图开始，不该因此挡住实时数据。
 */
async function backfill(id: number): Promise<void> {
  let points: MetricsPoint[] = [];
  try {
    points = (await api.metrics.history(id)).points;
  } catch {
    return;
  }
  // 期间换了主机：这份历史属于上一台机器，必须丢弃，否则会把它的曲线接过来。
  if (target.value !== id) return;

  const filled = points.slice(-HISTORY_SIZE).map(fromPoint);
  // 回填是异步的，期间可能已经收到实时采样点：按时间取新的，别把它们丢掉。
  const newest = filled.length > 0 ? filled[filled.length - 1].at : 0;
  const live = samples.value.filter((sample) => sample.at > newest);
  const merged = [...filled, ...live];
  samples.value = merged.length > HISTORY_SIZE ? merged.slice(merged.length - HISTORY_SIZE) : merged;
}

function appendSample(sample: Sample): void {
  const next = [...samples.value, sample];
  samples.value = next.length > HISTORY_SIZE ? next.slice(next.length - HISTORY_SIZE) : next;
}

/**
 * 取一次当前值，让开窗那一刻立刻有数可显示。
 *
 * 只在开窗与切主机时调用：之后的值由订阅来的长驻流持续推送。
 * 走缓存而非 fresh —— 流本身在持续写这份缓存，那里面的点比再跑一次采集更新。
 */
async function refreshMetrics(): Promise<void> {
  const id = target.value;
  if (id === null) return;

  try {
    const data = await api.metrics.get(id, false);
    metrics.value = data;
    lastError.value = '';
    // 采样点自己带着时间戳与"哪些指标这一刻不可用"，曲线据此对齐与断线。
    appendSample(toSample(data));
  } catch (err) {
    lastError.value = describe(err, '采集指标失败');
  }
}

// ---------- 长驻流订阅 ----------

/** 当前的长驻流连接。用普通变量而非 ref：它的变化不需要驱动渲染。 */
let socket: WebSocket | null = null;

/**
 * 订阅的开关由"窗口是否真的在看"决定。
 *
 * 这与轮询时代的 useVisiblePoll 是同一个判断，目的却不同：那时是为了少发请求，
 * 现在是为了让服务端把流停掉 —— 没人看的常驻 SSH 会话是纯粹的浪费。
 *
 * 窗口最小化是 v-show 的结果，没有对应事件，只能低频检查。这个检查本身
 * 不产生任何网络请求，与之前"按档位去目标机上跑命令"完全不是一回事。
 */
const VISIBILITY_CHECK_MS = 1000;
let visibilityTimer: number | undefined;

function windowVisible(): boolean {
  if (document.hidden) return false;
  const el = root.value;
  return el ? el.offsetParent !== null : false;
}

/** 该不该有订阅：窗口可见、选了目标、且不在暂停档。 */
function wantsStream(): boolean {
  return windowVisible() && target.value !== null && !paused.value;
}

function syncStream(): void {
  if (wantsStream()) {
    if (!socket) openStream();
  } else {
    closeStream();
  }
}

/**
 * 建立长驻流。
 *
 * 旧连接的回调必须整体失效：重连是"先 close 再新建"，而 close 事件要到下一轮
 * 事件循环才派发 —— 那时 socket 已指向新连接，旧连接的 onclose 会把新连接
 * 一并改写掉（TerminalSession 踩过同一个坑）。
 */
function openStream(): void {
  const id = target.value;
  if (id === null) return;

  const ws = new WebSocket(metricsStreamURL());
  socket = ws;
  const isCurrent = (): boolean => socket === ws;

  ws.onopen = () => {
    ws.send(JSON.stringify({ type: 'start', hostId: id, intervalMs: rate.value.intervalMs }));
  };

  ws.onmessage = (event: MessageEvent<string>) => {
    if (!isCurrent()) return;

    let payload: MetricsStreamEvent;
    try {
      payload = JSON.parse(event.data) as MetricsStreamEvent;
    } catch {
      return;
    }

    // 服务端自己会重连目标机，这里只把状态显示出来，不打断用户。
    if (payload.type === 'error') {
      lastError.value = payload.error ?? '指标流暂时不可用';
      return;
    }
    if (payload.metrics) {
      metrics.value = payload.metrics;
      lastError.value = '';
      appendSample(toSample(payload.metrics));
    }
  };

  ws.onclose = () => {
    // 只处理"这一条连接结束了"，不在这里立刻重连：连不上时的紧循环只会打满日志。
    // 下一次可见性检查（最多一秒后）会重新建立。
    if (isCurrent()) socket = null;
  };
}

function closeStream(): void {
  const ws = socket;
  socket = null;
  if (ws) ws.close();
}

// 档位变了，服务端那条流的节奏就作废了：重建订阅让新的间隔生效。
watch(sampleRate, () => {
  closeStream();
  syncStream();
});

// immediate：窗口带着主机打开时也要回填，否则开窗那一刻只有"从零开始的实时值"。
watch(
  target,
  (id) => {
    if (id === null) return;
    // 换主机后旧曲线与新机器无关，必须清空，否则图上会连出一段不存在的趋势。
    samples.value = [];
    metrics.value = null;
    hoverIndex.value = -1;
    // 换主机同样要重建流：服务端的流是按主机复用的，旧流收不到新机器的数据。
    closeStream();
    // 先回填历史、再取实时值：backfill 内部不会覆盖回填期间已到达的实时点。
    void backfill(id).then(() => refreshMetrics());
    syncStream();
  },
  { immediate: true },
);

onMounted(() => {
  // 首次订阅要等挂载之后：root 还没挂载时 offsetParent 是 null，会被判成不可见。
  syncStream();
  visibilityTimer = window.setInterval(syncStream, VISIBILITY_CHECK_MS);
  document.addEventListener('visibilitychange', syncStream);
});

// 关窗即退订：服务端在最后一个订阅者离开时会停掉那条长驻采集流。
onUnmounted(() => {
  if (visibilityTimer !== undefined) window.clearInterval(visibilityTimer);
  document.removeEventListener('visibilitychange', syncStream);
  closeStream();
});

/** 当前 CPU%；没有基准时是 NaN，界面显示「—」而不是一个假的 0%。 */
const cpuPercent = computed(() => (metrics.value?.deltaReady ? metrics.value.cpuPercent : NaN));
const memPercent = computed(() => (metrics.value ? memPercentOf(metrics.value) : 0));
const swapPercent = computed(() => {
  const m = metrics.value;
  return m && m.swapTotal > 0 ? ((m.swapTotal - m.swapFree) / m.swapTotal) * 100 : 0;
});

/**
 * 顶部指标条的四张卡。
 *
 * 它们回答的是"这台机器现在什么水位"，数字必须取当前值而不是曲线均值 ——
 * 均值已经交给下面的图，重复一遍反而让人不知道该信哪个。
 * 取不到时给「—」，不编造 0。
 */
const statCards = computed(() => {
  const m = metrics.value;
  if (!m) return [];

  const disks = m.disks ?? [];
  // 容量按"最满的那一块"报：磁盘告警看的从来不是平均值。
  const worst = disks.reduce<(typeof disks)[number] | null>(
    (acc, disk) => (acc === null || disk.usePercent > acc.usePercent ? disk : acc),
    null,
  );

  return [
    {
      key: 'cpu',
      label: 'CPU',
      value: Number.isFinite(cpuPercent.value) ? cpuPercent.value.toFixed(0) : '—',
      unit: '%',
      foot: `${m.cpuCores} 核 · 负载 ${m.load1.toFixed(2)} / ${m.load5.toFixed(2)} / ${m.load15.toFixed(2)}`,
      text: false,
    },
    {
      key: 'mem',
      label: 'MEMORY',
      value: memPercent.value.toFixed(0),
      unit: '%',
      foot: `${formatBytes(m.memUsed)} / ${formatBytes(m.memTotal)}`,
      text: false,
    },
    {
      key: 'disk',
      label: 'DISK',
      value: worst ? worst.usePercent.toFixed(0) : '—',
      unit: worst ? '%' : '',
      foot: worst
        ? `${worst.mountPoint} · ${formatBytes(worst.used)} / ${formatBytes(worst.total)}`
        : '暂无挂载点信息',
      text: false,
    },
    {
      key: 'net',
      label: 'NETWORK',
      value: formatRate((m.netRxRate ?? 0) + (m.netTxRate ?? 0)),
      unit: '',
      foot: `下行 ${formatRate(m.netRxRate ?? 0)} · 上行 ${formatRate(m.netTxRate ?? 0)}`,
      // 带宽读数带单位（"1.8 MB/s"），26px 会撑破卡片，降一档字号。
      text: true,
    },
  ];
});

/** 每个采样点对应的时刻，交给曲线判断时间缺口。 */
const sampleTimes = computed(() => samples.value.map((s) => s.at));
const rxValues = computed(() => samples.value.map((s) => s.netRx));
const txValues = computed(() => samples.value.map((s) => s.netTx));
const diskReadValues = computed(() => samples.value.map((s) => s.diskRead));
const diskWriteValues = computed(() => samples.value.map((s) => s.diskWrite));

/**
 * 速率曲线的纵轴上限：按当前窗口内的峰值自适应。
 *
 * 必须先把 NaN（没有基准的采样点）滤掉 —— Math.max 只要收到一个 NaN，
 * 结果就整体变成 NaN，曲线会直接消失。
 */
function peakOf(fallback: number, lists: number[][]): number {
  const values = lists.flat().filter((value) => Number.isFinite(value));
  return Math.max(fallback, ...values) * 1.25;
}

const netMax = computed(() => peakOf(1024, [rxValues.value, txValues.value]));
const diskMax = computed(() => peakOf(1024, [diskReadValues.value, diskWriteValues.value]));

// ---------- 合并曲线（CPU / 内存 / 加速卡） ----------

/** 悬停指向的采样下标；-1 表示指针不在图上。 */
const hoverIndex = ref(-1);

/**
 * 合并图里的序列。
 *
 * 只有百分比量纲的指标才进得来：网络与磁盘是速率（B/s），和 0–100% 共用
 * 一个纵轴会把它们压成贴底的直线 —— 那两张卡留在右边各自用自适应上限。
 */
const mergedSeries = computed(() => {
  const times = sampleTimes.value;
  // name 只出现在悬停浮层里：浮层离上方的图例较远，光靠颜色对不回是哪一条。
  const series = [
    { values: samples.value.map((s) => s.cpu), color: CPU_COLOR, times, name: 'CPU' },
    { values: samples.value.map((s) => s.mem), color: MEM_COLOR, times, name: '内存' },
  ];
  for (const accel of metrics.value?.accelerators ?? []) {
    const key = accelKey(accel);
    // 该时刻采集结果里没有这台设备（例如驱动重载期间）就是 undefined → NaN，
    // 曲线在那里断开，而不是从"消失前"直接连到"回来之后"。
    series.push({
      values: samples.value.map((s) => s.accel[key] ?? NaN),
      color: accelColor(accel),
      times,
      name: accelLabel(accel),
    });
  }
  return series;
});

/** 取某个采样点上某个指标的值；没有可用值时返回 null（界面显示「—」）。 */
function sampleValueAt(index: number, pick: (s: Sample) => number | undefined): number | null {
  const sample = samples.value[index];
  if (!sample) return null;
  const value = pick(sample);
  return value === undefined || !Number.isFinite(value) ? null : value;
}

/** 图例：名称、颜色与数值（悬停时是那一刻的值，否则是当前值）。 */
const legendItems = computed(() => {
  const hovering = hoverIndex.value >= 0;
  const pick = (get: (s: Sample) => number | undefined, current: number): number | null =>
    hovering ? sampleValueAt(hoverIndex.value, get) : current;

  const items = [
    { key: 'cpu', name: 'CPU', color: CPU_COLOR, value: pick((s) => s.cpu, cpuPercent.value) },
    { key: 'mem', name: '内存', color: MEM_COLOR, value: pick((s) => s.mem, memPercent.value) },
  ];
  for (const accel of metrics.value?.accelerators ?? []) {
    const key = accelKey(accel);
    items.push({
      key,
      name: accelLabel(accel),
      color: accelColor(accel),
      value: pick((s) => s.accel[key], accel.utilPercent),
    });
  }
  return items;
});

/** 没有可用值（未知、或那一刻没采到）统一显示「—」。 */
function legendText(value: number | null | undefined): string {
  if (value === null || value === undefined || !Number.isFinite(value) || value < 0) return '—';
  return `${value.toFixed(1)}%`;
}

function formatAgo(milliseconds: number): string {
  const seconds = Math.max(0, Math.round(milliseconds / 1000));
  if (seconds < 90) return `${seconds} 秒前`;
  const minutes = Math.round(seconds / 60);
  return minutes < 90 ? `${minutes} 分钟前` : `${Math.round(minutes / 60)} 小时前`;
}

/**
 * 悬停时刻距现在多久。
 *
 * 用采样点自带的时间戳算真实差值，而不是"下标 × 采样周期"估算：曲线中间
 * 可能有缺口（窗口被最小化、采集失败），按下标推算会把十分钟前的数据
 * 报成"12 秒前"。
 */
const hoverAgo = computed(() => {
  const sample = samples.value[hoverIndex.value];
  return sample ? `≈ ${formatAgo(Date.now() - sample.at)}` : '';
});

/** 合并卡脚注：原来三张卡的明细并成一行。 */
const mergedFoot = computed(() => {
  const m = metrics.value;
  if (!m) return '等待采样…';

  const parts = [
    `${m.cpuCores} 核 · 负载 ${m.load1.toFixed(2)} / ${m.load5.toFixed(2)} / ${m.load15.toFixed(2)}`,
    `内存 ${formatBytes(m.memUsed)} / ${formatBytes(m.memTotal)}`,
  ];
  if (m.swapTotal > 0) parts.push(`交换分区 ${swapPercent.value.toFixed(0)}%`);

  for (const accel of m.accelerators ?? []) {
    const mem =
      accel.memTotal > 0 ? ` · 显存 ${formatBytes(accel.memUsed)} / ${formatBytes(accel.memTotal)}` : '';
    const temp = accel.tempC >= 0 ? ` · ${accel.tempC.toFixed(0)}°C` : '';
    parts.push(`${accelLabel(accel)} ${accel.name || '未知设备'}${mem}${temp}`);
  }
  return parts.join(' · ');
});

function onHover(index: number): void {
  hoverIndex.value = index;
}

const ledClass = computed(() => {
  if (lastError.value) return 'led--danger';
  if (target.value === null) return 'led--sig led--live';
  return metrics.value ? 'led--ok' : 'led--sig led--live';
});

// ---------- GPU / NPU ----------

/** 同一块加速设备在曲线表里的稳定键。 */
function accelKey(accel: Accelerator): string {
  return accelKeyOf(accel.kind, accel.id, accel.chip);
}

/** 卡片标题：GPU 0 / NPU 1；多核模块额外带上芯片号（NPU 0:1）。 */
function accelLabel(accel: Accelerator): string {
  const prefix = accel.kind === 'gpu' ? 'GPU' : 'NPU';
  return accel.chip > 0 ? `${prefix} ${accel.id}:${accel.chip}` : `${prefix} ${accel.id}`;
}

/** 紫色给 GPU、绿色给 NPU，和 CPU 橙 / 内存青两色拉开距离。 */
function accelColor(accel: Accelerator): string {
  return accel.kind === 'gpu' ? '#a78bfa' : '#56d68b';
}
</script>

<template>
  <div ref="root" class="monitor">
    <div class="monitor__bar page-bar">
      <div class="monitor__identity">
        <p class="page-eyebrow">TELEMETRY</p>
        <span class="monitor__name">
          <span class="led" :class="ledClass" aria-hidden="true" />
          {{ metrics?.hostname || hostName || '资源监控' }}
        </span>
        <span class="monitor__sub">
          内核 {{ metrics?.kernel || '—' }} · 已运行 {{ formatUptime(metrics?.uptimeSeconds ?? 0) }} ·
          {{ metrics?.cpuModel || '未知 CPU' }}
        </span>
      </div>
      <n-space size="small">
        <!-- 采样频率是"这一屏看的分辨率"，主机胶囊回答的是"看的是哪台机器"，
             后者放在最右端，和其他窗口的页头保持一致。 -->
        <n-select
          v-model:value="sampleRate"
          size="small"
          class="monitor__rate"
          :options="RATES.map((item) => ({ label: item.label, value: item.value }))"
        />
        <n-tag v-if="paused" size="small" type="warning">采样已暂停</n-tag>
        <host-picker
          v-if="hostId === undefined"
          v-model:host-id="target"
          v-model:host-name="targetName"
          include-local
        />
      </n-space>
    </div>

    <div v-if="lastError" class="monitor__error">
      <span class="led led--danger" aria-hidden="true" />
      {{ lastError }}
    </div>

    <!-- 指标条：一屏先回答"现在什么水位"，曲线负责"怎么走到这里的"。 -->
    <div v-if="statCards.length > 0" class="ui-stats">
      <article v-for="card in statCards" :key="card.key" class="ui-card ui-stat">
        <span class="ui-stat__label">{{ card.label }}</span>
        <b class="ui-stat__value" :class="{ 'is-text': card.text }">
          {{ card.value }}<span v-if="card.unit">{{ card.unit }}</span>
        </b>
        <span class="ui-stat__foot">{{ card.foot }}</span>
      </article>
    </div>

    <div class="monitor__cards">
      <!--
        CPU / 内存 / 加速卡合并成一张图：三者都是 0–100% 的百分比量纲，叠在一起
        才看得出"谁在吃资源"的相互关系。下面两张各占一半，留给网络与磁盘。
      -->
      <article class="ui-card metric metric--merged">
        <div class="ui-card__head">
          <div class="ui-card__ident">
            <p class="ui-card__eyebrow">TELEMETRY</p>
            <h2 class="ui-card__title">{{ hoverIndex >= 0 ? hoverAgo : 'CPU / GPU / 内存' }}</h2>
          </div>
          <span class="ui-card__meta">{{ paused ? '采样已暂停' : '实时' }}</span>
        </div>
        <Sparkline
          :series="mergedSeries"
          :max="PERCENT_MAX"
          :gap-ms="gapMs"
          grid
          axes
          readout
          unit="%"
          @hover="onHover"
        />
        <div class="ui-legend">
          <span
            v-for="item in legendItems"
            :key="item.key"
            :style="{ color: item.color }"
          >
            <i :style="{ background: item.color }" aria-hidden="true" />
            {{ item.name }} {{ legendText(item.value) }}
          </span>
        </div>
        <div class="ui-card__foot">{{ mergedFoot }}</div>
      </article>

      <article class="ui-card metric">
        <div class="ui-card__head">
          <div class="ui-card__ident">
            <p class="ui-card__eyebrow">THROUGHPUT</p>
            <h2 class="ui-card__title">网络</h2>
          </div>
          <span class="ui-card__meta">
            {{ formatRate((metrics?.netRxRate ?? 0) + (metrics?.netTxRate ?? 0)) }}
          </span>
        </div>
        <div class="metric__netpair">
          <span class="metric__netleg is-rx">下行 {{ formatRate(metrics?.netRxRate ?? 0) }}</span>
          <span class="metric__netleg is-tx">上行 {{ formatRate(metrics?.netTxRate ?? 0) }}</span>
        </div>
        <Sparkline :values="rxValues" :times="sampleTimes" :max="netMax" :gap-ms="gapMs" :color="MEM_COLOR" />
        <Sparkline :values="txValues" :times="sampleTimes" :max="netMax" :gap-ms="gapMs" :color="CPU_COLOR" />
        <div class="ui-card__foot">
          <span>累计下行 {{ formatBytes(metrics?.netRxBytes ?? 0) }}</span>
          <span>累计上行 {{ formatBytes(metrics?.netTxBytes ?? 0) }}</span>
        </div>
      </article>

      <article class="ui-card metric">
        <div class="ui-card__head">
          <div class="ui-card__ident">
            <p class="ui-card__eyebrow">STORAGE I/O</p>
            <h2 class="ui-card__title">磁盘读写</h2>
          </div>
          <span class="ui-card__meta">
            {{ formatRate((metrics?.diskReadRate ?? 0) + (metrics?.diskWriteRate ?? 0)) }}
          </span>
        </div>
        <div class="metric__duo">
          <span class="metric__duoleg is-read">读取 {{ formatRate(metrics?.diskReadRate ?? 0) }}</span>
          <span class="metric__duoleg is-write">写入 {{ formatRate(metrics?.diskWriteRate ?? 0) }}</span>
        </div>
        <Sparkline
          :values="diskReadValues"
          :times="sampleTimes"
          :max="diskMax"
          :gap-ms="gapMs"
          :color="MEM_COLOR"
        />
        <Sparkline
          :values="diskWriteValues"
          :times="sampleTimes"
          :max="diskMax"
          :gap-ms="gapMs"
          :color="CPU_COLOR"
        />
        <div class="ui-card__foot">
          <span>累计读取 {{ formatBytes(metrics?.diskReadBytes ?? 0) }}</span>
          <span>累计写入 {{ formatBytes(metrics?.diskWriteBytes ?? 0) }}</span>
        </div>
      </article>
    </div>

    <article class="ui-card monitor__disks">
      <div class="ui-card__head">
        <div class="ui-card__ident">
          <p class="ui-card__eyebrow">VOLUMES</p>
          <h2 class="ui-card__title">磁盘挂载点</h2>
        </div>
        <span class="ui-card__meta">{{ metrics?.disks.length ?? 0 }} 个</span>
      </div>
      <div class="disks">
        <div v-for="disk in metrics?.disks ?? []" :key="disk.mountPoint" class="disks__row">
          <span class="disks__mount" :title="disk.filesystem">{{ disk.mountPoint }}</span>
          <n-progress
            class="disks__bar"
            type="line"
            :percentage="disk.usePercent"
            :show-indicator="false"
            :height="6"
            :border-radius="3"
            :status="disk.usePercent >= 90 ? 'error' : disk.usePercent >= 75 ? 'warning' : 'default'"
          />
          <span class="disks__num">{{ formatBytes(disk.used) }} / {{ formatBytes(disk.total) }}</span>
          <span class="disks__pct">{{ disk.usePercent.toFixed(0) }}%</span>
        </div>
        <p v-if="(metrics?.disks.length ?? 0) === 0" class="disks__empty">暂无挂载点信息</p>
      </div>
    </article>
  </div>
</template>
