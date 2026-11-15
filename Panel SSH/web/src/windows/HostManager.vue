<script setup lang="ts">
import { computed, h, inject, onMounted, onUnmounted, ref, watch } from 'vue';
import {
  NButton,
  NDataTable,
  NDropdown,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NModal,
  NRadioButton,
  NRadioGroup,
  NSpace,
  useDialog,
  useMessage,
  type DataTableColumns,
  type DropdownOption,
} from 'naive-ui';

import { ApiError, api, type AuthType, type Host, type HostForm, type HostMetricsBrief, type HostStatus, type LocalPrivilege, type Metrics, type TestResult } from '../api/client';
import { formatBytes, formatLatency, formatRate, formatTestResult } from '../api/format';
import Sparkline from '../components/Sparkline.vue';
import { useVisiblePoll } from '../desktop/useVisiblePoll';
import { WindowManagerKey } from '../desktop/useWindowManager';

/**
 * 这台主机能不能登录。
 *
 * 三种情况都算：配了凭据；目标机不需要凭据（none）；用面板本机的密钥（localkey）。
 * 只有"选了密码/私钥但凭据留空"是登录不了的 —— 那是占位主机。
 */
function canLogin(row: Host): boolean {
  return row.hasSecret || row.authType === 'none' || row.authType === 'localkey';
}

/** 这种认证方式需不需要用户填凭据输入框。 */
function needsSecret(authType: AuthType): boolean {
  return authType !== 'none' && authType !== 'localkey';
}

/**
 * 从粘贴进来的连接串里拆出地址、端口、用户名。
 *
 * 支持的都是"从终端复制过来"的常见形态：
 *   user@host
 *   user@host:2222
 *   host:2222
 *   ssh -p 2222 user@host
 *   ssh user@host -p 2222
 *
 * 只做能确定的拆分。认不出的部分原样保留 —— 地址被悄悄改错，
 * 比多问用户一次更糟（现象会变成 no such host，而原因藏在输入框里）。
 * IPv6 的 [::1]:22 不拆端口：冒号歧义太大，原样交给后端。
 */
function parseTargetInput(raw: string): { addr: string; port?: number; username?: string } {
  let target = raw.trim();
  let port: number | undefined;

  // 整条 ssh 命令：取 -p 的端口，再去掉命令名与选项，剩下的目标 token。
  if (/^ssh(\s|$)/i.test(target)) {
    const portMatch = target.match(/(?:^|\s)-p\s*(\d+)/);
    if (portMatch) port = Number(portMatch[1]);

    const tokens = target.split(/\s+/).slice(1);
    const candidate = tokens
      .filter((token) => token !== '' && !token.startsWith('-'))
      // -p 后面那个数字不是目标。
      .filter((token) => !/^\d+$/.test(token))
      .pop();
    if (candidate) target = candidate;
  }

  // user@host
  let username: string | undefined;
  const at = target.lastIndexOf('@');
  if (at > 0) {
    username = target.slice(0, at);
    target = target.slice(at + 1);
  }

  // host:port
  if (!target.startsWith('[')) {
    const colon = target.lastIndexOf(':');
    if (colon > 0 && /^\d+$/.test(target.slice(colon + 1))) {
      port ??= Number(target.slice(colon + 1));
      target = target.slice(0, colon);
    }
  }

  return { addr: target, port, username };
}

/**
 * 把地址框里的连接串规范成三个字段。
 *
 * 在地址框失焦时执行，用户能立刻看到拆分结果，而不是等到连接失败才发现
 * 地址里混着 user@。解析不出内容时什么都不改。
 */
function normalizeTarget(): void {
  const parsed = parseTargetInput(form.value.addr);
  if (!parsed.addr) return;

  form.value.addr = parsed.addr;
  if (parsed.port !== undefined) form.value.port = parsed.port;
  if (parsed.username) form.value.username = parsed.username;
}

// 表单独立于 HostForm：n-select / n-input-number 的取值类型更宽，
// 用更宽松的本地状态承接，提交时再收敛为 HostForm。
interface FormState {
  name: string;
  addr: string;
  port: number;
  username: string;
  authType: AuthType;
  secret: string;
  /** 提权口令（root 口令）：需要 root 的操作用它回答 su 的提示，留空则改用登录凭据走 sudo。 */
  escalateSecret: string;
  group: string;
  remark: string;
}

const message = useMessage();
const dialog = useDialog();
const wm = inject(WindowManagerKey);

const hosts = ref<Host[]>([]);
const loading = ref(false);
const saving = ref(false);
const showForm = ref(false);
const editingId = ref<number | null>(null);
const keyword = ref('');
const form = ref<FormState>(blankForm());

/**
 * 视图模式：表格 / 卡片。
 *
 * 两者用的是同一份数据、同一批动作，差别只在信息密度 —— 表格适合逐列核对地址
 * 与角色，卡片适合一眼看清某台机器的整体状态。选择记在本地：它是个人偏好，
 * 不该每开一次窗口重选一次。
 */
type ViewMode = 'table' | 'card';

const VIEW_KEY = 'panelssh.hosts.view';

const viewMode = ref<ViewMode>(readViewMode());

function readViewMode(): ViewMode {
  try {
    return localStorage.getItem(VIEW_KEY) === 'card' ? 'card' : 'table';
  } catch {
    // 隐私模式下 localStorage 会抛异常：回到默认视图即可，不值得打扰用户。
    return 'table';
  }
}

watch(viewMode, (mode) => {
  try {
    localStorage.setItem(VIEW_KEY, mode);
  } catch {
    // 同上：存不下只是下次要重选一次，不影响使用。
  }
});

/** 主机 ID → 在线状态。只保存最近一轮探测的结果。 */
const statuses = ref<Map<number, HostStatus>>(new Map());

/**
 * 主机 ID → 「测试」实测的 SSH 延迟（TCP + 密钥交换 + 认证）。
 *
 * 刻意与 statuses 分开存：状态列的轮询口径是 TCP 建连（10 秒缓存），这里是
 * 用户主动测出的真实登录耗时。存在同一张表里的话，15 秒后的下一轮轮询就会
 * 把实测值抹掉 —— 用户点完看到的数字转眼变了，比不显示更糟。
 */
const measured = ref<Map<number, number>>(new Map());

/** 状态轮询间隔。与后端的探测缓存窗口（10s）配合，每轮只探一次。 */
const STATUS_POLL_MS = 15000;

let statusTimer: number | undefined;
// 上一轮还没回来就不再发起：主机都没响应时一轮可能跑满超时，
// 定时器叠加只会让请求越堆越多，数字反而更不准。
let statusPending = false;

/** 表单里的「测试」按钮：用它验证当前填进去的这份凭据能不能连上。 */
const testingDraft = ref(false);
const draftResult = ref<TestResult | null>(null);

