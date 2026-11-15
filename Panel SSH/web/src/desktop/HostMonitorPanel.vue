<script setup lang="ts">
/**
 * 主机监控面板：右上角胶囊展开后列出的全部监控目标及其实时指标。
 *
 * 为什么要有它：胶囊本身只能同时显示一台主机的数字，而"现在到底哪台机器有压力"
 * 恰恰需要横向对比 —— 挨个打开监控窗口再看一遍不是个可用的做法。
 *
 * 列表的第一项固定是「宿主机」（面板自己所在的机器）。它由后端本地直采，不占用
 * 主机列表里的一行，也不需要 SSH 凭据 —— 但用户关心的"这台面板还健康吗"与其他
 * 主机是同一件事，所以放在一起比。远端主机排在它后面。
 *
 * 卡片形态而非表格行：每台要展示的是"CPU、内存、网络、磁盘"四组读数，
 * 表格行摊不开这些维度（只能再堆列，横向越挤越难扫）。改成每台一张卡后，
 * 圆环承担百分比、IO 区承担速率，一台机器的状况在一眼之内读完。
 *
 * 刷新策略上比单台监控窗口更克制：N 台主机就是每轮 N 条远端采集命令（每条都要
 * 在目标机上读一遍 /proc 并执行 df），所以间隔取 5 秒而不是窗口的 3 秒。
 * 服务端按主机做了去重与短时缓存，已经开着监控窗口的那台会直接复用同一份采样。
 *
 * 组件只在面板展开时挂载（外层用 v-if），所以定时器的生命周期就是组件的生命周期，
 * 不存在「收起之后还在后台轮询」这回事。
 */
import { computed, onMounted, ref } from 'vue';

import { LOCAL_HOST_ID, LOCAL_HOST_NAME, type WatchedHost } from './watchedHost';
import { useVisiblePoll } from './useVisiblePoll';
import { ApiError, api, type Host, type Metrics } from '../api/client';
import { formatRate, formatUptime } from '../api/format';

const emit = defineEmits<{
  (e: 'pick', target: WatchedHost): void;
  (e: 'close'): void;
}>();

const POLL_MS = 5000;

// 圆环几何：r=16 时周长约 100.5。
// 用 stroke-dasharray + dashoffset 画进度，比 SVG arc path 少一半代码且不用算弧线端点。
const RING_R = 16;
const RING_C = 2 * Math.PI * RING_R;

/** 卡片上的一个监控目标：宿主机，或一台远端主机。 */
interface Target {
  id: number;
  name: string;
  /** 名字后面的小字：远端是登录目标；宿主机没有地址和登录用户，留空。 */
  subtitle: string;
}

interface Row {
  metrics: Metrics | null;
  error: string;
}

const root = ref<HTMLElement | null>(null);
const hosts = ref<Host[]>([]);
const rows = ref<Record<number, Row>>({});
const listError = ref('');
const loadingList = ref(false);
/** 首轮采集是否已完成，用于避免挂载瞬间重复采一遍。 */
const booted = ref(false);

/** 宿主机永远排在第一项；它是内置目标，与主机列表的加载结果无关。 */
const targets = computed<Target[]>(() => [
  { id: LOCAL_HOST_ID, name: LOCAL_HOST_NAME, subtitle: '' },
  ...hosts.value.map((host) => ({
    id: host.id,
    name: host.name,
    subtitle: `${host.username}@${host.addr}:${host.port}`,
  })),
]);

function describe(err: unknown): string {
  if (err instanceof ApiError) return err.status === 401 ? '会话已过期' : err.message;
  return '采集失败';
}

async function loadHosts(): Promise<void> {
  loadingList.value = true;
  try {
    hosts.value = await api.hosts.list();
    listError.value = '';
  } catch (err) {
    listError.value = describe(err);
  } finally {
    loadingList.value = false;
  }
}

/**
 * 并发采集全部目标的指标，结果逐台落位。
 *
 * 每台的失败各自吞掉：一台机器连不上不该让整个面板变成一块错误提示，
 * 其余主机的数字仍然是有用的 —— 那台机器自身会显示为「离线」。
 *
 * 不能等全部返回再一起渲染：离线主机的 TCP 连接要耗到超时（约 20 秒），
 * 而这期间宿主机明明已经采到了数据，却会跟着一起显示成「—」。
 * 因此谁先回来谁先显示，慢的那台只影响它自己那张卡。
 */
const inflight = new Set<number>();

async function refreshMetrics(): Promise<void> {
  await Promise.all(
    targets.value.map(async (target): Promise<void> => {
      // 上一轮还没回来就跳过：轮询是 5 秒一次，而离线主机要 20 秒才超时，
      // 不挡的话同一台机器会叠出四个并发拨号。
      if (inflight.has(target.id)) return;
      inflight.add(target.id);
      try {
        const metrics = await api.metrics.get(target.id);
        rows.value = { ...rows.value, [target.id]: { metrics, error: '' } };
      } catch (err) {
        rows.value = { ...rows.value, [target.id]: { metrics: null, error: describe(err) } };
      } finally {
        inflight.delete(target.id);
      }
    }),
  );
}

