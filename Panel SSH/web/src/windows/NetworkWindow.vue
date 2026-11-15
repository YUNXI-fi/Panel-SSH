<script setup lang="ts">
/**
 * 网络管理窗口。
 *
 * 两个部分：只读的现状展示（接口、路由、DNS、监听端口），以及带确认回滚
 * 保护的变更操作。
 *
 * 变更的核心约束：改地址、把网口挂到网桥上、停用连接，都会在生效的瞬间
 * 切断面板自己赖以操作的那条 SSH。因此每次变更都拆成两步 ——
 * 面板先把命令延迟下发到目标机，再由 NetworkManager 的检查点兜底：
 * 窗口内没等到确认，目标机自动把配置还原。前端的职责就是在主机恢复后
 * 尽快确认，并在确认失败（说明变更其实没生效）时如实相告，而不是
 * 看到"主机又能连上了"就宣布成功。
 */
import { computed, h, onBeforeUnmount, ref, watch } from 'vue';
import {
  NAlert,
  NButton,
  NCheckbox,
  NDataTable,
  NDropdown,
  NInput,
  NInputNumber,
  NModal,
  NRadio,
  NRadioGroup,
  NSelect,
  NSpace,
  NTabPane,
  NTabs,
  useDialog,
  useMessage,
  type DataTableColumns,
  type DropdownOption,
} from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import NetworkTopology from './NetworkTopology.vue';
import OvsBridgePanel, { type RateFeed, type RateTrace } from './OvsBridgePanel.vue';
import { ledClass, ownerLabel, stateClass, stateLabel, typeLabel } from './netLabels';
import { useVisiblePoll } from '../desktop/useVisiblePoll';
import {
  ApiError,
  api,
  type EscalationInfo,
  type HostsChangeRequest,
  type NetChangeRequest,
  type NetHostEntry,
  type NetInterface,
  type NetListener,
  type NetRoute,
  type NetworkChange,
  type NetworkSnapshot,
} from '../api/client';
import { LOCAL_HOST_ID } from '../desktop/watchedHost';
import { formatBytes, formatRate } from '../api/format';

const props = defineProps<{ hostId?: number; hostName?: string }>();

const message = useMessage();
const dialog = useDialog();

// 与服务端 2 秒的采样缓存窗口配合：间隔必须大于它，否则每轮都命中缓存，
// 拿不到新的计数快照，速率会一直停在上一个值。
const NETWORK_INTERVAL_MS = 3000;

/**
 * 变更下发后、首次尝试确认之前要等待的时间。
 *
 * 生效命令是在目标机上延迟 3 秒执行的，若下发一返回就去核对状态，
 * 十有八九会读到"还没变"，把一次正常变更误报成失败。
 */
const CONFIRM_DELAY_MS = 6000;

const root = ref<HTMLElement | null>(null);
const target = ref<number | null>(props.hostId ?? null);
/** 目标显示名，供页头与主机胶囊使用：从桌面打开时由选择器填充。 */
const targetName = ref(props.hostName ?? '');

watch(
  () => props.hostId,
  (id) => {
    if (id !== undefined) {
      target.value = id;
      targetName.value = props.hostName ?? '';
    }
  },
);

const snapshot = ref<NetworkSnapshot | null>(null);
const fetching = ref(false);
const lastError = ref('');
const listenerFilter = ref('');
// 目标机的提权方式：变更下发要 root，弹窗前据此提醒用户；
// 无法提权的机器则提前说明这次注定失败，而不是让他点了才知道。
const escalation = ref<EscalationInfo | null>(null);

// ---- 变更状态 ----

const pending = ref<NetworkChange | null>(null);
const pendingLog = ref('');
/** 服务端判定变更未生效时的说明。非空表示这次变更实际上没成功。 */
const verifyNote = ref('');
const applying = ref(false);
const committing = ref(false);
/** 早于这个时刻不做确认核对，见 CONFIRM_DELAY_MS。 */
const confirmNotBefore = ref(0);
const now = ref(Date.now());

let clockTimer: number | undefined;

// 倒计时只在有未决变更时走动：没有变更时每秒唤醒一次纯属浪费。
watch(pending, (value) => {
  if (value && clockTimer === undefined) {
    clockTimer = window.setInterval(() => {
      now.value = Date.now();
    }, 1000);
  } else if (!value && clockTimer !== undefined) {
    window.clearInterval(clockTimer);
    clockTimer = undefined;
  }
});

onBeforeUnmount(() => {
  if (clockTimer !== undefined) window.clearInterval(clockTimer);
});

const remainingSeconds = computed(() => {
  if (!pending.value) return 0;
  const deadline = new Date(pending.value.expiresAt).getTime();
  if (Number.isNaN(deadline)) return 0;
  return Math.max(0, Math.round((deadline - now.value) / 1000));
});

const expired = computed(() => pending.value !== null && remainingSeconds.value <= 0);

const countdownText = computed(() => {
  const total = remainingSeconds.value;
  if (total <= 0) return '已结束';
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return minutes > 0 ? `${minutes} 分 ${seconds} 秒` : `${seconds} 秒`;
});

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 504) return `${err.message}。请检查目标机负载与网络`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

/**
 * 取目标机的提权方式，供变更前的提醒使用。
 *
 * 不复用 refresh 那条链路：提醒是辅助信息，它的失败不该影响网络数据的展示，
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

/** 无法提权时的警告：变更注定失败，必须在动手前说清楚，而不是让它变成点了才知道的失败。 */
const escalationWarning = computed(() => {
  const info = escalation.value;
  if (!info || info.mode === 'root' || info.escalatable) return '';
  return '该主机无法提权，而此操作需要 root：目标机既不是 root 登录，也没有可用的提权路径（sudo/su）。请在面板里为该主机配置提权口令（root 口令，走 su），或以 root 登录后再试。';
});

// ---- OVS ----

const ovs = computed(() => snapshot.value?.ovs ?? null);

/**
 * OVS 相关界面是否出现。
 *
 * 判据是目标机上有没有 ovs-vsctl，而不是"它是不是某个特定系统"：
 * 谁装了 Open vSwitch 谁就能用这套功能，装了别的系统的机器一样适用。
 */
const ovsAvailable = computed(() => ovs.value?.available === true);

/**
 * 当前默认路由的出口设备。
 *
 * 给 OVS 面板标注"动这个口会切断面板自己那条连接"用。后端把空的目的地址
 * 归一成 default，IPv6 的默认路由则可能原样写成 ::/0，两种都算。
 */
const defaultRouteDevs = computed(() =>
  (snapshot.value?.routes ?? [])
    .filter((route) => route.dst === 'default' || route.dst === '::/0')
    .map((route) => route.dev),
);

// ---- 速率曲线 ----

/**
 * 速率曲线保留的采样点数。
 *
 * 轮询间隔 3 秒，40 个点约等于最近 2 分钟 —— 与资源监控窗口的曲线窗同一口径。
 */
const HISTORY_SIZE = 40;

/** 每个采样点的时刻（epoch 毫秒），供曲线判断时间缺口。 */
const rateTimes = ref<number[]>([]);

/**
 * 按接口名索引的收发速率采样历史。
 *
 * 由窗口持有而不是让 OVS 面板自己攒：OVS 面板不在当前标签页时会被销毁，
 * 历史放在那边，每次切回来都得从零开始画。窗口的轮询一直在跑，也就一直是满的。
 */
const rateHistory = ref<Record<string, RateTrace>>({});

/** 曲线的时间缺口阈值：超过两轮半没有采样就在图上断开。 */
const rateGapMs = NETWORK_INTERVAL_MS * 2.5;

/**
 * 把这次快照追加进速率历史。
 *
 * 只保留这一份快照里仍然存在的接口 —— 接口都没了，曲线留着也没有意义，
 * 这样也就不必再定一条"多久清一次历史"的规则。
 *
 * 首次采样没有基准（deltaReady 为假），此时记 NaN：曲线在这里断开，
 * 而不是把 0 当成"真的没有流量"画一条贴底的直线。
 */
function recordRates(snap: NetworkSnapshot): void {
  const at = Date.parse(snap.sampledAt);
  const stamp = Number.isNaN(at) ? Date.now() : at;
  // 同一份快照会被重复送达（服务端缓存命中、点刷新与轮询撞在一起），按时刻去重：
  // 叠一个重复点在曲线里就是一个凭空的尖峰。
  if (rateTimes.value[rateTimes.value.length - 1] === stamp) return;

  const next: Record<string, RateTrace> = {};
  for (const iface of snap.interfaces) {
    const prev = rateHistory.value[iface.name];
    const rx = [...(prev?.rx ?? []), snap.deltaReady ? iface.rxRate : NaN];
    const tx = [...(prev?.tx ?? []), snap.deltaReady ? iface.txRate : NaN];
    // 超出窗口的部分从头部砍掉：曲线只关心最近这一段。
    const extra = rx.length - HISTORY_SIZE;
    next[iface.name] = extra > 0 ? { rx: rx.slice(extra), tx: tx.slice(extra) } : { rx, tx };
  }
  rateHistory.value = next;
  rateTimes.value = [...rateTimes.value, stamp].slice(-HISTORY_SIZE);
}

