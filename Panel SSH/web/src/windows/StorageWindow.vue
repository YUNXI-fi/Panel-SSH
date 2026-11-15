<script setup lang="ts">
/**
 * 存储管理窗口。
 *
 * 三块内容：块设备（含分区）、已挂载的文件系统、/etc/fstab。
 *
 * 与网络窗口的根本差别：这里的操作不会切断面板赖以操作的 SSH ——
 * 挂载与卸载失败的代价是"这次没成功"，而不是"再也连不上"。
 * 因此不需要确认回滚那一套，改为单次同步执行 + 执行后状态复核。
 * 复核这一步不能省：umount 报成功但目录仍挂着、fstab 条目写了却没生效，
 * 都属于"命令成功而结果不对"，把它们报成成功会直接损害用户对面板的信任。
 */
import { computed, h, ref, watch } from 'vue';
import {
  NAlert,
  NButton,
  NCheckbox,
  NDataTable,
  NInput,
  NModal,
  NRadio,
  NRadioGroup,
  NSpace,
  NSpin,
  NTabPane,
  NTabs,
  useMessage,
  type DataTableColumns,
} from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import ConfirmDialog from './ConfirmDialog.vue';
import FormatDialog from './FormatDialog.vue';
import PartitionDialog from './PartitionDialog.vue';
import SmartDialog from './SmartDialog.vue';
import UsageDialog from './UsageDialog.vue';
import { useVisiblePoll } from '../desktop/useVisiblePoll';
import { LOCAL_HOST_ID } from '../desktop/watchedHost';
import {
  ApiError,
  api,
  type BlockDevice,
  type EscalationInfo,
  type FstabEntry,
  type StorageMount,
  type StorageRequest,
  type StorageSnapshot,
} from '../api/client';
import { formatBytes } from '../api/format';

const props = defineProps<{ hostId?: number; hostName?: string }>();

const message = useMessage();

// 存储状态变化很慢（插拔设备、挂载操作），5 秒一轮足够，
// 也让目标机少跑几遍 lsblk。
const STORAGE_INTERVAL_MS = 5000;

const root = ref<HTMLElement | null>(null);
const target = ref<number | null>(props.hostId ?? null);
// 从桌面打开时由选择器回填：页头要显示"在操作哪台机器"，
// 只认 props 会一直停在「未选择主机」。
const targetName = ref(props.hostName ?? '');

watch(
  () => props.hostId,
  (id) => {
    if (id !== undefined) target.value = id;
  },
);

const snapshot = ref<StorageSnapshot | null>(null);
const fetching = ref(false);
const applying = ref(false);
const lastError = ref('');
// 目标机的提权方式：挂载、卸载、写 fstab 都要 root，弹窗前据此提醒用户；
// 无法提权的机器则提前说明这次注定失败，而不是让他点了才知道。
const escalation = ref<EscalationInfo | null>(null);

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 504) return `${err.message}。请检查目标机负载与网络`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

function applySnapshot(next: StorageSnapshot | undefined): void {
  if (next) snapshot.value = next;
}

async function refresh(options: { fresh?: boolean; silent?: boolean } = {}): Promise<void> {
  const id = target.value;
  if (id === null) return;

  // 已有数据时的轮询不点亮按钮：每 5 秒闪一次的转圈只是噪音，也是这个按钮
  // 看起来一直在抽的原因。首屏还没有数据时要亮，否则占位区会从"正在读取"
  // 变成"暂无数据"，而那是两件完全不同的事。
  const showLoading = !options.silent || snapshot.value === null;
  if (showLoading) fetching.value = true;
  try {
    snapshot.value = await api.storage.snapshot(id, options.fresh ?? false);
    lastError.value = '';
  } catch (err) {
    lastError.value = describe(err, '读取存储信息失败');
  } finally {
    if (showLoading) fetching.value = false;
  }
}

/** 点刷新。已经在读时不再叠一次 —— 按钮不用 loading 变灰，重复点击要在这里挡掉。 */
function onRefreshClick(): void {
  if (fetching.value) return;
  void refresh({ fresh: true });
}

useVisiblePoll({
  root,
  intervalMs: STORAGE_INTERVAL_MS,
  task: () => refresh({ silent: true }),
  enabled: () => target.value !== null,
});

