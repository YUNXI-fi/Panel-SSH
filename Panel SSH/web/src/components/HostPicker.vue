<script setup lang="ts">
/**
 * 主机选择器。
 *
 * 桌面图标打开的资源监控 / 进程管理窗口没有主机上下文 ——
 * 「主机管理」这类全局工具不需要知道在哪台机器上操作，但采集指标必须知道，
 * 所以这两个窗口要先在这里选一台目标主机。
 *
 * 从主机管理那一行点开的窗口带了 hostId，不会走到这个组件。
 *
 * includeLocal 表示该模块支持把「宿主机」当作目标：宿主机是面板本机的直采通道，
 * 打开后它排在最前并默认选中 —— 面板就装在这台机器上，多数时候要管的正是它本身。
 * 仍有少数操作需要 SSH 会话，后端会明确拒绝宿主机；那类按钮由窗口自己提前禁用。
 */
import { computed, inject, onMounted, ref, watch } from 'vue';
import { NPopover } from 'naive-ui';

import { LOCAL_HOST_ID, LOCAL_HOST_NAME } from '../desktop/watchedHost';
import { WindowSubtitleKey } from '../desktop/useWindowManager';
import { ApiError, api, type Host, type HostStatus } from '../api/client';

const props = defineProps<{
  /** 是否把「宿主机」列为可选目标。 */
  includeLocal?: boolean;
}>();

const hostId = defineModel<number | null>('hostId', { default: null });
/** 选中目标的显示名，随 hostId 一起给出去 —— 胶囊上要显示它。 */
const hostName = defineModel<string>('hostName', { default: '' });

const hosts = ref<Host[]>([]);
const loading = ref(false);
const error = ref('');
/** 下拉列表的展开态。 */
const open = ref(false);
/** 各主机的在线状态，用来给胶囊点灯；取不到就不点，不编一个"在线"。 */
const statuses = ref<Map<number, HostStatus>>(new Map());

async function loadStatuses(): Promise<void> {
  try {
    const list = await api.hosts.status();
    statuses.value = new Map(list.map((item) => [item.id, item]));
  } catch {
    // 状态是辅助信息：拿不到就当"未探测"，选择列表照常可用。
  }
}

onMounted(async () => {
  // 宿主机不依赖主机列表：先把默认目标落位，胶囊一出现就指着它，
  // 不必等接口回来才从"读取中"跳到"宿主机"。
  // 外部已经指定了目标（从主机管理某一行点开的窗口）时不覆盖。
  if (hostId.value === null && props.includeLocal) {
    hostId.value = LOCAL_HOST_ID;
  }

  loading.value = true;
  try {
    hosts.value = await api.hosts.list();
    void loadStatuses();
    if (hostId.value !== null) return;
    // 没有宿主机这个选项时，只有一个候选目标就直接选中：不该逼用户点一下。
    if (hosts.value.length === 1) hostId.value = hosts.value[0].id;
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '读取主机列表失败';
  } finally {
    loading.value = false;
  }
});

/** 可选目标：开了 includeLocal 时宿主机排在最前。 */
const targets = computed(() => [
  ...(props.includeLocal ? [{ id: LOCAL_HOST_ID, name: LOCAL_HOST_NAME }] : []),
  ...hosts.value.map((host) => ({ id: host.id, name: host.name })),
]);

/** 胶囊上显示的文字：没有选中时说清楚该做什么，而不是留一个空壳。 */
const pillText = computed(() => {
  if (hostId.value === null) return loading.value ? '读取中…' : '选择主机';
  return hostName.value || `主机 #${hostId.value}`;
});

/**
 * 胶囊前的状态灯。
 *
 * 宿主机是面板自己，永远是通的；其余按探测结果点亮。
 * 还没探测到就不点灯（悬空灰点），不给一个看起来"在线"的默认值。
 */
function ledOf(id: number): Record<string, boolean> {
  if (id === LOCAL_HOST_ID) return { 'led--ok': true };
  const status = statuses.value.get(id);
  if (!status) return {};
  return status.online ? { 'led--ok': true } : { 'led--danger': true };
}

function pick(target: { id: number; name: string }): void {
  hostId.value = target.id;
  // watch 会把 hostName 同步过来，这里直接写一次是为了让胶囊在下一帧就更新，
  // 不必等 watch 的 flush。
  hostName.value = target.name;
  open.value = false;
}

// 名字跟着 id 走：外部只传了 id 时（从主机管理点开）名字由调用方自己给，
// 但用户在选择器里换了目标就必须把名字一并纠正过来，否则胶囊上会挂着上一个名字。
watch(
  [hostId, targets],
  () => {
    const found = targets.value.find((t) => t.id === hostId.value);
    if (found) hostName.value = found.name;
  },
  { immediate: true },
);

/**
 * 把选中的目标写进窗口标题。
 *
 * 同一个应用现在可以开多个窗口，两个「资源监控」光看标题分不出哪个是哪台机器，
 * 竖栏任务项也一样 —— 标题里带上主机名是"多开"这件事能用起来的前提。
 * inject 不到说明这个选择器不在窗口里，静默跳过。
 */
const setWindowSubtitle = inject(WindowSubtitleKey, null);

watch(hostName, (name) => setWindowSubtitle?.(name), { immediate: true });
</script>

<template>
  <!--
    胶囊本身就是组件根元素：不再套 .ui-strip 外壳，也不再套一层布局用的 div。
    形态与会话条里的一个会话标签同规格（灯 + 主机名 + 下拉箭头），点开是主机列表。
  -->
  <n-popover v-model:show="open" trigger="click" placement="bottom-start" :show-arrow="false">
    <template #trigger>
      <button
        class="hostpick__pill"
        :class="{ 'is-empty': hostId === null }"
        aria-haspopup="listbox"
        :aria-expanded="open"
        title="切换目标主机"
      >
        <span class="led" :class="hostId === null ? {} : ledOf(hostId)" aria-hidden="true" />
        <span class="hostpick__name">{{ pillText }}</span>
        <svg class="hostpick__caret" width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
          <path
            d="M2.5 4l2.5 2.5L7.5 4"
            fill="none"
            stroke="currentColor"
            stroke-width="1.4"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </button>
    </template>

    <div class="hostlist" role="listbox" aria-label="选择目标主机">
      <button
        v-for="target in targets"
        :key="target.id"
        class="hostlist__item"
        :class="{ 'is-active': target.id === hostId }"
        role="option"
        :aria-selected="target.id === hostId"
        @click="pick(target)"
      >
        <span class="led" :class="ledOf(target.id)" aria-hidden="true" />
        <span class="hostlist__name">{{ target.name }}</span>
      </button>

      <p v-if="error" class="hostlist__empty">{{ error }}</p>
      <p v-else-if="loading" class="hostlist__empty">正在读取主机列表…</p>
      <p v-else-if="targets.length === 0" class="hostlist__empty">
        还没有任何主机，请先到「主机管理」添加
      </p>
    </div>
  </n-popover>
</template>