/**
 * 表单一改，上一次的测试结论就失效了。
 * 不清掉的话，用户改完地址后那句"连接成功"还挂在那里，
 * 会让人误以为新地址也测过了 —— 比不显示更糟。
 */
watch(form, () => (draftResult.value = null), { deep: true });

const authOptions = [
  { label: '密码', value: 'password' },
  { label: '私钥', value: 'key' },
  // 目标机确实不需要凭据时选这一项（SSH none 认证，或服务端允许空密码）。
  // 它与"选了密码但留空"不同：后者是占位，登不上去。
  { label: '无需凭据', value: 'none' },
  // 用面板所在机器上已有的私钥（~/.ssh/id_ed25519 等）。
  // 适用于"本机 ssh 本来就能直接进"的机器，省掉复制私钥内容。
  { label: '本机 SSH 密钥', value: 'localkey' },
];

function blankForm(): FormState {
  return {
    name: '',
    addr: '',
    port: 22,
    username: 'root',
    authType: 'password',
    secret: '',
    escalateSecret: '',
    group: '',
    remark: '',
  };
}

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    // 会话过期需要重新登录，给出与普通错误不同的提示。
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

async function load(): Promise<void> {
  loading.value = true;
  try {
    hosts.value = await api.hosts.list();
    // 列表变化后立即补一轮状态：新增的主机不必干等到下一个轮询周期，
    // 「刷新」按钮也才是真的刷新了整屏信息。
    void refreshStatus();
    void loadLocalPrivilege();
  } catch (err) {
    message.error(describe(err, '加载主机列表失败'));
  } finally {
    loading.value = false;
  }
}

/**
 * 宿主机的提权账号。
 *
 * 宿主机不在主机表里（它没有地址、也不该被删除），因此这份配置单独读写。
 * 读失败按"未配置"显示：它是辅助信息，不该挡住主机列表。
 */
const localPriv = ref<LocalPrivilege>({ account: 'root', configured: false });
const showLocalPriv = ref(false);
const savingLocal = ref(false);
/** 提权认证方式：口令走 su / sudo -S；免密走 sudo -n，不需要保存口令。 */
type LocalAuthType = 'password' | 'nopasswd';
const localAuthOptions = [
  { label: '口令提权（su / sudo -S）', value: 'password' },
  { label: '免密 sudo（NOPASSWD）', value: 'nopasswd' },
];
const localForm = ref({ account: 'root', secret: '', authType: 'password' as LocalAuthType });

async function loadLocalPrivilege(): Promise<void> {
  try {
    localPriv.value = await api.localPrivilege.get();
  } catch {
    localPriv.value = { account: 'root', configured: false };
  }
}

function openLocalPrivilege(): void {
  localForm.value = {
    account: localPriv.value.account || 'root',
    secret: '',
    // 没有存过口令，说明走的是免密探测；打开时按现状回填。
    authType: localPriv.value.configured ? 'password' : 'nopasswd',
  };
  showLocalPriv.value = true;
}

async function saveLocalPrivilege(): Promise<void> {
  if (localForm.value.authType === 'nopasswd') {
    savingLocal.value = true;
    try {
      // 免密不需要任何凭据：清掉可能残留的口令，剩下的交给 sudo -n 探测。
      if (localPriv.value.configured) localPriv.value = await api.localPrivilege.clear();
      showLocalPriv.value = false;
      message.success('已切换为免密 sudo：不保存口令，面板将用 sudo -n 探测');
    } catch (err) {
      message.error(describe(err, '切换免密 sudo 失败'));
    } finally {
      savingLocal.value = false;
    }
    return;
  }
  if (!localForm.value.secret.trim()) {
    message.warning('请填写提权口令');
    return;
  }
  savingLocal.value = true;
  try {
    localPriv.value = await api.localPrivilege.save(
      localForm.value.account.trim() || 'root',
      localForm.value.secret,
    );
    showLocalPriv.value = false;
    message.success(
      localPriv.value.account === 'root'
        ? '已保存：宿主机将以 su 提权'
        : '已保存：宿主机将以 sudo 提权',
    );
  } catch (err) {
    message.error(describe(err, '保存提权账号失败'));
  } finally {
    savingLocal.value = false;
  }
}

async function clearLocalPrivilege(): Promise<void> {
  try {
    localPriv.value = await api.localPrivilege.clear();
    message.success('已清除宿主机的提权账号');
  } catch (err) {
    message.error(describe(err, '清除提权账号失败'));
  }
}

/**
 * 拉取一次全部主机的在线状态。
 *
 * 失败不弹提示：这是每 15 秒自动跑一次的后台探测，弹一次错误等于变成每 15 秒
 * 骚扰一次；而状态只是辅助信息，下一轮自然会重试。
 */
async function refreshStatus(): Promise<void> {
  if (statusPending) return;
  statusPending = true;
  try {
    const list = await api.hosts.status();
    statuses.value = new Map(list.map((item) => [item.id, item]));
    // 状态刚更新，在线名单才是准的。表格与卡片都读这份指标，
    // 两种视图都立刻补一轮 —— 否则要干等一个 30 秒周期才看到读数。
    void refreshMetrics();
  } catch {
    // 忽略：见函数注释。
  } finally {
    statusPending = false;
  }
}

/**
 * 刷新按钮：列表、状态、指标一起重取。
 *
 * 提权方式不再预取：表格只展示状态/CPU/内存/延迟，而预取要真的登录每台机器
 * （每 5 分钟一轮）。需要知道能不能提权时，行内「测试」会当场给出结论。
 */
function refresh(): void {
  void load();
}

/**
 * 「状态」的口径，表格与卡片共用这一处。
 *
 * 真掉线由 TCP 轮询说了算 —— 实测值再新也不能盖掉"现在连不上"；其次是
 * 「测试」实测出的 SSH 耗时；都没有时退回轮询的 TCP 建连耗时。两种数字靠
 * 悬停提示区分，不为了它们分裂出第二列。
 */
function statusView(row: Host): { text: string; cls: string; title: string } {
  // 宿主机就是面板自己，永远可达；没有 TCP 探测这一说。
  if (row.id === LOCAL_HOST_ID) {
    return { text: '在线', cls: '', title: '面板所在的这台机器' };
  }
  const st = statuses.value.get(row.id);
  if (st && !st.online) {
    return { text: '下线', cls: 'is-off', title: 'TCP 探测不通' };
  }
  const sshMs = measured.value.get(row.id);
  if (sshMs !== undefined) {
    return { text: formatLatency(sshMs), cls: 'num', title: 'SSH 握手 + 认证耗时（点「测试」实测）' };
  }
  if (!st) {
    return { text: '—', cls: 'is-unknown', title: '尚未探测' };
  }
  return { text: formatLatency(st.latencyMs), cls: 'num', title: 'TCP 建连耗时（不含认证）' };
}

