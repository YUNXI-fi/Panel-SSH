<script setup lang="ts">
/**
 * 右上角常驻状态胶囊。
 *
 * 参考桌面系统的做法：把"此刻最该被看到的一组实时数字"常驻在屏幕角上，
 * 不占工作区、不打断操作。对运维面板来说这组数字就是正在被监控的那台主机的
 * 负载与流量 —— 打开监控窗口后不必再切回去，余光就能看到它是否在飙升。
 * 用户没主动选过目标时，它盯的就是宿主机（面板自己所在的那台机器）。
 *
 * 点击展开主机监控面板：胶囊只有一格宽度，横向对比"哪台机器现在有压力"必须
 * 看到全部主机，这就是面板存在的意义。
 *
 * 显隐规则：存在最大化窗口时整体隐藏。它位于画布右上角，而最大化窗口的标题栏
 * 右端正是最小化/最大化/关闭三个按钮的位置，常驻浮在上面会挡住它们。
 */
import { computed, inject, onBeforeUnmount, ref, watch } from 'vue';

import HostMonitorPanel from './HostMonitorPanel.vue';
import { WindowManagerKey } from './useWindowManager';
import { useVisiblePoll } from './useVisiblePoll';
import { LOCAL_HOST, LOCAL_HOST_ID, watchedHost, type WatchedHost } from './watchedHost';
import { ApiError, api, type Metrics } from '../api/client';
import { formatRate } from '../api/format';

// 与资源监控窗口同频；服务端有采样缓存，两边同时开着也会合并成一次采集。
const POLL_MS = 3000;

const wm = inject(WindowManagerKey);

const root = ref<HTMLElement | null>(null);
const metrics = ref<Metrics | null>(null);
const failed = ref(false);
const expanded = ref(false);

/**
 * 关闭行为：只有按 Esc、或点面板头部的收起按钮才收起。
 *
 * 刻意不做"点空白处收起"：面板是用户主动点开、要对着看一会儿的东西，
 * 手一抖碰到画布就把它收走，比多按一次 Esc 更烦人。
 * 所以收起入口必须显眼：它就在面板头部右上角 —— 展开后胶囊按钮已让位给那一行，
 * 出口也该在那里，而不是让用户去够一个已经消失的目标。
 */
function closeOnEscape(event: KeyboardEvent): void {
  if (event.key === 'Escape') expanded.value = false;
}

// 只在展开期间挂监听：收起后还留在 document 上就是纯粹的常驻开销。
watch(expanded, (isOpen) => {
  if (isOpen) {
    document.addEventListener('keydown', closeOnEscape);
  } else {
    document.removeEventListener('keydown', closeOnEscape);
  }
});

onBeforeUnmount(() => {
  document.removeEventListener('keydown', closeOnEscape);
});

const hidden = computed(() => wm?.windows.some((w) => w.maximized) ?? false);

async function refresh(): Promise<void> {
  const target = watchedHost.value;
  try {
    metrics.value = await api.metrics.get(target.id);
    failed.value = false;
  } catch (err) {
    // 记住的那台主机可能已经被删掉：退回宿主机，别让胶囊挂着一盏永远不灭的红灯，
    // 也别让用户面对一个点不开的监控目标。
    if (err instanceof ApiError && err.status === 404 && target.id !== LOCAL_HOST_ID) {
      watchedHost.value = LOCAL_HOST;
      return;
    }
    // 胶囊是辅助信息，取数失败只把指示灯变红，不弹消息打断用户。
    failed.value = true;
  }
}

useVisiblePoll({
  root,
  intervalMs: POLL_MS,
  task: () => refresh(),
  enabled: () => !hidden.value,
});

// 换主机时立刻拉一次，否则胶囊上会挂着上一台机器的数字直到下一轮轮询。
watch(watchedHost, () => {
  metrics.value = null;
  failed.value = false;
  void refresh();
});