/**
 * 交给 OVS 面板的一整套曲线数据。
 *
 * 打成一个对象而不是拆成四个 prop：它们是同一个数据源的四个面，分开传既要在
 * 调用处拼四遍，也让面板的 props 平白长一截。
 */
const rateFeed = computed<RateFeed>(() => ({
  history: rateHistory.value,
  times: rateTimes.value,
  gapMs: rateGapMs,
  deltaReady: snapshot.value?.deltaReady ?? false,
}));

// ---- 采集 ----

async function refresh(options: { fresh?: boolean; silent?: boolean } = {}): Promise<void> {
  const id = target.value;
  if (id === null) return;

  // 已有数据时的轮询不点亮按钮：每 3 秒闪一次的转圈只是噪音，也是这个按钮
  // 看起来一直在抽的原因。首屏还没有数据时要亮，否则占位区会从"正在读取"
  // 变成"暂无数据"，而那是两件完全不同的事。
  const showLoading = !options.silent || snapshot.value === null;
  if (showLoading) fetching.value = true;
  try {
    snapshot.value = await api.network.snapshot(id, options.fresh ?? false);
    recordRates(snapshot.value);
    lastError.value = '';
    // 能读到快照说明主机可达。此时若有未确认的变更，就该去核对并确认它。
    void tryCommit();
  } catch (err) {
    lastError.value = describe(err, '读取网络信息失败');
  } finally {
    if (showLoading) fetching.value = false;
  }
}

/** 点刷新。已经在读时不再叠一次 —— 按钮不用 loading 变灰，重复点击要在这里挡掉。 */
function onRefreshClick(): void {
  if (fetching.value) return;
  void refresh({ fresh: true });
}

/** 恢复未决变更。窗口刚打开时服务端可能还留着上一次会话下发的变更。 */
async function loadPending(): Promise<void> {
  const id = target.value;
  if (id === null) return;
  try {
    const result = await api.network.change(id);
    if (result.change) {
      pending.value = result.change;
      pendingLog.value = result.log ?? '';
      // 这条变更是别处下发的，生效命令很可能早就跑完了，不必再等延迟。
      confirmNotBefore.value = 0;
    }
  } catch {
    // 读不到不影响只读展示，静默处理。
  }
}

useVisiblePoll({
  root,
  intervalMs: NETWORK_INTERVAL_MS,
  task: () => refresh({ silent: true }),
  enabled: () => target.value !== null,
});

watch(target, (id) => {
  if (id === null) return;
  // 换主机时先清空：否则新主机的数据到达前，窗口里还挂着上一台的接口列表，
  // 而这时候点操作会打到错误的机器上。
  snapshot.value = null;
  // 曲线同理：那是上一台机器的波形，接过来看就是在读假数据。
  rateHistory.value = {};
  rateTimes.value = [];
  lastError.value = '';
  pending.value = null;
  pendingLog.value = '';
  verifyNote.value = '';
  // 待结束的目标也要清：它记的是上一台机器上的 pid，留着会让确认弹窗
  // 在新主机上继续可点，那就等于照着旧 pid 去杀新机器上的进程。
  killTarget.value = null;
  void refresh({ fresh: true });
  void loadPending();
  void loadEscalation();
});

// ---- 变更流程 ----

async function submitChange(payload: NetChangeRequest): Promise<void> {
  const id = target.value;
  if (id === null) return;

  applying.value = true;
  try {
    const result = await api.network.apply(id, payload);
    pending.value = result.change;
    pendingLog.value = '';
    verifyNote.value = '';
    confirmNotBefore.value = Date.now() + CONFIRM_DELAY_MS;
    closeForm();
    // 本地快照已经过时，但此刻主机很可能正在断连，强制刷新只会立刻报错，
    // 交给下一轮轮询自然更新。
    message.info('变更已下发。若连接中断，面板会在主机恢复后自动确认');
  } catch (err) {
    message.error(describe(err, '下发变更失败'));
  } finally {
    applying.value = false;
  }
}

/**
 * 核对并确认变更。
 *
 * 服务端会拿当前状态与预期比对：对不上时返回 409，说明这一刻还没达成目标
 * 状态 —— 可能是变更没生效，也可能只是 DHCP 还在取地址。因此这里只把结论
 * 记下来继续等，不当作终局：真正的终局是"窗口结束"或用户手动决定。
 */
async function tryCommit(): Promise<void> {
  const change = pending.value;
  const id = target.value;
  if (!change || id === null || committing.value) return;
  if (Date.now() < confirmNotBefore.value) return;
  if (expired.value) return;

  committing.value = true;
  try {
    const result = await api.network.commit(id);
    pending.value = null;
    pendingLog.value = '';
    verifyNote.value = '';
    // 变更本身已经核对通过并生效了。persistNote 非空只说明开机自恢复没更新
    // 成功 —— 那种情况必须说出来，否则用户会以为重启之后这个配置还在。
    if (result.persistNote) {
      message.warning(result.persistNote);
    } else {
      message.success('变更已生效');
    }
    void refresh({ fresh: true });
  } catch (err) {
    if (err instanceof ApiError && err.status === 409) {
      verifyNote.value = err.message;
    }
    // 其余错误（主机还没回来、超时）保持原状，下一轮轮询再试。
  } finally {
    committing.value = false;
  }
}

/**
 * 立即回滚要先确认。
 *
 * 这个按钮就挨着"知道了"和"仍要保留"，点错一次就会立刻把配置还原并断开连接，
 * 而回滚本身不可撤销，所以必须再问一次。
 */
function rollbackNow(): void {
  const lines = ['将把目标机的网络配置恢复到本次变更之前。生效过程中面板与它的连接可能短暂中断。'];
  // 回滚同样需要 root：把已有的提权提醒并进来，别让用户在确认框里看不到它。
  const escalationText = escalationWarning.value || escalationInfo.value;
  if (escalationText) lines.push(escalationText);

  dialog.warning({
    title: '立即回滚网络变更',
    // content 只接受字符串或返回 VNode 的函数；这里要靠 pre-wrap 保留两段之间的
    // 换行，所以用函数返回一个带样式的 div。
    content: () => h('div', { style: 'white-space: pre-wrap' }, lines.join('\n')),
    positiveText: '回滚',
    negativeText: '取消',
    onPositiveClick: () => doRollback(),
  });
}

/** 真正执行回滚。确认框点"回滚"后才会走到这里。 */
async function doRollback(): Promise<void> {
  const id = target.value;
  if (id === null) return;
  try {
    await api.network.rollback(id);
    pending.value = null;
    pendingLog.value = '';
    verifyNote.value = '';
    message.success('已回滚到变更前的配置');
    void refresh({ fresh: true });
  } catch (err) {
    message.error(describe(err, '回滚失败'));
  }
}

/** 明知状态与预期不符，仍然保留这次配置（销毁检查点）。 */
async function keepAnyway(): Promise<void> {
  const id = target.value;
  if (id === null) return;
  confirming.value = false;
  try {
    const result = await api.network.commit(id, true);
    pending.value = null;
    pendingLog.value = '';
    verifyNote.value = '';
    message.warning(result.persistNote || '已保留当前配置。若主机失联，请通过带外方式恢复');
    void refresh({ fresh: true });
  } catch (err) {
    message.error(describe(err, '保留配置失败'));
  }
}

/** 确认窗口已结束：目标机上的检查点已把配置回滚，面板这边收尾即可。 */
function dismissExpired(): void {
  pending.value = null;
  pendingLog.value = '';
  verifyNote.value = '';
}

/**
 * 卸掉目标机上的 OVS 开机自恢复服务。
 *
 * 它在用户机器上加过一个 systemd 单元，因此删掉它同样要经过确认框 ——
 * 确认文案在 OVS 面板里，这里只负责真正发请求。
 */
async function ovsUnpersist(): Promise<void> {
  const id = target.value;
  if (id === null) return;
  try {
    await api.network.ovsUnpersist(id);
    message.success('已卸掉目标机上的开机自恢复服务');
    void refresh({ fresh: true });
  } catch (err) {
    message.error(describe(err, '卸载开机自恢复失败'));
  }
}

const showCommands = ref(false);
const confirming = ref(false);
const showTopology = ref(false);

// ---- 接口操作 ----

type FormKind = 'address' | 'mtu' | 'bridge-create' | 'bridge-attach';