/**
 * 取目标机的提权方式，供操作前的提醒使用。
 *
 * 不复用 refresh 那条链路：提醒是辅助信息，它的失败不该影响存储数据的展示，
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

/** 无法提权时的警告：操作注定失败，必须在动手前说清楚，而不是让它变成点了才知道的失败。 */
const escalationWarning = computed(() => {
  const info = escalation.value;
  if (!info || info.mode === 'root' || info.escalatable) return '';
  return '该主机无法提权，而此操作需要 root：目标机既不是 root 登录，也没有可用的提权路径（sudo/su）。请在面板里为该主机配置提权口令（root 口令，走 su），或以 root 登录后再试。';
});

watch(target, (id) => {
  if (id === null) return;
  // 换主机时先清空：否则新主机的数据到达前，窗口里还挂着上一台的设备列表，
  // 这时候点卸载会打到错误的机器上。
  snapshot.value = null;
  lastError.value = '';
  void refresh({ fresh: true });
  void loadEscalation();
});

/**
 * 宿主机只读。
 *
 * 后端对宿主机的存储变更一律拒绝（面板不该改自己所在系统的分区表与 fstab），
 * 读取仍走本地直采。这里提前禁用变更按钮，避免让用户点了才看到 400。
 */
const readOnlyHost = computed(() => target.value === LOCAL_HOST_ID);

// ---- 操作执行 ----

async function runAction(payload: StorageRequest): Promise<boolean> {
  const id = target.value;
  if (id === null) return false;

  applying.value = true;
  try {
    const result = await api.storage.apply(id, payload);
    applySnapshot(result.snapshot);
    closeDialogs();

    if (!result.verified) {
      // 命令跑完了但状态不对：既不能报成功，也不该说失败 ——
      // 把复核结论原样带给用户，让他知道该去看哪一项。
      message.warning(`${result.summary}：${result.verifyNote ?? '状态复核未通过'}`);
    } else {
      message.success(result.summary);
    }
    void refresh({ fresh: true });
    return true;
  } catch (err) {
    message.error(describe(err, '操作失败'));
    return false;
  } finally {
    applying.value = false;
  }
}

// ---- 设备列表（把 lsblk 的树拍平成带层级的行）----

interface DeviceRow extends BlockDevice {
  depth: number;
}

const deviceRows = computed<DeviceRow[]>(() => {
  const rows: DeviceRow[] = [];
  const walk = (devices: BlockDevice[], depth: number): void => {
    for (const device of devices) {
      rows.push({ ...device, depth });
      walk(device.children ?? [], depth + 1);
    }
  };
  walk(snapshot.value?.devices ?? [], 0);
  return rows;
});

const mounts = computed(() => snapshot.value?.mounts ?? []);
const fstab = computed(() => snapshot.value?.fstab ?? []);
const raid = computed(() => snapshot.value?.raid ?? []);
const busyMounts = computed(() => mounts.value.filter((item) => item.kind !== 'virtual').length);

/**
 * 顶部容量概览。
 *
 * 四个数回答"这机器的存储现在是什么状态"，与下面按设备/挂载点铺开的明细互补：
 * 明细回答"是哪一块盘"，这里回答"整体还剩多少"。虚拟文件系统（tmpfs/proc）
 * 的容量由内存决定，计入总量会让读数变成假的大盘，一律排除。
 */
const storageStats = computed(() => {
  const real = mounts.value.filter((item) => !item.virtual && item.total > 0 && item.usePercent >= 0);
  const total = real.reduce((sum, item) => sum + item.total, 0);
  const used = real.reduce((sum, item) => sum + item.used, 0);
  const worst = real.reduce<(typeof real)[number] | null>(
    (acc, item) => (acc === null || item.usePercent > acc.usePercent ? item : acc),
    null,
  );

  return [
    {
      key: 'devices',
      label: 'DEVICES',
      value: String(snapshot.value?.devices.length ?? 0),
      unit: '',
      foot: `${raid.value.length} 组 RAID · ${fstab.value.length} 条 fstab`,
      text: false,
    },
    {
      key: 'mounts',
      label: 'MOUNTED',
      value: String(busyMounts.value),
      unit: '',
      foot: `共 ${mounts.value.length} 个挂载点（含虚拟）`,
      text: false,
    },
    {
      key: 'capacity',
      label: 'CAPACITY',
      value: total > 0 ? formatBytes(total) : '—',
      unit: '',
      foot: used > 0 ? `已用 ${formatBytes(used)}` : '暂无容量信息',
      text: true,
    },
    {
      key: 'usage',
      label: 'PEAK USAGE',
      value: worst ? worst.usePercent.toFixed(0) : '—',
      unit: worst ? '%' : '',
      foot: worst ? worst.target : '暂无挂载点信息',
      text: false,
    },
  ];
});

