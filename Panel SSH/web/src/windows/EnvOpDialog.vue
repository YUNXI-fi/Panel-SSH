<script setup lang="ts">
/**
 * 环境操作详情：完整的步骤级审计日志（含 stdout/stderr）与回滚入口。
 */
import { computed, ref, watch } from 'vue';
import { NAlert, NButton, NModal, NSpace, NSpin, NTag } from 'naive-ui';

import { ApiError, api, type EnvOp } from '../api/client';
import { formatTime } from '../api/format';

const props = defineProps<{
  show: boolean;
  opId: number | null;
}>();
const emit = defineEmits<{
  (e: 'update:show', v: boolean): void;
  (e: 'rollback', op: EnvOp): void;
}>();

const op = ref<EnvOp | null>(null);
const loading = ref(false);
const error = ref('');
const expandedSteps = ref<Set<number>>(new Set());

const actionText: Record<string, string> = {
  install: '安装',
  upgrade: '升级',
  remove: '卸载',
  'upgrade-all': '全部升级',
  mirror: '切换安装源',
  rollback: '回滚',
  'env-set': '设置变量',
  'env-unset': '删除变量',
  'env-restore': '还原变量',
};

const statusType = {
  running: 'info',
  success: 'success',
  failed: 'error',
  interrupted: 'warning',
} as const;

const canRollback = computed(() => {
  const o = op.value;
  if (!o) return false;
  // 中断的操作无法判断实际改了什么；回滚操作不能再套回滚。
  // 环境变量的"还原"同理：它自己不是一次可再还原的写入（后端也只接受
  // env-set/env-unset 作为还原来源），给它一个按钮只会点出一个错误。
  return (
    (o.status === 'success' || o.status === 'failed') &&
    o.action !== 'rollback' &&
    o.action !== 'env-restore'
  );
});

watch(
  () => [props.show, props.opId] as const,
  async ([show, id]) => {
    if (!show || id === null) return;
    loading.value = true;
    error.value = '';
    op.value = null;
    expandedSteps.value = new Set();
    try {
      op.value = await api.env.op(id);
      // 默认展开失败的步骤，成功步骤折叠（大部分时候只排障失败步）。
      for (const step of op.value.steps ?? []) {
        if (step.status === 'failed') expandedSteps.value.add(step.seq);
      }
    } catch (err) {
      error.value = err instanceof ApiError ? err.message : '加载操作详情失败';
    } finally {
      loading.value = false;
    }
  },
);

function toggleStep(seq: number): void {
  const next = new Set(expandedSteps.value);
  if (next.has(seq)) next.delete(seq);
  else next.add(seq);
  expandedSteps.value = next;
}

function durationText(ms: number): string {
  if (!ms) return '';
  if (ms < 1000) return `${ms} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}
</script>

<template>
  <n-modal
    :show="show"
    preset="card"
    title="环境操作详情"
    style="width: 800px"
    @update:show="emit('update:show', $event)"
  >
    <n-spin :show="loading">
      <n-alert v-if="error" type="error" :title="error" class="op__alert" />
      <div v-if="op" class="op">
        <div class="op__head">
          <span class="op__target">#{{ op.id }} · {{ op.target }}</span>
          <n-tag size="small">{{ actionText[op.action] ?? op.action }}</n-tag>
          <n-tag size="small" :type="statusType[op.status]">
            {{ { running: '执行中', success: '成功', failed: '失败', interrupted: '已中断' }[op.status] }}
          </n-tag>
          <n-tag v-if="op.compensated" size="small" type="warning">已补偿</n-tag>
          <n-tag v-if="op.verified" size="small" type="success">复核通过</n-tag>
        </div>
        <div class="op__meta">
          <span>{{ op.hostName }}</span>
          <span>{{ formatTime(op.createdAt) }}</span>
          <span v-if="op.originOpId">来源操作 #{{ op.originOpId }}</span>
        </div>

        <div v-if="op.summary" class="op__summary">{{ op.summary }}</div>
        <div v-if="op.verifyNote" class="op__verify">复核：{{ op.verifyNote }}</div>

        <div class="op__section-title">操作日志（{{ op.steps?.length ?? 0 }} 步）</div>
        <div class="op__steps">
          <div v-for="step in op.steps" :key="step.seq" class="step">
            <button class="step__head" @click="toggleStep(step.seq)">
              <span
                class="step__led"
                :class="`led--${step.status === 'success' ? 'ok' : step.status === 'failed' ? 'danger' : step.status === 'skipped' ? '' : 'live'}`"
              />
              <span class="step__seq">{{ step.seq }}</span>
              <span class="step__name">{{ step.name }}</span>
              <n-tag v-if="step.exitCode >= 0" size="tiny" :type="step.status === 'success' ? 'success' : 'error'">
                {{ step.exitCode }}
              </n-tag>
              <span class="step__time">{{ durationText(step.durationMs) }}</span>
              <span class="step__caret">{{ expandedSteps.has(step.seq) ? '▾' : '▸' }}</span>
            </button>
            <pre
              v-if="expandedSteps.has(step.seq)"
              class="step__log"
              :class="{ 'step__log--err': !!step.stderr }"
            >{{ step.stdout }}{{ step.stderr }}</pre>
          </div>
        </div>
      </div>
    </n-spin>

    <template #footer>
      <n-space justify="end">
        <n-button @click="emit('update:show', false)">关闭</n-button>
        <n-button
          v-if="canRollback"
          type="warning"
          :disabled="!op"
          @click="op && emit('rollback', op)"
        >
          回滚这次操作
        </n-button>
      </n-space>
    </template>
  </n-modal>
</template>

<style scoped>
.op__alert {
  margin-bottom: 10px;
}
.op__head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.op__target {
  font-weight: 600;
  font-size: 14px;
}
.op__meta {
  display: flex;
  gap: 14px;
  margin: 8px 0;
  font-size: 12px;
  color: var(--text-2, #888);
}
.op__summary {
  font-size: 13px;
  margin-bottom: 4px;
}
.op__verify {
  font-size: 12px;
  color: var(--warning-color, #e0a95c);
  margin-bottom: 4px;
}
.op__section-title {
  font-size: 13px;
  font-weight: 600;
  margin: 14px 0 8px;
}
.op__steps {
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
  max-height: 260px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}
.step__log--err {
  border-left: 2px solid var(--error-color, #e88080);
}
</style>
