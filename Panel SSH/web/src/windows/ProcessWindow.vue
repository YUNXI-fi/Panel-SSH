<script setup lang="ts">
/**
 * 进程管理窗口。
 *
 * 原先进程表和指标卡挤在同一个「监控」窗口里：一台忙机器上进程表有几百行、
 * 九列，分到窗口下半截后可视高度只够看几行，横向也要和指标卡抢宽度。
 * 拆成独立窗口后它可以独占整屏高度。
 *
 * 两种打开方式：
 *   - 主机管理那一行点「进程」——带 hostId，直接采集；
 *   - 桌面「进程管理」图标——没有主机上下文，先让用户选一台。
 */
import { computed, h, onMounted, onUnmounted, ref, watch } from 'vue';
import {
  NAlert,
  NButton,
  NDataTable,
  NInput,
  NModal,
  NSpace,
  useMessage,
  type DataTableColumns,
  type InputInst,
} from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import { useVisiblePoll } from '../desktop/useVisiblePoll';
import { ApiError, api, type EscalationInfo, type Process } from '../api/client';
import { formatBytes } from '../api/format';

const props = defineProps<{ hostId?: number; hostName?: string }>();

const message = useMessage();

// ps 相对 /proc 读取更重，进程表按 3 秒刷新已经足够。
const PROCESS_INTERVAL_MS = 3000;

const root = ref<HTMLElement | null>(null);

// 窗口自己持有目标主机：带 hostId 打开时是它，从桌面图标打开时由选择器填充。
const target = ref<number | null>(props.hostId ?? null);
/** 目标显示名，供标题使用：从桌面打开时它由选择器填充（含「宿主机」）。 */
const targetName = ref(props.hostName ?? '');

watch(
  () => props.hostId,
  (id) => {
    if (id === undefined) return;
    target.value = id;
    targetName.value = props.hostName ?? '';
  },
);

const processes = ref<Process[]>([]);
const processTotal = ref(0);
/**
 * 用户可见的「正在取数据」：首次进入、换主机、手动刷新、结束进程后重取。
 * 轮询不置位它 —— 每 3 秒闪一次的按钮转圈和表格遮罩一样，只属于噪音。
 */
const busy = ref(false);
const lastError = ref('');
const filter = ref('');
/** 最近一次成功拉取的时刻，给工具条上的「更新于」用。 */
const updatedAt = ref('');

/** 状态筛选：与文本筛选叠加，回答「只看正在跑的」这类最常见的诉求。 */
type StateFilter = 'all' | 'run' | 'sleep' | 'zombie';
const stateFilter = ref<StateFilter>('all');
const stateFilters: { value: StateFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'run', label: '运行' },
  { value: 'sleep', label: '睡眠' },
  { value: 'zombie', label: '僵尸' },
];

const filterInput = ref<InputInst | null>(null);

const killTarget = ref<Process | null>(null);
const killing = ref(false);
// 目标机的提权方式：结束别人的进程要 root，弹窗前据此提醒用户；
// 无法提权的机器则提前说明这次注定失败，而不是让他点了才知道。
const escalation = ref<EscalationInfo | null>(null);

// n-modal 需要布尔型的 show，而这里的目标本身是可空的进程对象，
// 用带 setter 的计算属性把两者桥接起来。
const showKill = computed({
  get: () => killTarget.value !== null,
  set: (value: boolean) => {
    if (!value) killTarget.value = null;
  },
});

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 504) return `${err.message}。请检查目标机负载与网络`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

/**
 * 取目标机的提权方式，供结束进程前的提醒使用。
 *
 * 不复用进程列表那条链路：提醒是辅助信息，它的失败不该影响进程列表的展示，
 * 也就不该出现在窗口顶部的错误栏里。
 */
async function loadEscalation(): Promise<void> {
  const id = target.value;
  if (id === null) return;
  try {
    escalation.value = await api.hosts.escalation(id);
  } catch {
    // 取不到就不显示提醒，静默处理即可：这只是辅助信息，用报错打扰用户反而添乱。
    escalation.value = null;
  }
}

/** 会以 root 执行时的提醒。已以 root 登录的机器不提示 —— 这句话对它没有信息量。 */
const escalationInfo = computed(() => {
  const info = escalation.value;
  if (!info || info.mode === 'root' || !info.escalatable) return '';
  return `本次操作会以 root 身份执行：面板将自动提权（${info.label}）。`;
});