async function boot(): Promise<void> {
  // 主机列表可能刚被别处改过（新增/删除），每次展开都重新取一遍。
  await loadHosts();
  await refreshMetrics();
  booted.value = true;
}

// 组件是在面板展开那一刻才创建的，所以挂载即加载 ——
// 不必再让外层传一个「你现在可见了吗」的开关进来。
onMounted(() => void boot());

useVisiblePoll({
  root,
  intervalMs: POLL_MS,
  task: () => refreshMetrics(),
  // 首轮采集由 boot 负责：它要等主机列表到手，一次把所有目标采完，
  // 比"挂载瞬间只采宿主机、5 秒后再采一遍"少一轮请求。
  enabled: () => booted.value,
});

function rowOf(target: Target): Row | null {
  return rows.value[target.id] ?? null;
}

/**
 * 圆环用的百分比数值，无数据时为 null。
 *
 * 与文本显示共用一个函数：圆环的角度和中心的数字必须来自同一个值，
 * 分成两份实现迟早会出现"环画到 60% 而数字写 61%"。
 */
function cpuPct(target: Target): number | null {
  const m = rowOf(target)?.metrics;
  // 首次采样没有计数器基准，此时不能画成 0% —— 那会被读成"机器空闲"。
  if (!m || !m.deltaReady) return null;
  return Math.min(100, Math.max(0, m.cpuPercent));
}

function memPct(target: Target): number | null {
  const m = rowOf(target)?.metrics;
  if (!m || m.memTotal <= 0) return null;
  return Math.min(100, Math.max(0, (m.memUsed / m.memTotal) * 100));
}

/** 圆环中心与 aria-label 共用的文案。 */
function ringText(pct: number | null): string {
  return pct === null ? '—' : `${pct.toFixed(0)}%`;
}

function uptimeText(target: Target): string {
  const m = rowOf(target)?.metrics;
  if (!m) return '';
  return `运行 ${formatUptime(m.uptimeSeconds)}`;
}

/** 统一取速率，缺失时给占位符而不是 0 —— 0 会被当成"当前没有流量"。 */
function rateOf(target: Target, kind: 'rx' | 'tx' | 'read' | 'write'): string {
  const m = rowOf(target)?.metrics;
  if (!m) return '—';
  if (kind === 'rx') return formatRate(m.netRxRate);
  if (kind === 'tx') return formatRate(m.netTxRate);
  if (kind === 'read') return formatRate(m.diskReadRate);
  return formatRate(m.diskWriteRate);
}

function ledClass(target: Target): string {
  const row = rowOf(target);
  if (!row) return 'led--sig led--live';
  if (row.error) return 'led--danger';
  if (!row.metrics) return 'led--sig led--live';
  return 'led--ok';
}

/**
 * 整卡的可访问名。
 *
 * 必须把读数写进名字里，而不是给卡片内部的元素挂 aria-label：
 * 卡片整体是一个 <button>，而按钮的子元素在可访问性树中是 presentational 的
 * （内容一律并入按钮自己的名字）。内部再怎么标 role="img" 都会被忽略 ——
 * 那样写出来的"无障碍标签"是自我安慰，屏幕阅读器一个字都读不到。
 */
function cardLabel(target: Target): string {
  const row = rowOf(target);
  if (!row) return `${target.name}，正在连接`;
  if (row.error) return `${target.name}，采集失败：${row.error}`;
  return (
    `${target.name}，CPU ${ringText(cpuPct(target))}，内存 ${ringText(memPct(target))}，` +
    `上行 ${rateOf(target, 'tx')}，下行 ${rateOf(target, 'rx')}，` +
    `读取 ${rateOf(target, 'read')}，写入 ${rateOf(target, 'write')}。打开资源监控`
  );
}

/** 卡片底部的失败原因。 */
function noteOf(target: Target): string {
  return rowOf(target)?.error ?? '';
}

const okCount = computed(() => targets.value.filter((t) => rowOf(t)?.metrics).length);
</script>

