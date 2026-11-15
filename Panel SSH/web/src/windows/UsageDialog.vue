<script setup lang="ts">
/**
 * 目录用量分析。
 *
 * 与 SMART 同样是按需触发：du 是在真的遍历目录树，一个几百万文件的数据目录
 * 要跑上一分钟。把它挂进 5 秒一轮的轮询，等于持续给目标机的磁盘加压。
 *
 * 只下钻一层。"这个盘被什么占满了"通常在第一层就有答案；要看更深，
 * 对着某一层再点一次比一次搬来整棵树更省事，也更省这次 SSH。
 */
import { computed, ref, watch } from 'vue';
import { NButton, NModal, NSpace } from 'naive-ui';

import { ApiError, api, type DiskUsage } from '../api/client';
import { formatBytes } from '../api/format';

const props = defineProps<{
  show: boolean;
  hostId: number | null;
  /** 要统计的挂载点目录。服务端只会接受确实挂着的路径。 */
  target: string;
}>();

const emit = defineEmits<{ (e: 'update:show', value: boolean): void }>();

const visible = computed({
  get: () => props.show,
  set: (value) => emit('update:show', value),
});

const usage = ref<DiskUsage | null>(null);
const loading = ref(false);
const error = ref('');

async function load(): Promise<void> {
  if (props.hostId === null || props.target === '') return;

  loading.value = true;
  error.value = '';
  try {
    usage.value = await api.storage.usage(props.hostId, props.target);
  } catch (err) {
    usage.value = null;
    error.value = err instanceof ApiError ? err.message : '统计目录用量失败';
  } finally {
    loading.value = false;
  }
}

watch(
  () => props.show,
  (on) => {
    if (on) void load();
  },
);

/** 每一项相对总计的占比；总计为 0 时不画条，避免画出一条毫无意义的满格。 */
function percentOf(bytes: number): number {
  const total = usage.value?.total ?? 0;
  if (total <= 0) return 0;
  return Math.min(100, (bytes / total) * 100);
}
</script>

<template>
  <n-modal v-model:show="visible" preset="card" title="目录用量" style="width: 640px">
    <div class="usage">
      <p class="usage__target num">{{ target }}</p>

      <p v-if="loading" class="usage__hint">正在统计，这一步可能要跑一会儿…</p>
      <p v-else-if="error" class="usage__error">{{ error }}</p>

      <template v-else-if="usage">
        <p class="usage__total">
          总计 <strong class="num">{{ formatBytes(usage.total) }}</strong>
        </p>

        <!-- 有读不到的子目录时，上面那个总计就只是个下限。
             不说出来的话，"总计 4 KB"会被当成"这个盘是空的"。 -->
        <p v-if="usage.warning" class="usage__warn">{{ usage.warning }}</p>

        <ul class="usage__list">
          <li v-for="entry in usage.entries" :key="entry.path" class="usage__row">
            <span class="usage__name num" :title="entry.path">{{ entry.name }}</span>
            <span class="usage__bar">
              <span :style="{ width: `${percentOf(entry.bytes)}%` }" />
            </span>
            <span class="usage__size num">{{ formatBytes(entry.bytes) }}</span>
            <span class="usage__pct num">{{ percentOf(entry.bytes).toFixed(0) }}%</span>
          </li>
        </ul>
        <p v-if="usage.entries.length === 0" class="usage__hint">这个目录下没有子项</p>

        <p v-if="usage.truncated" class="usage__note">
          只列出占用最大的 {{ usage.entries.length }} 项，还有更小的没有显示。
        </p>
      </template>
    </div>

    <template #footer>
      <n-space justify="end">
        <n-button :loading="loading" @click="load">重新统计</n-button>
        <n-button type="primary" @click="visible = false">关闭</n-button>
      </n-space>
    </template>
  </n-modal>
</template>