/**
 * 无法提权时的警告。
 *
 * 措辞与存储/网络不同：结束进程是"能提权就提权"（后端用 escalatePreferred）——
 * 杀自己的进程本来就不需要 root，所以这里不能说"操作会失败"，只能说清
 * 哪一类进程会失败。
 */
const escalationWarning = computed(() => {
  const info = escalation.value;
  if (!info || info.mode === 'root' || info.escalatable) return '';
  return '该主机无法提权：结束其他用户（含 root）的进程会失败，只有你自己的进程能被结束。若要结束别人的进程，请在面板里为该主机配置提权口令（root 口令），或以 root 登录后再试。';
});

async function refreshProcesses(options: { fresh?: boolean; silent?: boolean } = {}): Promise<void> {
  const id = target.value;
  // 还没选目标主机时不该发请求 —— 也挡住了轮询在选主机前空转。
  if (id === null) return;

  if (!options.silent) busy.value = true;
  try {
    const data = await api.metrics.processes(id, options.fresh ?? false);
    processes.value = data.processes;
    processTotal.value = data.total;
    lastError.value = '';
    updatedAt.value = new Date().toLocaleTimeString('zh-CN', { hour12: false });
  } catch (err) {
    lastError.value = describe(err, '读取进程列表失败');
  } finally {
    if (!options.silent) busy.value = false;
  }
}

useVisiblePoll({
  root,
  intervalMs: PROCESS_INTERVAL_MS,
  task: () => refreshProcesses({ silent: true }),
  enabled: () => target.value !== null,
});

watch(target, (id) => {
  if (id === null) return;
  // 换主机时先清空，否则新主机的数据到达前，表格里还挂着上一台的进程，
  // 这时候点「结束」会打到错误的机器上。
  processes.value = [];
  processTotal.value = 0;
  updatedAt.value = '';
  killTarget.value = null;
  void refreshProcesses({ fresh: true });
  void loadEscalation();
});

/**
 * 表格遮罩：只在表格还空着的时候盖。
 * 表里已经有数据时刷新不该蒙层 —— 那些数据仍然可读，蒙一层等于让整张表
 * “消失”一瞬，3 秒一次就是持续闪烁。
 */
const tableLoading = computed(() => busy.value && processes.value.length === 0);

/** 进程过滤：一台忙机器上进程表有几百行，靠肉眼找某一类进程并不现实。 */
const filteredProcesses = computed(() => {
  const kw = filter.value.trim().toLowerCase();
  return processes.value.filter((p) => {
    if (!matchesState(p)) return false;
    if (kw === '') return true;
    return `${p.pid} ${p.user} ${p.command} ${p.state}`.toLowerCase().includes(kw);
  });
});

/** 取 ps 状态码首字母，与表格里的状态胶囊用同一套判定。 */
function stateKey(state: string): string {
  return (state || '').trim().charAt(0).toUpperCase();
}

function matchesState(proc: Process): boolean {
  const key = stateKey(proc.state);
  if (stateFilter.value === 'run') return key === 'R';
  if (stateFilter.value === 'sleep') return key === 'S' || key === 'I';
  if (stateFilter.value === 'zombie') return key === 'Z';
  return true;
}

/**
 * 从完整命令里取进程名。
 *
 * ps 只给了 command，没有单独的 comm 字段；取第一个 token 去路径、去尾部冒号
 * （`postgres: checkpointer` → `postgres`），内核线程保留方括号形式。
 */
function procName(command: string): string {
  const cmd = (command || '').trim();
  if (cmd === '') return '—';
  if (cmd.startsWith('[')) {
    const end = cmd.indexOf(']');
    return end > 0 ? cmd.slice(0, end + 1) : cmd;
  }
  const head = cmd.split(/\s+/)[0];
  const base = head.split('/').pop() || head;
  return base.replace(/:$/, '') || '—';
}

/**
 * 斜杠键聚焦筛选框：设计稿把「/」画成了搜索框的一部分，
 * 这个提示必须真的能用，否则就只是装饰。
 */
function onSlashFocus(event: KeyboardEvent): void {
  if (event.key !== '/' || event.ctrlKey || event.metaKey || event.altKey) return;
  const el = root.value;
  // 只在当前窗口被聚焦时响应：进程窗口可能开着但被别的窗口盖住。
  if (!el || !el.closest('.win--active')) return;
  const target = event.target as HTMLElement | null;
  if (target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable)) {
    return;
  }
  event.preventDefault();
  filterInput.value?.focus();
}

