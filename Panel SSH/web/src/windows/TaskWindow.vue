<script setup lang="ts">
/**
 * 任务记录窗口：批量执行与文件操作（哈希 / 压缩 / 解压 / 上传 / 下载）的历史。
 *
 * 与 BatchWindow 的分工 —— 那边负责"发起并实时看输出"，这边负责"事后回看"。
 *
 * 这里刻意不展示命令输出正文：输出不落库（见后端 store 里 tasks 表的说明），
 * 历史能回答的是"哪台机器、成没成、退出码多少、花了多久"。若把输出也存下来，
 * 单批 200 台 × 每台 256 KiB 会让内嵌数据库迅速膨胀。
 *
 * 文件操作没有"每台机器"的概念，汇报的是"处理了多少个文件"，因此列表措辞、
 * 详情里的列、以及明细表都按 kind 分叉。
 */
import { computed, onMounted, ref, watch } from 'vue';
import { NButton, NTag, useMessage } from 'naive-ui';

import {
  ApiError,
  api,
  copyText,
  triggerDownload,
  type Task,
  type TaskItem,
  type TaskResult,
} from '../api/client';
import { formatBytes, formatTime } from '../api/format';

/**
 * taskId 由通知中心传入，表示"打开就直接看这一条任务"。
 *
 * 它是可选的外部指令，而不是窗口自身状态：窗口打开后用户仍可自由切换任务，
 * 此时不再回写这个 prop。
 */
const props = defineProps<{ taskId?: number }>();

const message = useMessage();

const tasks = ref<Task[]>([]);
const results = ref<TaskResult[]>([]);
const items = ref<TaskItem[]>([]);
const selectedId = ref<number | null>(null);
const loading = ref(false);
const detailLoading = ref(false);
const lastError = ref('');

const RESULT_META: Record<
  TaskResult['status'],
  { label: string; type: 'success' | 'warning' | 'error'; led: string }
> = {
  success: { label: '成功', type: 'success', led: 'led--ok' },
  // 命令跑完了但退出码非 0，与"根本没跑起来"是两回事，颜色必须区分开。
  failed: { label: '退出码非 0', type: 'warning', led: 'led--warn' },
  error: { label: '执行失败', type: 'error', led: 'led--danger' },
};

/**
 * 文件类任务的结果标签。
 *
 * 与批量执行共用同一套 status 取值，但措辞必须换掉：文件操作里 "failed" 表示
 * 有个别文件没处理成，而整个操作是跑完了的，照搬"退出码非 0"会让人摸不着头脑。
 */
const FILE_RESULT_META: Record<TaskResult['status'], { label: string; type: 'success' | 'warning' | 'error' }> = {
  success: { label: '成功', type: 'success' },
  failed: { label: '部分失败', type: 'warning' },
  error: { label: '失败', type: 'error' },
};

/** 任务种类的展示信息。 */
const KIND_META: Record<Task['kind'], { label: string; fileOp: boolean }> = {
  batch: { label: '批量执行', fileOp: false },
  hash: { label: '计算哈希', fileOp: true },
  compress: { label: '压缩', fileOp: true },
  extract: { label: '解压', fileOp: true },
  upload: { label: '上传', fileOp: true },
  download: { label: '下载', fileOp: true },
  transfer: { label: '跨主机传输', fileOp: true },
};

/**
 * 明细表最多渲染这么多行。
 *
 * 一个几万文件的目录算完哈希就是几万条明细，全铺进 DOM 会把浏览器卡住；
 * 但库里存的是完整的，复制与导出拿的也是完整清单 —— 只有渲染被截断。
 */
const itemRenderLimit = 500;

const selected = computed(() => tasks.value.find((t) => t.id === selectedId.value) ?? null);
const visibleItems = computed(() => items.value.slice(0, itemRenderLimit));

function isFileTask(task: Task): boolean {
  return KIND_META[task.kind].fileOp;
}