const deviceTypes: Record<string, string> = {
  disk: '磁盘',
  part: '分区',
  lvm: '逻辑卷',
  raid: 'RAID',
  rom: '光驱',
  loop: '镜像',
};

function deviceTypeLabel(type: string): string {
  return deviceTypes[type] ?? type;
}

/**
 * 该设备能不能挂载。
 *
 * 三个前提缺一不可：有文件系统（否则得先格式化）、没挂载在任何地方、
 * 不是系统盘。整盘上还有分区时也不给挂载按钮 —— 那种盘该挂的是分区。
 */
function canMount(device: BlockDevice): boolean {
  if (device.system || device.readOnly) return false;
  if (device.fstype === '' || device.fstype === 'swap') return false;
  if (device.mountPoints.length > 0) return false;
  if (device.type === 'disk' && device.children.length > 0) return false;
  return true;
}

/** 设备的挂载点（整盘要把子级的挂载点也算上，否则看起来像没用过）。 */
function deviceMountPoints(device: BlockDevice): string[] {
  const points = [...device.mountPoints];
  for (const child of device.children ?? []) {
    points.push(...deviceMountPoints(child));
  }
  return points;
}

/**
 * 设备卡里的用量条。
 *
 * 一个设备可能挂着多个分区，这里取第一个有真实容量的挂载点：
 * 设备页回答"这块盘用了多少"，完整的分区容量在「挂载点」页里。
 * 虚拟文件系统没有容量概念，直接跳过，避免画出 0% 的假条。
 */
function deviceUsage(device: BlockDevice): { percent: number; used: number; total: number } | null {
  for (const target of deviceMountPoints(device)) {
    const mount = mounts.value.find(
      (item) => item.target === target && !item.virtual && item.total > 0 && item.usePercent >= 0,
    );
    if (mount) {
      return { percent: mount.usePercent, used: mount.used, total: mount.total };
    }
  }
  return null;
}

function hasNfs(): boolean {
  return (snapshot.value?.tools ?? []).includes('nfs');
}

function hasCifs(): boolean {
  return (snapshot.value?.tools ?? []).includes('cifs');
}

// ---- 弹窗 ----

type DialogKind = 'mount-local' | 'mount-network' | 'unmount';

const dialog = ref<DialogKind | null>(null);
const dialogDevice = ref<BlockDevice | null>(null);
const dialogMount = ref<StorageMount | null>(null);

const localForm = ref({ target: '', options: '', persist: true });
const netForm = ref({
  protocol: 'nfs' as 'nfs' | 'cifs',
  server: '',
  share: '',
  username: '',
  password: '',
  target: '',
  options: '',
  persist: true,
});

const showDialog = computed({
  get: () => dialog.value !== null,
  set: (value: boolean) => {
    if (!value) dialog.value = null;
  },
});

const dialogTitle = computed(() => {
  switch (dialog.value) {
    case 'mount-local':
      return `挂载 ${dialogDevice.value?.path ?? ''}`;
    case 'mount-network':
      return '挂载网络存储';
    case 'unmount':
      return `卸载 ${dialogMount.value?.target ?? ''}`;
    default:
      return '';
  }
});

function closeDialogs(): void {
  dialog.value = null;
  dialogDevice.value = null;
  dialogMount.value = null;
}

/** 默认挂载点：放进 /mnt 下，用设备名或标签，避免和系统目录撞车。 */
function defaultTarget(device: BlockDevice): string {
  return `/mnt/${device.label || device.name}`;
}

function openLocalMount(device: BlockDevice): void {
  dialogDevice.value = device;
  localForm.value = { target: defaultTarget(device), options: '', persist: true };
  dialog.value = 'mount-local';
}

function openNetworkMount(): void {
  netForm.value = {
    protocol: hasNfs() ? 'nfs' : 'cifs',
    server: '',
    share: '',
    username: '',
    password: '',
    target: '',
    options: '',
    persist: true,
  };
  dialog.value = 'mount-network';
}

function openUnmount(mount: StorageMount): void {
  dialogMount.value = mount;
  dialog.value = 'unmount';
}

function parseOptions(raw: string): string[] {
  return raw
    .split(/[\s,，]+/)
    .map((item) => item.trim())
    .filter((item) => item !== '');
}

function submitLocalMount(): void {
  const device = dialogDevice.value;
  if (!device) return;
  void runAction({
    action: 'mount-local',
    device: device.path,
    target: localForm.value.target.trim(),
    options: parseOptions(localForm.value.options),
    persist: localForm.value.persist,
  });
}