/** 状态灯：与「状态」同一口径，只有确认在线才点绿灯。 */
function ledClass(row: Host): string {
  if (row.id === LOCAL_HOST_ID) return 'led--ok';
  const st = statuses.value.get(row.id);
  if (!st) return '';
  return st.online ? 'led--ok' : 'led--danger';
}

/** 卡片副行：登录账号与认证方式。地址已经放在标题行，不再重复。 */
function tileMeta(row: Host): string {
  // 宿主机没有登录账号，副行只留一句定位说明，由备注行承接。
  if (row.id === LOCAL_HOST_ID) return '';
  // 这几种情况必须说清，否则会被读成"能登录"：
  //   无需凭据 / 本机密钥 —— 真的能登进去；未配置凭据 —— 占位，登不进去。
  if (row.authType === 'none') return `${row.username} · 无需凭据`;
  if (row.authType === 'localkey') return `${row.username} · 本机密钥`;
  if (!row.hasSecret) return `${row.username} · 未配置凭据`;
  return `${row.username} · ${row.authType === 'key' ? '私钥认证' : '密码认证'}`;
}

/** 卡片备注行：分组与备注。没有时是空串，由 CSS 保留这一行的高度。 */
function tileNote(row: Host): string {
  return [row.group, row.remark].filter((value) => value !== '').join(' · ');
}

/** 卡片右上角的操作菜单。 */
function tileMenu(row: Host): DropdownOption[] {
  return [
    { label: '连接', key: 'connect' },
    { label: testingRowId.value === row.id ? '测试中…' : '测试', key: 'test', disabled: testingRowId.value === row.id },
    { label: '编辑', key: 'edit' },
    { type: 'divider', key: 'divider' },
    { label: '删除', key: 'remove' },
  ];
}

function onTileAction(key: string, row: Host): void {
  if (key === 'connect') connect(row);
  else if (key === 'test') void runTest(row);
  else if (key === 'edit') openEdit(row);
  else if (key === 'remove') askRemove(row);
}

/** 卡片视图的根元素：useVisiblePoll 靠它判断窗口是否被最小化。 */
const root = ref<HTMLElement | null>(null);

/** 主机 ID → 指标摘要。只有卡片视图会用到。 */
const metrics = ref<Map<number, HostMetricsBrief>>(new Map());

/**
 * 指标轮询间隔。
 *
 * 比状态探测（15 秒）慢得多：状态只探一次 TCP 端口，而采一次指标要在目标机上
 * 跑一段命令。卡片上的读数不需要秒级刷新，30 秒足够看出趋势。
 */
const METRICS_POLL_MS = 30000;

/**
 * 右侧「实时监控」卡跟随列表里选中的主机。
 *
 * 默认选中本机；点表格任意一行切换目标。切换时先清空再取数 —— 否则新数据
 * 到达前卡片上还挂着上一台的读数，那比空着更容易被误读。
 *
 * 8 秒一轮：比列表的指标轮询快，因为这里画的是一条正在走动的曲线；
 * 同一时刻只追一台主机，代价可控。
 */
const TELEMETRY_POLL_MS = 8000;
const TELEMETRY_HISTORY = 60;

/** 0 表示本机（hostId 0 是面板所在主机）。 */
const selectedId = ref(0);
const selectedName = ref('本机');
const telemMetrics = ref<Metrics | null>(null);
const cpuHistory = ref<number[]>([]);

const telemCpuText = computed(() =>
  telemMetrics.value === null ? '—' : telemMetrics.value.cpuPercent.toFixed(0),
);

const telemMemPercent = computed<number | null>(() => {
  const snapshot = telemMetrics.value;
  if (!snapshot || snapshot.memTotal <= 0) return null;
  return (snapshot.memUsed / snapshot.memTotal) * 100;
});

const telemDiskPercent = computed<number | null>(() => {
  const disks = telemMetrics.value?.disks ?? [];
  if (disks.length === 0) return null;
  return disks.reduce((max, disk) => Math.max(max, disk.usePercent), 0);
});

function percentText(value: number | null): string {
  return value === null || !Number.isFinite(value) ? '—' : `${value.toFixed(0)}%`;
}

function percentWidth(value: number | null): string {
  if (value === null || !Number.isFinite(value)) return '0';
  return `${Math.max(0, Math.min(100, value))}%`;
}

const telemMemText = computed(() => percentText(telemMemPercent.value));
const telemMemWidth = computed(() => percentWidth(telemMemPercent.value));
const telemDiskText = computed(() => percentText(telemDiskPercent.value));
const telemDiskWidth = computed(() => percentWidth(telemDiskPercent.value));

// ---- 概要信息（主机名 / 系统 / CPU / GPU / 内存 / 存储）----

const telemHostname = computed(() => telemMetrics.value?.hostname || selectedName.value);

/** 发行版优先，取不到时退回内核版本 —— 后者至少能说明"这是什么系统"。 */
const telemOs = computed(() => telemMetrics.value?.osName || telemMetrics.value?.kernel || '—');

const telemCpu = computed(() => {
  const snapshot = telemMetrics.value;
  if (!snapshot || snapshot.cpuModel === '') return '—';
  return snapshot.cpuCores > 0 ? `${snapshot.cpuModel} · ${snapshot.cpuCores} 核` : snapshot.cpuModel;
});

const telemGpu = computed(() => {
  const list = telemMetrics.value?.accelerators ?? [];
  if (list.length === 0) return '未检测到';
  return list
    .map((item) => `${item.name}${item.utilPercent >= 0 ? ` ${item.utilPercent.toFixed(0)}%` : ''}`)
    .join(' / ');
});

const telemMemDetail = computed(() => {
  const snapshot = telemMetrics.value;
  if (!snapshot || snapshot.memTotal <= 0) return '—';
  return `${formatBytes(snapshot.memUsed)} / ${formatBytes(snapshot.memTotal)}`;
});

/** 存储取根挂载点；没有根就取第一个，摘要只回答"主盘用了多少"。 */
const telemDiskDetail = computed(() => {
  const disks = telemMetrics.value?.disks ?? [];
  if (disks.length === 0) return '—';
  const main = disks.find((disk) => disk.mountPoint === '/') ?? disks[0];
  return `${main.usePercent.toFixed(0)}% · ${formatBytes(main.used)} / ${formatBytes(main.total)}`;
});

async function refreshTelemetry(): Promise<void> {
  try {
    const snapshot = await api.metrics.get(selectedId.value, false);
    telemMetrics.value = snapshot;
    if (Number.isFinite(snapshot.cpuPercent)) {
      cpuHistory.value = [...cpuHistory.value, snapshot.cpuPercent].slice(-TELEMETRY_HISTORY);
    }
  } catch {
    // 后台轮询失败不弹提示：卡片上的 — 与列表里的状态已经说明没采到。
  }
}

