<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';
import { NButton, NInput, NInputNumber, NProgress, NSelect, NSpace, NTag, useMessage } from 'naive-ui';

import { ApiError, api, batchURL, type BatchMessage, type Host } from '../api/client';

const message = useMessage();

type Status = 'pending' | 'running' | 'success' | 'failed' | 'error' | 'cancelled';

interface ResultRow {
  hostId: number;
  hostName: string;
  status: Status;
  exitCode: number | null;
  error: string;
  note: string;
  durationMs: number;
  output: string;
  hasStderr: boolean;
  expanded: boolean;
}

const STATUS_META: Record<
  Status,
  { label: string; type: 'default' | 'info' | 'success' | 'warning' | 'error'; led: string }
> = {
  pending: { label: '等待', type: 'default', led: '' },
  running: { label: '执行中', type: 'info', led: 'led--sig led--live' },
  success: { label: '成功', type: 'success', led: 'led--ok' },
  // 命令跑完了但退出码非 0，与"根本没跑起来"是两回事，用不同颜色区分。
  failed: { label: '退出码非 0', type: 'warning', led: 'led--warn' },
  error: { label: '执行失败', type: 'error', led: 'led--danger' },
  cancelled: { label: '已取消', type: 'default', led: '' },
};

const hosts = ref<Host[]>([]);
const selected = ref<number[]>([]);
const command = ref('');
const timeoutSec = ref(60);
const running = ref(false);
const finished = ref(0);
const total = ref(0);
const rows = ref<ResultRow[]>([]);
const allExpanded = ref(true);

let socket: WebSocket | null = null;

const hostOptions = computed(() =>
  hosts.value.map((host) => ({
    label: `${host.name}（${host.addr}:${host.port}）`,
    value: host.id,
  })),
);

const canRun = computed(
  () => !running.value && selected.value.length > 0 && command.value.trim() !== '',
);

const summary = computed(() => {
  const count = (status: Status) => rows.value.filter((r) => r.status === status).length;
  return {
    success: count('success'),
    failed: count('failed'),
    error: count('error'),
    cancelled: count('cancelled'),
  };
});

const progressPercent = computed(() => (total.value > 0 ? (finished.value / total.value) * 100 : 0));

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

async function loadHosts(): Promise<void> {
  try {
    hosts.value = await api.hosts.list();
  } catch (err) {
    message.error(describe(err, '加载主机列表失败'));
  }
}

function selectAll(): void {
  selected.value = hosts.value.map((h) => h.id);
}

function clearSelection(): void {
  selected.value = [];
}

function rowOf(hostId: number): ResultRow | undefined {
  return rows.value.find((row) => row.hostId === hostId);
}

function run(): void {
  if (!canRun.value) return;

  const nameById = new Map(hosts.value.map((host) => [host.id, host.name]));
  rows.value = selected.value.map((id) => ({
    hostId: id,
    hostName: nameById.get(id) ?? `#${id}`,
    status: 'pending',
    exitCode: null,
    error: '',
    note: '',
    durationMs: 0,
    output: '',
    hasStderr: false,
    expanded: allExpanded.value,
  }));
  finished.value = 0;
  total.value = rows.value.length;
  running.value = true;

  socket = new WebSocket(batchURL());

  socket.onopen = () => {
    socket?.send(
      JSON.stringify({
        type: 'start',
        hostIds: selected.value,
        command: command.value,
        timeoutSec: timeoutSec.value,
      }),
    );
  };

  socket.onmessage = (event: MessageEvent<string>) => {
    let msg: BatchMessage;
    try {
      msg = JSON.parse(event.data) as BatchMessage;
    } catch {
      return;
    }
    handleMessage(msg);
  };

  socket.onerror = () => {
    message.error('批量执行连接异常');
    stop();
  };

  // 服务端发完 done 就会关闭连接，这里兜底处理异常断开。
  socket.onclose = () => stop();
}