/**
 * 列表项的 LED 反映"这次任务整体怎么样"，所以看计数而不是看状态：
 * 一个 done 的任务也可能满屏都是红色。
 */
function taskLed(task: Task): string {
  if (task.status === 'running') return 'led--sig led--live';
  if (task.status === 'interrupted') return 'led--warn';
  if (task.errorCount > 0) return 'led--danger';
  if (task.failedCount > 0) return 'led--warn';
  return 'led--ok';
}

function taskLabel(task: Task): string {
  if (task.status === 'running') return '执行中';
  if (task.status === 'interrupted') return '已中断';
  return '已完成';
}

/**
 * 清单文本：<摘要>  <相对路径>。
 *
 * 与文件管理里导出的那份格式一致，正是 sha256sum -c 认得的：导出的文件可以
 * 原样拿到目标机上校验。没算出摘要的行不写进去 —— 带一条错的摘要去校验，
 * 只会得到一句莫名其妙的失败。
 */
function manifest(): string {
  return items.value
    .filter((row) => row.hash && !row.error)
    .map((row) => `${row.hash}  ${row.path}`)
    .join('\n');
}

async function copyManifest(): Promise<void> {
  const text = manifest();
  if (!text) {
    message.warning('这次任务没有可复制的摘要');
    return;
  }
  if (!(await copyText(text))) {
    message.error('复制失败，请手动选中内容');
    return;
  }
  message.success(`已复制 ${text.split('\n').length} 条摘要`);
}