const formKind = ref<FormKind | null>(null);
const formInterface = ref<NetInterface | null>(null);
const form = ref({
  method: 'manual' as 'manual' | 'auto',
  addresses: '',
  gateway: '',
  dns: '',
  mtu: 1500,
  bridgeName: 'br0',
  bridgeMethod: 'manual' as 'manual' | 'auto',
  bridgeAddresses: '',
  bridgeGateway: '',
  bridgeDNS: '',
  members: [] as string[],
  attachBridge: null as string | null,
});

/**
 * 变更表单的开关。
 *
 * 必须带 setter：n-modal 的 v-model:show 会把关闭动作写回来（关闭按钮、点遮罩、
 * Esc 都走这一条路）。写成只读 computed 时这次写入会被静默丢弃，formKind 仍是
 * 原值，弹窗就永远关不掉 —— 只有「取消」按钮（直接调 closeForm）能关。
 */
const showForm = computed({
  get: () => formKind.value !== null,
  set: (value: boolean) => {
    if (!value) closeForm();
  },
});

/** 变更表单的标题。 */
const formTitle = computed(() => {
  switch (formKind.value) {
    case 'address':
      return `修改地址 - ${formInterface.value?.name ?? ''}`;
    case 'mtu':
      return `修改 MTU - ${formInterface.value?.name ?? ''}`;
    case 'bridge-create':
      return '新建网桥';
    case 'bridge-attach':
      return `把 ${formInterface.value?.name ?? ''} 加入网桥`;
    default:
      return '';
  }
});

function canManage(iface: NetInterface): boolean {
  return snapshot.value?.manager === 'NetworkManager' && iface.connName !== '' && iface.type !== 'loopback';
}

/** 可加入网桥的候选成员：排除回环，以及已经挂在别的网桥上的网口。 */
const attachableInterfaces = computed(() =>
  (snapshot.value?.interfaces ?? []).filter((iface) => iface.type !== 'loopback' && iface.master === '' && canManage(iface)),
);

const bridgeOptions = computed(() =>
  (snapshot.value?.interfaces ?? [])
    .filter((iface) => iface.type === 'bridge')
    .map((iface) => ({ label: iface.name, value: iface.name })),
);

const memberOptions = computed(() =>
  attachableInterfaces.value.map((iface) => ({ label: `${iface.name}（${iface.connName}）`, value: iface.name })),
);

/** 接口卡片右上角的操作菜单。 */
function interfaceActions(iface: NetInterface): DropdownOption[] {
  // 属性是只读入口：不受 NetworkManager 管理的网口同样有信息可看，
  // 因此它不能跟着下面的变更项一起被挡掉。
  const properties: DropdownOption = { label: '属性…', key: 'properties' };
  if (!canManage(iface)) return [properties];

  const options: DropdownOption[] = [
    { label: '修改地址…', key: 'address' },
    { label: iface.adminUp ? '停用此连接' : '启用此连接', key: 'toggle' },
  ];
  if (iface.type === 'ethernet') {
    options.splice(1, 0, { label: '修改 MTU…', key: 'mtu' });
  }
  if (iface.type === 'bridge') {
    options.push({ label: '删除此网桥…', key: 'bridge-delete' });
  } else if (iface.master === '') {
    options.push({ label: '加入网桥…', key: 'bridge-attach' });
  } else {
    options.push({ label: `从 ${iface.master} 移出`, key: 'bridge-detach' });
  }
  // 只读项放最后，并用分隔线跟变更操作隔开。
  options.push({ type: 'divider', key: 'properties-divider' }, properties);
  return options;
}

function openForm(kind: FormKind, iface: NetInterface | null): void {
  formKind.value = kind;
  formInterface.value = iface;
  form.value.method = 'manual';
  form.value.addresses = '';
  form.value.gateway = '';
  form.value.dns = '';
  form.value.mtu = iface?.mtu ?? 1500;
  form.value.bridgeName = 'br0';
  form.value.bridgeMethod = 'manual';
  form.value.bridgeAddresses = '';
  form.value.bridgeGateway = '';
  form.value.bridgeDNS = '';
  form.value.members = [];
  form.value.attachBridge = bridgeOptions.value[0]?.value ?? null;
}

function closeForm(): void {
  formKind.value = null;
  formInterface.value = null;
}

/** 把逗号/换行分隔的输入切成条目。用户从别处粘贴地址时往往混用两种分隔符。 */
function splitList(raw: string): string[] {
  return raw
    .split(/[\s,，;；]+/)
    .map((item) => item.trim())
    .filter((item) => item !== '');
}

function submitForm(): void {
  const type = formKind.value;
  if (type === null) return;

  const payload: NetChangeRequest = { action: type === 'bridge-create' ? 'bridge-create' : type };

  if (type === 'address') {
    payload.action = 'address';
    payload.interface = formInterface.value?.name ?? '';
    payload.method = form.value.method;
    if (form.value.method === 'manual') {
      payload.addresses = splitList(form.value.addresses);
      payload.gateway = form.value.gateway.trim();
      payload.dns = splitList(form.value.dns);
    }
  } else if (type === 'mtu') {
    payload.action = 'mtu';
    payload.interface = formInterface.value?.name ?? '';
    payload.mtu = form.value.mtu;
  } else if (type === 'bridge-create') {
    payload.action = 'bridge-create';
    payload.bridge = form.value.bridgeName.trim();
    payload.members = form.value.members;
    payload.method = form.value.bridgeMethod;
    if (form.value.bridgeMethod === 'manual') {
      payload.addresses = splitList(form.value.bridgeAddresses);
      payload.gateway = form.value.bridgeGateway.trim();
      payload.dns = splitList(form.value.bridgeDNS);
    }
  } else {
    payload.action = 'bridge-attach';
    payload.interface = formInterface.value?.name ?? '';
    payload.bridge = form.value.attachBridge ?? '';
  }

  void submitChange(payload);
}

// ---- 需要二次确认的危险操作 ----

interface DangerAction {
  title: string;
  message: string;
  detail: string;
  confirmText: string;
  danger: boolean;
  payload: NetChangeRequest;
}

const danger = ref<DangerAction | null>(null);

const showDanger = computed({
  get: () => danger.value !== null,
  set: (value: boolean) => {
    if (!value) danger.value = null;
  },
});

function askToggle(iface: NetInterface): void {
  const turningOff = iface.adminUp;
  danger.value = {
    title: turningOff ? `停用 ${iface.name}` : `启用 ${iface.name}`,
    message: turningOff
      ? `将停用连接「${iface.connName}」，该网口上的连接会立即中断；若它是面板当前使用的管理口，面板会失去连接。`
      : `将启用连接「${iface.connName}」。`,
    detail: '变更由 NetworkManager 检查点保护：窗口内未确认会自动回滚到现在的配置。',
    confirmText: turningOff ? '停用' : '启用',
    danger: turningOff,
    payload: { action: 'link-state', interface: iface.name, up: !iface.adminUp },
  };
}

function askDetach(iface: NetInterface): void {
  danger.value = {
    title: `把 ${iface.name} 移出 ${iface.master}`,
    message: `移出后 ${iface.name} 会按自己的连接配置重新取地址，${iface.master} 可能因此失去网络。`,
    detail: '变更由 NetworkManager 检查点保护：窗口内未确认会自动回滚到现在的配置。',
    confirmText: '移出',
    danger: true,
    payload: { action: 'bridge-detach', interface: iface.name },
  };
}

function askDeleteBridge(iface: NetInterface): void {
  danger.value = {
    title: `删除网桥 ${iface.name}`,
    message:
      iface.members.length > 0
        ? `将删除网桥 ${iface.name} 并释放成员 ${iface.members.join('、')}。网桥上的 IP 配置会一并消失。`
        : `将删除网桥 ${iface.name}。`,
    detail: '变更由 NetworkManager 检查点保护：窗口内未确认会自动回滚到现在的配置。',
    confirmText: '删除',
    danger: true,
    payload: { action: 'bridge-delete', bridge: iface.name },
  };
}

function onInterfaceAction(key: string, iface: NetInterface): void {
  switch (key) {
    case 'address':
      openForm('address', iface);
      break;
    case 'mtu':
      openForm('mtu', iface);
      break;
    case 'toggle':
      askToggle(iface);
      break;
    case 'bridge-attach':
      openForm('bridge-attach', iface);
      break;
    case 'bridge-detach':
      askDetach(iface);
      break;
    case 'bridge-delete':
      askDeleteBridge(iface);
      break;
    case 'properties':
      propertiesInterface.value = iface;
      break;
  }
}

// ---- 接口属性 ----

const propertiesInterface = ref<NetInterface | null>(null);