/**
 * 中止本次批量执行。
 *
 * 直接关掉连接即可：服务端用 CloseRead 得到"对端断开"的信号并取消整批任务，
 * 不会让已经在跑的命令继续占着目标机。把还没开始的条目标成"已取消"，
 * 是为了让用户看清哪些根本没执行，而不是让它们永远停在"等待"。
 */
function cancel(): void {
  if (!running.value) return;
  for (const row of rows.value) {
    if (row.status === 'pending') row.status = 'cancelled';
  }
  message.info('已中止本次批量执行');
  stop();
}

function handleMessage(msg: BatchMessage): void {
  const hostId = msg.hostId ?? -1;

  switch (msg.type) {
    case 'begin':
      total.value = msg.total ?? total.value;
      break;

    case 'hostStart': {
      const row = rowOf(hostId);
      if (row) {
        row.status = 'running';
        if (msg.hostName) row.hostName = msg.hostName;
      }
      break;
    }

    case 'output': {
      const row = rowOf(hostId);
      if (row) {
        row.output += msg.data ?? '';
        if (msg.stream === 'stderr') row.hasStderr = true;
      }
      break;
    }

    case 'hostEnd': {
      const row = rowOf(hostId);
      if (row) {
        if (msg.hostName) row.hostName = msg.hostName;
        row.exitCode = msg.exitCode ?? null;
        row.error = msg.error ?? '';
        row.note = msg.note ?? '';
        row.durationMs = msg.durationMs ?? 0;
        row.status = row.error ? 'error' : row.exitCode === 0 ? 'success' : 'failed';
      }
      finished.value += 1;
      break;
    }

    case 'done':
      stop();
      break;

    case 'error':
      message.error(msg.message ?? '批量执行失败');
      stop();
      break;

    default:
      break;
  }
}

function stop(): void {
  running.value = false;
  socket?.close();
  socket = null;
}

function toggleAll(): void {
  allExpanded.value = !allExpanded.value;
  for (const row of rows.value) row.expanded = allExpanded.value;
}

async function copyText(text: string, okText: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
    message.success(okText);
  } catch {
    message.error('复制失败，浏览器可能未授予剪贴板权限');
  }
}

async function copyAll(): Promise<void> {
  const text = rows.value
    .map((row) => `# ${row.hostName}  状态=${STATUS_META[row.status].label} 退出码=${row.exitCode ?? 'N/A'}\n${row.output}`)
    .join('\n');
  await copyText(text, '已复制全部输出');
}

async function copyRow(row: ResultRow): Promise<void> {
  await copyText(row.output, `已复制 ${row.hostName} 的输出`);
}

onMounted(loadHosts);

onBeforeUnmount(() => {
  socket?.close();
  socket = null;
});
</script>