function exportManifest(): void {
  const text = manifest();
  if (!text) {
    message.warning('这次任务没有可导出的摘要');
    return;
  }
  // 末尾补一个换行：没有它，文本编辑器里最后一行常被当成没写完。
  const blob = new Blob([`${text}\n`], { type: 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  triggerDownload(url, `${selected.value?.algo ?? 'hash'}-任务${selected.value?.id ?? 0}.txt`);
  // 不能立刻撤销：浏览器可能还没把这段 Blob 读完，那样落盘的是空文件。
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

async function load(): Promise<void> {
  loading.value = true;
  try {
    tasks.value = await api.tasks.list();
    lastError.value = '';

    // 优先展示外部指定的那条任务（从通知中心点进来时带着 taskId）；
    // 否则尽量保留原来选中的任务，它不存在了（或首次打开）就退回最新一条，
    // 省掉"进来先点一下"这一步。
    const wanted = props.taskId;
    if (wanted !== undefined && tasks.value.some((t) => t.id === wanted)) {
      await selectTask(wanted);
    } else if (!tasks.value.some((t) => t.id === selectedId.value)) {
      await selectTask(tasks.value[0]?.id ?? null);
    }
  } catch (err) {
    lastError.value = describe(err, '加载任务记录失败');
  } finally {
    loading.value = false;
  }
}

async function selectTask(id: number | null): Promise<void> {
  selectedId.value = id;
  results.value = [];
  items.value = [];
  if (id === null) return;

  detailLoading.value = true;
  try {
    const detail = await api.tasks.get(id);
    // 请求返回前用户可能已经点了别的任务，此时丢弃这次结果，避免串台。
    if (selectedId.value === id) {
      results.value = detail.results;
      items.value = detail.items;
    }
  } catch (err) {
    if (selectedId.value === id) lastError.value = describe(err, '加载任务详情失败');
  } finally {
    detailLoading.value = false;
  }
}

// 窗口已经开着时，用户又点了另一条任务通知：换掉要展示的任务，
// 而不是往桌面上再叠一个任务窗口。
// （窗口是首次打开时 taskId 在初始 props 里，由 load() 处理，不走这里。）
watch(
  () => props.taskId,
  (id) => {
    if (id !== undefined) void selectTask(id);
  },
);

onMounted(load);
</script>

<template>
  <div class="tasks">
    <div class="tasks__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">TASKS</p>
        <span class="page-title">
          任务记录
          <span class="tasks__count page-count">{{ tasks.length }} 条</span>
        </span>
      </div>
      <span class="tasks__spacer" />
      <n-button size="small" :loading="loading" @click="load">刷新</n-button>
    </div>

    <p v-if="lastError" class="tasks__error">{{ lastError }}</p>

    <div class="tasks__body">
      <div class="tasks__list">
        <button
          v-for="task in tasks"
          :key="task.id"
          class="tasks__item"
          :class="{ 'is-active': task.id === selectedId }"
          @click="selectTask(task.id)"
        >
          <span class="led" :class="taskLed(task)" aria-hidden="true" />
          <span class="tasks__item-main">
            <span class="tasks__item-cmd" :title="task.command">
              <span class="tasks__kind">{{ KIND_META[task.kind].label }}</span>
              {{ task.command }}
            </span>
            <span class="tasks__item-meta">
              #{{ task.id }} · {{ formatTime(task.createdAt) }} ·
              <template v-if="isFileTask(task)">
                {{ task.hostName || '未知主机' }} · {{ task.total }} 项
              </template>
              <template v-else>{{ task.total }} 台</template>
            </span>
          </span>
          <span class="tasks__item-stat">
            <template v-if="task.status === 'running'">…</template>
            <!-- 文件操作只有一台主机、一个结果，"几个成功几个失败"在这里没有意义，
                 整体好坏交给 LED 与详情里的说明。 -->
            <template v-else-if="!isFileTask(task)">
              <span v-if="task.successCount > 0" class="is-ok">{{ task.successCount }}</span>
              <span v-if="task.failedCount > 0" class="is-warn">{{ task.failedCount }}</span>
              <span v-if="task.errorCount > 0" class="is-danger">{{ task.errorCount }}</span>
            </template>
          </span>
        </button>

        <p v-if="!loading && tasks.length === 0" class="tasks__empty">
          还没有任务记录。跑一次批量执行，或在文件管理里做一次操作，这里会留下痕迹。
        </p>
      </div>

      <div class="tasks__detail">
        <template v-if="selected">
          <header class="tasks__detail-head">
            <span class="led" :class="taskLed(selected)" aria-hidden="true" />
            <span class="tasks__detail-title num">任务 #{{ selected.id }}</span>
            <n-tag size="small" :bordered="false">{{ KIND_META[selected.kind].label }}</n-tag>
            <n-tag size="small" :bordered="false">{{ taskLabel(selected) }}</n-tag>
            <span class="tasks__spacer" />
            <span class="tasks__detail-time num">
              {{ formatTime(selected.createdAt) }}
              <template v-if="selected.status !== 'running'">
                → {{ formatTime(selected.finishedAt) }}
              </template>
            </span>
          </header>

          <pre class="tasks__command">{{ selected.command }}</pre>

          <p class="tasks__summary num">
            <template v-if="isFileTask(selected)">
              主机 {{ selected.hostName || '未知' }} · 共 {{ selected.total }} 项
              <template v-if="selected.algo"> · 算法 {{ selected.algo.toUpperCase() }}</template>
              <template v-if="selected.status === 'running'"> · 处理中</template>
            </template>
            <template v-else>
              共 {{ selected.total }} 台 · 单机超时 {{ selected.timeoutSec }} 秒
              <template v-if="selected.status === 'running'">
                · 已回传 {{ results.length }} 台
              </template>
              <template v-else>
                · 成功 {{ selected.successCount }} · 退出码非 0 {{ selected.failedCount }} · 失败
                {{ selected.errorCount }}
                <!-- 结果数少于总数说明任务被中途打断了，这件事必须显式说出来，
                     否则用户只会看到一堆"消失"的主机。 -->
                <template v-if="results.length < selected.total">
                  · 未执行 {{ selected.total - results.length }}
                </template>
              </template>
            </template>
          </p>

          <div class="tasks__results">
            <p v-if="detailLoading" class="tasks__empty">加载中…</p>

            <div v-for="row in results" :key="row.hostId" class="tasks__result">
              <div class="tasks__result-head">
                <span
                  class="led"
                  :class="RESULT_META[row.status].led"
                  aria-hidden="true"
                />
                <span class="tasks__result-host">{{ row.hostName || `#${row.hostId}` }}</span>
                <n-tag
                  size="small"
                  :type="isFileTask(selected) ? FILE_RESULT_META[row.status].type : RESULT_META[row.status].type"
                  :bordered="false"
                >
                  {{ isFileTask(selected) ? FILE_RESULT_META[row.status].label : RESULT_META[row.status].label }}
                </n-tag>
                <!-- 文件操作没有"命令退出码"这回事，那个 0 只会让人以为漏了什么。 -->
                <n-tag v-if="!isFileTask(selected)" size="small" :bordered="false">
                  退出码 {{ row.exitCode }}
                </n-tag>
                <span class="tasks__spacer" />
                <span v-if="row.note" class="tasks__note">{{ row.note }}</span>
                <span class="tasks__dur num">{{ row.durationMs }} ms</span>
              </div>
              <p v-if="row.error" class="tasks__result-error">{{ row.error }}</p>
            </div>

            <p v-if="!detailLoading && results.length === 0" class="tasks__empty">
              {{
                selected.status === 'running'
                  ? '任务正在执行，还没有结果回传。'
                  : '这次任务没有留下结果。'
              }}
            </p>
          </div>

          <div v-if="isFileTask(selected)" class="tasks__items">
            <div class="tasks__items-bar">
              <span class="silk">明细</span>
              <span class="tasks__count num">{{ items.length }} 条</span>
              <span class="tasks__spacer" />
              <n-button
                v-if="selected.kind === 'hash'"
                size="tiny"
                :disabled="!items.length"
                @click="copyManifest"
              >
                复制清单
              </n-button>
              <n-button
                v-if="selected.kind === 'hash'"
                size="tiny"
                :disabled="!items.length"
                @click="exportManifest"
              >
                导出清单
              </n-button>
            </div>

            <!-- 明细被截断只有两种原因：库里就没存全（压缩/解压超过上限），
                 或者只是没渲染完。两种都要说清楚，否则用户会以为记录丢了。 -->
            <p v-if="items.length > itemRenderLimit" class="tasks__items-hint">
              共 {{ items.length }} 条明细，界面只列出前 {{ itemRenderLimit }} 条；复制与导出仍是完整清单。
            </p>
            <p v-else-if="items.length < selected.total" class="tasks__items-hint">
              共 {{ selected.total }} 项，这里只留下了 {{ items.length }} 条明细。
            </p>

            <div v-if="items.length" class="tasks__items-scroll">
              <table class="tasks__items-table">
                <thead>
                  <tr>
                    <th>路径</th>
                    <th v-if="selected.kind === 'hash'" class="is-num">大小</th>
                    <th v-if="selected.kind === 'hash'">摘要</th>
                    <th>备注</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(row, index) in visibleItems" :key="index">
                    <td class="tasks__items-path" :title="row.path">{{ row.path }}</td>
                    <td v-if="selected.kind === 'hash'" class="is-num num">
                      {{ row.size < 0 ? '—' : formatBytes(row.size) }}
                    </td>
                    <td v-if="selected.kind === 'hash'" class="tasks__items-hash">
                      {{ row.hash || '—' }}
                    </td>
                    <td class="tasks__items-err">{{ row.error }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <p v-else-if="!detailLoading" class="tasks__empty">这次操作没有留下明细。</p>
          </div>
        </template>

        <p v-else class="tasks__empty">左侧选择一条任务记录查看详情。</p>
      </div>
    </div>
  </div>
</template>