/** n-modal 的 v-model 需要一个可写布尔值：关闭即清空目标接口。 */
const showProperties = computed({
  get: () => propertiesInterface.value !== null,
  set: (value: boolean) => {
    if (!value) propertiesInterface.value = null;
  },
});

/**
 * 属性弹窗里的键值对。
 *
 * 口径与展开行一致，额外补上连接信息与实时速率 —— 这两项在展开行里没有，
 * 但排查"这个网口现在到底连到哪、跑多快"时正好要看。
 */
const propertiesFacts = computed<[string, string][]>(() => {
  const iface = propertiesInterface.value;
  if (!iface) return [];
  return [
    ['接口', iface.name],
    ['类型', typeLabel(iface.type)],
    ['归属', ownerLabel(iface.owner) || '—'],
    ['状态', stateLabel(iface)],
    ['地址', addrText(iface)],
    ['MAC', iface.mac || '—'],
    ['MTU', String(iface.mtu)],
    ['所属网桥', iface.master || '—'],
    ['网桥成员', iface.members.length ? iface.members.join('、') : '—'],
    ['连接名', iface.connName || '—'],
    ['连接状态', iface.connState || '—'],
    ['速率', iface.speedMbps > 0 ? `${iface.speedMbps} Mb/s` : '—'],
    ['实时速率', `↓ ${rateOne(iface.rxRate)} ↑ ${rateOne(iface.txRate)}`],
    ['累计流量', `↓ ${formatBytes(iface.rxBytes)} ↑ ${formatBytes(iface.txBytes)}`],
  ];
});

function confirmDanger(): void {
  const action = danger.value;
  danger.value = null;
  if (action) void submitChange(action.payload);
}

// ---- 展示 ----

const interfaces = computed(() => snapshot.value?.interfaces ?? []);
const connectedCount = computed(() => interfaces.value.filter((iface) => iface.up).length);
const physicalCount = computed(() => interfaces.value.filter((iface) => iface.owner === 'physical').length);
const containerCount = computed(() => interfaces.value.filter((iface) => iface.owner === 'container').length);

/**
 * 顶部概览：吞吐、连接数与丢包率。
 *
 * 流量按所有接口求和：用户问的是"这台机器进出了多少"，而不是"eth0 多少"——
 * 逐接口的读数在下面的接口表里，这里给总量。首次采样没有基准值时速率为 0，
 * 此时显示「采样中」而不是一个假的 0 B/s。
 */
const netStats = computed(() => {
  const s = snapshot.value;
  const ready = s?.deltaReady ?? false;
  const rx = interfaces.value.reduce((sum, iface) => sum + iface.rxRate, 0);
  const tx = interfaces.value.reduce((sum, iface) => sum + iface.txRate, 0);
  const sockets = s?.connections ?? { tcp: 0, udp: 0 };
  const loss = s?.packetLoss;
  // 没有报文时百分比没有意义（0 / 0），显示占位符而不是一个看起来很好的 0%。
  const lossReady = (loss?.packets ?? 0) + (loss?.drops ?? 0) > 0;

  return [
    {
      key: 'rx',
      label: 'DOWNLOAD',
      value: ready ? formatRate(rx) : '采样中',
      unit: '',
      foot: `${connectedCount.value} 个接口在线`,
      text: true,
    },
    {
      key: 'tx',
      label: 'UPLOAD',
      value: ready ? formatRate(tx) : '采样中',
      unit: '',
      foot: `${physicalCount.value} 个物理网口 · ${containerCount.value} 个容器网络`,
      text: true,
    },
    {
      key: 'connections',
      label: 'CONNECTIONS',
      value: String(sockets.tcp + sockets.udp),
      unit: '',
      foot: `TCP ${sockets.tcp} · UDP ${sockets.udp}`,
      text: false,
    },
    {
      key: 'loss',
      label: 'PACKET LOSS',
      value: lossReady ? (loss?.percent ?? 0).toFixed(2) : '—',
      unit: lossReady ? '%' : '',
      // 口径如实写：内核只给累计计数，说成"近 15 分钟"是编的。
      foot: '自开机累计 · 排除回环',
      text: false,
    },
  ];
});

const showContainers = ref(false);

/**
 * 接口表格的数据。
 *
 * 容器网络（Docker 的网桥与一堆 veth）默认收起来：一台跑着十几个容器的机器
 * 上它们能有几十项，会把真正的物理网口挤出视野 —— 而"这台机器有几张真网卡"
 * 恰恰是用户打开这个窗口最想知道的事。
 */
const visibleInterfaces = computed(() =>
  interfaces.value.filter((iface) => showContainers.value || iface.owner !== 'container'),
);

/** 右栏只摆最常用的几条；完整的监听表在「监听端口」页签里。 */
const topListeners = computed(() => snapshot.value?.listeners.slice(0, 5) ?? []);

interface LinkNotice {
  key: string;
  text: string;
  hint: string;
  level: 'ok' | 'warn';
}

/**
 * 链路提示。
 *
 * 只从快照已有的字段推导，不编造数据：管理态已启用但运行态没起来，说明
 * 网线没插或对端端口没开；没有默认路由，说明这台机器多半出不了外网。
 * 两条都正常时给一条"未发现异常"，空白卡片比一句确认更让人怀疑没读到。
 */
const linkNotices = computed<LinkNotice[]>(() => {
  const notices: LinkNotice[] = [];
  for (const iface of interfaces.value) {
    // 容器网卡默认不展示，它的链路状态也不该在提示里喧宾夺主。
    if (iface.owner === 'container') continue;
    if (iface.adminUp && !iface.up) {
      notices.push({
        key: `down-${iface.name}`,
        text: `${iface.name} 已启用但链路未连接`,
        hint: '网线或对端未接通',
        level: 'warn',
      });
    }
  }
  if (snapshot.value && !snapshot.value.routes.some((route) => route.dst === 'default')) {
    notices.push({
      key: 'no-default-route',
      text: '未发现默认路由',
      hint: '目标机可能无法访问外网',
      level: 'warn',
    });
  }
  if (notices.length === 0) {
    notices.push({ key: 'healthy', text: '链路状态正常', hint: '未发现异常', level: 'ok' });
  }
  return notices;
});

/** 表格里的实时速率。没有基准值时给占位符，而不是把 0 B/s 当成真的没有流量。 */
function rateOne(rate: number): string {
  return snapshot.value?.deltaReady ? formatRate(rate) : '采样中';
}

/** 地址一行摆完：多个地址用 · 隔开，动态获取的在后面标出来。 */
function addrText(iface: NetInterface): string {
  if (iface.addrs.length === 0) return '—';
  return iface.addrs
    .map((addr) => `${addr.addr}/${addr.prefix}${addr.dynamic ? ' 动态' : ''}`)
    .join(' · ');
}

/**
 * 接口表：一行一个接口，展开行放 MAC / MTU / 网桥关系与累计流量。
 *
 * 默认窗口 1180px，主栏要给右侧的监听与提示卡片留位置，所以列宽按最小可读值
 * 给；更窄的窗口由表格横向滚动承接，不把一行接口折成两行 —— 逐行的对照
 * 关系比省一次滚动更重要。
 */
const interfaceColumns = computed<DataTableColumns<NetInterface>>(() => [
  {
    type: 'expand',
    width: 28,
    renderExpand: (row) => {
      const facts: [string, string][] = [
        ['MAC', row.mac || '—'],
        ['MTU', String(row.mtu)],
        ['所属网桥', row.master || '—'],
        ['网桥成员', row.members.length ? row.members.join('、') : '—'],
        ['累计流量', `↓ ${formatBytes(row.rxBytes)} ↑ ${formatBytes(row.txBytes)}`],
        ['网络连接', row.connName ? `${row.connName} · ${row.connState}` : '未由 NetworkManager 托管'],
      ];
      return h(
        'div',
        { class: 'netiface__facts' },
        facts.map(([label, value]) =>
          h('div', { class: 'netiface__fact' }, [
            h('span', { class: 'netiface__fact-k' }, label),
            h('span', { class: 'netiface__fact-v num' }, value),
          ]),
        ),
      );
    },
  },
  {
    title: '接口',
    key: 'name',
    width: 148,
    render: (row) =>
      h('div', { class: 'netiface__ident' }, [
        h('span', { class: 'netiface__name num' }, row.name),
        h('span', { class: 'netiface__tags' }, [
          h('span', { class: 'netiface__tag' }, typeLabel(row.type)),
          ownerLabel(row.owner)
            ? h('span', { class: `netiface__tag is-${row.owner}` }, ownerLabel(row.owner))
            : null,
        ]),
      ]),
  },
  {
    title: '地址',
    key: 'addr',
    width: 172,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, addrText(row)),
  },
  {
    title: '速率',
    key: 'speed',
    width: 76,
    render: (row) =>
      h('span', { class: 'num' }, row.speedMbps > 0 ? `${row.speedMbps} Mb/s` : '—'),
  },
  {
    title: '接收',
    key: 'rxRate',
    width: 84,
    render: (row) => h('span', { class: 'num' }, rateOne(row.rxRate)),
  },
  {
    title: '发送',
    key: 'txRate',
    width: 84,
    render: (row) => h('span', { class: 'num' }, rateOne(row.txRate)),
  },
  {
    title: '状态',
    key: 'state',
    width: 88,
    render: (row) =>
      h('span', { class: ['netiface__state', stateClass(row)] }, [
        h('span', { class: ['led', ledClass(row)], 'aria-hidden': 'true' }),
        stateLabel(row),
      ]),
  },
  {
    title: '操作',
    key: 'actions',
    width: 76,
    render: (row) =>
      h(
        NDropdown,
        {
          trigger: 'click',
          placement: 'bottom-end',
          options: interfaceActions(row),
          disabled: pending.value !== null,
          onSelect: (key: string | number) => onInterfaceAction(String(key), row),
        },
        {
          default: () =>
            h(
              NButton,
              { size: 'tiny', quaternary: true, 'aria-label': `${row.name} 的操作` },
              { default: () => '操作' },
            ),
        },
      ),
  },
]);