/** 点表格一行：切到那台主机的读数。 */
function selectHost(row: Host): void {
  selectedId.value = row.id;
  selectedName.value = row.name;
}

function rowClassName(row: Host): string {
  return selectedId.value === row.id ? 'hosts__row--active' : '';
}

/**
 * 先铺一段历史曲线，再交给实时采样往后接。
 *
 * 只靠 8 秒一次的实时点，切到一台远端主机的头十几秒里折线是空的 ——
 * 读数和折线同时为这件事最容易让人以为"没采到"。
 */
async function loadHistory(): Promise<void> {
  try {
    const history = await api.metrics.history(selectedId.value);
    cpuHistory.value = history.points.slice(-TELEMETRY_HISTORY).map((point) => point.cpuPercent);
  } catch {
    // 历史取不到就只靠实时采样，不影响当前读数。
  }
  void refreshTelemetry();
}

watch(selectedId, () => {
  telemMetrics.value = null;
  cpuHistory.value = [];
  void loadHistory();
});

useVisiblePoll({
  root,
  intervalMs: TELEMETRY_POLL_MS,
  task: () => refreshTelemetry(),
  enabled: () => true,
});

void loadHistory();

let metricsPending = false;

/**
 * 采一轮指标。
 *
 * 只采「确认在线」的主机：离线的主机连不上，采它们只会一直耗到超时，
 * 还会在目标机上留下一串失败登录。状态列刚刚才探过，这份名单是现成的。
 *
 * 失败不弹提示：这是后台轮询，弹一次就等于每 30 秒骚扰一次；
 * 卡片上的 — 已经如实说明了"没采到"。
 */
async function refreshMetrics(): Promise<void> {
  if (metricsPending) return;
  // 宿主机固定在内：它在列表里始终可见，读数不该因为"没探测"而空着。
  const online = [
    LOCAL_HOST_ID,
    ...hosts.value.filter((row) => statuses.value.get(row.id)?.online).map((row) => row.id),
  ];

  metricsPending = true;
  try {
    const list = await api.hosts.metrics(online);
    metrics.value = new Map(list.map((item) => [item.id, item]));
  } catch {
    // 忽略：见函数注释。
  } finally {
    metricsPending = false;
  }
}

/**
 * 指标轮询：表格与卡片共用同一份采样。
 *
 * 只在窗口可见时跑（useVisiblePoll 负责），窗口最小化时不会继续往目标机扇出命令。
 */
useVisiblePoll({
  root,
  intervalMs: METRICS_POLL_MS,
  task: () => refreshMetrics(),
  enabled: () => true,
});

/** 切换视图时立刻补一轮，不必干等 30 秒。 */
watch(viewMode, () => {
  void refreshMetrics();
});

/** 指标值：null 一律显示 —，不把"没采到"画成 0。 */
function metricText(value: number | null): string {
  return value === null ? '—' : `${value.toFixed(0)}%`;
}

/** 进度条宽度：没采到时留 0，与数值一起被读作"未知"。 */
function metricWidth(value: number | null): string {
  return `${value ?? 0}%`;
}

/** 容量读数：0 表示没采到，显示 — 而不是 "0 B / 0 B"。 */
function capacityText(used: number, total: number): string {
  if (total <= 0) return '—';
  return `${formatBytes(used)} / ${formatBytes(total)}`;
}

/**
 * 宿主机是内置目标（ID 0）：不在主机表里，也不可删除、不可编辑。
 *
 * 列表把它当固定的第一项展示，状态与指标都走本地采集 —— 面板就跑在它上面，
 * 没有"连不上"这回事。
 */
const LOCAL_HOST_ID = 0;
const LOCAL_HOST: Host = {
  id: LOCAL_HOST_ID,
  name: '宿主机',
  addr: '',
  port: 0,
  username: '',
  authType: 'none',
  hasSecret: false,
  group: '',
  remark: '面板所在的这台机器',
  createdAt: '',
  updatedAt: '',
};

/**
 * 客户端过滤：主机规模是"人工维护的清单"量级（几十到几百台），
 * 全部数据本来就随列表接口一次取回，再为筛选跑一趟服务端是多余的。
 *
 * 宿主机固定排在最前；关键词命中它时也保留，否则跟着一起隐藏。
 */
const filtered = computed(() => {
  const kw = keyword.value.trim().toLowerCase();
  const rows =
    kw === ''
      ? [...hosts.value]
      : hosts.value.filter((row) =>
          [row.name, row.addr, row.username, row.group, row.remark]
            .join(' ')
            .toLowerCase()
            .includes(kw),
        );
  const localHit = `${LOCAL_HOST.name} ${LOCAL_HOST.remark}`.toLowerCase().includes(kw);
  return localHit ? [LOCAL_HOST, ...rows] : rows;
});

function openCreate(): void {
  editingId.value = null;
  form.value = blankForm();
  showForm.value = true;
}

function openEdit(host: Host): void {
  editingId.value = host.id;
  // 凭据与提权口令都不回填：后端从不返回明文，"留空"即表示保持原值不变。
  form.value = {
    name: host.name,
    addr: host.addr,
    port: host.port,
    username: host.username,
    authType: host.authType,
    secret: '',
    escalateSecret: '',
    group: host.group,
    remark: host.remark,
  };
  showForm.value = true;
}

async function save(): Promise<void> {
  if (!form.value.name.trim()) {
    message.warning('请填写主机名称');
    return;
  }
  if (!form.value.addr.trim()) {
    message.warning('请填写主机地址');
    return;
  }
  if (!form.value.username.trim()) {
    message.warning('请填写登录用户名');
    return;
  }
  // 凭据是可选的：允许先把机器登记进来（地址、端口、用户名），凭据以后再补。
  // 这类主机在列表里会标成「未配置」，连接类操作会被明确挡下。
  // SSH 登录本身仍然必须有凭据 —— 那是协议要求，不是这里放宽的东西。

  const payload: HostForm = { ...form.value };
  saving.value = true;
  try {
    if (editingId.value === null) {
      await api.hosts.create(payload);
      message.success('主机已添加');
    } else {
      await api.hosts.update(editingId.value, payload);
      message.success('主机已更新');
    }
    showForm.value = false;
    await load();
  } catch (err) {
    message.error(describe(err, '保存失败'));
  } finally {
    saving.value = false;
  }
}

/**
 * 用弹窗里当前的配置试连一次，不落库。
 *
 * 结果就地显示而不是弹 toast：用户往往要改几轮地址或凭据才连得上，
 * 让结论留在弹窗里，改完还能对着上一轮的错误看。
 */