onMounted(() => window.addEventListener('keydown', onSlashFocus));
onUnmounted(() => window.removeEventListener('keydown', onSlashFocus));

function askKill(proc: Process): void {
  killTarget.value = proc;
}

async function confirmKill(signal: 'TERM' | 'KILL'): Promise<void> {
  const proc = killTarget.value;
  const id = target.value;
  if (!proc || id === null) return;

  killing.value = true;
  try {
    await api.metrics.kill(id, proc.pid, signal);
    killTarget.value = null;
    message.success(`已向 PID ${proc.pid} 发送 SIG${signal}`);
    // 强制刷新：被结束的进程必须立刻从列表里消失，而不是等缓存过期。
    await refreshProcesses({ fresh: true });
  } catch (err) {
    message.error(describe(err, '结束进程失败'));
  } finally {
    killing.value = false;
  }
}

/**
 * ps 的状态码映射成人话与语义色。
 *
 * 原始状态是 R/S/D/T/Z 这类单字母，直接摆在表里只有熟悉 ps 的人看得懂；
 * 颜色只给需要被注意的状态（僵尸/停止/不可中断），运行中与睡眠保持中性。
 */
function stateView(state: string): { label: string; tone: string } {
  const key = stateKey(state);
  if (key === '') return { label: '—', tone: 'is-off' };
  if (key === 'R') return { label: '运行', tone: '' };
  if (key === 'Z') return { label: '僵尸', tone: 'is-danger' };
  if (key === 'T') return { label: '已停止', tone: 'is-warn' };
  if (key === 'D') return { label: '不可中断', tone: 'is-warn' };
  if (key === 'S' || key === 'I') return { label: '睡眠', tone: 'is-off' };
  return { label: state.trim(), tone: 'is-off' };
}

const processColumns: DataTableColumns<Process> = [
  {
    title: 'PID',
    key: 'pid',
    width: 84,
    sorter: (a, b) => a.pid - b.pid,
    render: (row) => h('span', { class: 'num' }, String(row.pid)),
  },
  {
    // 命令的首段（可执行名）单独成列：一眼能认出"这是什么进程"，
    // 完整命令行放在最后一列，窗口变窄时先省略参数而不是把名字挤掉。
    title: '进程',
    key: 'name',
    width: 150,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'procwin__name' }, procName(row.command)),
  },
  {
    title: '用户',
    key: 'user',
    width: 104,
    render: (row) => h('span', { class: 'num' }, row.user),
  },
  {
    // 不带 %：这一列取自 ps 的 pcpu，是"CPU 时间 / 已运行时间"的比值，
    // 多线程进程会占满多个核而超过 100（8 核机器上可到 800），
    // 写成 % 会让人以为它封顶 100。条形按 100% 封顶，超额部分不再撑宽。
    title: 'CPU',
    key: 'cpu',
    width: 112,
    sorter: (a, b) => a.cpu - b.cpu,
    defaultSortOrder: 'descend',
    render: (row) => {
      const width = Math.min(100, Math.max(0, row.cpu));
      return h('div', { class: 'procwin__cpu' }, [
        h('span', { class: 'ui-bar procwin__cpu-track' }, [
          h('i', { class: row.cpu >= 50 ? 'is-warn' : 'is-sig', style: { width: `${width}%` } }),
        ]),
        h('b', { class: 'num' }, row.cpu.toFixed(1)),
      ]);
    },
  },
  {
    title: '内存',
    key: 'mem',
    width: 76,
    sorter: (a, b) => a.mem - b.mem,
    render: (row) => h('span', { class: 'num' }, row.mem.toFixed(1)),
  },
  {
    title: '常驻内存',
    key: 'rssKiB',
    width: 100,
    sorter: (a, b) => a.rssKiB - b.rssKiB,
    render: (row) => h('span', { class: 'num' }, formatBytes(row.rssKiB * 1024)),
  },
  {
    title: '状态',
    key: 'state',
    width: 92,
    render: (row) => {
      const view = stateView(row.state);
      return h('span', { class: ['ui-status', view.tone] }, [h('i'), view.label]);
    },
  },
  {
    title: '已运行',
    key: 'elapsed',
    width: 100,
    render: (row) => h('span', { class: 'num' }, row.elapsed),
  },
  {
    title: '命令',
    key: 'command',
    minWidth: 260,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'procwin__cmd' }, row.command),
  },
  {
    title: '操作',
    key: 'actions',
    width: 78,
    render: (row) =>
      h(
        NButton,
        { size: 'small', quaternary: true, type: 'error', onClick: () => askKill(row) },
        { default: () => '结束' },
      ),
  },
];

