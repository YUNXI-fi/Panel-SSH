<script setup lang="ts">
/**
 * 环境变更执行框：通过 WebSocket 实时展示每个步骤的状态与输出。
 *
 * 单台主机一个实例（多主机批量在后端并发，前端首版只对当前选中主机操作）。
 * 执行中关闭窗口等价于中止：和批量执行同一语义，后端收到断连会杀掉在跑的步骤。
 */
import { computed, nextTick, ref, watch, type ComponentPublicInstance } from 'vue';
import { NAlert, NButton, NModal, NTag } from 'naive-ui';

import { envRunURL, type EnvRunRequest, type EnvWSMsg } from '../api/client';

interface StepState {
  seq: number;
  name: string;
  status: 'running' | 'success' | 'failed';
  exitCode: number;
  durationMs: number;
  stdout: string;
  stderr: string;
  expanded: boolean;
  /** 日志是否吸附在底部：用户手动往上翻后暂停自动滚动，翻回底部再恢复。 */
  pinned: boolean;
}

const props = defineProps<{
  show: boolean;
  hostId: number;
  hostName: string;
  request: EnvRunRequest | null;
}>();
const emit = defineEmits<{
  (e: 'update:show', v: boolean): void;
  (e: 'done'): void;
}>();

const steps = ref<StepState[]>([]);
const running = ref(false);
const fatalError = ref('');
const result = ref<{
  status: string;
  compensated: boolean;
  verified: boolean;
  verifyNote: string;
  summary: string;
  error: string;
} | null>(null);
const expandedAll = ref(true);

/** 每个步骤日志元素，按 seq 索引；输出增长时据此把它们滚到底部。 */
const logRefs = new Map<number, HTMLElement>();
const stepsBox = ref<HTMLElement | null>(null);
/** 外层步骤列表是否吸附在底部，逻辑与单个日志相同。 */
let stepsPinned = true;

let socket: WebSocket | null = null;

function distanceToBottom(el: HTMLElement): number {
  return el.scrollHeight - el.scrollTop - el.clientHeight;
}

function setLogRef(seq: number, el: Element | ComponentPublicInstance | null): void {
  if (el instanceof HTMLElement) logRefs.set(seq, el);
  else logRefs.delete(seq);
}

function onLogScroll(step: StepState, event: Event): void {
  step.pinned = distanceToBottom(event.target as HTMLElement) < 24;
}

function onStepsScroll(): void {
  const el = stepsBox.value;
  if (el) stepsPinned = distanceToBottom(el) < 24;
}

/**
 * 输出增长后把日志滚到底部。
 *
 * 吸附不是无条件的：用户往上翻看历史时，新输出不能把他拽回底部；
 * 翻回底部（24px 内）后自动恢复吸附，和终端的行为一致。
 */
watch(
  () => steps.value,
  async () => {
    await nextTick();
    for (const step of steps.value) {
      if (!step.expanded || !step.pinned) continue;
      const el = logRefs.get(step.seq);
      if (el) el.scrollTop = el.scrollHeight;
    }
    const box = stepsBox.value;
    if (box && stepsPinned) box.scrollTop = box.scrollHeight;
  },
  { deep: true },
);

watch(
  () => props.show,
  (show) => {
    if (show) {
      start();
    } else {
      cleanup();
    }
  },
);

function start(): void {
  steps.value = [];
  logRefs.clear();
  stepsPinned = true;
  result.value = null;
  fatalError.value = '';
  running.value = true;

  socket = new WebSocket(envRunURL());
  socket.onopen = () => {
    socket?.send(JSON.stringify({ type: 'start', hostIds: [props.hostId], request: props.request }));
  };
  socket.onmessage = (event: MessageEvent<string>) => {
    let msg: EnvWSMsg;
    try {
      msg = JSON.parse(event.data) as EnvWSMsg;
    } catch {
      return;
    }
    handle(msg);
  };
  socket.onerror = () => {
    fatalError.value = '连接异常，请查看操作记录确认实际执行情况';
    running.value = false;
  };
  socket.onclose = () => {
    running.value = false;
    socket = null;
  };
}

function handle(msg: EnvWSMsg): void {
  switch (msg.type) {
    case 'stepStart': {
      steps.value.push({
        seq: msg.seq ?? 0,
        name: msg.name ?? '',
        status: 'running',
        exitCode: -1,
        durationMs: 0,
        stdout: '',
        stderr: '',
        expanded: expandedAll.value,
        pinned: true,
      });
      break;
    }
    case 'stepLog': {
      const step = steps.value.find((s) => s.seq === msg.seq);
      if (step && msg.data) {
        if (msg.stream === 'stderr') step.stderr += msg.data;
        else step.stdout += msg.data;
      }
      break;
    }
    case 'stepEnd': {
      const step = steps.value.find((s) => s.seq === msg.seq);
      if (step) {
        step.status = (msg.status as StepState['status']) ?? 'failed';
        step.exitCode = msg.exitCode ?? -1;
        step.durationMs = msg.durationMs ?? 0;
      }
      break;
    }
    case 'hostEnd': {
      result.value = {
        status: msg.status ?? (msg.error ? 'failed' : 'unknown'),
        compensated: msg.compensated ?? false,
        verified: msg.verified ?? false,
        verifyNote: msg.verifyNote ?? '',
        summary: msg.summary ?? '',
        error: msg.error ?? '',
      };
      running.value = false;
      break;
    }
    case 'error':
      fatalError.value = msg.error ?? '执行失败';
      running.value = false;
      break;
    case 'done':
      running.value = false;
      break;
    default:
      break;
  }
}