async function testDraft(): Promise<void> {
  if (!form.value.addr.trim()) {
    message.warning('请填写主机地址');
    return;
  }
  if (!form.value.username.trim()) {
    message.warning('请填写登录用户名');
    return;
  }
  // 凭据留空是合法的：新增态表示"稍后再补"，编辑态表示"沿用已保存的那份"。
  // 两种情况下后端都会退化成只验证端口连通（见 handleTestHostDraft），
  // 并在结论里写明"未登录"，不会被误读成这台机器已经能连。

  testingDraft.value = true;
  draftResult.value = null;
  try {
    draftResult.value = await api.hosts.testDraft({
      id: editingId.value,
      addr: form.value.addr,
      port: form.value.port,
      username: form.value.username,
      authType: form.value.authType,
      secret: form.value.secret,
    });
  } catch (err) {
    draftResult.value = { ok: false, message: describe(err, '连接测试失败'), latencyMs: 0 };
  } finally {
    testingDraft.value = false;
  }
}

/**
 * 删除是不可逆操作，用对话框而不是下拉里的直接执行：
 * 下拉菜单一旦点开就已经"确认过"了，没有二次拦截的位置。
 */
function askRemove(host: Host): void {
  dialog.warning({
    title: '删除主机',
    content: `确认删除「${host.name}」？该主机的已存凭据会一并删除，且不可恢复。`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: () => remove(host),
  });
}

async function remove(host: Host): Promise<void> {
  try {
    await api.hosts.remove(host.id);
    message.success('主机已删除');
    await load();
  } catch (err) {
    message.error(describe(err, '删除失败'));
  }
}

function connect(host: Host): void {
  // 没有凭据时打开终端只会得到一条"密码为空" —— 那是在用户已经点进去之后才告诉他
  // 做不到。挡在入口处，并直接指出下一步该做什么。
  // authType=none 的主机不受这条限制：它本来就不需要凭据。
  if (!canLogin(host)) {
    message.warning(`「${host.name}」还没有配置登录凭据，请先点「编辑」补上密码或私钥`);
    return;
  }
  wm?.open('terminal', `终端 - ${host.name}`, { hostId: host.id, hostName: host.name });
}

/** 打开当前选中主机的资源监控：右侧那张小卡只给读数，细节交给完整的监控窗口。 */
function openSelectedMonitor(): void {
  wm?.open('monitor', `资源监控 - ${selectedName.value}`, {
    hostId: selectedId.value,
    hostName: selectedName.value,
  });
}

/** 正在测试的主机 ID。测试要连两次目标机（握手 + 提权自检），必须给按钮上转圈。 */
const testingRowId = ref<number | null>(null);

/**
 * 「测试」：一次点击回答两件事 —— 登录这台机器要多久，以及面板在这台机器上
 * 能怎么提权。
 *
 * 顺序是"先测连接，再问提权"：凭据不对时不必再花一次 SSH 去问提权路径，
 * 那只会多等一个超时。
 *
 * 结论一律用弹窗而不是浮层：里面既有实测数字，也可能有 su 的原始报错
 * （"Authentication failure" 这类），那是用户据此配置提权口令的唯一线索，
 * 不能被自动消失的提示吃掉。
 */
async function runTest(host: Host): Promise<void> {
  testingRowId.value = host.id;
  try {
    // 这一段量的是 TCP + 密钥交换 + 认证，比状态列的 TCP 建连更接近"登录一次要多久"。
    const conn = await api.hosts.test(host.id);
    if (!conn.ok) {
      showTestResult(host, `连接失败：${conn.message}`, 'error');
      return;
    }

    // 只验证了端口连通。这里必须停下：没有凭据必然登录失败，
    // 再问一次提权只会多等一个超时，还会把"网络是通的"盖成一条错误。
    if (conn.needSecret) {
      showTestResult(
        host,
        `网络可达：${formatLatency(conn.latencyMs)}\n这台主机还没有配置登录凭据，无法登录，也探不到账号权限。\n请点「编辑」补上密码或私钥。`,
        'warning',
      );
      return;
    }

    const role = await api.hosts.checkPrivilege(host.id);

    // 回填延迟列：改成这台机器实测出来的值，而不是轮询/缓存里的近似量。
    // 提权结果只进下面那段结论 —— 提权列已经移除，再往前端存一份没人读。
    measured.value.set(host.id, conn.latencyMs);

    showTestResult(
      host,
      `SSH 延迟：${formatLatency(conn.latencyMs)}（含密钥交换与认证）\n账号状态：${role.detail}`,
      // 连得上但提权不通是警告而非错误：机器是好的，缺的是权限配置。
      role.ok ? 'success' : 'warning',
    );
  } catch (err) {
    message.error(describe(err, '测试失败'));
  } finally {
    testingRowId.value = null;
  }
}

function showTestResult(host: Host, body: string, level: 'success' | 'warning' | 'error'): void {
  const options = {
    title: `测试 - ${host.name}`,
    // 结论里有多行原始报错，保住换行才读得通。
    content: () => h('div', { style: 'white-space: pre-wrap' }, body),
    positiveText: '知道了',
  };
  if (level === 'success') dialog.success(options);
  else if (level === 'warning') dialog.warning(options);
  else dialog.error(options);
}

/**
 * 表格「状态」列：圆点 + 文字。
 *
 * 与预览页的 status 胶囊同构：颜色只表达状态，文字只写结论，
 * 延迟这种可量化信息放到单独的列里。
 */
function stateOf(row: Host): { label: string; cls: string; title: string } {
  if (row.id === LOCAL_HOST_ID) {
    return { label: '在线', cls: 'is-on', title: '面板所在的这台机器' };
  }
  const st = statuses.value.get(row.id);
  if (!st) return { label: '探测中', cls: 'is-unknown', title: '正在探测 TCP 连通性' };
  if (!st.online) return { label: '离线', cls: 'is-off', title: 'TCP 探测不通' };
  return { label: '在线', cls: 'is-on', title: 'TCP 端口可达（不含认证）' };
}

/** 「延迟」列：优先用户「测试」实测的 SSH 耗时，其次 TCP 建连耗时。 */
function latencyText(row: Host): string {
  // 宿主机不走网络，"延迟"对它没有意义。
  if (row.id === LOCAL_HOST_ID) return '本机';
  const st = statuses.value.get(row.id);
  if (st && !st.online) return '—';
  const ssh = measured.value.get(row.id);
  if (ssh !== undefined) return formatLatency(ssh);
  if (st && st.online) return formatLatency(st.latencyMs);
  return '—';
}