const routeColumns: DataTableColumns<NetRoute> = [
  {
    title: '目标',
    key: 'dst',
    minWidth: 200,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, row.dst),
  },
  {
    title: '网关',
    key: 'gateway',
    width: 170,
    render: (row) => h('span', { class: 'num' }, row.gateway || '—'),
  },
  {
    title: '接口',
    key: 'dev',
    width: 110,
    render: (row) => h('span', { class: 'num' }, row.dev || '—'),
  },
  {
    // 来源交代这条路由是谁装的：内核直连、DHCP 下发、RA 通告还是管理员静态加的。
    // 排查"网关为什么变了"时这一列往往是唯一的线索。
    title: '来源',
    key: 'protocol',
    width: 110,
    render: (row) => h('span', { class: 'num' }, row.protocol || '—'),
  },
  {
    title: '度量',
    key: 'metric',
    width: 80,
    render: (row) => h('span', { class: 'num' }, String(row.metric)),
  },
  {
    title: '族',
    key: 'family',
    width: 64,
    render: (row) => h('span', { class: 'num' }, row.family === 'ipv6' ? 'v6' : 'v4'),
  },
];

const listenerColumns: DataTableColumns<NetListener> = [
  {
    title: '协议',
    key: 'protocol',
    width: 80,
    render: (row) => h('span', { class: 'num' }, row.protocol.toUpperCase()),
  },
  {
    title: '监听地址',
    key: 'address',
    minWidth: 180,
    render: (row) => h('span', { class: 'num' }, row.address),
  },
  {
    title: '端口',
    key: 'port',
    width: 90,
    sorter: (a, b) => a.port - b.port,
    render: (row) => h('span', { class: 'num' }, String(row.port)),
  },
  {
    title: '进程',
    key: 'process',
    minWidth: 160,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, row.process || '—'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 86,
    render: (row) =>
      // 读不到 pid 时按钮置灰：pid 只有以 root 重采那份监听表才有，而目标机提不了权时
      // 那份整段不存在 —— 这种行上的"结束"没有任何目标可指，禁用比让它失败更诚实。
      h(
        NButton,
        {
          size: 'small',
          quaternary: true,
          type: 'error',
          disabled: row.pid === 0,
          title: row.pid === 0 ? '读不到进程号（该主机无法提权），无法结束' : undefined,
          onClick: () => askKill(row),
        },
        { default: () => '结束' },
      ),
  },
];

/**
 * 监听端口过多时靠肉眼翻页不现实，这里做一次前端过滤。
 *
 * 端口号要一并参与匹配：用户最常见的动作是"看看 8080 是谁占的"，
 * 而 8080 是端口列的内容，不参与匹配就搜不到。
 */
const filteredListeners = computed(() => {
  const rows = snapshot.value?.listeners ?? [];
  const keyword = listenerFilter.value.trim().toLowerCase();
  if (keyword === '') return rows;
  return rows.filter((row) =>
    `${row.protocol} ${row.address}:${row.port} ${row.process}`.toLowerCase().includes(keyword),
  );
});

// ---- 结束监听进程 ----

/** 待结束的那一条监听记录。 */
const killTarget = ref<NetListener | null>(null);
const killing = ref(false);

// n-modal 的 v-model:show 需要布尔型的 show，这里把可空的目标桥接过去。
const showKill = computed({
  get: () => killTarget.value !== null,
  set: (value: boolean) => {
    if (!value) killTarget.value = null;
  },
});

function askKill(row: NetListener): void {
  killTarget.value = row;
}

async function confirmKill(signal: 'TERM' | 'KILL'): Promise<void> {
  const row = killTarget.value;
  const id = target.value;
  if (!row || id === null) return;

  killing.value = true;
  try {
    await api.metrics.kill(id, row.pid, signal);
    killTarget.value = null;
    message.success(`已向 PID ${row.pid} 发送 SIG${signal}`);
    // 强制刷新：占着这个端口的进程已经收到信号，列表得立刻反映出来 ——
    // 否则用户会以为没生效，对着同一行再点一次。
    await refresh({ fresh: true });
  } catch (err) {
    message.error(describe(err, '结束进程失败'));
  } finally {
    killing.value = false;
  }
}

// ---- Hosts（/etc/hosts） ----

const HOSTS_PATH = '/etc/hosts';

const hostsFilter = ref('');

/**
 * 这台机器上的 hosts 能不能改。
 *
 * 两条边界都来自后端，这里只是提前把它们表现出来：宿主机是面板自己所在的
 * 系统（后端直接拒绝写它），提不了权的机器则注定写不动。让用户对着一个
 * 点了才报错的按钮，是最没必要的一种失败。
 */
const hostsEditable = computed(
  () => target.value !== null && target.value !== LOCAL_HOST_ID && !escalationWarning.value,
);

const hostsDisabledReason = computed(() => {
  if (target.value === LOCAL_HOST_ID) {
    return `宿主机只提供只读查看，不在面板上改它自己的 ${HOSTS_PATH}`;
  }
  return escalationWarning.value;
});

const filteredHosts = computed(() => {
  const rows = snapshot.value?.hosts ?? [];
  const keyword = hostsFilter.value.trim().toLowerCase();
  if (keyword === '') return rows;
  return rows.filter((row) =>
    `${row.ip} ${row.names.join(' ')} ${row.comment}`.toLowerCase().includes(keyword),
  );
});

const hostColumns: DataTableColumns<NetHostEntry> = [
  {
    title: 'IP 地址',
    key: 'ip',
    width: 160,
    render: (row) => h('span', { class: 'num' }, row.ip),
  },
  {
    title: '主机名',
    key: 'names',
    minWidth: 200,
    render: (row) => h('span', { class: 'num' }, row.names[0] ?? '—'),
  },
  {
    title: '别名',
    key: 'aliases',
    minWidth: 160,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, row.names.slice(1).join('、') || '—'),
  },
  {
    title: '注释',
    key: 'comment',
    minWidth: 140,
    ellipsis: { tooltip: true },
    render: (row) => row.comment || '—',
  },
  {
    title: '操作',
    key: 'actions',
    width: 132,
    render: (row) =>
      h(NSpace, { size: 4 }, {
        default: () => [
          h(
            NButton,
            {
              size: 'small',
              quaternary: true,
              disabled: !hostsEditable.value,
              onClick: () => openHostForm('edit', row),
            },
            { default: () => '编辑' },
          ),
          h(
            NButton,
            {
              size: 'small',
              quaternary: true,
              type: 'error',
              disabled: !hostsEditable.value,
              onClick: () => askRemoveHost(row),
            },
            { default: () => '删除' },
          ),
        ],
      }),
  },
];

/** 新增 / 编辑记录的弹窗状态，null 表示关闭。 */
const hostForm = ref<{
  kind: 'add' | 'edit';
  row: NetHostEntry | null;
  ip: string;
  names: string;
  comment: string;
} | null>(null);

const savingHost = ref(false);

const showHostForm = computed({
  get: () => hostForm.value !== null,
  set: (value: boolean) => {
    if (!value) hostForm.value = null;
  },
});

const hostFormTitle = computed(() => (hostForm.value?.kind === 'add' ? '新增 Hosts 记录' : '编辑 Hosts 记录'));

function openHostForm(kind: 'add' | 'edit', row: NetHostEntry | null): void {
  hostForm.value = {
    kind,
    row,
    ip: row?.ip ?? '',
    // 主机名与别名合在一个输入框里：它们在文件里本来就是同一行的一串名字，
    // 拆成两个框等于让用户自己判断"哪个才算别名"。
    names: row ? row.names.join(' ') : '',
    comment: row?.comment ?? '',
  };
}