<template>
  <div class="batch">
    <div class="batch__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">BATCH</p>
        <h2 class="page-title">
          批量执行
          <span class="page-count">已选 {{ selected.length }} 台</span>
        </h2>
      </div>
      <span class="page-meta">同一条命令在多台主机上并行下发</span>
    </div>

    <div class="batch__form">
      <section class="batch__panel">
        <header class="batch__panel-head">
          <span class="batch__panel-title">目标主机</span>
          <span class="batch__spacer" />
          <n-button size="small" :disabled="running" @click="selectAll">全选</n-button>
          <n-button
            size="small"
            :disabled="running || selected.length === 0"
            @click="clearSelection"
          >
            清空
          </n-button>
        </header>
        <n-select
          v-model:value="selected"
          class="batch__hosts"
          multiple
          filterable
          :options="hostOptions"
          :max-tag-count="6"
          placeholder="选择要在哪些主机上执行"
          :disabled="running"
        />
      </section>

      <section class="batch__panel">
        <header class="batch__panel-head">
          <span class="batch__panel-title">命令</span>
          <span class="batch__hint">支持多行，按顺序执行</span>
        </header>
        <n-input
          v-model:value="command"
          type="textarea"
          :rows="4"
          :disabled="running"
          placeholder="例如：uptime && df -h /"
          :input-props="{ 'aria-label': '要执行的命令' }"
        />
      </section>

      <div class="batch__actions">
        <span class="batch__hint">单机超时</span>
        <!-- 不显示步进按钮：30 余像素高的输入框里，步进的命中区域必然
             小于 WCAG 2.2 要求的 24×24，而数字可直接输入、方向键也原生支持增减。 -->
        <n-input-number
          v-model:value="timeoutSec"
          :min="1"
          :max="1800"
          :show-button="false"
          :disabled="running"
          style="width: 110px"
          :input-props="{ 'aria-label': '单机超时秒数' }"
        />
        <span class="batch__hint">秒（上限 1800）</span>

        <span class="batch__spacer" />

        <n-space size="small">
          <n-button size="small" :disabled="running || rows.length === 0" @click="toggleAll">
            {{ allExpanded ? '全部折叠' : '全部展开' }}
          </n-button>
          <n-button size="small" :disabled="rows.length === 0" @click="copyAll">复制输出</n-button>
          <n-button v-if="running" size="small" type="error" @click="cancel">中止执行</n-button>
          <n-button v-else size="small" type="primary" :disabled="!canRun" @click="run">
            执行
          </n-button>
        </n-space>
      </div>

      <div v-if="total > 0" class="batch__progressbar">
        <n-progress
          type="line"
          :percentage="progressPercent"
          :show-indicator="false"
          :height="4"
          :border-radius="2"
          :status="running ? 'info' : summary.error > 0 ? 'error' : summary.failed > 0 ? 'warning' : 'success'"
        />
        <span class="batch__progress num">
          {{ finished }} / {{ total }}
          <template v-if="finished > 0">
            · 成功 {{ summary.success }} · 退出码非 0 {{ summary.failed }} · 失败 {{ summary.error }}
            <template v-if="summary.cancelled > 0">· 已取消 {{ summary.cancelled }}</template>
          </template>
        </span>
      </div>
    </div>

    <div class="batch__results">
      <p v-if="rows.length === 0" class="batch__empty">
        选择主机并输入命令后点击「执行」。输出会在执行过程中实时回传，无需等待全部完成。
      </p>

      <section v-for="row in rows" :key="row.hostId" class="batch__card">
        <header class="batch__card-head" @click="row.expanded = !row.expanded">
          <span class="led" :class="STATUS_META[row.status].led" aria-hidden="true" />
          <span class="batch__host">{{ row.hostName }}</span>
          <n-tag size="small" :type="STATUS_META[row.status].type" :bordered="false">
            {{ STATUS_META[row.status].label }}
          </n-tag>
          <n-tag v-if="row.exitCode !== null" size="small" :bordered="false">
            退出码 {{ row.exitCode }}
          </n-tag>
          <n-tag v-if="row.hasStderr" size="small" :bordered="false" type="warning">含 stderr</n-tag>
          <span v-if="row.durationMs > 0" class="batch__dur num">{{ row.durationMs }} ms</span>
          <span class="batch__spacer" />
          <span v-if="row.note" class="batch__note">{{ row.note }}</span>
          <button
            v-if="row.output"
            class="batch__copy"
            :aria-label="`复制 ${row.hostName} 的输出`"
            @click.stop="copyRow(row)"
          >
            复制
          </button>
          <span class="batch__toggle">{{ row.expanded ? '收起' : '展开' }}</span>
        </header>

        <p v-if="row.error" class="batch__error">{{ row.error }}</p>

        <pre v-if="row.expanded && row.output" class="batch__output">{{ row.output }}</pre>
        <p v-else-if="row.expanded && !row.output && row.status !== 'running' && row.status !== 'pending'" class="batch__noout">
          无输出
        </p>
      </section>
    </div>
  </div>
</template>