function submitNetworkMount(): void {
  const form = netForm.value;
  void runAction({
    action: form.protocol === 'nfs' ? 'mount-nfs' : 'mount-cifs',
    server: form.server.trim(),
    share: form.share.trim(),
    username: form.username,
    password: form.password,
    target: form.target.trim(),
    options: parseOptions(form.options),
    persist: form.persist,
  });
}

function submitUnmount(): void {
  const mount = dialogMount.value;
  if (!mount) return;
  void runAction({ action: 'unmount', target: mount.target });
}

// ---- 磁盘级操作（格式化 / 分区 / 健康 / 用量）----

/**
 * 这四项各自开一个弹窗，而不是挤进上面那套挂载表单：
 * 它们的风险与交互本来就不同 —— 格式化要挑文件系统并二次确认，分区管理是
 * 一张会变的列表，健康和用量则只是看一眼就关。
 */
const formatDevice = ref<BlockDevice | null>(null);
const partitionDevice = ref<BlockDevice | null>(null);
const smartDevice = ref('');
const usageTarget = ref('');

/** 正在确认移除的 fstab 条目。 */
const fstabDrop = ref<FstabEntry | null>(null);

/** 按路径在设备树里查找。 */
function findDeviceByPath(devices: BlockDevice[], path: string): BlockDevice | null {
  for (const device of devices) {
    if (device.path === path) return device;
    const found = findDeviceByPath(device.children ?? [], path);
    if (found) return found;
  }
  return null;
}

// 弹窗拿到的是"打开那一刻"的设备对象，而快照每 5 秒会把整棵树重建一次。
// 不跟着替换的话，分区列表会僵在打开时的样子 —— 刚建好的分区在弹窗里看不见，
// 用户只能关掉再打开一次来确认操作到底成没成。
watch(snapshot, (next) => {
  if (!next) return;
  if (formatDevice.value) {
    formatDevice.value = findDeviceByPath(next.devices, formatDevice.value.path);
  }
  if (partitionDevice.value) {
    partitionDevice.value = findDeviceByPath(next.devices, partitionDevice.value.path);
  }
});

/** 能否格式化：非系统盘、没挂载、非只读，且不是"还有分区的整盘"。 */
function canFormat(device: BlockDevice): boolean {
  if (device.system || device.readOnly || device.busy) return false;
  if (device.mountPoints.length > 0) return false;
  if (device.type === 'disk' && device.children.length > 0) return false;
  return true;
}

/** 分区管理只对整盘开放：分区之下没有"分区表"这一层。 */
function canPartition(device: BlockDevice): boolean {
  return device.type === 'disk';
}

/** SMART 是设备级的计数器，分区没有自己的一份。 */
function canSmart(device: BlockDevice): boolean {
  return device.type === 'disk';
}

const fstabSummary = computed(() => {
  const entry = fstabDrop.value;
  if (!entry) return '';
  // 无法提权时这次移除注定失败，把警告并进摘要里（它是红色警示样式），
  // 比埋在下面的补充说明中更容易被看见。
  return `从 /etc/fstab 里移除 ${entry.target}（${entry.source}）的条目。${escalationWarning.value}`;
});

const fstabDetail = computed(() => {
  const entry = fstabDrop.value;
  if (!entry) return '';
  const note = escalationInfo.value;
  // 目录还挂着却把条目删了，重启后它不会自己回来 —— 这是最容易踩空的一步。
  if (mounts.value.some((item) => item.target === entry.target)) {
    return `这个目录当前仍然挂着：条目移除后它不会立刻消失，但重启之后不会再自动挂载。${note}`;
  }
  return `移除条目不会动到目录里的数据，只是让系统不再自动挂载它。${note}`;
});

async function confirmFstabDrop(): Promise<void> {
  const entry = fstabDrop.value;
  if (!entry) return;
  const done = await runAction({ action: 'fstab-remove', target: entry.target });
  if (done) fstabDrop.value = null;
}

// ---- 展示 ----

const fstabColumns: DataTableColumns<FstabEntry> = [
  {
    title: '设备',
    key: 'source',
    minWidth: 220,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, row.source),
  },
  {
    title: '挂载点',
    key: 'target',
    minWidth: 160,
    render: (row) => h('span', { class: 'num' }, row.target),
  },
  {
    title: '类型',
    key: 'fstype',
    width: 100,
    render: (row) => h('span', { class: 'num' }, row.fstype),
  },
  {
    title: '选项',
    key: 'options',
    minWidth: 200,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, row.options),
  },
  {
    title: '操作',
    key: 'actions',
    width: 84,
    // 只删条目、不卸载：断开一块不再插上的盘、又想保留它原来的挂载点时，
    // 没有别的办法表达这个意图（卸载会把目录一起处理掉）。
    render: (row) =>
      h(
        NButton,
        {
          size: 'small',
          quaternary: true,
          type: 'error',
          disabled: readOnlyHost.value,
          onClick: () => {
            fstabDrop.value = row;
          },
        },
        { default: () => '移除' },
      ),
  },
];