const columns = computed<DataTableColumns<Host>>(() => [
  {
    title: '主机',
    key: 'name',
    minWidth: 168,
    sorter: (a, b) => a.name.localeCompare(b.name, 'zh-Hans-CN'),
    // 分组与备注并入名称下方的副行，避免为它们单独占列而挤压操作区。
    render: (row) => {
      const sub = [row.group, row.remark].filter((v) => v !== '').join(' · ');
      return h('div', [h('div', { class: 'hosts__name' }, row.name), sub ? h('div', { class: 'row-sub' }, sub) : null]);
    },
  },
  {
    title: '地址',
    key: 'addr',
    minWidth: 146,
    sorter: (a, b) => a.addr.localeCompare(b.addr),
    render: (row) =>
      h('span', { class: 'num' }, row.id === LOCAL_HOST_ID ? '本机' : `${row.addr}:${row.port}`),
  },
  {
    title: 'CPU',
    key: 'cpu',
    width: 112,
    align: 'left',
    render: (row) => {
      const pct = metrics.value.get(row.id)?.cpuPercent ?? null;
      return h('div', { class: 'hosts__cpu' }, [
        h('span', { class: 'hosts__cpu-track' }, [h('i', { style: { width: metricWidth(pct) } })]),
        h('b', { class: 'num' }, metricText(pct)),
      ]);
    },
  },
  {
    title: '内存',
    key: 'mem',
    width: 70,
    render: (row) => h('span', { class: 'num' }, metricText(metrics.value.get(row.id)?.memPercent ?? null)),
  },
  {
    title: '延迟',
    key: 'latency',
    width: 84,
    render: (row) => h('span', { class: 'num hosts__latency' }, latencyText(row)),
  },
  {
    title: '状态',
    key: 'state',
    width: 86,
    render: (row) => {
      const view = stateOf(row);
      return h('span', { class: `hosts__state ${view.cls}`, title: view.title }, [
        h('i', { 'aria-hidden': 'true' }),
        view.label,
      ]);
    },
  },
  {
    title: '操作',
    key: 'actions',
    // 只留四个按钮。文件、监控、进程、网络、存储都已经在桌面上各有一个图标，
    // 打开后窗口内会自己出现主机选择器 —— 没必要在每一行里再排一遍，
    // 那反而把「连上这台机器」这个最高频的动作挤到视线的角落。
    //
    // 「测试」紧挨着「连接」：它是"连上了但不知道能不能改"时的下一站 ——
    // 一次点击给出登录耗时与提权方式，结论直接回答环境管理的写操作能不能用。
    width: 200,
    render: (row) => {
      // 宿主机是内置目标：不能编辑、不能删除，"提权账号"是它唯一的配置项。
      if (row.id === LOCAL_HOST_ID) {
        return h(NSpace, { size: 4, wrap: false, align: 'center' }, {
          default: () => [
            h(
              NButton,
              { size: 'small', type: 'primary', onClick: () => connect(row) },
              { default: () => '连接' },
            ),
            h(
              NButton,
              { size: 'small', onClick: () => openLocalPrivilege() },
              { default: () => (localPriv.value.configured ? '提权账号' : '配置提权') },
            ),
          ],
        });
      }
      return h(
        NSpace,
        { size: 4, wrap: false, align: 'center' },
        {
          default: () => [
            h(
              NButton,
              { size: 'small', type: 'primary', onClick: () => connect(row) },
              { default: () => '连接' },
            ),
            h(
              NButton,
              { size: 'small', loading: testingRowId.value === row.id, onClick: () => runTest(row) },
              { default: () => '测试' },
            ),
            h(NButton, { size: 'small', onClick: () => openEdit(row) }, { default: () => '编辑' }),
            h(NButton, { size: 'small', onClick: () => askRemove(row) }, { default: () => '删除' }),
          ],
        },
      );
    },
  },
]);

// 单击行把右侧监控切到这台主机；双击仍直接开终端 —— 桌面应用的通用约定，
// 比先瞄准"连接"按钮更快。
function rowProps(row: Host) {
  return {
    style: 'cursor: default',
    onClick: () => selectHost(row),
    onDblclick: () => connect(row),
  };
}

onMounted(() => {
  void load();
  statusTimer = window.setInterval(() => void refreshStatus(), STATUS_POLL_MS);
});

// 关窗即停止轮询：窗口组件随窗口关闭而卸载，不清定时器的话它会一直在后台跑。
onUnmounted(() => {
  if (statusTimer !== undefined) window.clearInterval(statusTimer);
});
</script>

