<script setup lang="ts">
/**
 * 操作快照窗口：跨主机的环境变更总览与快照回看。
 *
 * 与环境管理里的「操作记录」页签分工不同 —— 那边是"这台机器上刚才做了什么"，
 * 这边是"所有机器上一共做过什么、各自留下了什么快照、跟上一次比装了什么"。
 *
 * 这个窗口只读：它不发起变更，唯一的写操作是"回滚"，且复用环境管理那套
 * 预检 → 执行对话框，回滚逻辑一行都不在这里重写。
 *
 * 快照内容是按需拉的：列表只带落盘统计（有没有、多大、多少个包），
 * 选中某一条才去解析 packages.tsv —— 解析要借目标机的包管理器，是有代价的。
 */
import { computed, onMounted, ref, watch } from 'vue';
import { NAlert, NButton, NInput, NSelect, NTag } from 'naive-ui';

import {
  ApiError,
  api,
  type EnvOp,
  type EnvPlan,
  type EnvRunRequest,
  type SnapshotDetail,
  type SnapshotOp,
} from '../api/client';
import { formatBytes, formatTime } from '../api/format';

import EnvOpDialog from './EnvOpDialog.vue';
import EnvPlanDialog from './EnvPlanDialog.vue';
import EnvRunDialog from './EnvRunDialog.vue';

/** 差异与包清单在界面上最多各显示多少条，其余只给总数。 */
const ROW_LIMIT = 60;

const ops = ref<SnapshotOp[]>([]);
const loading = ref(false);
const lastError = ref('');

const hostFilter = ref<number | 'all'>('all');
const actionFilter = ref<string>('all');
const selectedId = ref<number | null>(null);

const detail = ref<SnapshotDetail | null>(null);
const detailLoading = ref(false);
/** 包清单的关键字过滤：只在本地过滤已经取回来的这份清单。 */
const keyword = ref('');

const opShow = ref(false);
const opId = ref<number | null>(null);

const planShow = ref(false);
const planLoading = ref(false);
const plan = ref<EnvPlan | null>(null);
const runShow = ref(false);
const runRequest = ref<EnvRunRequest | null>(null);

const OP_STATUS: Record<EnvOp['status'], { label: string; type: 'info' | 'success' | 'error' | 'warning' }> = {
  running: { label: '执行中', type: 'info' },
  success: { label: '成功', type: 'success' },
  failed: { label: '失败', type: 'error' },
  interrupted: { label: '已中断', type: 'warning' },
};