function usePercentText(mount: StorageMount): string {
  if (mount.usePercent < 0) return '—';
  return `${mount.usePercent.toFixed(0)}%`;
}

/** 容量条宽度。没有容量概念时返回 0，条形自然不显示。 */
function useBarWidth(mount: StorageMount): string {
  if (mount.usePercent < 0) return '0%';
  return `${Math.min(100, Math.max(0, mount.usePercent))}%`;
}

function useBarClass(mount: StorageMount): string {
  if (mount.usePercent >= 90) return 'is-danger';
  if (mount.usePercent >= 75) return 'is-warn';
  return '';
}

const topLedClass = computed(() => {
  if (lastError.value) return 'led--danger';
  if (target.value === null) return 'led--sig led--live';
  return 'led--ok';
});
</script>

<template>
  <div ref="root" class="stowin">
    <div class="stowin__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">STORAGE</p>
        <span class="stowin__title page-title">
          <span class="led" :class="topLedClass" aria-hidden="true" />
          存储管理
        </span>
        <span class="stowin__note page-meta">
          <template v-if="snapshot">
            {{ targetName || '未选择主机' }} · {{ snapshot.devices.length }} 个磁盘/设备 ·
            {{ busyMounts }} 个已挂载
          </template>
          <template v-else>{{ targetName || '未选择主机' }}</template>
        </span>
      </div>
      <n-button size="small" :disabled="readOnlyHost" @click="openNetworkMount">挂载网络存储</n-button>
      <!-- 与网络管理同一个理由：用图标换掉文字，而不是在文字前再插一个，
           否则按钮会随"正在读取"的状态变宽，把工具栏推一下。 -->
      <n-button size="small" type="primary" class="refresh-btn" @click="onRefreshClick">
        <template #icon v-if="fetching">
          <n-spin :size="16" />
        </template>
        <template v-if="!fetching">刷新</template>
      </n-button>

      <!-- 主机胶囊放页头右端：它和标题回答的是同一个问题（现在看的是哪台机器）。 -->
      <host-picker
        v-if="props.hostId === undefined"
        v-model:host-id="target"
        v-model:host-name="targetName"
        include-local
      />
    </div>

    <div v-if="readOnlyHost" class="stowin__readonly">
      宿主机只提供只读查看：面板不修改自己所在的系统，挂载、卸载与格式化请在远端主机上操作。
    </div>

    <div v-if="lastError" class="stowin__error">
      <span class="led led--danger" aria-hidden="true" />
      {{ lastError }}
    </div>

    <!-- 容量概览：先给整体水位，再往下看具体是哪块盘。 -->
    <div v-if="snapshot" class="ui-stats">
      <article v-for="card in storageStats" :key="card.key" class="ui-card ui-stat">
        <span class="ui-stat__label">{{ card.label }}</span>
        <b class="ui-stat__value" :class="{ 'is-text': card.text }">
          {{ card.value }}<span v-if="card.unit">{{ card.unit }}</span>
        </b>
        <span class="ui-stat__foot">{{ card.foot }}</span>
      </article>
    </div>

    <n-tabs v-if="snapshot" class="stowin__tabs" type="line" animated>
      <n-tab-pane name="devices" :tab="`设备 (${deviceRows.length})`">
        <div class="stodevices">
          <article
            v-for="row in deviceRows"
            :key="row.path"
            class="stodevice"
            :class="[`is-depth-${Math.min(row.depth, 2)}`, { 'is-system': row.system }]"
          >
            <header class="stodevice__head">
              <span class="stodevice__badge">{{ deviceTypeLabel(row.type) }}</span>
              <span class="stodevice__name num">{{ row.path }}</span>
              <span class="stodevice__size num">{{ formatBytes(row.size) }}</span>
              <!-- 这三类标记决定了用户能对它做什么，放在最显眼的位置 -->
              <span v-if="row.system" class="ui-tag is-danger">系统盘</span>
              <span v-if="row.removable" class="ui-tag">可移动</span>
              <span v-if="!row.fstype" class="ui-tag is-warn">未格式化</span>
              <span class="stodevice__spacer" />
              <n-button
                v-if="row.mountPoints.length > 0"
                size="tiny"
                quaternary
                :disabled="applying || readOnlyHost"
                @click="openUnmount({ target: row.mountPoints[0] } as StorageMount)"
              >
                卸载
              </n-button>
              <n-button
                v-else-if="canMount(row)"
                size="tiny"
                quaternary
                :disabled="applying || readOnlyHost"
                @click="openLocalMount(row)"
              >
                挂载…
              </n-button>
              <!-- 格式化独立成一支，不接在上面的 v-else-if 链尾：
                   否则"已格式化但没挂载"的设备就只剩「挂载」一个出口，
                   用户想换文件系统时无路可走。 -->
              <n-button
                v-if="canFormat(row)"
                size="tiny"
                quaternary
                :disabled="readOnlyHost"
                @click="formatDevice = row"
              >
                格式化…
              </n-button>
              <n-button
                v-if="canPartition(row)"
                size="tiny"
                quaternary
                :disabled="readOnlyHost"
                @click="partitionDevice = row"
              >
                分区…
              </n-button>
              <n-button v-if="canSmart(row)" size="tiny" quaternary @click="smartDevice = row.path">
                健康
              </n-button>
            </header>

            <div class="stodevice__facts">
              <div v-if="row.model" class="stodevice__row">
                <span class="stodevice__k">型号</span>
                <span class="stodevice__v num">{{ row.model }}</span>
              </div>
              <div v-if="row.fstype" class="stodevice__row">
                <span class="stodevice__k">文件系统</span>
                <span class="stodevice__v num">
                  {{ row.fstype }}<template v-if="row.label"> · {{ row.label }}</template>
                </span>
              </div>
              <div v-if="row.uuid" class="stodevice__row">
                <span class="stodevice__k">UUID</span>
                <span class="stodevice__v num">{{ row.uuid }}</span>
              </div>
              <div v-if="deviceMountPoints(row).length" class="stodevice__row">
                <span class="stodevice__k">挂载于</span>
                <span class="stodevice__v num">{{ deviceMountPoints(row).join('、') }}</span>
              </div>
            </div>

            <div v-if="deviceUsage(row)" class="stodevice__usage">
              <span class="stodevice__usage-pct num">{{ deviceUsage(row)?.percent.toFixed(0) }}%</span>
              <span
                class="stodevice__usage-bar"
                :class="{ 'is-warn': (deviceUsage(row)?.percent ?? 0) >= 75 }"
              >
                <span :style="{ width: `${Math.max(0, Math.min(100, deviceUsage(row)?.percent ?? 0))}%` }" />
              </span>
              <span class="stodevice__usage-cap num">
                {{ formatBytes(deviceUsage(row)?.used ?? 0) }} / {{ formatBytes(deviceUsage(row)?.total ?? 0) }}
              </span>
            </div>
          </article>
        </div>
      </n-tab-pane>

      <n-tab-pane name="mounts" :tab="`挂载点 (${busyMounts})`">
        <div class="stomounts">
          <article v-for="mount in mounts" :key="mount.target" class="stomount">
            <header class="stomount__head">
              <span class="stomount__target num">{{ mount.target }}</span>
              <span v-if="mount.kind === 'network'" class="ui-tag">网络</span>
              <!-- 这一条决定了"重启后它还在不在"，是用户最常困惑的点 -->
              <span v-if="mount.persistent" class="ui-tag is-ok">开机自动挂载</span>
              <span v-else class="ui-tag is-warn">临时挂载</span>
              <span class="stomount__spacer" />
              <span class="stomount__use num">{{ usePercentText(mount) }}</span>
              <!-- 容量快满时，"是什么占满了"比"还剩多少"更接近用户要的答案。
                   虚拟文件系统没有真实占用，不给这个入口。 -->
              <n-button v-if="!mount.virtual" size="tiny" quaternary @click="usageTarget = mount.target">
                用量
              </n-button>
              <n-button
                size="tiny"
                quaternary
                :disabled="applying || readOnlyHost"
                @click="openUnmount(mount)"
              >
                卸载
              </n-button>
            </header>

            <div class="stomount__source num">{{ mount.source }} · {{ mount.fstype }}</div>

            <div class="stomount__bar" :class="useBarClass(mount)">
              <span :style="{ width: useBarWidth(mount) }" />
            </div>
            <div class="stomount__capacity num">
              <template v-if="mount.usePercent >= 0">
                已用 {{ formatBytes(mount.used) }} / 共 {{ formatBytes(mount.total) }} · 可用
                {{ formatBytes(mount.avail) }}
              </template>
              <template v-else>容量由远端决定</template>
            </div>
          </article>
        </div>
      </n-tab-pane>

      <n-tab-pane name="fstab" :tab="`fstab (${fstab.length})`">
        <n-data-table
          class="stowin__table"
          :columns="fstabColumns"
          :data="fstab"
          :bordered="false"
          :row-key="(row: FstabEntry) => `${row.source}-${row.target}`"
          size="small"
          flex-height
        />
      </n-tab-pane>

      <!-- 没有阵列时不占一个标签页：一块空标签比没有标签更让人困惑 -->
      <n-tab-pane v-if="raid.length > 0" name="raid" :tab="`RAID (${raid.length})`">
        <ul class="storaid">
          <li
            v-for="array in raid"
            :key="array.name"
            class="storaid__row"
            :class="{ 'is-bad': array.degraded }"
          >
            <span
              class="led"
              :class="array.healthy ? 'led--ok' : 'led--danger'"
              aria-hidden="true"
            />
            <span class="storaid__name num">{{ array.name }}</span>
            <span class="storaid__level num">{{ array.level }}</span>
            <span class="storaid__state">{{ array.state }}</span>
            <span class="storaid__members num" :title="array.members.join('、')">
              {{ array.members.join('、') || '—' }}
            </span>
            <span class="storaid__spacer" />
            <!-- 成员数比容量更值得摆在前面：它直接说明这个阵列还能扛几次盘故障 -->
            <span class="storaid__count num">{{ array.working }} / {{ array.total }}</span>
            <span class="storaid__size num">{{ formatBytes(array.blocks * 1024) }}</span>
          </li>
        </ul>
        <p v-if="raid.some((item) => item.degraded)" class="storaid__warn">
          有阵列的成员盘不全。它现在仍然在提供服务，但已经经不起再掉一块盘。
        </p>
      </n-tab-pane>
    </n-tabs>

    <div v-else-if="!lastError" class="stowin__placeholder">
      {{ target === null ? '请先选择一台主机' : fetching ? '正在读取存储信息…' : '暂无数据' }}
    </div>

    <!-- 挂载本地设备 -->
    <n-modal v-model:show="showDialog" preset="card" :title="dialogTitle" style="width: 560px" @after-leave="closeDialogs">
      <div class="stoform">
        <!-- 提权提醒放在确认内容最前面：用户就是在这张弹窗里按下"挂载/卸载"的，
             得在按之前知道这次会不会以 root 执行、以及是不是注定失败。 -->
        <n-alert v-if="escalationWarning" type="error">{{ escalationWarning }}</n-alert>
        <n-alert v-else-if="escalationInfo" type="info">{{ escalationInfo }}</n-alert>
        <template v-if="dialog === 'mount-local'">
          <div class="stoform__field">
            <span class="stoform__label">设备</span>
            <span class="stoform__value num">
              {{ dialogDevice?.path }}
              <template v-if="dialogDevice?.label">· {{ dialogDevice.label }}</template>
              <template v-if="dialogDevice?.fstype">· {{ dialogDevice.fstype }}</template>
            </span>
          </div>
          <div class="stoform__field">
            <span class="stoform__label">挂载点</span>
            <n-input v-model:value="localForm.target" placeholder="/mnt/data" />
          </div>
          <div class="stoform__field">
            <span class="stoform__label">选项</span>
            <n-input v-model:value="localForm.options" placeholder="留空使用默认；可填 ro、noatime 等" />
          </div>
          <div class="stoform__field">
            <span class="stoform__label" />
            <n-checkbox v-model:checked="localForm.persist">开机自动挂载（写入 /etc/fstab）</n-checkbox>
          </div>
          <p class="stoform__tip">
            写入 fstab 时会自动加上 nofail：即使这块盘之后没有插上，系统也能正常启动。
          </p>
        </template>

        <template v-else-if="dialog === 'mount-network'">
          <div class="stoform__field">
            <span class="stoform__label">协议</span>
            <n-radio-group v-model:value="netForm.protocol" size="small">
              <n-radio value="nfs" :disabled="!hasNfs">NFS</n-radio>
              <n-radio value="cifs" :disabled="!hasCifs">SMB / CIFS</n-radio>
            </n-radio-group>
          </div>
          <div class="stoform__field">
            <span class="stoform__label">服务器</span>
            <n-input v-model:value="netForm.server" placeholder="192.168.1.5 或 nas.local" />
          </div>
          <div class="stoform__field">
            <span class="stoform__label">{{ netForm.protocol === 'nfs' ? '导出路径' : '共享名' }}</span>
            <n-input
              v-model:value="netForm.share"
              :placeholder="netForm.protocol === 'nfs' ? '/export/data' : 'share1'"
            />
          </div>
          <template v-if="netForm.protocol === 'cifs'">
            <div class="stoform__field">
              <span class="stoform__label">用户名</span>
              <n-input v-model:value="netForm.username" placeholder="可留空表示匿名访问" />
            </div>
            <div class="stoform__field">
              <span class="stoform__label">密码</span>
              <n-input
                v-model:value="netForm.password"
                type="password"
                show-password-on="click"
                placeholder="留空表示无密码"
                :input-props="{ autocomplete: 'new-password', 'aria-label': '共享密码' }"
              />
            </div>
          </template>
          <div class="stoform__field">
            <span class="stoform__label">挂载点</span>
            <n-input v-model:value="netForm.target" placeholder="/mnt/share" />
          </div>
          <div class="stoform__field">
            <span class="stoform__label">选项</span>
            <n-input
              v-model:value="netForm.options"
              :placeholder="netForm.protocol === 'nfs' ? '留空使用 vers=4.1' : '留空使用 vers=3.0'"
            />
          </div>
          <div class="stoform__field">
            <span class="stoform__label" />
            <n-checkbox v-model:checked="netForm.persist">开机自动挂载（写入 /etc/fstab）</n-checkbox>
          </div>
          <p class="stoform__tip">
            网络存储的 fstab 条目会自动带上 nofail 与 _netdev：服务器不可达时系统照常启动，不会卡在开机过程。
          </p>
          <p v-if="!hasNfs && !hasCifs" class="stoform__warn">
            目标主机上既没有 mount.nfs 也没有 mount.cifs，需要先安装 nfs-common 或 cifs-utils。
          </p>
        </template>

        <template v-else-if="dialog === 'unmount'">
          <p class="stoform__confirm">
            即将卸载 <strong class="num">{{ dialogMount?.target }}</strong>
            <template v-if="dialogMount?.source">（{{ dialogMount.source }}）</template>。
          </p>
          <p class="stoform__tip">
            若有进程正在使用这个目录，卸载会被拒绝；可以先在「进程管理」里确认是谁在占用。
          </p>
          <p v-if="dialogMount?.persistent" class="stoform__warn">
            该挂载点写在 /etc/fstab 里。卸载时会一并移除它的条目，否则重启后它会自己再挂回来。
          </p>
        </template>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button @click="closeDialogs">取消</n-button>
          <n-button
            v-if="dialog === 'mount-local'"
            type="primary"
            :loading="applying"
            @click="submitLocalMount"
          >
            挂载
          </n-button>
          <n-button
            v-else-if="dialog === 'mount-network'"
            type="primary"
            :loading="applying"
            @click="submitNetworkMount"
          >
            挂载
          </n-button>
          <n-button v-else type="error" :loading="applying" @click="submitUnmount">卸载</n-button>
        </n-space>
      </template>
    </n-modal>

    <format-dialog
      :show="formatDevice !== null"
      :host-id="target"
      :device="formatDevice"
      :format-targets="snapshot?.formatTargets ?? {}"
      :tools="snapshot?.tools ?? []"
      @update:show="(value: boolean) => { if (!value) formatDevice = null; }"
      @done="refresh({ fresh: true })"
    />

    <partition-dialog
      :show="partitionDevice !== null"
      :host-id="target"
      :device="partitionDevice"
      :tools="snapshot?.tools ?? []"
      @update:show="(value: boolean) => { if (!value) partitionDevice = null; }"
      @done="refresh({ fresh: true })"
    />

    <smart-dialog
      :show="smartDevice !== ''"
      :host-id="target"
      :device="smartDevice"
      @update:show="(value: boolean) => { if (!value) smartDevice = ''; }"
    />

    <usage-dialog
      :show="usageTarget !== ''"
      :host-id="target"
      :target="usageTarget"
      @update:show="(value: boolean) => { if (!value) usageTarget = ''; }"
    />

    <confirm-dialog
      :show="fstabDrop !== null"
      title="移除 fstab 条目"
      :summary="fstabSummary"
      :detail="fstabDetail"
      :phrase="fstabDrop?.target ?? ''"
      :loading="applying"
      @update:show="(value: boolean) => { if (!value) fstabDrop = null; }"
      @confirm="confirmFstabDrop"
    />
  </div>
</template>