async function submitHostForm(): Promise<void> {
  const form = hostForm.value;
  const id = target.value;
  if (!form || id === null) return;

  const payload: HostsChangeRequest = {
    action: form.kind === 'add' ? 'add' : 'update',
    // 带上"我读到的是哪一份"：服务端写入前会重读比对，对不上就拒绝，
    // 而不是拿这份旧内容把别人刚写进去的记录盖掉。
    hostsMd5: snapshot.value?.hostsMd5 ?? '',
    ip: form.ip,
    names: form.names.split(/\s+/).filter(Boolean),
    comment: form.comment,
  };
  if (form.kind === 'edit' && form.row) {
    payload.line = form.row.line;
    payload.oldNames = form.row.names;
  }

  savingHost.value = true;
  try {
    const result = await api.network.hostsChange(id, payload);
    hostForm.value = null;
    if (result.verified) {
      message.success(form.kind === 'add' ? '已新增记录' : '已保存修改');
    } else {
      // 改动确实写进去了，只是复核没通过 —— 这句话既不能当成功也不能当失败：
      // 说清楚"已写入但没验上"，用户才知道该去看一眼。
      message.warning(result.verifyNote || `改动已写入，但复核没通过，请检查 ${HOSTS_PATH}`);
    }
    await refresh({ fresh: true });
  } catch (err) {
    message.error(describe(err, '保存失败'));
  } finally {
    savingHost.value = false;
  }
}

function askRemoveHost(row: NetHostEntry): void {
  const id = target.value;
  if (id === null) return;

  dialog.warning({
    title: '删除 Hosts 记录',
    content: `将从 ${HOSTS_PATH} 中删除：${row.ip} ${row.names.join(' ')}`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: () => removeHost(row, id),
  });
}

async function removeHost(row: NetHostEntry, id: number): Promise<void> {
  try {
    const result = await api.network.hostsChange(id, {
      action: 'remove',
      hostsMd5: snapshot.value?.hostsMd5 ?? '',
      line: row.line,
      oldNames: row.names,
    });
    if (result.verified) {
      message.success('已删除记录');
    } else {
      message.warning(result.verifyNote || `删除已写入，但复核没通过，请检查 ${HOSTS_PATH}`);
    }
    await refresh({ fresh: true });
  } catch (err) {
    message.error(describe(err, '删除失败'));
  }
}

const topLedClass = computed(() => {
  if (lastError.value) return 'led--danger';
  if (target.value === null) return 'led--sig led--live';
  return 'led--ok';
});

const bannerClass = computed(() => {
  if (expired.value) return 'is-expired';
  if (verifyNote.value) return 'is-warning';
  return 'is-busy';
});
</script>