/** 动作名 -> 显示文案。与环境管理里的写法一致，两处不能各说各话。 */
const ACTION_TEXT: Record<string, string> = {
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

const ACTION_OPTIONS = [
  { label: '全部动作', value: 'all' },
  { label: '安装', value: 'install' },
  { label: '升级', value: 'upgrade' },
  { label: '卸载', value: 'remove' },
  { label: '全部升级', value: 'upgrade-all' },
  { label: '切换安装源', value: 'mirror' },
  { label: '回滚', value: 'rollback' },
  { label: '环境变量', value: 'env' },
];

/** 主机筛选项由列表自身归纳：快照记录里的主机名就是它的显示名。 */
const hostOptions = computed(() => {
  const seen = new Map<number, string>();
  for (const op of ops.value) {
    if (!seen.has(op.hostId)) seen.set(op.hostId, op.hostName || `#${op.hostId}`);
  }
  return [
    { label: '全部主机', value: 'all' as const },
    ...[...seen].map(([id, name]) => ({ label: name, value: id })),
  ];
});

const filtered = computed(() =>
  ops.value.filter((op) => {
    if (hostFilter.value !== 'all' && op.hostId !== hostFilter.value) return false;
    if (actionFilter.value === 'env') return op.action.startsWith('env-');
    if (actionFilter.value !== 'all' && op.action !== actionFilter.value) return false;
    return true;
  }),
);

const selected = computed(() => ops.value.find((op) => op.id === selectedId.value) ?? null);

/**
 * 这条操作能不能从这里回滚。
 *
 * 与环境管理的判定一致，只多一条：环境变量类操作（env-*）不能走包变更的
 * WebSocket 通道，它们只有一个"把文件还原成操作前"的同步接口，入口在
 * 环境管理里 —— 在这里给个按钮只会点出一个错误。
 */
const canRollback = computed(() => {
  const op = selected.value;
  if (!op) return false;
  return (op.status === 'success' || op.status === 'failed') && !op.action.startsWith('env-');
});

/** 包清单按关键字本地过滤：快照是历史数据，没必要为一次搜索再连目标机。 */
const filteredPackages = computed(() => {
  const list = detail.value?.packages ?? [];
  const kw = keyword.value.trim().toLowerCase();
  if (!kw) return list;
  return list.filter((p) => p.name.toLowerCase().includes(kw));
});

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

function actionText(action: string): string {
  return ACTION_TEXT[action] ?? action;
}

/** 列表项的 LED 反映这条操作整体怎么样：中断、补偿、复核不过都要看得出来。 */
function opLed(op: SnapshotOp): string {
  if (op.status === 'running') return 'led--sig led--live';
  if (op.status === 'interrupted') return 'led--warn';
  if (op.status === 'failed') return 'led--danger';
  if (!op.verified || op.compensated) return 'led--warn';
  return 'led--ok';
}

/** 快照标记：有快照 / 已被清理 / 没留下快照，三者在列表上必须能一眼分开。 */
function snapshotLabel(op: SnapshotOp): string {
  if (op.hasSnapshot) return `${op.packageCount} 个包`;
  if (op.pruned) return '快照已清理';
  return '无快照';
}

async function load(): Promise<void> {
  loading.value = true;
  try {
    const res = await api.snapshots.list();
    ops.value = res.ops;
    lastError.value = '';
    // 尽量保持原来选中的那条，它不在列表里了就退回第一条 ——
    // 省掉"打开窗口先点一下"这一步，回滚之后列表刷新也不会丢掉选中项。
    const wanted = ops.value.some((op) => op.id === selectedId.value)
      ? selectedId.value
      : (filtered.value[0]?.id ?? ops.value[0]?.id ?? null);
    await select(wanted);
  } catch (err) {
    lastError.value = describe(err, '加载操作快照失败');
  } finally {
    loading.value = false;
  }
}

async function select(id: number | null): Promise<void> {
  selectedId.value = id;
  detail.value = null;
  keyword.value = '';
  if (id === null) return;

  detailLoading.value = true;
  try {
    const res = await api.snapshots.get(id);
    // 请求返回前用户可能已经点了别的记录，丢弃这次结果避免串台。
    if (selectedId.value === id) detail.value = res;
  } catch (err) {
    if (selectedId.value === id) lastError.value = describe(err, '加载快照内容失败');
  } finally {
    detailLoading.value = false;
  }
}

function openSteps(): void {
  if (!selected.value) return;
  opId.value = selected.value.id;
  opShow.value = true;
}

/**
 * 从步骤日志对话框里点了"回滚这次操作"。
 *
 * 走的仍是同一套预检链，只是先把对话框收起来 —— 两个模态叠在一起时，
 * 用户分不清哪一个才是"现在要确认的东西"。
 */
function rollbackFromDialog(): void {
  opShow.value = false;
  void rollback();
}

async function rollback(): Promise<void> {
  const op = selected.value;
  if (!op) return;
  plan.value = null;
  planShow.value = true;
  planLoading.value = true;
  try {
    plan.value = await api.env.plan(op.hostId, { action: 'rollback', originOpId: op.id });
  } catch (err) {
    planShow.value = false;
    lastError.value = describe(err, '回滚预检失败');
  } finally {
    planLoading.value = false;
  }
}

function confirmPlan(): void {
  const op = selected.value;
  if (!op) return;
  runRequest.value = { action: 'rollback', originOpId: op.id };
  planShow.value = false;
  runShow.value = true;
}

/** 回滚完成：列表与当前详情都可能变了（多了那条回滚操作、快照目录多了几份）。 */
async function afterRun(): Promise<void> {
  runShow.value = false;
  await load();
}

onMounted(load);

// 筛选条件变了之后，选中的那条可能已经被过滤掉：跟着换到可见的第一条，
// 否则右侧会一直停着一条"列表里根本找不到"的记录。
watch(filtered, (list) => {
  if (list.length === 0) return;
  if (!list.some((op) => op.id === selectedId.value)) void select(list[0].id);
});
</script>

<template>
  <div class="snap">
    <div class="snap__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">SNAPSHOTS</p>
        <span class="page-title">操作快照</span>
      </div>
      <n-select
        v-model:value="hostFilter"
        class="snap__filter"
        size="small"
        :options="hostOptions"
        aria-label="按主机筛选"
      />
      <n-select
        v-model:value="actionFilter"
        class="snap__filter"
        size="small"
        :options="ACTION_OPTIONS"
        aria-label="按动作筛选"
      />
      <span class="snap__count num">{{ filtered.length }} 条</span>
      <span class="snap__spacer" />
      <n-button size="small" :loading="loading" @click="load">刷新</n-button>
    </div>

    <p v-if="lastError" class="snap__error">{{ lastError }}</p>

    <div class="snap__body">
      <div class="snap__list">
        <button
          v-for="op in filtered"
          :key="op.id"
          class="snap__item"
          :class="{ 'is-active': op.id === selectedId }"
          @click="select(op.id)"
        >
          <span class="led" :class="opLed(op)" aria-hidden="true" />
          <span class="snap__item-main">
            <span class="snap__item-title" :title="op.target">
              {{ actionText(op.action) }} · {{ op.target }}
            </span>
            <span class="snap__item-meta num">
              #{{ op.id }} · {{ op.hostName || `#${op.hostId}` }} · {{ formatTime(op.createdAt) }}
            </span>
          </span>
          <span class="snap__item-stat num" :class="{ 'is-dim': !op.hasSnapshot }">
            {{ snapshotLabel(op) }}
          </span>
        </button>

        <p v-if="!loading && filtered.length === 0" class="snap__empty">
          {{
            ops.length === 0
              ? '还没有环境变更记录。在「环境管理」里装一次软件，这里就会出现它的快照。'
              : '当前筛选条件下没有记录。'
          }}
        </p>
      </div>

      <div class="snap__detail">
        <template v-if="selected">
          <header class="snap__head">
            <span class="led" :class="opLed(selected)" aria-hidden="true" />
            <span class="snap__title num">#{{ selected.id }}</span>
            <n-tag size="small" :bordered="false">{{ actionText(selected.action) }}</n-tag>
            <n-tag size="small" :type="OP_STATUS[selected.status].type" :bordered="false">
              {{ OP_STATUS[selected.status].label }}
            </n-tag>
            <n-tag v-if="selected.compensated" size="small" type="warning" :bordered="false">已补偿</n-tag>
            <n-tag v-if="selected.verified" size="small" type="success" :bordered="false">复核通过</n-tag>
            <span class="snap__spacer" />
            <span class="snap__time num">
              {{ formatTime(selected.createdAt) }}
              <template v-if="selected.status !== 'running'"> → {{ formatTime(selected.finishedAt) }}</template>
            </span>
          </header>

          <p class="snap__target">
            {{ selected.hostName || `#${selected.hostId}` }} · {{ selected.target }}
            <template v-if="selected.originOpId"> · 来源操作 #{{ selected.originOpId }}</template>
          </p>
          <p v-if="selected.summary" class="snap__summary">{{ selected.summary }}</p>
          <p v-if="selected.verifyNote" class="snap__verify">复核：{{ selected.verifyNote }}</p>

          <p v-if="detailLoading" class="snap__empty">正在读取快照…</p>

          <template v-else-if="detail">
            <n-alert v-if="!detail.available" type="warning" title="这条操作没有可读的快照">
              {{ detail.reason || detail.note }}
            </n-alert>

            <template v-else>
              <n-alert v-if="detail.note" type="info" class="snap__note">{{ detail.note }}</n-alert>

              <p class="snap__stat num">
                <template v-if="detail.parsed">
                  快照收录 {{ detail.packageTotal }} 个包（{{ detail.family }}）
                  <template v-if="detail.truncated">· 界面只列出前 {{ detail.packages.length }} 个</template>
                </template>
                <template v-else>包清单按原文展示（共 {{ detail.packageTotal }} 行）</template>
                · 安装源快照
                {{ detail.sourcesBytes > 0 ? formatBytes(detail.sourcesBytes) : '未留存' }}
              </p>

              <!--
                差异是这个窗口的主要信息：快照本身只是"那一刻装了什么"，
                用户真正要看的是"这次变更比上次多了/少了/升级了什么"。
              -->
              <div v-if="detail.diff" class="snap__diff">
                <div class="snap__diff-head">
                  <span class="silk">与上次快照相比</span>
                  <span class="num">#{{ detail.diff.baseOpId }} · {{ formatTime(detail.diff.baseAt) }}</span>
                  <span class="snap__diff-sum num">
                    新增 {{ detail.diff.added.length }} · 移除 {{ detail.diff.removed.length }} · 版本变化
                    {{ detail.diff.changed.length }}
                  </span>
                </div>

                <p
                  v-if="
                    detail.diff.added.length === 0 &&
                    detail.diff.removed.length === 0 &&
                    detail.diff.changed.length === 0
                  "
                  class="snap__empty"
                >
                  两次快照的包清单完全一致。
                </p>

                <div v-else class="snap__diff-cols">
                  <div class="snap__diff-col">
                    <div class="snap__diff-title">新增（{{ detail.diff.added.length }}）</div>
                    <div v-for="p in detail.diff.added.slice(0, ROW_LIMIT)" :key="p.name" class="snap__pkg num">
                      <span class="snap__pkg-name" :title="p.name">{{ p.name }}</span>
                      <span class="snap__pkg-ver" :title="p.version">{{ p.version }}</span>
                    </div>
                    <p v-if="detail.diff.added.length > ROW_LIMIT" class="snap__more num">
                      还有 {{ detail.diff.added.length - ROW_LIMIT }} 个未列出
                    </p>
                    <p v-if="detail.diff.added.length === 0" class="snap__empty">无</p>
                  </div>

                  <div class="snap__diff-col">
                    <div class="snap__diff-title">移除（{{ detail.diff.removed.length }}）</div>
                    <div
                      v-for="p in detail.diff.removed.slice(0, ROW_LIMIT)"
                      :key="p.name"
                      class="snap__pkg num"
                    >
                      <span class="snap__pkg-name" :title="p.name">{{ p.name }}</span>
                      <span class="snap__pkg-ver" :title="p.version">{{ p.version }}</span>
                    </div>
                    <p v-if="detail.diff.removed.length > ROW_LIMIT" class="snap__more num">
                      还有 {{ detail.diff.removed.length - ROW_LIMIT }} 个未列出
                    </p>
                    <p v-if="detail.diff.removed.length === 0" class="snap__empty">无</p>
                  </div>

                  <div class="snap__diff-col">
                    <div class="snap__diff-title">版本变化（{{ detail.diff.changed.length }}）</div>
                    <div v-for="c in detail.diff.changed.slice(0, ROW_LIMIT)" :key="c.name" class="snap__pkg num">
                      <span class="snap__pkg-name" :title="c.name">{{ c.name }}</span>
                      <span class="snap__pkg-ver" :title="`${c.from} → ${c.to}`">{{ c.from }} → {{ c.to }}</span>
                    </div>
                    <p v-if="detail.diff.changed.length > ROW_LIMIT" class="snap__more num">
                      还有 {{ detail.diff.changed.length - ROW_LIMIT }} 个未列出
                    </p>
                    <p v-if="detail.diff.changed.length === 0" class="snap__empty">无</p>
                  </div>
                </div>
              </div>
              <p v-else class="snap__empty">
                没有可对比的基准：同一主机上更早的快照都已被保留策略清理，这是现存最早的一条。
              </p>

              <div class="snap__packages">
                <div class="snap__pkg-bar">
                  <span class="silk">快照里的包清单</span>
                  <n-input
                    v-if="detail.parsed"
                    v-model:value="keyword"
                    class="snap__search"
                    size="small"
                    clearable
                    placeholder="按包名过滤"
                    aria-label="按包名过滤"
                  />
                  <span v-if="detail.parsed" class="num snap__pkg-count">
                    {{ filteredPackages.length }} / {{ detail.packages.length }}
                  </span>
                </div>

                <div v-if="detail.parsed" class="snap__pkg-list">
                  <div v-for="p in filteredPackages" :key="p.name" class="snap__pkg num">
                    <span class="snap__pkg-name" :title="p.name">{{ p.name }}</span>
                    <span class="snap__pkg-ver" :title="p.version">{{ p.version }}</span>
                  </div>
                  <p v-if="filteredPackages.length === 0" class="snap__empty">没有匹配的包。</p>
                </div>

                <!-- 解析不了（拿不到系统画像）时退回原文：历史必须永远读得到。 -->
                <pre v-else class="snap__raw">{{ detail.raw }}</pre>
              </div>
            </template>
          </template>

          <div class="snap__actions">
            <n-button size="small" @click="openSteps">查看步骤日志</n-button>
            <n-button v-if="canRollback" size="small" type="warning" @click="rollback">
              回滚这次操作
            </n-button>
            <span v-else-if="selected.action.startsWith('env-')" class="snap__hint">
              环境变量类操作不可从快照回滚，请到「环境管理 → 操作记录」里还原
            </span>
          </div>
        </template>

        <p v-else class="snap__empty">左侧选择一条操作记录查看它的快照。</p>
      </div>
    </div>

    <EnvOpDialog v-model:show="opShow" :op-id="opId" @rollback="rollbackFromDialog" />
    <EnvPlanDialog
      v-model:show="planShow"
      :plan="plan"
      :loading="planLoading"
      @confirm="confirmPlan"
    />
    <EnvRunDialog
      v-model:show="runShow"
      :host-id="selected?.hostId ?? 0"
      :host-name="selected?.hostName ?? ''"
      :request="runRequest"
      @done="afterRun"
    />
  </div>
</template>