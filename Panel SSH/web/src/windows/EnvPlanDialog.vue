<script setup lang="ts">
/**
 * 环境变更预检确认框：展示包管理器求解器给出的完整事务。
 *
 * 这是"冲突前置"策略的最后一道闸：安装/卸载会连带动哪些包、下载多少数据、
 * 有没有冲突，都必须在执行前让用户看到。sim.ok 为假时不允许确认。
 */
import { computed } from 'vue';
import { NAlert, NButton, NModal, NSpace, NSpin, NTag } from 'naive-ui';

import type { EnvChangeItem, EnvPlan } from '../api/client';

const props = defineProps<{
  show: boolean;
  plan: EnvPlan | null;
  loading: boolean;
}>();
const emit = defineEmits<{
  (e: 'update:show', v: boolean): void;
  (e: 'confirm'): void;
}>();

interface ChangeRow extends EnvChangeItem {
  kind: 'install' | 'upgrade' | 'downgrade' | 'remove';
}

const rows = computed<ChangeRow[]>(() => {
  const sim = props.plan?.sim;
  if (!sim) return [];
  return [
    ...sim.install.map((it) => ({ ...it, kind: 'install' as const })),
    ...sim.upgrade.map((it) => ({ ...it, kind: 'upgrade' as const })),
    ...sim.downgrade.map((it) => ({ ...it, kind: 'downgrade' as const })),
    ...sim.remove.map((it) => ({ ...it, kind: 'remove' as const })),
  ];
});

const kindMeta = {
  install: { label: '新装', type: 'info' as const },
  upgrade: { label: '升级', type: 'success' as const },
  downgrade: { label: '降级', type: 'warning' as const },
  remove: { label: '卸载', type: 'error' as const },
};

function downloadText(kb: number): string {
  if (kb <= 0) return '';
  if (kb >= 1_000_000) return `${(kb / 1_000_000).toFixed(1)} GB`;
  if (kb >= 1000) return `${(kb / 1000).toFixed(1)} MB`;
  return `${kb} KB`;
}

// 没有 sim（纯换源/tarball 类）或 sim.ok 时才可确认。
const canConfirm = computed(() => {
  if (props.loading || !props.plan) return false;
  return !props.plan.sim || props.plan.sim.ok;
});
</script>

<template>
  <n-modal
    :show="show"
    preset="card"
    title="变更预检"
    style="width: 760px"
    :mask-closable="false"
    @update:show="emit('update:show', $event)"
  >
    <n-spin :show="loading">
      <div v-if="plan" class="plan">
        <div class="plan__head">
          <span class="plan__target">{{ plan.target }}</span>
          <n-tag v-if="plan.sim" :type="plan.sim.ok ? 'success' : 'error'" size="small">
            {{ plan.sim.ok ? '求解通过' : '存在冲突' }}
          </n-tag>
          <n-tag v-else type="info" size="small">不涉及包事务</n-tag>
        </div>

        <n-alert
          v-if="plan.sim && !plan.sim.ok"
          type="error"
          title="依赖求解未通过，不能执行"
          class="plan__alert"
        >
          <div v-for="(line, i) in plan.sim.conflicts" :key="i" class="plan__conflict">{{ line }}</div>
        </n-alert>

        <div v-if="plan.preview" class="plan__preview">{{ plan.preview }}</div>
        <div v-if="plan.note" class="plan__note">{{ plan.note }}</div>

        <div v-if="plan.steps.length" class="plan__section">
          <div class="plan__section-title">执行步骤</div>
          <ol class="plan__steps">
            <li v-for="(step, i) in plan.steps" :key="i">{{ step }}</li>
          </ol>
        </div>

        <div v-if="rows.length" class="plan__section">
          <div class="plan__section-title">
            包管理器将变更 {{ rows.length }} 个包
            <span v-if="plan.sim && downloadText(plan.sim.downloadKB)" class="plan__size">
              · 预计下载 {{ downloadText(plan.sim.downloadKB) }}
            </span>
          </div>
          <div class="plan__table">
            <div class="plan__row plan__row--head">
              <span class="plan__c1">动作</span><span class="plan__c2">软件包</span>
              <span class="plan__c3">变更前</span><span class="plan__c4">变更后</span>
            </div>
            <div v-for="(row, i) in rows" :key="i" class="plan__row">
              <span class="plan__c1"><n-tag :type="kindMeta[row.kind].type" size="small">{{ kindMeta[row.kind].label }}</n-tag></span>
              <span class="plan__c2" :title="row.name">{{ row.name }}</span>
              <span class="plan__c3" :title="row.from">{{ row.from || '—' }}</span>
              <span class="plan__c4" :title="row.to">{{ row.to || '—' }}</span>
            </div>
          </div>
        </div>

        <div v-if="plan.sim && plan.sim.remove.length" class="plan__warn">
          求解器计划卸载 {{ plan.sim.remove.length }} 个包（多为被替换的旧包或冲突包），
          已在上方逐条列出；执行前会自动备份，事后可回滚。
        </div>
      </div>
    </n-spin>

    <template #footer>
      <n-space justify="end">
        <n-button @click="emit('update:show', false)">取消</n-button>
        <n-button
          type="primary"
          :disabled="!canConfirm"
          :loading="loading"
          @click="emit('confirm')"
        >
          确认执行
        </n-button>
      </n-space>
    </template>
  </n-modal>
</template>

<style scoped>
.plan__head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.plan__target {
  font-weight: 600;
  font-size: 15px;
}
.plan__alert {
  margin-bottom: 12px;
}
.plan__conflict {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}
.plan__preview,
.plan__note {
  font-size: 13px;
  color: var(--text-2, #888);
  margin-bottom: 8px;
  line-height: 1.6;
}
.plan__section {
  margin-top: 12px;
}
.plan__section-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 8px;
}
.plan__size {
  font-weight: 400;
  color: var(--text-2, #888);
}
.plan__steps {
  margin: 0;
  padding-left: 20px;
  font-size: 13px;
  line-height: 1.9;
}
.plan__table {
  border: 1px solid var(--border-1, #333);
  border-radius: 6px;
  max-height: 260px;
  overflow-y: auto;
}
.plan__row {
  display: grid;
  grid-template-columns: 64px 1fr 180px 180px;
  gap: 8px;
  padding: 5px 10px;
  font-size: 12px;
  border-bottom: 1px solid var(--border-1, #2a2a2a);
  align-items: center;
}
.plan__row--head {
  font-weight: 600;
  background: rgba(255, 255, 255, 0.03);
  position: sticky;
  top: 0;
}
.plan__c2,
.plan__c3,
.plan__c4 {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, monospace);
}
.plan__warn {
  margin-top: 10px;
  font-size: 12px;
  color: var(--warning-color, #e0a95c);
}
</style>