<template>
  <div ref="root" class="hosts">
    <div class="hosts__bar">
      <div class="hosts__heading-block">
        <p class="hosts__eyebrow">INVENTORY</p>
        <h2 class="hosts__heading">
          主机列表
          <span class="hosts__title num">
            {{ keyword ? `${filtered.length} / ${hosts.length}` : hosts.length }} 台
          </span>
        </h2>
      </div>
      <div class="hosts__filter">
        <n-input
          v-model:value="keyword"
          size="small"
          clearable
          placeholder="按名称 / 地址 / 用户名 / 分组筛选"
          :input-props="{ 'aria-label': '筛选主机' }"
        />
      </div>
      <n-space size="small" align="center">
        <n-radio-group v-model:value="viewMode" size="small">
          <n-radio-button value="table">表格</n-radio-button>
          <n-radio-button value="card">卡片</n-radio-button>
        </n-radio-group>
        <n-button size="small" :loading="loading" @click="refresh">刷新</n-button>
        <n-button size="small" type="primary" @click="openCreate">新增主机</n-button>
      </n-space>
    </div>

    <!--
      卡片视图：与表格同一份数据、同一批动作，只是按"一台机器一张卡"组织。
      每个区块都有固定的行高（名称、地址、备注各占一行、超长省略），因此不同
      内容的卡片高度天然一致，不会出现相邻卡片底边错位。
    -->
    <div v-if="viewMode === 'card'" class="hosts__grid">
      <div v-if="filtered.length === 0" class="hosts__empty hosts__empty--grid">
        <template v-if="hosts.length === 0">
          <p class="hosts__empty-title">还没有任何主机</p>
          <p class="hosts__empty-hint">
            添加一台主机后即可打开终端、浏览文件、查看资源占用；也可以对它执行批量命令。
          </p>
          <n-button size="small" type="primary" @click="openCreate">新增主机</n-button>
        </template>
        <template v-else>
          <p class="hosts__empty-title">没有匹配「{{ keyword }}」的主机</p>
          <p class="hosts__empty-hint">换个关键词，或清空筛选框查看全部 {{ hosts.length }} 台。</p>
          <n-button size="small" @click="keyword = ''">清空筛选</n-button>
        </template>
      </div>

      <template v-else>
        <div
          v-for="row in filtered"
          :key="row.id"
          class="hosts__tile"
          @dblclick="connect(row)"
        >
          <div class="hosts__tile-head">
            <span class="led" :class="ledClass(row)" />
            <span class="hosts__tile-name" :title="row.name">{{ row.name }}</span>
            <span
              class="hosts__tile-addr num"
              :title="row.id === LOCAL_HOST_ID ? '面板所在的这台机器' : `${row.addr}:${row.port}`"
            >
              {{ row.id === LOCAL_HOST_ID ? '本机' : `${row.addr}:${row.port}` }}
            </span>
          </div>

          <p class="hosts__tile-meta num" :title="tileMeta(row)">{{ tileMeta(row) }}</p>
          <p class="hosts__tile-note" :title="tileNote(row)">{{ tileNote(row) || '—' }}</p>

          <!--
            指标行：CPU / 内存 / 存储各占等宽一格。
            未采到的值显示 — 且进度条留空，而不是画成 0% ——
            0% 的含义是"机器空闲"，与"没采到"不能混。
          -->
          <dl class="hosts__metrics">
            <div class="hosts__metric">
              <dt class="hosts__metric-key">CPU</dt>
              <dd class="hosts__metric-val num">{{ metricText(metrics.get(row.id)?.cpuPercent ?? null) }}</dd>
              <div class="hosts__metric-bar">
                <span
                  class="hosts__metric-fill is-cpu"
                  :style="{ width: metricWidth(metrics.get(row.id)?.cpuPercent ?? null) }"
                />
              </div>
            </div>
            <div class="hosts__metric">
              <dt class="hosts__metric-key">内存</dt>
              <dd
                class="hosts__metric-val num"
                :title="capacityText(metrics.get(row.id)?.memUsed ?? 0, metrics.get(row.id)?.memTotal ?? 0)"
              >
                {{ metricText(metrics.get(row.id)?.memPercent ?? null) }}
              </dd>
              <div class="hosts__metric-bar">
                <span
                  class="hosts__metric-fill is-mem"
                  :style="{ width: metricWidth(metrics.get(row.id)?.memPercent ?? null) }"
                />
              </div>
            </div>
            <div class="hosts__metric">
              <dt class="hosts__metric-key">存储</dt>
              <dd
                class="hosts__metric-val num"
                :title="capacityText(metrics.get(row.id)?.diskUsed ?? 0, metrics.get(row.id)?.diskTotal ?? 0)"
              >
                {{ metricText(metrics.get(row.id)?.diskPercent ?? null) }}
              </dd>
              <div class="hosts__metric-bar">
                <span
                  class="hosts__metric-fill is-disk"
                  :style="{ width: metricWidth(metrics.get(row.id)?.diskPercent ?? null) }"
                />
              </div>
            </div>
          </dl>

          <footer class="hosts__tile-foot">
            <span
              class="hosts__tile-latency num"
              :class="statusView(row).cls"
              :title="statusView(row).title"
            >
              {{ statusView(row).text }}
            </span>
            <span class="hosts__tile-foot-spacer" />
            <!-- 宿主机是内置目标：只有提权账号可配，没有测试、编辑与删除。 -->
            <template v-if="row.id === LOCAL_HOST_ID">
              <n-button size="small" @click.stop="openLocalPrivilege">
                {{ localPriv.configured ? '提权账号' : '配置提权' }}
              </n-button>
            </template>
            <template v-else>
              <n-button size="small" type="primary" @click.stop="connect(row)">连接</n-button>
              <n-dropdown
                trigger="click"
                size="small"
                :options="tileMenu(row)"
                @select="(key: string | number) => onTileAction(String(key), row)"
              >
                <n-button size="small" quaternary aria-label="更多操作" title="更多操作">更多</n-button>
              </n-dropdown>
            </template>
          </footer>
        </div>
      </template>
    </div>

    <!--
      表格视图：左侧主机列表，右侧实时监控。
      两块卡的分工与设计稿一致 —— 列表回答"有哪些机器、现在什么状态"，
      右侧回答"本机正在被谁吃掉"。监控只采本机，不随列表行变化：
      远端主机的详情在资源监控窗口里看，避免在列表窗口里再开一条采样链路。
    -->
    <div v-else class="hosts__split">
      <div class="hosts__surface">
        <n-data-table
          :columns="columns"
          :data="filtered"
          :loading="loading"
          :bordered="false"
          :row-key="(row: Host) => row.id"
          :row-props="rowProps"
          :row-class-name="rowClassName"
          size="small"
          flex-height
          style="flex: 1; min-height: 0"
        >
          <!-- 空状态给出下一步动作，而不是只显示一句"无数据" -->
          <template #empty>
            <div class="hosts__empty">
              <template v-if="hosts.length === 0">
                <p class="hosts__empty-title">还没有任何主机</p>
                <p class="hosts__empty-hint">
                  添加一台主机后即可打开终端、浏览文件、查看资源占用；也可以对它执行批量命令。
                </p>
                <n-button size="small" type="primary" @click="openCreate">新增主机</n-button>
              </template>
              <template v-else>
                <p class="hosts__empty-title">没有匹配「{{ keyword }}」的主机</p>
                <p class="hosts__empty-hint">换个关键词，或清空筛选框查看全部 {{ hosts.length }} 台。</p>
                <n-button size="small" @click="keyword = ''">清空筛选</n-button>
              </template>
            </div>
          </template>
        </n-data-table>
      </div>

      <aside class="hosts__telemetry">
        <header class="hosts__telem-head">
          <div class="hosts__telem-ident">
            <p class="page-eyebrow">TELEMETRY</p>
            <h3 class="hosts__telem-title">
              实时监控
              <span class="page-count">{{ selectedId === 0 ? '本机' : selectedName }}</span>
            </h3>
          </div>
          <n-tag size="small" :bordered="false" type="success">流式</n-tag>
        </header>

        <!-- 选中主机的静态画像：一眼回答"这是什么机器、装了什么、还有多少余量" -->
        <dl class="hosts__telem-info">
          <div>
            <dt>主机名</dt>
            <dd :title="telemHostname">{{ telemHostname }}</dd>
          </div>
          <div>
            <dt>系统</dt>
            <dd :title="telemOs">{{ telemOs }}</dd>
          </div>
          <div>
            <dt>CPU</dt>
            <dd :title="telemCpu">{{ telemCpu }}</dd>
          </div>
          <div>
            <dt>GPU</dt>
            <dd :title="telemGpu">{{ telemGpu }}</dd>
          </div>
          <div>
            <dt>内存</dt>
            <dd class="num">{{ telemMemDetail }}</dd>
          </div>
          <div>
            <dt>存储</dt>
            <dd class="num">{{ telemDiskDetail }}</dd>
          </div>
        </dl>

        <div class="hosts__telem-gauge">
          <b class="num">{{ telemCpuText }}</b>
          <span class="hosts__telem-unit">%</span>
          <em class="hosts__telem-label">CPU</em>
        </div>

        <Sparkline :values="cpuHistory" :max="100" unit="%" />

        <div class="hosts__telem-meters">
          <div class="hosts__telem-meter">
            <span>MEM</span>
            <i><b :style="{ width: telemMemWidth }" /></i>
            <em class="num">{{ telemMemText }}</em>
          </div>
          <div class="hosts__telem-meter">
            <span>DISK</span>
            <i><b :style="{ width: telemDiskWidth }" /></i>
            <em class="num">{{ telemDiskText }}</em>
          </div>
        </div>

        <div class="hosts__telem-net">
          <span class="num">↓ {{ formatRate(telemMetrics?.netRxRate ?? 0) }}</span>
          <span class="num">↑ {{ formatRate(telemMetrics?.netTxRate ?? 0) }}</span>
        </div>

        <footer class="hosts__telem-foot">
          <n-button size="tiny" quaternary @click="openSelectedMonitor">打开资源监控</n-button>
        </footer>
      </aside>
    </div>

    <n-modal
      v-model:show="showLocalPriv"
      preset="card"
      title="宿主机提权账号"
      style="width: 520px"
    >
      <n-form label-placement="left" label-width="84" :model="localForm">
        <n-form-item
          label="认证方式"
          :feedback="
            localForm.authType === 'password'
              ? 'root 走 su，面板运行用户走 sudo -S；口令加密保存'
              : '不保存口令；面板以自身身份执行 sudo -n，需要该账号在 sudoers 中配置 NOPASSWD'
          "
        >
          <n-select v-model:value="localForm.authType" :options="localAuthOptions" />
        </n-form-item>

        <template v-if="localForm.authType === 'password'">
          <n-form-item
            label="账号"
            :feedback="'root：用 root 口令走 su；填面板运行用户：用该用户的 sudo 口令走 sudo -S'"
          >
            <n-input v-model:value="localForm.account" placeholder="root" />
          </n-form-item>
          <n-form-item label="口令">
            <n-input
              v-model:value="localForm.secret"
              type="password"
              show-password-on="click"
              placeholder="提权口令"
            />
          </n-form-item>
        </template>
      </n-form>
      <p v-if="localForm.authType === 'password'" class="hosts__local-note">
        口令加密保存在面板数据库中，读取接口不会回显。面板进程本身能拿到这份口令，
        因此面板被攻破等同于该账号失守。
      </p>
      <p v-else class="hosts__local-note">
        密钥只用于远程主机的登录认证，本机的 su / sudo 不接受密钥；没有 NOPASSWD 时只能用口令提权。
      </p>
      <template #footer>
        <n-space justify="space-between">
          <n-button
            v-if="localPriv.configured"
            type="error"
            quaternary
            @click="clearLocalPrivilege"
          >
            清除
          </n-button>
          <n-space>
            <n-button @click="showLocalPriv = false">取消</n-button>
            <n-button type="primary" :loading="savingLocal" @click="saveLocalPrivilege">
              保存
            </n-button>
          </n-space>
        </n-space>
      </template>
    </n-modal>

    <n-modal
      v-model:show="showForm"
      preset="card"
      :title="editingId === null ? '新增主机' : '编辑主机'"
      style="width: 540px"
    >
      <n-form label-placement="left" label-width="86" :model="form">
        <n-form-item label="名称">
          <n-input v-model:value="form.name" placeholder="例如：生产-Web-01" />
        </n-form-item>
        <n-form-item label="地址" :feedback="'可直接粘贴 user@host 或整条 ssh 命令，失焦后自动拆分'">
          <n-input
            v-model:value="form.addr"
            placeholder="IP 或域名；也支持粘贴 user@host:port"
            @blur="normalizeTarget"
          />
        </n-form-item>
        <n-form-item label="端口">
          <!-- 不显示步进按钮：在 30 余像素高的输入框里，步进的命中区域
               必然小于 WCAG 2.2 要求的 24×24，而数字本身可以直接输入，
               方向键也原生支持增减。 -->
          <n-input-number
            v-model:value="form.port"
            :min="1"
            :max="65535"
            :show-button="false"
            style="width: 100%"
            :input-props="{ 'aria-label': '端口' }"
          />
        </n-form-item>
        <n-form-item label="用户名">
          <n-input v-model:value="form.username" />
        </n-form-item>
        <n-form-item label="认证方式">
          <n-select
            :value="form.authType"
            :options="authOptions"
            @update:value="
              (value: AuthType) => {
                form.authType = value;
                // 切到不需要凭据的方式时清空已输入的凭据，避免它被一起提交：
                // 那条密文存进去后 hasSecret 会变成 true，列表就会显示成
                // “已配置”，可实际登录方式根本不用它 —— 两份事实互相矛盾。
                if (!needsSecret(value)) form.secret = '';
              }
            "
          />
        </n-form-item>
        <n-form-item v-if="needsSecret(form.authType)" :label="form.authType === 'key' ? '私钥' : '密码'"
          :feedback="
            editingId === null
              ? '可选。留空表示先把主机登记进来，凭据以后再补 —— 这类主机能看在线状态、能测端口连通，但无法登录。'
              : '留空表示保持原凭据不变。'
          "
        >
          <n-input
            v-model:value="form.secret"
            :type="form.authType === 'key' ? 'textarea' : 'password'"
            :rows="6"
            show-password-on="click"
            :placeholder="editingId === null ? '可选，留空则稍后补' : '留空表示保持原凭据不变'"
          />
        </n-form-item>
        <n-form-item v-else label="凭据">
          <span class="row-sub">
            <template v-if="form.authType === 'localkey'">
              使用面板所在机器上已有的私钥（~/.ssh/id_ed25519、id_rsa、id_ecdsa）；
              带口令的密钥面板无法解锁，会被跳过。
            </template>
            <template v-else>
              目标机允许无凭据登录（SSH none 认证或空的登录密码）时选这一项；
              保存后会直接尝试登录，能否成功取决于服务端配置。
            </template>
          </span>
        </n-form-item>
        <n-form-item
          label="提权口令"
          feedback="需要 root 的操作（装软件、改网络与存储等）用它回答 su 的口令提示；留空则不套 su，改用登录凭据走 sudo（仅密码认证可用）。"
        >
          <n-input
            v-model:value="form.escalateSecret"
            type="password"
            show-password-on="click"
            :placeholder="editingId === null ? '可选，一般是这台机器的 root 口令' : '留空表示保持原口令不变'"
          />
        </n-form-item>
        <n-form-item label="分组">
          <n-input v-model:value="form.group" placeholder="可选，例如：生产 / 测试" />
        </n-form-item>
        <n-form-item label="备注">
          <n-input v-model:value="form.remark" placeholder="可选" />
        </n-form-item>
      </n-form>

      <template #footer>
        <div class="hosts__footer">
          <!-- 结论放在按钮左侧的空白里，不挤占按钮排布 -->
          <p
            v-if="draftResult"
            class="hosts__test-result"
            :class="draftResult.ok ? 'is-ok' : 'is-bad'"
          >
            {{ formatTestResult(draftResult) }}
          </p>
          <span v-else />

          <n-space>
            <n-button :loading="testingDraft" @click="testDraft">测试</n-button>
            <n-button @click="showForm = false">取消</n-button>
            <n-button type="primary" :loading="saving" @click="save">保存</n-button>
          </n-space>
        </div>
      </template>
    </n-modal>
  </div>
</template>