function abort(): void {
  // 关连接即中止：后端会终止目标机上正在跑的步骤。
  cleanup();
  emit('update:show', false);
}

function cleanup(): void {
  if (socket) {
    socket.onclose = null;
    socket.close();
    socket = null;
  }
  running.value = false;
}

function closeDone(): void {
  cleanup();
  emit('update:show', false);
  emit('done');
}

function toggleAll(): void {
  expandedAll.value = !expandedAll.value;
  for (const s of steps.value) {
    s.expanded = expandedAll.value;
    // 重新展开时回到最新输出，而不是停在折叠前的旧位置。
    if (s.expanded) s.pinned = true;
  }
}

/** 展开某一步时同样回到最新输出。 */
function toggleStep(step: StepState): void {
  step.expanded = !step.expanded;
  if (step.expanded) step.pinned = true;
}

function durationText(ms: number): string {
  if (!ms) return '';
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}

const actionText = computed(() => {
  const map: Record<string, string> = {
    install: '安装',
    upgrade: '升级',
    remove: '卸载',
    'upgrade-all': '全部升级',
    mirror: '切换安装源',
    rollback: '回滚',
  };
  return props.request ? map[props.request.action] ?? '变更' : '变更';
});
</script>

<template>
  <n-modal
    :show="show"
    preset="card"
    :title="`正在${actionText}：${hostName}`"
    style="width: 780px"
    :mask-closable="false"
    :closable="false"
    @update:show="emit('update:show', $event)"
  >
    <n-alert v-if="fatalError" type="error" :title="fatalError" class="run__alert" />

    <n-alert
      v-if="result"
      :type="result.error || result.status === 'failed' ? 'error' : result.verified ? 'success' : 'warning'"
      :title="result.error ? '执行失败' : result.status === 'failed' ? '变更失败（已补偿）' : '执行完成'"
      class="run__alert"
    >
      <div>{{ result.error || result.summary }}</div>
      <div v-if="result.compensated && !result.error" class="run__sub">
        系统已自动还原安装源并尝试修复包状态
      </div>
      <div v-if="!result.verified && !result.error && result.verifyNote" class="run__sub">
        版本复核：{{ result.verifyNote }}
      </div>
    </n-alert>

    <div class="run__toolbar">
      <span class="run__hint">{{ running ? '正在执行，输出实时刷新…' : `共 ${steps.length} 个步骤` }}</span>
      <n-button size="tiny" quaternary @click="toggleAll">全部展开/折叠</n-button>
    </div>

    <div ref="stepsBox" class="run__steps" @scroll="onStepsScroll">
      <div v-for="step in steps" :key="step.seq" class="step">
        <button class="step__head" @click="toggleStep(step)">
          <span class="step__led" :class="`led--${step.status === 'success' ? 'ok' : step.status === 'failed' ? 'danger' : 'live'}`" />
          <span class="step__seq">{{ step.seq }}</span>
          <span class="step__name">{{ step.name }}</span>
          <n-tag v-if="step.exitCode >= 0" size="tiny" :type="step.status === 'success' ? 'success' : 'error'">
            {{ step.exitCode }}
          </n-tag>
          <span class="step__time">{{ durationText(step.durationMs) }}</span>
          <span class="step__caret">{{ step.expanded ? '▾' : '▸' }}</span>
        </button>
        <pre
          v-if="step.expanded"
          :ref="(el) => setLogRef(step.seq, el)"
          class="step__log"
          :class="{ 'step__log--err': step.stderr }"
          @scroll="onLogScroll(step, $event)"
        >{{ step.stdout }}{{ step.stderr }}</pre>
      </div>
      <div v-if="!steps.length && running" class="run__pending">正在建立连接…</div>
    </div>

    <template #footer>
      <n-button v-if="running" @click="abort">中止</n-button>
      <n-button v-else type="primary" @click="closeDone">完成</n-button>
    </template>
  </n-modal>
</template>

<style scoped>
.run__alert {
  margin-bottom: 12px;
}
.run__sub {
  font-size: 12px;
  opacity: 0.85;
  margin-top: 4px;
}
.run__toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 8px;
}
.run__hint {
  font-size: 12px;
  color: var(--text-2, #888);
}
.run__steps {
  max-height: 420px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.step__head {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 6px 10px;
  background: rgba(255, 255, 255, 0.03);
  border: 1px solid var(--border-1, #2a2a2a);
  border-radius: 6px;
  cursor: pointer;
  color: inherit;
  font-size: 13px;
  text-align: left;
}
.step__seq {
  color: var(--text-2, #888);
  font-family: var(--font-mono, monospace);
  min-width: 18px;
}
.step__name {
  flex: 1;
}
.step__time {
  font-size: 11px;
  color: var(--text-2, #888);
  font-family: var(--font-mono, monospace);
}
.step__caret {
  color: var(--text-2, #888);
}
.step__log {
  margin: 4px 0 0;
  padding: 8px 10px;
  background: rgba(0, 0, 0, 0.35);
  border-radius: 6px;
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  line-height: 1.5;
  max-height: 220px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}
.step__log--err {
  border-left: 2px solid var(--error-color, #e88080);
}
.run__pending {
  color: var(--text-2, #888);
  font-size: 13px;
  padding: 20px;
  text-align: center;
}
</style>