// 最大化窗口会盖住胶囊的位置，此时面板必须收起 ——
// 否则它会浮在一个已经隐形的锚点上，既定位错误也无法关闭。
watch(hidden, (isHidden) => {
  if (isHidden) expanded.value = false;
});

const cpuText = computed(() => {
  const m = metrics.value;
  // 首次采样没有计数器基准，此时显示占位符而不是 0% —— 0% 会被当成"机器空闲"。
  if (!m || !m.deltaReady) return '—';
  return `${m.cpuPercent.toFixed(1)}%`;
});

const memText = computed(() => {
  const m = metrics.value;
  if (!m || m.memTotal <= 0) return '—';
  return `${((m.memUsed / m.memTotal) * 100).toFixed(0)}%`;
});

const netText = computed(() => {
  const m = metrics.value;
  if (!m) return '—';
  return `↑${formatRate(m.netTxRate)} ↓${formatRate(m.netRxRate)}`;
});

const ledClass = computed(() => {
  if (failed.value) return 'led--danger';
  return metrics.value ? 'led--ok' : 'led--sig led--live';
});

/**
 * 从面板里选中一个监控目标。
 *
 * 两件事一起做：把胶囊的读数目标切过去，并打开它的资源监控窗口
 * （同一个目标已经开着就聚焦那个窗口，不重复开）。
 */
function openHost(target: WatchedHost): void {
  watchedHost.value = { id: target.id, name: target.name };

  const existing = wm?.windows.find((w) => w.appKey === 'monitor' && w.props.hostId === target.id);
  if (existing && wm) {
    wm.focus(existing.id);
  } else {
    wm?.open(
      'monitor',
      // 与桌面图标同名。那个图标叫「资源监控」，窗口标题若只写「监控」，
      // 会让人以为点开的是另一样东西。
      `资源监控 - ${target.name}`,
      { hostId: target.id, hostName: target.name },
      { w: 1200, h: 560 },
    );
  }
  expanded.value = false;
}
</script>

<template>
  <div ref="root" v-show="!hidden" class="statuspill">
    <!-- 展开后这一条整体让位给面板头部（面板会顶到胶囊的位置上）：
         两者都在说"当前是哪台机器"，并排显示就是同一件事说两遍。
         收起入口随之转移到面板头部的收起按钮上。 -->
    <button
      v-if="!expanded"
      class="statuspill__main"
      title="查看全部主机"
      aria-haspopup="dialog"
      :aria-expanded="expanded"
      :aria-label="`主机监控，当前 ${watchedHost.name}。展开可切换监控目标`"
      @click="expanded = !expanded"
    >
      <span class="led" :class="ledClass" aria-hidden="true" />
      <span class="statuspill__host">{{ watchedHost.name }}</span>

      <span class="statuspill__sep" aria-hidden="true" />

      <span class="statuspill__item">
        <span class="statuspill__key">CPU</span>
        <span class="num">{{ cpuText }}</span>
      </span>

      <span class="statuspill__sep" aria-hidden="true" />

      <span class="statuspill__item">
        <span class="statuspill__key">MEM</span>
        <span class="num">{{ memText }}</span>
      </span>

      <span class="statuspill__sep" aria-hidden="true" />

      <span class="statuspill__item">
        <span class="statuspill__key">NET</span>
        <span class="num">{{ netText }}</span>
      </span>

      <!-- 可展开指示：没有它，用户不会知道这里能点开一层。 -->
      <svg class="statuspill__caret" width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
        <path d="M2.5 4l2.5 2.5L7.5 4" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" />
      </svg>
    </button>
  </div>

  <!--
    面板必须放在胶囊「外面」。
    胶囊自己带 z-index，会形成一个层叠上下文 —— 放在它内部的子元素无论把
    z-index 调多低，都只能在胶囊那一层里排队，永远压不过窗口。想让它落到
    窗口之下，只能让它成为胶囊的兄弟节点，与胶囊共用画布这个定位父级。
  -->
  <host-monitor-panel
    v-if="expanded"
    class="statuspill__panel"
    @pick="openHost"
    @close="expanded = false"
  />
</template>