const ledClass = computed(() => {
  if (lastError.value) return 'led--danger';
  if (target.value === null) return 'led--sig led--live';
  return 'led--ok';
});
</script>

<template>
  <div ref="root" class="procwin">
    <div class="procwin__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">PROCESSES</p>
        <span class="procwin__title page-title">
          <span class="led" :class="ledClass" aria-hidden="true" />
          进程管理
        </span>
        <span class="procwin__note page-meta">
          {{ targetName || '未选择主机' }} · {{ processTotal }} 个进程
          <template v-if="processes.length < processTotal">，展示前 {{ processes.length }} 个</template>
          <template v-if="filter || stateFilter !== 'all'">· 匹配 {{ filteredProcesses.length }} 个</template>
        </span>
      </div>

      <!-- 主机胶囊放页头右端：它和标题回答的是同一个问题（现在看的是哪台机器）。 -->
      <host-picker
        v-if="props.hostId === undefined"
        v-model:host-id="target"
        v-model:host-name="targetName"
        include-local
      />
    </div>

    <div class="ui-toolbar">
      <n-input
        ref="filterInput"
        v-model:value="filter"
        class="procwin__filter"
        size="small"
        clearable
        placeholder="搜索 PID / 用户 / 命令…"
        :input-props="{ 'aria-label': '筛选进程' }"
      >
        <template #prefix>
          <svg class="procwin__search" viewBox="0 0 24 24" aria-hidden="true">
            <circle cx="11" cy="11" r="6" />
            <path d="m16 16 4 4" />
          </svg>
        </template>
        <template #suffix>
          <kbd class="procwin__kbd">/</kbd>
        </template>
      </n-input>

      <!-- 状态胶囊：把 ps 的单字母状态翻成最常用的四种口径，与文本筛选叠加。 -->
      <div class="ui-chips">
        <button
          v-for="chip in stateFilters"
          :key="chip.value"
          type="button"
          class="ui-chip"
          :class="{ 'is-active': stateFilter === chip.value }"
          @click="stateFilter = chip.value"
        >
          {{ chip.label }}
        </button>
      </div>

      <div class="ui-toolbar__right">
        <span v-if="updatedAt" class="procwin__stamp">更新于 {{ updatedAt }}</span>
        <n-button size="small" :loading="busy" @click="refreshProcesses({ fresh: true })">
          刷新
        </n-button>
      </div>
    </div>

    <div v-if="lastError" class="procwin__error">
      <span class="led led--danger" aria-hidden="true" />
      {{ lastError }}
    </div>

    <section class="ui-card ui-card--flush procwin__card">
      <div class="ui-card__head">
        <div class="ui-card__ident">
          <h2 class="ui-card__title">进程列表</h2>
        </div>
        <span class="ui-card__meta">
          {{ filteredProcesses.length }} / {{ processTotal }}
        </span>
      </div>

      <n-data-table
        class="procwin__table"
        :columns="processColumns"
        :data="filteredProcesses"
        :loading="tableLoading"
        :bordered="false"
        :row-key="(row: Process) => row.pid"
        size="small"
        flex-height
      />
    </section>

    <n-modal v-model:show="showKill" preset="card" title="结束进程" style="width: 480px">
      <p class="kill__hint">
        即将结束 <strong>PID {{ killTarget?.pid }}</strong>（{{ killTarget?.command }}）。
      </p>
      <p class="kill__hint">
        默认先发送 SIGTERM，让进程有机会保存数据并释放资源；仅当它无响应时才使用强制结束。
      </p>
      <!-- 提权提醒放在说明之后、按钮之前：这里没有 flex 布局，放末尾就不必再补间距 -->
      <n-alert v-if="escalationWarning" type="error">{{ escalationWarning }}</n-alert>
      <n-alert v-else-if="escalationInfo" type="info">{{ escalationInfo }}</n-alert>
      <template #footer>
        <n-space justify="end">
          <n-button @click="killTarget = null">取消</n-button>
          <n-button :loading="killing" @click="confirmKill('TERM')">发送 SIGTERM</n-button>
          <n-button type="error" :loading="killing" @click="confirmKill('KILL')">强制结束</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>