<template>
  <div ref="root" class="hostpanel" role="dialog" aria-label="主机监控列表">
    <!-- 这一条就是展开态下的胶囊：它替代了收起时的 statuspill__main，
         所以高度取得与胶囊一致、顶部沿用胶囊的圆角，展开前后那一行不跳位。 -->
    <header class="hostpanel__head">
      <span class="hostpanel__title">主机监控</span>
      <span class="hostpanel__count num">{{ okCount }} / {{ targets.length }} 台在线</span>
      <!-- 收起而不是刷新：面板每 5 秒自采一轮，展开那一刻也刚采过，没有"该刷新却没刷"的时候。
           而「收起」是这一行唯一还缺的出口 —— 原来的收起入口是胶囊按钮，展开后它已经不在了。 -->
      <button class="hostpanel__close" aria-label="收起主机监控面板" title="收起" @click="emit('close')">
        <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
          <path
            d="M2.5 6.5L5 4l2.5 2.5"
            fill="none"
            stroke="currentColor"
            stroke-width="1.4"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </button>
    </header>

    <div v-if="listError" class="hostpanel__error">{{ listError }}</div>

    <ul v-else class="hostpanel__list">
      <li v-for="target in targets" :key="target.id">
        <!--
          整卡是一个按钮：点哪都能开该目标的资源监控。
          卡内内容整体 aria-hidden —— 按钮的子元素在可访问性树里本就是
          presentational 的，显式标出来只是让"名字由 aria-label 承担"这件事
          在代码里可见，避免后来者又往卡内元素上挂无效的 aria 属性。
        -->
        <button class="hostcard" :aria-label="cardLabel(target)" @click="emit('pick', target)">
          <header class="hostcard__head" aria-hidden="true">
            <span class="led" :class="ledClass(target)" />
            <span class="hostcard__name">{{ target.name }}</span>
            <span v-if="target.subtitle" class="hostcard__addr">{{ target.subtitle }}</span>
            <span class="hostcard__uptime num">{{ uptimeText(target) }}</span>
          </header>

          <div class="hostcard__body" aria-hidden="true">
            <!-- 圆环：dasharray + dashoffset 画进度，起点已由 CSS 转到 12 点方向。
                 标签放在环外（下方），不能叠在环上——环的描边是亮色，
                 文字压上去会与其同色系，对比度实测只有 1.1:1。 -->
            <div class="ring">
              <div class="ring__dial">
                <svg class="ring__svg" viewBox="0 0 40 40">
                  <circle class="ring__track" cx="20" cy="20" :r="RING_R" />
                  <circle
                    class="ring__fill is-cpu"
                    cx="20"
                    cy="20"
                    :r="RING_R"
                    :stroke-dasharray="RING_C"
                    :stroke-dashoffset="RING_C * (1 - (cpuPct(target) ?? 0) / 100)"
                  />
                </svg>
                <span class="ring__value num">{{ ringText(cpuPct(target)) }}</span>
              </div>
              <span class="ring__label">CPU</span>
            </div>

            <div class="ring">
              <div class="ring__dial">
                <svg class="ring__svg" viewBox="0 0 40 40">
                  <circle class="ring__track" cx="20" cy="20" :r="RING_R" />
                  <circle
                    class="ring__fill is-mem"
                    cx="20"
                    cy="20"
                    :r="RING_R"
                    :stroke-dasharray="RING_C"
                    :stroke-dashoffset="RING_C * (1 - (memPct(target) ?? 0) / 100)"
                  />
                </svg>
                <span class="ring__value num">{{ ringText(memPct(target)) }}</span>
              </div>
              <span class="ring__label">内存</span>
            </div>

            <!-- 速率排成 2×2：左列网络、右列磁盘。
                 起初是 4 行、数值用 1fr 推到卡片最右，结果标签与数值之间
                 隔着半张卡的空白，扫读时极易串行。 -->
            <dl class="hostcard__io">
              <div class="hostcard__ioitem">
                <dt class="hostcard__iokey is-tx">↑ 上行</dt>
                <dd class="hostcard__ioval num">{{ rateOf(target, 'tx') }}</dd>
              </div>
              <div class="hostcard__ioitem">
                <dt class="hostcard__iokey is-read">R 读取</dt>
                <dd class="hostcard__ioval num">{{ rateOf(target, 'read') }}</dd>
              </div>
              <div class="hostcard__ioitem">
                <dt class="hostcard__iokey is-rx">↓ 下行</dt>
                <dd class="hostcard__ioval num">{{ rateOf(target, 'rx') }}</dd>
              </div>
              <div class="hostcard__ioitem">
                <dt class="hostcard__iokey is-write">W 写入</dt>
                <dd class="hostcard__ioval num">{{ rateOf(target, 'write') }}</dd>
              </div>
            </dl>
          </div>
        </button>

        <p v-if="noteOf(target)" class="hostcard__note">{{ noteOf(target) }}</p>
      </li>
    </ul>

    <!-- 宿主机卡片刻意不参与这个空态：面板自己这台机器永远有数据可看，
         没有任何远端主机只是"列表里暂时只有宿主机"。 -->
    <p v-if="!listError && !loadingList && hosts.length === 0" class="hostpanel__empty">
      还没有添加任何主机。到「主机管理」添加后，它们也会出现在这里。
    </p>
  </div>
</template>