<template>
  <div ref="root" class="netwin">
    <div class="netwin__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">NETWORK</p>
        <span class="netwin__title page-title">
          <span class="led" :class="topLedClass" aria-hidden="true" />
          网络管理
        </span>
        <span class="netwin__note page-meta">
          <template v-if="snapshot">
            {{ targetName || '未选择主机' }} · 配置后端 {{ snapshot.manager }} · 物理口
            {{ physicalCount }} · 容器 {{ containerCount }} · {{ connectedCount }} 个已连接
          </template>
          <template v-else>{{ targetName || '未选择主机' }}</template>
        </span>
      </div>
      <!--
        工具栏这几个动作只留图标：并排的按钮一多，文字会把上面那行统计
        （配置后端 · 物理口 · 容器 · 已连接）挤没，而那行是每次打开窗口最想看的。
        语义交给 title 悬浮提示。图标槽位尺寸写死，传入禁用、轮询都不改宽度。
      -->
      <n-button
        v-if="snapshot?.manager === 'NetworkManager'"
        size="small"
        class="netbtn"
        title="新建网桥"
        :disabled="pending !== null"
        @click="openForm('bridge-create', null)"
      >
        <svg class="netbtn__glyph"><use href="#glyph-bridge" /></svg>
      </n-button>
      <n-button
        size="small"
        class="netbtn"
        title="网络拓扑图"
        :disabled="snapshot === null"
        @click="showTopology = true"
      >
        <svg class="netbtn__glyph"><use href="#glyph-topology" /></svg>
      </n-button>
      <!--
        不用 n-button 的 loading：它会在按钮里再插一个转圈图标，内容一换宽度就变，
        每轮轮询推一次工具栏就是那一抽。这里让刷新图标自己转 —— 它的形状本来就是
        "转一圈"，转起来比旁边多一个圈更贴题，宽度也纹丝不动。
      -->
      <n-button size="small" type="primary" class="netbtn" title="刷新" @click="onRefreshClick">
        <svg class="netbtn__glyph" :class="{ 'is-spinning': fetching }">
          <use href="#glyph-refresh" />
        </svg>
      </n-button>

      <!-- 主机胶囊放页头右端：它和标题回答的是同一个问题（现在看的是哪台机器）。 -->
      <host-picker
        v-if="props.hostId === undefined"
        v-model:host-id="target"
        v-model:host-name="targetName"
        include-local
      />
    </div>

    <div v-if="lastError" class="netwin__error">
      <span class="led led--danger" aria-hidden="true" />
      {{ lastError }}
    </div>

    <!--
      变更进行中的横幅：这是整个窗口最该被看见的东西。
      它要回答三个问题——正在改什么、还剩多少确认时间、如果失败会怎样。
    -->
    <div v-if="pending" class="netbanner" :class="bannerClass">
      <span class="led" :class="expired ? 'led--danger' : verifyNote ? 'led--warn led--live' : 'led--sig led--live'" aria-hidden="true" />
      <div class="netbanner__body">
        <div class="netbanner__title">{{ pending.summary }}</div>
        <div class="netbanner__meta">
          <template v-if="expired">
            确认窗口已结束，目标机应已自动回滚到变更前的配置，请刷新核对。
          </template>
          <template v-else-if="verifyNote">
            {{ verifyNote }} —— 面板会继续核对，你也可以直接回滚。
          </template>
          <template v-else-if="committing">正在核对变更结果…</template>
          <template v-else>
            等待主机恢复并确认 · 剩余 {{ countdownText }} 后自动回滚
          </template>
        </div>
      </div>
      <n-button size="small" quaternary @click="showCommands = true">命令与日志</n-button>
      <n-button v-if="!expired" size="small" @click="rollbackNow">立即回滚</n-button>
      <template v-if="expired">
        <n-button size="small" type="primary" @click="dismissExpired">知道了</n-button>
      </template>
      <!--
        "仍要保留"要到窗口快结束时才出现。刚开始核对那几轮里，DHCP 取地址这类
        正常延迟也会让状态暂时对不上，此时给一个"强行保留"的按钮，等于在诱导
        用户在配置尚未生效时就把回滚退路销毁掉。
      -->
      <template v-else-if="verifyNote && remainingSeconds <= 30">
        <n-button size="small" type="warning" @click="confirming = true">仍要保留…</n-button>
      </template>
    </div>

    <!-- 概览：先给整机进出量、连接数与丢包率，逐接口读数在下面的接口表里。 -->
    <div v-if="snapshot" class="ui-stats">
      <article v-for="card in netStats" :key="card.key" class="ui-card ui-stat">
        <span class="ui-stat__label">{{ card.label }}</span>
        <b class="ui-stat__value" :class="{ 'is-text': card.text }">
          {{ card.value }}<span v-if="card.unit">{{ card.unit }}</span>
        </b>
        <span class="ui-stat__foot">{{ card.foot }}</span>
      </article>
    </div>

    <n-tabs v-if="snapshot" class="netwin__tabs" type="line" animated>
      <n-tab-pane name="interfaces" :tab="`接口 (${interfaces.length})`">
        <div class="netiface">
          <div class="ui-grid netiface__grid">
            <article class="ui-card ui-card--flush netiface__main">
              <div class="ui-card__head">
                <div class="ui-card__ident">
                  <p class="ui-card__eyebrow">INTERFACES</p>
                  <h2 class="ui-card__title">网络接口</h2>
                </div>
                <div class="ui-card__tools">
                  <!--
                    容器网络默认不铺开。它和物理网口在表格里长得一样，
                    一台跑满容器的机器上几十个 veth 会把真正的网卡挤到滚动条下面。
                  -->
                  <n-checkbox v-if="containerCount > 0" v-model:checked="showContainers" size="small">
                    容器网络 {{ containerCount }}
                  </n-checkbox>
                  <span class="ui-card__meta">
                    {{ visibleInterfaces.length }} / {{ interfaces.length }} 个接口
                  </span>
                </div>
              </div>
              <n-data-table
                class="netiface__table"
                :columns="interfaceColumns"
                :data="visibleInterfaces"
                :bordered="false"
                :row-key="(row: NetInterface) => row.name"
                :scroll-x="760"
                size="small"
                flex-height
              />
            </article>

            <aside class="ui-side netiface__side">
              <article class="ui-card">
                <div class="ui-card__head">
                  <div class="ui-card__ident">
                    <p class="ui-card__eyebrow">LISTENING</p>
                    <h2 class="ui-card__title">监听端口</h2>
                  </div>
                  <span class="ui-card__meta">{{ snapshot.listeners.length }}</span>
                </div>
                <div v-if="topListeners.length > 0" class="ui-kv">
                  <div
                    v-for="port in topListeners"
                    :key="`${port.protocol}-${port.address}-${port.port}`"
                  >
                    <span>{{ port.port }} / {{ port.protocol }}</span>
                    <b>{{ port.process || '—' }}</b>
                  </div>
                </div>
                <p v-else class="netiface__empty">没有监听端口</p>
              </article>

              <article class="ui-card">
                <div class="ui-card__head">
                  <div class="ui-card__ident">
                    <p class="ui-card__eyebrow">NOTICE</p>
                    <h2 class="ui-card__title">链路提示</h2>
                  </div>
                  <span class="ui-card__meta">{{ linkNotices.length }}</span>
                </div>
                <ul class="netiface__notices">
                  <li v-for="notice in linkNotices" :key="notice.key">
                    <span
                      class="led"
                      :class="notice.level === 'warn' ? 'led--warn' : 'led--ok'"
                      aria-hidden="true"
                    />
                    <div>
                      <b>{{ notice.text }}</b>
                      <span class="netiface__notice-hint">{{ notice.hint }}</span>
                    </div>
                  </li>
                </ul>
              </article>
            </aside>
          </div>
        </div>
      </n-tab-pane>

      <!--
        桥接页签紧挨着「接口」：它操作的就是接口本身，只是把网口挂到 OVS
        网桥上或摘下来，放远了会让人以为它是另一件不相干的事。

        它只在目标机装了 Open vSwitch 时出现，且不新建也不删除 OVS 网桥 ——
        这些网桥通常由目标机自己的网络服务维护（飞牛的 enp1s0-ovs 就是），
        删掉会连带毁掉那台机器的网络配置。
      -->
      <n-tab-pane v-if="ovsAvailable" name="ovs" :tab="`OVS 桥接 (${ovs?.bridges.length ?? 0})`">
        <ovs-bridge-panel
          v-if="ovs"
          :ovs="ovs"
          :interfaces="snapshot.interfaces"
          :rates="rateFeed"
          :default-route-devs="defaultRouteDevs"
          :escalation-warning="escalationWarning"
          :escalation-info="escalationInfo"
          :disabled="pending !== null"
          @apply="submitChange"
          @unpersist="ovsUnpersist"
        />
      </n-tab-pane>

      <n-tab-pane name="routes" :tab="`路由 (${snapshot.routes.length})`">
        <n-data-table
          class="netwin__table"
          :columns="routeColumns"
          :data="snapshot.routes"
          :bordered="false"
          :row-key="(row: NetRoute) => `${row.family}-${row.dst}-${row.dev}`"
          size="small"
          flex-height
        />
      </n-tab-pane>

      <n-tab-pane name="dns" tab="DNS">
        <div class="netdns">
          <section class="netdns__block">
            <h4 class="netdns__title">解析服务器</h4>
            <!-- resolv.conf 里只有本机 stub 地址时，直接把上游藏起来会让人误以为
                 这台机器真的在用 127.0.0.53 解析域名，必须显式说明。 -->
            <p v-if="snapshot.dns.stub" class="netdns__hint">
              /etc/resolv.conf 指向本机 stub resolver，真实上游在下方「按链路」里。
            </p>
            <ul v-if="snapshot.dns.servers.length" class="netdns__list">
              <li v-for="server in snapshot.dns.servers" :key="server" class="num">{{ server }}</li>
            </ul>
            <p v-else class="netdns__empty">未配置</p>
          </section>

          <section class="netdns__block">
            <h4 class="netdns__title">按链路</h4>
            <ul v-if="snapshot.dns.byLink.length" class="netdns__list">
              <li v-for="link in snapshot.dns.byLink" :key="link.link">
                <span class="netdns__link">{{ link.link }}</span>
                <span class="num">{{ link.servers.join(' , ') }}</span>
              </li>
            </ul>
            <p v-else class="netdns__empty">未按链路单独配置</p>
          </section>

          <section class="netdns__block">
            <h4 class="netdns__title">搜索域</h4>
            <ul v-if="snapshot.dns.search.length" class="netdns__list">
              <li v-for="domain in snapshot.dns.search" :key="domain" class="num">{{ domain }}</li>
            </ul>
            <p v-else class="netdns__empty">未配置</p>
          </section>
        </div>
      </n-tab-pane>

      <n-tab-pane name="hosts" :tab="`Hosts (${snapshot.hosts.length})`">
        <div class="nethosts">
          <div class="nethosts__bar">
            <n-input
              v-model:value="hostsFilter"
              class="nethosts__filter"
              size="small"
              clearable
              placeholder="按 IP / 主机名 / 注释筛选"
              :input-props="{ 'aria-label': '筛选 Hosts 记录' }"
            />
            <n-button
              size="small"
              type="primary"
              :disabled="!hostsEditable"
              :title="hostsDisabledReason || undefined"
              @click="openHostForm('add', null)"
            >
              新增条目
            </n-button>
          </div>
          <!-- 不能改的原因摆在表格上方，而不是只在按钮的 title 里：触屏点不出 title，
               而"为什么这台机器改不了"正是用户此刻唯一需要知道的事。 -->
          <n-alert v-if="!hostsEditable && hostsDisabledReason" type="warning">
            {{ hostsDisabledReason }}
          </n-alert>
          <n-data-table
            class="netwin__table"
            :columns="hostColumns"
            :data="filteredHosts"
            :bordered="false"
            :row-key="(row: NetHostEntry) => String(row.line)"
            size="small"
            flex-height
          />
        </div>
      </n-tab-pane>

      <n-tab-pane name="listeners" :tab="`监听端口 (${snapshot.listeners.length})`">
        <div class="netlisten">
          <n-input
            v-model:value="listenerFilter"
            class="netlisten__filter"
            size="small"
            clearable
            placeholder="按端口 / 地址 / 进程筛选"
            :input-props="{ 'aria-label': '筛选监听端口' }"
          />
          <n-data-table
            class="netwin__table"
            :columns="listenerColumns"
            :data="filteredListeners"
            :bordered="false"
            :row-key="(row: NetListener) => `${row.protocol}-${row.address}-${row.port}`"
            size="small"
            flex-height
          />
        </div>
      </n-tab-pane>
    </n-tabs>

    <div v-else-if="!lastError" class="netwin__placeholder">
      {{ target === null ? '请先选择一台主机' : fetching ? '正在读取网络信息…' : '暂无数据' }}
    </div>

    <!-- 结束监听端口上的进程：与进程窗口同一套确认流程，默认先 SIGTERM -->
    <n-modal v-model:show="showKill" preset="card" title="结束监听进程" style="width: 480px">
      <p class="kill__hint">
        即将结束 <strong>PID {{ killTarget?.pid }}</strong>（{{ killTarget?.process || '未知进程' }}），
        它监听在 {{ killTarget?.protocol?.toUpperCase() }} {{ killTarget?.address }}:{{ killTarget?.port }}。
      </p>
      <p class="kill__hint">
        默认先发送 SIGTERM，让进程有机会保存数据并释放资源；仅当它无响应时才使用强制结束。
      </p>
      <!-- 这里只说"会以 root 执行"，不说"无法提权就会失败"：pid 能显示出来本身
           就说明提权可用（它来自以 root 重采的那份监听表），而结束自己的进程
           本来也不需要 root —— 那句警告摆在这个弹窗里是错的。 -->
      <n-alert v-if="escalationInfo" type="info">{{ escalationInfo }}</n-alert>
      <template #footer>
        <n-space justify="end">
          <n-button @click="killTarget = null">取消</n-button>
          <n-button :loading="killing" @click="confirmKill('TERM')">发送 SIGTERM</n-button>
          <n-button type="error" :loading="killing" @click="confirmKill('KILL')">强制结束</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 新增 / 编辑 Hosts 记录。表单样式复用网络变更表单的类，字段标签与输入框
         因此和别处对齐在同一条竖线上。 -->
    <n-modal v-model:show="showHostForm" preset="card" :title="hostFormTitle" style="width: 480px">
      <div v-if="hostForm" class="netform">
        <div class="netform__field">
          <span class="netform__label">IP 地址</span>
          <n-input
            v-model:value="hostForm.ip"
            size="small"
            placeholder="例如 192.168.1.10"
            :input-props="{ 'aria-label': 'IP 地址' }"
          />
        </div>
        <div class="netform__field">
          <span class="netform__label">主机名</span>
          <n-input
            v-model:value="hostForm.names"
            size="small"
            placeholder="多个用空格分隔：nas nas.lan"
            :input-props="{ 'aria-label': '主机名与别名' }"
          />
        </div>
        <div class="netform__field">
          <span class="netform__label">注释</span>
          <n-input
            v-model:value="hostForm.comment"
            size="small"
            placeholder="可留空"
            :input-props="{ 'aria-label': '注释' }"
          />
        </div>
      </div>
      <p class="netform__tip">
        将写入目标机的 {{ HOSTS_PATH }}。保存前面板会先核对文件有没有被别处改过，
        已被改过时会让你刷新后重试，而不是把别人的改动盖掉。
      </p>
      <template #footer>
        <n-space justify="end">
          <n-button @click="hostForm = null">取消</n-button>
          <n-button type="primary" :loading="savingHost" @click="submitHostForm">保存</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 变更表单：改地址 / MTU / 新建网桥 / 加入网桥 -->
    <n-modal v-model:show="showForm" preset="card" :title="formTitle" style="width: 560px" @after-leave="closeForm">
      <div class="netform">
        <!-- 提权提醒放在表单最前面：用户就是在这张弹窗里按下"下发变更"的，
             得在按之前知道这次会不会以 root 执行、以及是不是注定失败。 -->
        <n-alert v-if="escalationWarning" type="error">{{ escalationWarning }}</n-alert>
        <n-alert v-else-if="escalationInfo" type="info">{{ escalationInfo }}</n-alert>
        <template v-if="formKind === 'address'">
          <div class="netform__field">
            <span class="netform__label">获取方式</span>
            <n-radio-group v-model:value="form.method" size="small">
              <n-radio value="manual">静态地址</n-radio>
              <n-radio value="auto">DHCP（自动获取）</n-radio>
            </n-radio-group>
          </div>
          <template v-if="form.method === 'manual'">
            <div class="netform__field">
              <span class="netform__label">地址</span>
              <n-input
                v-model:value="form.addresses"
                type="textarea"
                :rows="2"
                placeholder="192.168.1.50/24（多个地址用逗号或换行分隔）"
              />
            </div>
            <div class="netform__field">
              <span class="netform__label">网关</span>
              <n-input v-model:value="form.gateway" placeholder="留空表示不使用网关" />
            </div>
            <div class="netform__field">
              <span class="netform__label">DNS</span>
              <n-input v-model:value="form.dns" placeholder="223.5.5.5, 1.1.1.1（可留空）" />
            </div>
          </template>
        </template>

        <template v-else-if="formKind === 'mtu'">
          <div class="netform__field">
            <span class="netform__label">MTU</span>
            <n-input-number v-model:value="form.mtu" :min="68" :max="65535" style="width: 180px" />
          </div>
          <p class="netform__tip">常见取值：以太网 1500，开启巨帧的交换机与存储网络常用 9000。</p>
        </template>

        <template v-else-if="formKind === 'bridge-create'">
          <div class="netform__field">
            <span class="netform__label">网桥名</span>
            <n-input v-model:value="form.bridgeName" placeholder="br0" style="width: 200px" />
          </div>
          <div class="netform__field">
            <span class="netform__label">成员网口</span>
            <n-select
              v-model:value="form.members"
              multiple
              :options="memberOptions"
              placeholder="选择要挂到网桥上的网口"
            />
          </div>
          <div class="netform__field">
            <span class="netform__label">网桥地址</span>
            <n-radio-group v-model:value="form.bridgeMethod" size="small">
              <n-radio value="manual">静态地址</n-radio>
              <n-radio value="auto">DHCP（自动获取）</n-radio>
            </n-radio-group>
          </div>
          <template v-if="form.bridgeMethod === 'manual'">
            <div class="netform__field">
              <span class="netform__label">地址</span>
              <n-input v-model:value="form.bridgeAddresses" placeholder="192.168.1.10/24" />
            </div>
            <div class="netform__field">
              <span class="netform__label">网关</span>
              <n-input v-model:value="form.bridgeGateway" placeholder="192.168.1.1" />
            </div>
            <div class="netform__field">
              <span class="netform__label">DNS</span>
              <n-input v-model:value="form.bridgeDNS" placeholder="223.5.5.5（可留空）" />
            </div>
          </template>
          <p class="netform__tip">
            成员网口原有的 IP 配置会被网桥接管：地址要配在网桥上，而不是网口上。
          </p>
        </template>

        <template v-else-if="formKind === 'bridge-attach'">
          <div class="netform__field">
            <span class="netform__label">目标网桥</span>
            <n-select v-model:value="form.attachBridge" :options="bridgeOptions" placeholder="选择网桥" />
          </div>
          <p class="netform__tip">
            {{ formInterface?.name }} 现有的地址会失效，由网桥统一提供地址；若它正承载面板的连接，会立即中断。
          </p>
        </template>

        <div class="netform__warning">
          <span class="led led--warn" aria-hidden="true" />
          <span>
            变更生效时会短暂断开与目标机的连接。面板已要求 NetworkManager 建立回滚检查点：
            若 2 分钟内未确认，目标机会自动还原到现在的配置。
          </span>
        </div>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button @click="closeForm">取消</n-button>
          <n-button type="primary" :loading="applying" @click="submitForm">下发变更</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 接口属性：只读，任何网口都能打开 -->
    <n-modal
      v-model:show="showProperties"
      preset="card"
      :title="`接口属性 - ${propertiesInterface?.name ?? ''}`"
      style="width: 560px"
    >
      <div class="netiface__props">
        <div v-for="[label, value] in propertiesFacts" :key="label" class="netiface__fact">
          <span class="netiface__fact-k">{{ label }}</span>
          <span class="netiface__fact-v num">{{ value }}</span>
        </div>
      </div>
      <template #footer>
        <n-space justify="end">
          <n-button @click="propertiesInterface = null">关闭</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 危险操作二次确认 -->
    <n-modal v-model:show="showDanger" preset="card" :title="danger?.title" style="width: 520px">
      <p class="netconfirm__line">{{ danger?.message }}</p>
      <p class="netconfirm__note">{{ danger?.detail }}</p>
      <!-- 提权提醒放在说明之后、按钮之前：这里没有 flex 布局，放末尾就不必再补间距 -->
      <n-alert v-if="escalationWarning" type="error">{{ escalationWarning }}</n-alert>
      <n-alert v-else-if="escalationInfo" type="info">{{ escalationInfo }}</n-alert>
      <template #footer>
        <n-space justify="end">
          <n-button @click="danger = null">取消</n-button>
          <n-button :type="danger?.danger ? 'error' : 'primary'" @click="confirmDanger">
            {{ danger?.confirmText }}
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 确认失败时仍要保留：这一步必须再问一次 -->
    <n-modal v-model:show="confirming" preset="card" title="仍要保留这次配置？" style="width: 520px">
      <p class="netconfirm__line">
        目标机当前的网络状态与本次变更的预期不一致，通常是变更没能生效、已由检查点还原。
      </p>
      <p class="netconfirm__note">
        继续保留会销毁回滚检查点。若这台机器正是靠面板连接的，之后将无法通过面板把它改回来。
      </p>
      <template #footer>
        <n-space justify="end">
          <n-button @click="confirming = false">取消</n-button>
          <n-button type="warning" @click="keepAnyway">仍然保留</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 命令与执行日志 -->
    <n-modal v-model:show="showCommands" preset="card" title="变更命令与执行日志" style="width: 720px">
      <div class="netlog">
        <h4 class="netlog__title">在目标机上延迟执行的命令</h4>
        <pre class="netlog__code">{{ pending?.risky.join('\n') }}</pre>
        <h4 class="netlog__title">执行日志</h4>
        <pre class="netlog__code">{{ pendingLog || '（暂无输出。目标机可能正处于断连重连的过程中）' }}</pre>
      </div>
    </n-modal>

    <!--
      网络拓扑。接口列表回答"每个口什么状态"，这张图回答"它们怎么连起来的" ——
      排查断网时两个问题都要回答，但顺序应是先结构后状态。
      数据直接取当前这份快照，弹窗打开期间轮询还在跑，图上跟着一起更新。
    -->
    <n-modal
      v-model:show="showTopology"
      preset="card"
      title="网络拓扑"
      style="width: min(820px, 94vw)"
    >
      <div v-if="snapshot" class="nettopo">
        <network-topology
          :interfaces="snapshot.interfaces"
          :ovs="ovs"
          :default-route-devs="defaultRouteDevs"
        />
      </div>
    </n-modal>
  </div>
</template>
