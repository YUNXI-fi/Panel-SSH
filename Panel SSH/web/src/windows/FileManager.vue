<script setup lang="ts">
import { computed, h, inject, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import {
  NAlert,
  NButton,
  NCheckbox,
  NDataTable,
  NDatePicker,
  NForm,
  NFormItem,
  NInput,
  NModal,
  NDropdown,
  NSpace,
  NTag,
  useDialog,
  useMessage,
  type DataTableColumns,
  type DropdownOption,
} from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import FileDropMenu from '../components/FileDropMenu.vue';
import { WindowManagerKey, WindowSubtitleKey } from '../desktop/useWindowManager';
import { LOCAL_HOST_ID } from '../desktop/watchedHost';
import { beginFileDrag, endFileDrag, internalDragPayload, type FileDragPayload } from './fileDrag';
import { registerFileTarget } from './fileTargets';
import { crossHostTransfer, type CrossHostProgress, type CrossHostResult } from './fileTransfer';
import { notifyFilesChanged, onFilesChanged } from './fileEvents';
import { isTextKind, previewKindOf } from './filePreview';
import {
  ApiError,
  api,
  copyText,
  triggerDownload,
  type ArchiveEvent,
  type EscalationInfo,
  type FileAttr,
  type FileEntry,
  type FileListing,
  type HashAlgo,
  type HashEvent,
  type StorageMount,
} from '../api/client';
import { formatBytes, formatTime } from '../api/format';

const props = defineProps<{ hostId?: number; hostName?: string }>();

const message = useMessage();
const dialog = useDialog();
// 「在终端中打开」要新开一个终端窗口，这里取窗口管理器；取不到就不显示该菜单项。
const wm = inject(WindowManagerKey, null);

// 窗口自己持有目标主机：带 hostId 打开时就是它（从主机管理那一行进来），
// 从桌面图标打开时由窗口内的选择器填充。
const target = ref<number | null>(props.hostId ?? null);

/**
 * 当前目标主机的显示名。
 *
 * 从主机管理那一行进来时由 props 给，从桌面图标进来时由选择器回填 ——
 * 「在终端中打开」要拿它拼新窗口的标题，缺了就只能写一个「终端」。
 */
const pickedName = ref(props.hostName ?? '');
const targetName = computed(() => pickedName.value || props.hostName || '');

watch(
  () => props.hostId,
  (id) => {
    if (id !== undefined) target.value = id;
  },
);

/**
 * 目标主机 ID。
 *
 * 从桌面图标进入时，选好主机之前文件列表是空的，而"对某个文件做点什么"的操作
 * 全部挂在文件列表的工具栏与右键菜单上 —— 也就是说，它们不可能在未选主机时
 * 被触发。所以这里可以直接断言非空；真正需要判空的是唯一的数据入口 load()。
 */
const hostId = computed(() => target.value as number);

/**
 * 是否需要在窗口内选主机。
 *
 * 判断用的是 prop 而不是上面那个 computed：从桌面图标进来时 prop 是 undefined，
 * 从主机管理那一行进来时一定有值。
 */
const needsPicker = computed(() => props.hostId === undefined);

// 目标机的提权方式。写入 /etc 这类位置被拒时面板会自动提权重做一次，
// 写文件、删除、上传都是高频操作，所以只在窗口顶部提示一次，不逐次弹窗确认。
const escalation = ref<EscalationInfo | null>(null);

/**
 * 目录读取失败的常驻说明。
 *
 * 只用 message.error 是不够的：提示几秒后消失，列表还停在旧内容或空态，
 * 用户会以为"这个目录本来就是空的"。502 表示目标机的 SSH/SFTP 连不上，
 * 必须留在界面上，直到下一次读取成功。
 */
const loadError = ref('');

/**
 * 取目标机的提权方式。
 *
 * 不复用 load 那条链路：提示只是辅助信息，它的失败不该影响文件列表的展示，
 * 也就不该出现在文件列表的错误提示里。
 */
async function loadEscalation(): Promise<void> {
  const id = target.value;
  if (id === null) {
    escalation.value = null;
    return;
  }
  try {
    escalation.value = await api.hosts.escalation(id);
  } catch {
    // 取不到就不显示提示，静默处理即可：这只是辅助信息，用报错打扰用户反而添乱。
    escalation.value = null;
  }
}

/**
 * 顶部的提权说明。
 *
 * 由于提权是面板自动完成的，这里要回答的是"哪些位置会被提权、提不了权时
 * 该怎么办"，而不是让用户为每一次写操作复核一次。
 */
const escalationNotice = computed<{ level: 'error' | 'info'; text: string } | null>(() => {
  const info = escalation.value;
  // 已以 root 登录的机器不存在权限问题，这句话对它没有信息量。
  if (!info || info.mode === 'root') return null;
  if (!info.escalatable) {
    // 宿主机的提权账号在「主机管理」里配置（它不在主机表里，没法和远端共用表单）。
    if (target.value === LOCAL_HOST_ID) {
      return {
        level: 'error',
        text: '当前无法修改 root 属主的文件或目录（如 /etc、/usr）：请到「主机管理」为宿主机配置提权账号与口令，或以 root 运行面板。',
      };
    }
    return {
      level: 'error',
      text: '该主机无法提权：改动 root 属主的文件或目录（如 /etc、/usr）会被拒绝。请在面板里为该主机配置提权口令（root 口令），或以 root 登录。',
    };
  }
  return {
    level: 'info',
    text: `写入 /etc 这类只有 root 能改的位置时，面板会自动提权（${info.label}）；改动你自己的文件不受影响。`,
  };
});

const listing = ref<FileListing>({ path: '/', parent: '', entries: [] });
const loading = ref(false);
const uploading = ref(false);

/**
 * 窗口副标题：目标主机 + 当前目录。
 *
 * 标题栏只写"文件管理"，同一个应用开两个窗口时分不出看的是哪台机器、哪个目录；
 * 副标题把这两件事补上，竖栏任务项读的也是同一个值。
 */
const setWindowSubtitle = inject(WindowSubtitleKey, null);

watch(
  [targetName, () => listing.value.path],
  ([name, path]) => setWindowSubtitle?.(name ? `${name} · ${path}` : ''),
  // post：主机选择器自己也会写副标题，本窗口的"主机 · 目录"要后写才能盖住它。
  { immediate: true, flush: 'post' },
);

/**
 * 当前目录所在分区的容量。
 *
 * 不按目录统计：du 只对已挂载目录开放，也不该在每次进目录时遍历整棵子树。
 * 进到深层目录时，"这块盘还剩多少"比单个目录的体积更有用。
 */
const mounts = ref<StorageMount[]>([]);
const capacityLoading = ref(false);

async function loadCapacity(): Promise<void> {
  const id = target.value;
  if (id === null) {
    mounts.value = [];
    return;
  }
  capacityLoading.value = true;
  try {
    const snapshot = await api.storage.snapshot(id);
    mounts.value = snapshot.mounts;
  } catch {
    // 容量是辅助信息：读不到就不显示，不打断文件列表本身。
    mounts.value = [];
  } finally {
    capacityLoading.value = false;
  }
}

/** 当前目录落在哪个挂载点上：取最长的前缀匹配，深层挂载点优先。 */
const currentMount = computed<StorageMount | null>(() => {
  const path = listing.value.path;
  let best: StorageMount | null = null;
  for (const mount of mounts.value) {
    if (mount.usePercent < 0 || mount.total <= 0) continue;
    const prefix = mount.target === '/' ? '/' : `${mount.target.replace(/\/$/, '')}/`;
    if (path !== mount.target && !path.startsWith(prefix)) continue;
    if (!best || mount.target.length > best.target.length) best = mount;
  }
  return best;
});

/** 剩余空间占比：与"已用"共用同一条容量条的另一半。 */
const capacityFreePercent = computed(() => {
  const mount = currentMount.value;
  return mount ? Math.max(0, 100 - mount.usePercent) : 0;
});

/** 常用位置：进站后最常回看的几个目录，点一下直接跳过去。 */
const PLACES = [
  { path: '/', hint: '根目录' },
  { path: '/var/www', hint: '站点根目录' },
  { path: '/etc', hint: '系统配置' },
  { path: '/var/log', hint: '系统与服务日志' },
];

/** 刷新：目录与容量一起重读，避免列表已经变了、容量还停在旧值。 */
function refresh(): void {
  void load();
  void loadCapacity();
  // 提权账号刚在「主机管理」改过时，这条提示要跟着变 —— 否则用户刷新了目录，
  // 顶部还在说"无法提权"。
  void loadEscalation();
}

/**
 * 正在进行的文件任务。
 *
 * 同一时刻只允许一个：两条进度并排出现时，用户反而更难看清哪条是哪个；
 * 而上传与下载争同一台远端机器的带宽时，两条进度都会变得很难看。
 *
 * 压缩、解压与哈希也走这块 UI：它们同样有"要等一会儿"的过程，而它们的时间刻度是
 * 条目数（tar -v / zip 的逐条输出 / 逐文件的摘要），不是字节数 —— 所以单位要跟着
 * kind 走。
 */
const transfer = ref<{
  kind: '上传' | '下载' | '压缩' | '解压' | '哈希' | '跨主机复制' | '跨主机移动';
  label: string;
  loaded: number;
  total: number;
  /** 显示单位：上传下载是字节，压缩解压哈希是条目数。 */
  unit: 'bytes' | 'items';
  /** 拿不到分母时给用户的说法（如"正在统计条目数…"），有它才不会显得卡住。 */
  note?: string;
} | null>(null);

/** 百分比；拿不到总长度时为 null —— 那种情况只能显示"已传多少"。 */
const transferPercent = computed(() => {
  const current = transfer.value;
  if (!current || current.total <= 0) return null;
  return Math.min(100, Math.round((current.loaded / current.total) * 100));
});

const transferText = computed(() => {
  const current = transfer.value;
  if (!current) return '';
  if (current.unit === 'items') {
    // 条目数报"第几项 / 共几项"比百分比更有信息量：用户能对上目录里的文件数。
    return current.total > 0 ? `${current.loaded}/${current.total} 项` : '统计中…';
  }
  const size = formatBytes(current.loaded);
  const percent = transferPercent.value;
  return percent === null ? size : `${percent}% · ${size}`;
});

const checkedKeys = ref<Array<string | number>>([]);
const fileInput = ref<HTMLInputElement | null>(null);
const dragActive = ref(false);

/**
 * 列表 / 卡片视图。
 *
 * 只改变排布，不改变数据与操作：两种视图共用 listing.entries、checkedKeys
 * 以及同一套行操作菜单，因此切换视图不会丢勾选，也不会出现"卡片里能做、列表里做不了"。
 */
const viewMode = ref<'list' | 'card'>('list');

/** 一个按钮切两种布局：按钮上显示"当前是什么"，点一下换到另一个。 */
function toggleView(): void {
  viewMode.value = viewMode.value === 'list' ? 'card' : 'list';
}

function isChecked(entry: FileEntry): boolean {
  return checkedKeys.value.includes(entry.path);
}

function setChecked(entry: FileEntry, checked: boolean): void {
  const next = checkedKeys.value.filter((key) => String(key) !== entry.path);
  if (checked) next.push(entry.path);
  checkedKeys.value = next;
}

/** 单击名称与表格一致：目录进入，文件不动作；文件的双击行为交给卡片本身。 */
function openEntry(entry: FileEntry): void {
  if (entry.isDir) enter(entry);
}

/*
 * 路径编辑：平时是面包屑，点一下整条路径换成输入框（像浏览器地址栏）。
 *
 * 常驻一个回填当前路径的输入框，屏幕上就有两份同样的路径；改成按需切换后，
 * 想跳转时点路径、敲完回车，平时仍是可点的层级导航。
 */
const editingPath = ref(false);
const pathDraft = ref('');
const pathInput = ref<InstanceType<typeof NInput> | null>(null);

function beginPathEdit(): void {
  pathDraft.value = listing.value.path;
  editingPath.value = true;
  nextTick(() => pathInput.value?.focus());
}

function cancelPathEdit(): void {
  editingPath.value = false;
}

function submitPathJump(): void {
  const target = pathDraft.value.trim();
  if (target === '' || target === listing.value.path) {
    editingPath.value = false;
    return;
  }
  if (!target.startsWith('/')) {
    message.warning('请输入以 / 开头的绝对路径');
    return;
  }
  editingPath.value = false;
  void load(target);
}

/** 这个文件有没有对应的查看器。没有的类型保持原来的"双击下载"。 */
function canPreview(entry: FileEntry): boolean {
  return !entry.isDir && previewKindOf(entry.name) !== 'none';
}

/**
 * 在独立窗口里打开查看器。
 *
 * 每个文件一个窗口：预览与编辑可以并排对照（改配置时同时开着文档），
 * 也不会把文件列表挤掉 —— 关掉窗口就回到原来的目录位置。
 * 窗口标题用文件名，副标题由查看器自己写成主机名。
 */
function openViewer(entry: FileEntry, mode: 'preview' | 'source' | 'edit'): void {
  if (!wm) return;
  wm.open(
    'fileviewer',
    entry.name,
    { hostId: hostId.value, hostName: targetName.value, entry, initialMode: mode },
    { w: 1040, h: 720 },
  );
}

/** 打开文件：能预览就开查看器窗口，不能预览就下载。 */
function openFile(entry: FileEntry): void {
  if (entry.isDir) {
    enter(entry);
    return;
  }
  if (canPreview(entry)) {
    openViewer(entry, 'preview');
    return;
  }
  void download(entry);
}

/** 只有文本类能进编辑：图片 / PDF 没有可编辑的正文，强行进去会是一个空编辑器。 */
function editFile(entry: FileEntry): void {
  if (!isTextKind(previewKindOf(entry.name))) return;
  openViewer(entry, 'edit');
}

// 新建目录 / 重命名
const showMkdir = ref(false);
const mkdirName = ref('');
const showRename = ref(false);
const renameTarget = ref<FileEntry | null>(null);
const renameName = ref('');

/**
 * 复制 / 移动的剪贴板。
 *
 * 粘贴的目标就是"当前目录"，所以只需要记住来源与模式；hostId / hostName 一并
 * 存下，是为了让"粘到另一台机器"走跨主机传输，也让提示里说得出东西是从哪来的。
 */
const clipboard = ref<{
  mode: 'copy' | 'move';
  paths: string[];
  hostId: number;
  hostName: string;
} | null>(null);

// 改权限
const showChmod = ref(false);
const chmodMode = ref('755');
const chmodPaths = ref<string[]>([]);
const MODE_PRESETS = [
  { mode: '644', hint: '文件：属主可读写，同组与其他用户只读' },
  { mode: '755', hint: '目录或脚本：属主可读写执行，其他用户可读可执行' },
  { mode: '600', hint: '私密文件：只有属主可读写（如密钥）' },
  { mode: '700', hint: '私有目录：只有属主可进入' },
];

/*
 * 权限位矩阵。
 *
 * 八进制只回答"最终是几"，但用户真正在改的是"谁能读、谁能写、谁能执行"。
 * 这里用 3×3 的位矩阵表达：行是作用对象，列是动作，点格子即翻转那一位；
 * 上方的八进制与符号形式随点选实时变化，输入框仍然保留，熟手可以直接敲数字。
 */
const CHMOD_ROWS = [
  { key: 'owner', label: '属主', shift: 6 },
  { key: 'group', label: '属组', shift: 3 },
  { key: 'other', label: '其他', shift: 0 },
];
const CHMOD_BITS = [
  { key: 'read', label: '读', letter: 'r', mask: 4 },
  { key: 'write', label: '写', letter: 'w', mask: 2 },
  { key: 'exec', label: '执行', letter: 'x', mask: 1 },
];
/** 特殊位在八进制最高位，单独一行，不塞进 3×3。 */
const CHMOD_SPECIAL = [
  { key: 'setuid', label: 'setuid', mask: 4 },
  { key: 'setgid', label: 'setgid', mask: 2 },
  { key: 'sticky', label: 'sticky', mask: 1 },
];

/** 当前输入的权限位；还不是合法的 3-4 位八进制时返回 null（矩阵全灭，但不改输入框里的字）。 */
const chmodBits = computed(() => {
  const raw = chmodMode.value.trim();
  if (!/^[0-7]{3,4}$/.test(raw)) return null;
  return Number.parseInt(raw, 8);
});

/** 符号形式（如 rwxr-xr-x）：把数字直译回来，不用心算。 */
const chmodSymbolic = computed(() => {
  const bits = chmodBits.value;
  if (bits === null) return '---------';
  let text = '';
  for (const row of CHMOD_ROWS) {
    const triple = (bits >> row.shift) & 7;
    const special = ((bits >> 9) & 7) & (row.key === 'owner' ? 4 : row.key === 'group' ? 2 : 1);
    text += triple & 4 ? 'r' : '-';
    text += triple & 2 ? 'w' : '-';
    if (triple & 1) text += special ? (row.key === 'other' ? 't' : 's') : 'x';
    else text += special ? (row.key === 'other' ? 'T' : 'S') : '-';
  }
  return text;
});

/** 把位值写回八进制：有特殊位时写 4 位，否则保持常见的 3 位。 */
function setChmodBits(value: number): void {
  const special = (value >> 9) & 7;
  const rest = value & 0o777;
  chmodMode.value = special
    ? `${special}${rest.toString(8).padStart(3, '0')}`
    : rest.toString(8).padStart(3, '0');
}

function hasChmodBit(shift: number, mask: number): boolean {
  const bits = chmodBits.value;
  return bits !== null && (((bits >> shift) & 7) & mask) !== 0;
}

function toggleChmodBit(shift: number, mask: number): void {
  // 输入不合法时从 0 起算：点一下正好得到只含这一位的合法值。
  const bits = chmodBits.value ?? 0;
  const current = (bits >> shift) & 7;
  setChmodBits((bits & ~(7 << shift)) | ((current ^ mask) << shift));
}

function hasChmodSpecial(mask: number): boolean {
  const bits = chmodBits.value;
  return bits !== null && (((bits >> 9) & 7) & mask) !== 0;
}

function toggleChmodSpecial(mask: number): void {
  const bits = chmodBits.value ?? 0;
  const special = (bits >> 9) & 7;
  setChmodBits((bits & 0o777) | ((special ^ mask) << 9));
}

// 压缩
const showCompress = ref(false);
const compressSource = ref<FileEntry | null>(null);
const compressFormat = ref<'tar.gz' | 'zip'>('tar.gz');
const compressName = ref('');
const compressing = ref(false);

/** 可解压的后缀，与后端 handleExtract 认的四种保持一致。 */
const ARCHIVE_SUFFIXES = ['.tar.gz', '.tgz', '.tar', '.zip'];

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    // 远端未响应是 504，与普通读写失败要给不同的建议
    if (err.status === 504) return `${err.message}。请检查目标机是否可达`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

function joinPath(dir: string, name: string): string {
  return dir === '/' ? `/${name}` : `${dir}/${name}`;
}

async function load(dir?: string): Promise<void> {
  const id = target.value;
  // 还没选主机：保持空态、不发请求 —— 否则拼出来的是 /api/hosts/undefined/...，
  // 用户会拿到一个 400，而不是"请先选择主机"。
  if (id === null) return;

  loading.value = true;
  loadError.value = '';
  try {
    listing.value = await api.files.list(id, dir ?? listing.value.path);
    checkedKeys.value = [];
  } catch (err) {
    loadError.value = describe(err, '读取目录失败');
    message.error(loadError.value);
  } finally {
    loading.value = false;
  }
}

function enter(entry: FileEntry): void {
  if (entry.isDir) void load(entry.path);
}

const crumbs = computed(() => {
  const items = [{ label: '/', path: '/' }];
  let acc = '';
  for (const part of listing.value.path.split('/').filter(Boolean)) {
    acc += `/${part}`;
    items.push({ label: part, path: acc });
  }
  return items;
});

/**
 * 根目录由路径条上的独立「/」段表达（点它回根），所以这里只取下层级。
 * 否则会出现 "主机 / / boot" 两个相邻斜杠。
 */
const pathCrumbs = computed(() => crumbs.value.slice(1));

async function download(entry: FileEntry): Promise<void> {
  if (transfer.value !== null) {
    message.info('已有传输正在进行，请等它结束');
    return;
  }

  // 先用列表里的大小垫着：XHR 拿到 Content-Length 之前，进度条才有东西可画。
  transfer.value = { kind: '下载', label: entry.name, loaded: 0, total: entry.size, unit: 'bytes' };
  try {
    const blob = await api.files.download(hostId.value, entry.path, (loaded, total) => {
      transfer.value = { kind: '下载', label: entry.name, loaded, total: total || entry.size, unit: 'bytes' };
    });

    const url = URL.createObjectURL(blob);
    triggerDownload(url, entry.name);
    // 不能立刻撤销：浏览器可能还没把这段 Blob 读完，那样落盘的是 0 字节文件。
    // 延迟释放，既不打断下载，也不至于一直占着内存。
    setTimeout(() => URL.revokeObjectURL(url), 60_000);
  } catch (err) {
    message.error(describe(err, '下载失败'));
  } finally {
    transfer.value = null;
  }
}

function openMkdir(): void {
  mkdirName.value = '';
  showMkdir.value = true;
}

async function submitMkdir(): Promise<void> {
  const name = mkdirName.value.trim();
  if (!name) {
    message.warning('请输入目录名');
    return;
  }
  if (name.includes('/')) {
    message.warning('目录名不能包含 /');
    return;
  }

  try {
    await api.files.mkdir(hostId.value, joinPath(listing.value.path, name));
    showMkdir.value = false;
    message.success('目录已创建');
    void load();
  } catch (err) {
    message.error(describe(err, '创建目录失败'));
  }
}

function startRename(entry: FileEntry): void {
  renameTarget.value = entry;
  renameName.value = entry.name;
  showRename.value = true;
}

async function submitRename(): Promise<void> {
  // 这里的局部变量不叫 target：setup 顶层已经有一个同名的"目标主机" ref，
  // 同名遮蔽读起来太容易误解 —— 一个是被改名的条目，一个是要在哪台机器上改。
  const entry = renameTarget.value;
  if (!entry) return;

  const name = renameName.value.trim();
  if (!name || name === entry.name) {
    showRename.value = false;
    return;
  }
  if (name.includes('/')) {
    message.warning('名称不能包含 /');
    return;
  }

  try {
    await api.files.rename(hostId.value, entry.path, joinPath(listing.value.path, name));
    showRename.value = false;
    message.success('已重命名');
    void load();
  } catch (err) {
    message.error(describe(err, '重命名失败'));
  }
}

async function removePaths(paths: string[]): Promise<void> {
  if (paths.length === 0) return;
  try {
    await api.files.remove(hostId.value, paths);
    message.success(`已删除 ${paths.length} 项`);
    void load();
  } catch (err) {
    message.error(describe(err, '删除失败'));
  }
}

/** 批量删除是不可逆的，先确认再执行。 */
function removeChecked(): void {
  const paths = checkedKeys.value.map(String);
  if (paths.length === 0) return;
  dialog.warning({
    title: '删除所选条目',
    content: `将删除 ${paths.length} 项，此操作不可恢复。目录必须为空才能删除。`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: () => removePaths(paths),
  });
}

function confirmRemove(entry: FileEntry): void {
  dialog.warning({
    title: '删除条目',
    content: `确认删除「${entry.name}」？此操作不可恢复。`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: () => removePaths([entry.path]),
  });
}

// ---- 复制路径 / 在终端中打开 ----

/** 上一级目录；根目录下的条目返回 "/"。 */
function parentOf(p: string): string {
  const index = p.lastIndexOf('/');
  return index <= 0 ? '/' : p.slice(0, index);
}

/**
 * 把一段文本写进本地剪贴板。
 *
 * 写法见 client.ts 的 copyText（要兼容用局域网 IP 的 http 访问面板的情况）；
 * 这里只负责把结果变成提示 —— 成功提示由调用方给：只有它知道复制出去的是什么
 * （一条路径、一份摘要清单）。
 */
async function writeClipboard(text: string, success: string): Promise<void> {
  if (await copyText(text)) {
    message.success(success);
    return;
  }
  message.error('复制失败，请手动选中内容');
}

/** 复制远端绝对路径。 */
async function copyPath(entry: FileEntry): Promise<void> {
  await writeClipboard(entry.path, '已复制路径');
}

/**
 * 在终端里打开这个目录。
 *
 * 新开一个终端窗口而不是复用已有的那个：多开本来是这次改动的目的，而"在这里
 * 敲几条命令"和用户已经开着的终端多半是两件事。cwd 由终端在连上之后自己 cd 过去。
 * 对文件则落在它所在的目录 —— 接下来要敲的命令基本都冲着这份文件来。
 */
function openInTerminal(entry: FileEntry): void {
  if (!wm) return;
  const name = targetName.value;
  wm.open('terminal', name ? `终端 - ${name}` : '终端', {
    hostId: hostId.value,
    hostName: name,
    cwd: entry.isDir ? entry.path : parentOf(entry.path),
  });
}

/**
 * 在终端里运行这个文件。
 *
 * 不在这里判断可执行位：缺权限、解释器不存在这些错误由 shell 原样回显，
 * 用户看到的与手敲一条命令完全一致。
 */
function runFile(entry: FileEntry): void {
  if (!wm) return;
  const name = targetName.value;
  wm.open('terminal', name ? `终端 - ${name}` : '终端', {
    hostId: hostId.value,
    hostName: name,
    cwd: parentOf(entry.path),
    runPath: entry.path,
  });
}

// ---- 剪贴板：复制 / 移动 / 粘贴 ----

function fillClipboard(mode: 'copy' | 'move', paths: string[]): void {
  if (paths.length === 0) return;
  clipboard.value = { mode, paths, hostId: hostId.value, hostName: targetName.value };
  const action = mode === 'copy' ? '复制' : '剪切';
  message.success(`已${action} ${paths.length} 项，进入目标目录后点「粘贴」`);
}

/** 工具栏里的批量入口：作用于勾选的行。 */
function clipChecked(mode: 'copy' | 'move'): void {
  fillClipboard(mode, checkedKeys.value.map(String));
}

/**
 * 「操作」下拉菜单。
 *
 * 新建目录、上传、刷新是每次进目录都可能用的高频动作，直接摆在路径条上；
 * 其余动作要先选中条目才有意义，收进下拉既不让路径条变长，
 * 也不会在没勾选时给出一排禁用按钮。
 *
 * 禁用态照抄按钮原样：剪贴板为空时粘贴不可点，没勾选时那四个不可点。
 */
const fileActions = computed<DropdownOption[]>(() => [
  { label: '复制选中', key: 'copy', disabled: checkedKeys.value.length === 0 },
  { label: '移动选中', key: 'move', disabled: checkedKeys.value.length === 0 },
  {
    label: clipboard.value ? `粘贴（${clipboard.value.paths.length} 项）` : '粘贴',
    key: 'paste',
    disabled: !clipboard.value,
  },
  { key: 'd1', type: 'divider' },
  { label: '计算哈希', key: 'hash', disabled: checkedKeys.value.length === 0 },
  {
    label: checkedKeys.value.length ? `删除选中（${checkedKeys.value.length}）` : '删除选中',
    key: 'remove',
    disabled: checkedKeys.value.length === 0,
  },
]);

function onFileAction(key: string): void {
  switch (key) {
    case 'copy':
      clipChecked('copy');
      break;
    case 'move':
      clipChecked('move');
      break;
    case 'paste':
      void pasteClipboard();
      break;
    case 'hash':
      hashChecked();
      break;
    case 'remove':
      removeChecked();
      break;
    default:
      break;
  }
}

async function pasteClipboard(): Promise<void> {
  const clip = clipboard.value;
  if (!clip) return;

  const dest = listing.value.path;

  // 来源在另一台主机：走面板中转（或直连），复制与移动都支持。
  if (clip.hostId !== hostId.value) {
    const ok = await transferAcross(
      clip.mode,
      { hostId: clip.hostId, hostName: clip.hostName, paths: clip.paths },
      dest,
    );
    if (ok && clip.mode === 'move') clipboard.value = null;
    return;
  }

  // 源就在当前目录时后端会直接报「源与目标相同」，不如在这里就说清楚该往哪贴。
  if (clip.paths.every((item) => parentOf(item) === dest)) {
    message.info('目标目录与来源相同，请先进入另一个目录');
    return;
  }

  try {
    if (clip.mode === 'copy') await api.files.copy(clip.hostId, clip.paths, dest);
    else await api.files.move(clip.hostId, clip.paths, dest);
    message.success(`已${clip.mode === 'copy' ? '复制' : '移动'} ${clip.paths.length} 项`);
    // 移动之后源已经不在了，这份剪贴板再留着只会让人重复粘贴一批失效路径。
    if (clip.mode === 'move') clipboard.value = null;
    notifyFilesChanged([clip.hostId]);
  } catch (err) {
    message.error(describe(err, clip.mode === 'copy' ? '复制失败' : '移动失败'));
  }
}

// ---- 跨主机搬运 / 拖拽 ----

/** 进度条上的一句话；直连只有根条目数，中转才有字节数。 */
function applyTransferProgress(
  kind: '跨主机复制' | '跨主机移动',
  label: string,
  progress: CrossHostProgress,
): void {
  if (progress.mode === 'direct') {
    transfer.value = {
      kind,
      label,
      loaded: progress.done,
      total: progress.total,
      unit: 'items',
      note: progress.path ? `直连传输中 · ${progress.path}` : progress.note,
    };
    return;
  }
  if (progress.mode === 'relay') {
    if (progress.bytesTotal > 0) {
      const name = progress.path.slice(progress.path.lastIndexOf('/') + 1);
      transfer.value = {
        kind,
        label,
        loaded: progress.bytes,
        total: progress.bytesTotal,
        unit: 'bytes',
        note: name ? `正在复制 ${name}` : '经面板中转',
      };
      return;
    }
    // 全是空文件时字节数恒为 0，退回条目数，否则进度条永远停在不确定态。
    transfer.value = {
      kind,
      label,
      loaded: progress.done,
      total: progress.total,
      unit: 'items',
      note: '经面板中转',
    };
    return;
  }
  transfer.value = { kind, label, loaded: 0, total: 0, unit: 'bytes', note: progress.note };
}

/**
 * 把一批条目从一台主机搬到当前主机。
 *
 * 同一台机器仍走已有的复制 / 移动接口：那是目标机上的 cp -a / mv，比经面板
 * 倒一遍数据快得多，也能按需提权。只有跨主机才走 transfer。
 */
async function transferAcross(
  mode: 'copy' | 'move',
  payload: FileDragPayload,
  dest: string,
): Promise<boolean> {
  const action = mode === 'copy' ? '复制' : '移动';

  if (payload.hostId === hostId.value) {
    try {
      if (mode === 'copy') await api.files.copy(payload.hostId, payload.paths, dest);
      else await api.files.move(payload.hostId, payload.paths, dest);
      message.success(`已${action} ${payload.paths.length} 项`);
      notifyFilesChanged([payload.hostId]);
      return true;
    } catch (err) {
      message.error(describe(err, mode === 'copy' ? '复制失败' : '移动失败'));
      return false;
    }
  }

  if (transfer.value !== null) {
    message.info('已有传输正在进行，请等它结束');
    return false;
  }

  const kind = mode === 'copy' ? '跨主机复制' : '跨主机移动';
  const from = payload.hostName || '源主机';
  const to = targetName.value || '目标主机';
  const label = `${from} → ${to}`;

  let result: CrossHostResult;
  try {
    result = await crossHostTransfer(
      {
        fromHostId: payload.hostId,
        toHostId: hostId.value,
        paths: payload.paths,
        target: dest,
        move: mode === 'move',
      },
      {
        onProgress: (progress) => applyTransferProgress(kind, label, progress),
        onNotice: (text) => message.warning(text),
      },
    );
  } finally {
    transfer.value = null;
  }

  if (result.outcome === 'done') {
    if (result.failed > 0) {
      message.warning(`已${action}，但有 ${result.failed} 个文件未能传输`);
    } else {
      message.success(`已${action} ${payload.paths.length} 项`);
    }
    // 两台机器的列表都可能变了：源端（移动时少了几项）和目标端（多出几项）。
    notifyFilesChanged([payload.hostId, hostId.value]);
    return true;
  }
  message.error(result.outcome === 'interrupted' ? `${kind}中断：${result.message}` : result.message);
  return false;
}

/**
 * 登记为「可接收拖放的文件窗口」。
 *
 * 桌面上的主机投放条靠这张表判断：这台机器是否已经有窗口开着、
 * 开着的话停在哪个目录。目标主机变了（顶部胶囊切换）就换一条登记。
 */
let unregisterTarget: (() => void) | null = null;

function refreshFileTarget(): void {
  unregisterTarget?.();
  unregisterTarget = null;
  const id = target.value;
  if (id === null) return;
  unregisterTarget = registerFileTarget({
    hostId: id,
    path: () => listing.value.path,
    accept: (payload, mode) => {
      void transferAcross(mode, payload, listing.value.path);
    },
  });
}

onMounted(refreshFileTarget);
watch(target, refreshFileTarget);
onBeforeUnmount(() => unregisterTarget?.());

/** 拖拽落下后的动作菜单。 */
const dropMenu = ref<{
  show: boolean;
  x: number;
  y: number;
  dest: string;
  payload: FileDragPayload | null;
}>({ show: false, x: 0, y: 0, dest: '', payload: null });

/** 拖到窗口上时的提示文案：内部拖拽与 OS 文件上传是两件不同的事。 */
const dropHint = ref('');

function isSamePlace(payload: FileDragPayload, dest: string): boolean {
  return (
    payload.hostId === hostId.value && payload.paths.every((item) => parentOf(item) === dest)
  );
}

function openDropMenu(event: DragEvent, dest: string): void {
  const payload = internalDragPayload();
  if (!payload) return;
  if (isSamePlace(payload, dest)) {
    message.info('目标目录与来源相同');
    return;
  }
  dropMenu.value = { show: true, x: event.clientX, y: event.clientY, dest, payload };
}

function onDropAction(action: 'copy' | 'move'): void {
  const menu = dropMenu.value;
  dropMenu.value = { ...menu, show: false };
  if (!menu.payload) return;
  void transferAcross(action, menu.payload, menu.dest);
}

/** 行 / 卡片开始拖动：拖动已勾选的行时整批带走，否则只带这一行。 */
function beginRowDrag(event: DragEvent, entry: FileEntry): void {
  const checked = checkedKeys.value.map(String);
  const paths = checked.includes(entry.path) ? checked : [entry.path];
  beginFileDrag(event.dataTransfer, {
    hostId: hostId.value,
    hostName: targetName.value,
    paths,
  });
}

function markRowDrop(target: EventTarget | null, on: boolean): void {
  if (!(target instanceof HTMLElement)) return;
  target.classList.toggle('fm__row--drop', on);
}

function clearRowDrops(): void {
  document.querySelectorAll('.fm__row--drop').forEach((el) => el.classList.remove('fm__row--drop'));
}

function onRowDragOver(event: DragEvent, entry: FileEntry): void {
  if (!entry.isDir || !internalDragPayload()) return;
  event.preventDefault();
  // 不往外冒：否则窗口根节点也会进入"拖到当前目录"的高亮态。
  event.stopPropagation();
  // 落在文件夹行上时收起整窗提示层，否则那层遮罩会盖住这一行的高亮。
  dragActive.value = false;
  markRowDrop(event.currentTarget, true);
}

function onRowDragLeave(event: DragEvent, entry: FileEntry): void {
  if (!entry.isDir) return;
  markRowDrop(event.currentTarget, false);
}

function onRowDrop(event: DragEvent, entry: FileEntry): void {
  if (!entry.isDir) return;
  event.preventDefault();
  event.stopPropagation();
  clearRowDrops();
  dragActive.value = false;
  dropHint.value = '';
  openDropMenu(event, entry.path);
  endFileDrag();
}

function onDragOver(): void {
  dragActive.value = true;
  dropHint.value = internalDragPayload()
    ? `松开后选择复制或移动到此目录：${listing.value.path}`
    : `松开即可上传到 ${listing.value.path}`;
}

// ---- 改权限 ----

/** 从 "-rw-r--r--" 这样的权限串里还原出八进制三位；认不出来时退回 755。 */
function modeOf(mode: string): string {
  const symbolic = mode.slice(-9);
  if (symbolic.length < 9) return '755';
  const bits = (triple: string): number =>
    (triple[0] === 'r' ? 4 : 0) + (triple[1] === 'w' ? 2 : 0) + (triple[2] === '-' ? 0 : 1);
  return `${bits(symbolic.slice(0, 3))}${bits(symbolic.slice(3, 6))}${bits(symbolic.slice(6, 9))}`;
}

function openChmod(entries: FileEntry[]): void {
  if (entries.length === 0) return;
  chmodPaths.value = entries.map((item) => item.path);
  // 单个条目时把现值填进去：用户多半是在原有权限上微调，而不是从零输一串。
  chmodMode.value = entries.length === 1 ? modeOf(entries[0].mode) : '755';
  showChmod.value = true;
}

async function submitChmod(): Promise<void> {
  const mode = chmodMode.value.trim();
  if (!/^[0-7]{3,4}$/.test(mode)) {
    message.warning('权限值必须是 3-4 位八进制数，例如 755');
    return;
  }
  try {
    await api.files.chmod(hostId.value, chmodPaths.value, mode);
    showChmod.value = false;
    message.success(`已修改 ${chmodPaths.value.length} 项的权限`);
    void load();
  } catch (err) {
    message.error(describe(err, '修改权限失败'));
  }
}

// ---- 压缩 / 解压 ----

function canExtract(entry: FileEntry): boolean {
  if (entry.isDir) return false;
  const lower = entry.name.toLowerCase();
  return ARCHIVE_SUFFIXES.some((suffix) => lower.endsWith(suffix));
}

/** 去掉已有的压缩包后缀：换格式时不该堆成 a.tar.gz.zip。 */
function archiveStem(name: string): string {
  return name.replace(/\.(tar\.gz|tgz|tar|zip)$/i, '') || name;
}

function openCompress(entry: FileEntry): void {
  compressSource.value = entry;
  compressFormat.value = 'tar.gz';
  compressName.value = `${entry.name}.tar.gz`;
  showCompress.value = true;
}

// 格式一换就把默认文件名的扩展名一起换掉，避免打出名实不符的包。
watch(compressFormat, (format) => {
  const entry = compressSource.value;
  if (!entry) return;
  compressName.value = `${archiveStem(entry.name)}.${format}`;
});

const compressDir = computed(() =>
  compressSource.value ? parentOf(compressSource.value.path) : '',
);

/**
 * 跑一次带进度的压缩 / 解压，并把过程画到进度条上。
 *
 * 为什么不能像其他操作那样"发个请求、等个结果"：这两个操作在大目录上要跑几分钟，
 * 期间的沉默会被当成"没点上"。服务端因此改成逐帧回传，这里负责把帧翻成界面。
 *
 * 返回终态。interrupted 表示流结束了却始终没收到结论 —— 网络断开时就是这样，
 * 不能当成成功。
 */
async function runArchive(
  kind: '压缩' | '解压',
  label: string,
  call: (onEvent: (event: ArchiveEvent) => void) => Promise<void>,
): Promise<'done' | 'error' | 'interrupted'> {
  let outcome: 'done' | 'error' | null = null;
  transfer.value = { kind, label, loaded: 0, total: 0, unit: 'items' };

  try {
    await call((event) => {
      switch (event.type) {
        case 'prepare':
          // 统计条目数要读一遍整个包，这段没有输出，得说清楚在做什么。
          transfer.value = { kind, label, loaded: 0, total: 0, unit: 'items', note: '正在统计条目数…' };
          break;
        case 'progress':
          transfer.value = { kind, label, loaded: event.done, total: event.total, unit: 'items' };
          break;
        case 'escalating':
          transfer.value = {
            kind,
            label,
            loaded: 0,
            total: 0,
            unit: 'items',
            note: '权限不足，正在以 root 重做…',
          };
          break;
        case 'error':
          outcome = 'error';
          message.error(event.message);
          break;          case 'done':
          outcome = 'done';
          break;
      }
    });
  } catch (err) {
    outcome = 'error';
    message.error(describe(err, `${kind}失败`));
  } finally {
    transfer.value = null;
  }

  if (outcome === null) {
    message.error(`${kind}中断：与面板的连接已断开`);
    return 'interrupted';
  }
  return outcome;
}

async function submitCompress(): Promise<void> {
  const entry = compressSource.value;
  if (!entry) return;

  const name = compressName.value.trim();
  if (!name) {
    message.warning('请输入压缩包名称');
    return;
  }
  if (name.includes('/')) {
    message.warning('压缩包名称不能包含 /');
    return;
  }
  if (name.startsWith('-')) {
    // zip 不认 -- 分隔符，以 - 开头的名字会被它当成选项。
    message.warning('压缩包名称不能以 - 开头');
    return;
  }
  if (transfer.value !== null) {
    message.info('已有传输正在进行，请等它结束');
    return;
  }

  compressing.value = true;
  try {
    const outcome = await runArchive('压缩', name, (onEvent) =>
      api.files.compress(hostId.value, entry.path, name, compressFormat.value, onEvent),
    );
    if (outcome === 'done') {
      showCompress.value = false;
      message.success(`已生成 ${name}`);
      void load();
    }
  } finally {
    compressing.value = false;
  }
}

async function extractArchive(entry: FileEntry): Promise<void> {
  if (transfer.value !== null) {
    message.info('已有传输正在进行，请等它结束');
    return;
  }

  const outcome = await runArchive('解压', entry.name, (onEvent) =>
    api.files.extract(hostId.value, entry.path, onEvent),
  );
  if (outcome === 'done') {
    message.success(`已解压到 ${listing.value.path}`);
    void load();
  }
}

// ---- 属性 ----

/**
 * 属性面板：上半是不可改的元数据，下半是可就地修改的权限 / 属主 / 时间。
 *
 * 读取与修改分成两次请求：打开面板本身绝不能改任何东西，所以保存时只提交
 * 真正改动过的项（见 submitAttr），而不是把表单整份发回去。
 */
const showAttr = ref(false);
const loadingAttr = ref(false);
const savingAttr = ref(false);
const attr = ref<FileAttr | null>(null);
const attrForm = ref({ mode: '', owner: '', group: '', mtime: 0, atime: 0 });

/**
 * ISO 时间串折成 epoch 毫秒 —— 日期选择器用的就是这个刻度。
 * 认不出来时给 0，界面上显示成 1970 而不是一个 Invalid Date。
 */
function msOf(value: string): number {
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? ms : 0;
}

/** ISO 时间串折成 epoch 秒 —— 接口与"改没改过"的比较都用这个刻度。 */
function secOf(value: string): number {
  return Math.floor(msOf(value) / 1000);
}

const attrType = computed(() => {
  const info = attr.value;
  if (!info) return '';
  if (info.isLink) return '符号链接';
  return info.isDir ? '目录' : '文件';
});

/** 后端没取到 ctime 时给的是 Go 的零值时间（0001-01-01），据此决定要不要显示这一行。 */
const hasCtime = computed(() => secOf(attr.value?.ctime ?? '') > 0);

/**
 * 哪些字段真的被改过。
 *
 * 时间直接比毫秒是可以的：两边的值都来自接口给的同一个 ISO 串，精度一致。
 * 但提交时要折成秒（接口的刻度是 epoch 秒），否则目标机会收到一个小数。
 */
const attrChanges = computed(() => {
  const info = attr.value;
  if (!info) return { mode: false, owner: false, time: false };
  const form = attrForm.value;
  return {
    mode: form.mode.trim() !== '' && form.mode.trim() !== info.modeOctal,
    // 属主或属组任一被改都算。空串表示"不改"而不是"清空" —— chown 必须有属主，
    // 没有"把属主删掉"这种操作。
    owner:
      (form.owner.trim() !== '' && form.owner.trim() !== info.owner) ||
      (form.group.trim() !== '' && form.group.trim() !== info.group),
    time: form.mtime !== msOf(info.mtime) || form.atime !== msOf(info.atime),
  };
});

const attrDirty = computed(() => {
  const changes = attrChanges.value;
  return changes.mode || changes.owner || changes.time;
});

async function openAttr(entry: FileEntry): Promise<void> {
  showAttr.value = true;
  loadingAttr.value = true;
  attr.value = null;
  try {
    const info = await api.files.attr(hostId.value, entry.path);
    attr.value = info;
    attrForm.value = {
      mode: info.modeOctal,
      owner: info.owner,
      group: info.group,
      mtime: msOf(info.mtime),
      atime: msOf(info.atime),
    };
  } catch (err) {
    // 读不到就没有面板可看，直接关掉比留一个空壳在里面好。
    showAttr.value = false;
    message.error(describe(err, '读取属性失败'));
  } finally {
    loadingAttr.value = false;
  }
}

async function submitAttr(): Promise<void> {
  const info = attr.value;
  if (!info) return;

  const changes = attrChanges.value;
  const mode = attrForm.value.mode.trim();
  if (changes.mode && !/^[0-7]{3,4}$/.test(mode)) {
    message.warning('权限值必须是 3-4 位八进制数，例如 755');
    return;
  }

  savingAttr.value = true;
  try {
    if (changes.mode) await api.files.chmod(hostId.value, [info.path], mode);
    if (changes.owner) {
      // chown user file 会顺手把属组设成该用户的登录组，所以属主变化时必须把
      // 属组一并提交 —— 留空会让"只改个属主"悄悄丢掉原来的组。
      await api.files.chown(
        hostId.value,
        [info.path],
        attrForm.value.owner.trim(),
        attrForm.value.group.trim() || info.group,
      );
    }
    if (changes.time) {
      // 选择器给的是毫秒，接口要的是 epoch 秒。
      await api.files.touch(
        hostId.value,
        [info.path],
        Math.floor(attrForm.value.mtime / 1000),
        Math.floor(attrForm.value.atime / 1000),
      );
    }
    showAttr.value = false;
    message.success('属性已更新');
    void load();
  } catch (err) {
    message.error(describe(err, '修改属性失败'));
  } finally {
    savingAttr.value = false;
  }
}

// ---- 计算哈希 ----

/** 与后端 hashAlgos 一一对应，顺序即展示顺序。 */
const HASH_ALGOS: Array<{ value: HashAlgo; label: string }> = [
  { value: 'sha256', label: 'SHA-256' },
  { value: 'sha512', label: 'SHA-512' },
  { value: 'sha1', label: 'SHA-1' },
  { value: 'md5', label: 'MD5' },
  { value: 'blake3', label: 'BLAKE3' },
];

/**
 * 文件在这一次计算里的状态。
 *
 * missed 与 failed 必须分开：failed 是"它报错了"，missed 是"没算出结果、也没有
 * 任何报错留下来"（通常是读不动）。合成一个会让用户以为背后藏着一句被丢掉的错误。
 */
type HashRowStatus = 'pending' | 'running' | 'done' | 'failed' | 'missed';

/**
 * 结果表的一行。
 *
 * size 缺失表示这条路拿不到大小（走目标机命令时 sha256sum 不输出），显示"—"；
 * error 非空表示这个文件没算出来，原因照原样显示在哈希值那一列。
 * algo 记下算这一行时用的算法：清单阶段为空，算完才有值 —— 用户改了算法但没重算时，
 * 表格里的旧值不该跟着改名字。
 * percent 只在"这个文件正在被读"时有值（0-100），算完即清空 —— 它描述的是过程，
 * 不是结果。为 null 表示这条路报不出字节进度，界面退回只显示已用时长。
 */
type HashRow = {
  path: string;
  size?: number;
  hash: string;
  error: string;
  algo: string;
  status: HashRowStatus;
  percent: number | null;
};

/** 状态列的文案与配色。 */
const HASH_STATUS: Record<HashRowStatus, { label: string; cls: string }> = {
  pending: { label: '等待中', cls: 'is-idle' },
  running: { label: '计算中', cls: 'is-run' },
  done: { label: '已完成', cls: 'is-ok' },
  failed: { label: '失败', cls: 'is-danger' },
  missed: { label: '未完成', cls: 'is-warn' },
};

const showHash = ref(false);
const hashing = ref(false);
const hashAlgo = ref<HashAlgo>('sha256');
const hashPaths = ref<string[]>([]);
const hashRootName = ref('');
const hashRows = ref<HashRow[]>([]);
const hashTotal = ref(0);
const hashDone = ref(0);
/** 这一轮算完过没有：按钮文案据此在"开始计算 / 重新计算"之间切。 */
const hashRan = ref(false);
const hashScanning = ref(false);
const hashScanError = ref('');

/** path → 行：结果帧按路径落到对应的那一行上，不必每次线性查找。 */
let hashIndex = new Map<string, HashRow>();
/** 结果按遍历顺序回来，"下一条还没算的"就是正在算的那一个，所以游标只往前推。 */
let hashCursor = 0;
let hashScanAbort: AbortController | null = null;
/**
 * 正在跑的这次计算的中止句柄。
 *
 * 中止只是"不再读剩下的文件"：已经算完的摘要保留在表格与任务记录里。
 * 断开连接会同时终止目标机上的那条命令，不会留下一个还在读盘的进程。
 */
let hashAbort: AbortController | null = null;

/**
 * 正在算的那一个文件已经算了多久。
 *
 * 目标机那边是一个摘要进程按顺序逐个读文件，**读完一个才吐一行结果**，所以一个几 GB 的
 * 文件会让"已完成数"长时间停在 0。没有这个计时，界面看起来就跟卡死一样 —— 用户没法
 * 分辨"在慢慢读"和"早就断了"。
 *
 * 起点取"上一条结果到达的时刻"：既然逐个处理，那一刻就是当前这个文件开始读的时刻。
 */
let hashLastAt = 0;
const hashTick = ref(Date.now());
let hashTickTimer: number | null = null;

/** 把毫秒折成 分:秒（超过一小时折成 时:分:秒）。 */
function elapsedText(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (n: number) => String(n).padStart(2, '0');
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

/** 当前这个文件已用的时长；不在计算中时为 0（那时计时器也停了）。 */
const hashElapsed = computed(() => (hashing.value ? elapsedText(hashTick.value - hashLastAt) : '0:00'));

function startHashTicker(): void {
  stopHashTicker();
  hashTick.value = Date.now();
  hashTickTimer = window.setInterval(() => {
    hashTick.value = Date.now();
  }, 1000);
}

function stopHashTicker(): void {
  if (hashTickTimer !== null) {
    window.clearInterval(hashTickTimer);
    hashTickTimer = null;
  }
}

/** 表格里有没有大小可显示；整列都是"—"时不如不显示这一列。 */
const showHashSize = computed(() => hashRows.value.some((row) => row.size !== undefined));

/** 当前算法的展示名，作为哈希值的前缀。 */
const hashAlgoLabel = computed(
  () => HASH_ALGOS.find((item) => item.value === hashAlgo.value)?.label ?? hashAlgo.value,
);

function hashRowKey(row: HashRow): string {
  return row.path;
}

const hashColumns = computed<DataTableColumns<HashRow>>(() => {
  const sizeColumn: DataTableColumns<HashRow> = showHashSize.value
    ? [
        {
          title: '大小',
          key: 'size',
          width: 100,
          render: (row) =>
            h('span', { class: 'num' }, row.size === undefined ? '—' : formatBytes(row.size)),
        },
      ]
    : [];
  // 表格开了虚拟滚动（几万文件的行也铺得进来），所以每一列都要给固定宽度。
  return [
    {
      title: '文件',
      key: 'path',
      width: 280,
      render: (row) => h('span', { class: 'fm__hash-path', title: row.path }, row.path),
    },
    ...sizeColumn,
    {
      title: '哈希值',
      key: 'hash',
      width: 350,
      render: (row) =>
        row.error
          ? h('span', { class: 'fm__hash-error' }, row.error)
          : h(
              'span',
              { class: 'fm__hash-value num' },
              row.hash
                ? [
                    // 算法名单独包一层（且不可选中）：摘要一键选中时要拿到纯摘要，
                    // 多带一个 "SHA-256: " 前缀，粘到校验命令里就用不了。
                    h('span', { class: 'fm__hash-algo' }, `${row.algo}: `),
                    row.hash,
                  ]
                : '',
            ),
    },
    {
      title: '状态',
      key: 'status',
      width: 130,
      render: (row) => {
        const meta = HASH_STATUS[row.status];
        if (row.status !== 'running') {
          return h('span', { class: ['fm__hash-status', meta.cls] }, meta.label);
        }
        // 正在算的这一行只报"读到哪儿了"：算的是哪个文件，同一行的「文件」列已经写着；
        // 整批还剩几个，工具栏上也有一个计数。列里再重复一遍只会把行撑高、把视线拉散。
        // 一帧字节都没报来时（拿不到读位置）退回只显示时长，不要挂一个假的 0%。
        const text =
          row.percent === null ? `已用 ${hashElapsed.value}` : `${row.percent}% · 已用 ${hashElapsed.value}`;
        return h('span', { class: ['fm__hash-status', 'is-run', 'num'], title: `正在算：${row.path}` }, text);
      },
    },
  ];
});

function openHash(entries: FileEntry[]): void {
  if (entries.length === 0) return;
  hashPaths.value = entries.map((item) => item.path);
  // 标题里要能看出"对什么算的"，否则用户是在等一个不知道自己选了什么的进度。
  hashRootName.value = entries.length === 1 ? entries[0].name : `选中 ${entries.length} 项`;
  showHash.value = true;
  void loadHashEntries();
}

/**
 * 打开弹窗时先把待计算的文件列出来。
 *
 * 这就是"动手之前先看清范围"：目录会被递归展开，若只在算完才知道有多少文件，
 * 用户是在为一个自己没看清的操作等进度。
 *
 * 清单与算法无关，所以换算法不必重扫；与计算分成两个请求，扫描期间界面照常可用。
 */
async function loadHashEntries(): Promise<void> {
  cancelHashScan();
  const ctrl = new AbortController();
  hashScanAbort = ctrl;

  hashIndex = new Map();
  hashCursor = 0;
  resetHashScanState();
  hashRows.value = [];
  hashTotal.value = 0;
  hashDone.value = 0;
  hashRan.value = false;
  hashScanError.value = '';
  hashScanning.value = true;

  try {
    await api.files.scan(
      hostId.value,
      hashPaths.value,
      (event) => {
        if (ctrl.signal.aborted) return;
        switch (event.type) {
          case 'entries':
            // 一批 200 条：这一批的大小已经随它下来了，但此刻行还没铺，先记边界。
            pendingScans.push({ paths: event.paths, sizes: event.sizes });
            appendHashRows(event.paths, event.sizes);
            hashTotal.value = event.total ?? hashRows.value.length;
            break;
          case 'done':
            // 收尾：把攒下的批次按边界归位（大小是随批次一起下来的）。
            settleHashScan();
            hashTotal.value = event.total ?? hashRows.value.length;
            break;
          case 'error':
            hashScanError.value = event.message;
            break;
        }
      },
      ctrl.signal,
    );
  } catch (err) {
    // 主动放弃（关了弹窗）不是失败，别把 AbortError 报给用户。
    if (!ctrl.signal.aborted) hashScanError.value = describe(err, '列出文件失败');
  } finally {
    if (hashScanAbort === ctrl) {
      hashScanAbort = null;
      hashScanning.value = false;
    }
  }
}

/** 扫描时收到的一个路径分组，带它那一组的大小。 */
type ScanBatch = { paths: string[]; sizes?: number[] };

/**
 * 扫描的组边界，按"已经铺了多少行"累计。
 *
 * 服务器攒够 scanBatchSize 条就发一批，大小随那一批一起下来。只要两边一批批地数，
 * 行号自然对得上 —— 所以这里存的是"下一批从第几行开始"。
 */
let scanCursor = 0;

/** 铺行时把这一批的边界记下来，等大小到齐后再按它归位。 */
let pendingScans: ScanBatch[] = [];

/** 扫描结束（或断开）时把批次清干净，免得下一轮打开弹窗时把旧边界算进去。 */
function resetHashScanState(): void {
  scanCursor = 0;
  pendingScans = [];
}

/**
 * 把大小补到对应的行上。
 *
 * 为什么不按下标直接配：大小的下标是"这一批里的第几个"，不是"表格里的第几行" ——
 * 服务器一批 200 条，第 3 批的第 1 个在表格里是第 401 行。当成行号用会把每行都配上
 * 别人的大小，而且不报错。
 */
function flushScanBatches(batch: ScanBatch): void {
  const count = batch.paths.length;
  for (let i = 0; i < count; i++) {
    const row = hashRows.value[scanCursor + i];
    if (!row) break;
    const size = batch.sizes?.[i];
    // -1 是"这条路拿不到大小"的记号；已经算出结果的行不改（它的大小是与摘要一起下来的）。
    if (size === undefined || size < 0 || row.size !== undefined) continue;
    row.size = size;
  }
  scanCursor += count;
}

/**
 * 扫描收尾：把攒下的批次按边界归位。
 *
 * 大小是在"这一批刚扫完"时就问到的，所以在过程中就能逐个补上 —— 不必等整个目录扫完
 * （几万文件要额外等好几秒，而那时进度条看着已经走满了）。
 */
function settleHashScan(): void {
  for (const batch of pendingScans) flushScanBatches(batch);
  resetHashScanState();
}

/** 把一批清单路径铺成"等待中"的行。 */
function appendHashRows(paths: string[], sizes?: number[]): void {
  for (let i = 0; i < paths.length; i++) {
    const size = sizes?.[i];
    indexHashRow({
      path: paths[i],
      // -1 是服务端"这条路拿不到大小"的记号，与"确实是 0 字节"不是一回事。
      size: size === undefined || size < 0 ? undefined : size,
      hash: '',
      error: '',
      algo: '',
      status: 'pending',
      percent: null,
    });
  }
}

/**
 * 把一行放进表格并登记索引，返回可供后续修改的那一份。
 *
 * 关键在返回的是**响应式代理**（从 hashRows 里读回来的那个）而不是原始对象：
 * 直接用原始对象改属性不会触发重渲染 —— 结果就是"服务端算完了，界面一片空白"，
 * 而且不报任何错。这个坑在"结果帧到达"这条路上尤其致命。
 */
function indexHashRow(row: HashRow): HashRow {
  hashRows.value.push(row);
  const live = hashRows.value[hashRows.value.length - 1];
  hashIndex.set(live.path, live);
  return live;
}

/**
 * 放弃正在跑的扫描。
 *
 * 流式请求不会因为弹窗关掉就自己停，得显式中止，否则目标机还在为一个已经关掉的
 * 弹窗遍历大目录。
 */
function cancelHashScan(): void {
  hashScanAbort?.abort();
  hashScanAbort = null;
  hashScanning.value = false;
  // 半途放弃时也要把攒下的批次归位：它们是已经问到手的大小，丢掉就是白问一趟。
  settleHashScan();
}

/** 把游标推到的第一条"等待中"标成计算中：它就是下一个要被算的文件。 */
function markRunning(): void {
  while (hashCursor < hashRows.value.length && hashRows.value[hashCursor].status !== 'pending') {
    hashCursor++;
  }
  if (hashCursor < hashRows.value.length) hashRows.value[hashCursor].status = 'running';
}

/** 把一条结果落到对应的那一行上，并推进"计算中"的标记。 */
function applyHashResult(event: {
  path: string;
  size?: number;
  hash?: string;
  error?: string;
}): void {
  let row = hashIndex.get(event.path);
  if (!row) {
    // 清单里没有它（扫描失败了，或者扫描之后目录里又多了文件）：补一行。
    // 总比让这条结果凭空消失好 —— 它是用户真正想要的东西。
    row = indexHashRow({
      path: event.path,
      size: undefined,
      hash: '',
      error: '',
      algo: '',
      status: 'pending',
      percent: null,
    });
    hashTotal.value = Math.max(hashTotal.value, hashRows.value.length);
  }
  if (event.size !== undefined) row.size = event.size;
  row.hash = event.hash ?? '';
  row.error = event.error ?? '';
  row.algo = hashAlgoLabel.value;
  row.status = row.error ? 'failed' : 'done';
  // 百分比描述的是"正在读"，这一行已经读完了，留着它只会让人以为还在算。
  row.percent = null;
  // 这一个文件算完了，接下来那个的计时从此刻起算。
  hashLastAt = Date.now();
  markRunning();
}

/**
 * 目标机报"开始处理这个文件了"。
 *
 * 比"游标指向下一条等待中的"更准：目标机那边才是真正在决定算哪个文件，
 * 而进度要贴到哪一行上全靠这个路径。顺带把计时起点挪到这一刻 —— 它才是这个文件
 * 开始被读的时刻。
 */
function markHashItem(path: string, size?: number): void {
  // 一次只算一个文件：把上一行还挂着的"计算中"摘掉（它没留下结果，比如文件读不了），
  // 否则界面上会同时有两行在跳同一个计时，看着像在并行算两个。
  for (const other of hashRows.value) {
    if (other.path !== path && other.status === 'running') {
      other.status = 'pending';
      other.percent = null;
    }
  }

  let row = hashIndex.get(path);
  if (!row) {
    row = indexHashRow({
      path,
      size: undefined,
      hash: '',
      error: '',
      algo: '',
      status: 'pending',
      percent: null,
    });
    hashTotal.value = Math.max(hashTotal.value, hashRows.value.length);
  }
  row.status = 'running';
  if (size !== undefined && size >= 0 && row.size === undefined) row.size = size;
  row.percent = 0;
  hashLastAt = Date.now();
}

/**
 * 目标机报"这个文件读到第几字节了"。
 *
 * 只贴到"正在读"的那一行上，并且路径要对得上：stdout 与 stderr 是两条独立的流，
 * 一行迟到的进度帧完全可能在下一个文件已经开始之后才到 —— 贴错了就会让百分比乱跳。
 */
function applyHashBytes(path: string | undefined, bytes?: number, total?: number): void {
  if (bytes === undefined || total === undefined || total <= 0) return;
  const row = path === undefined ? undefined : hashIndex.get(path);
  if (!row || row.status !== 'running') return;
  const ratio = Math.min(1, Math.max(0, bytes / total));
  row.percent = Math.round(ratio * 100);
}

/** 收场：还没算出结果的行记成"未完成"，而不是一直停在"等待中"。 */
function settlePending(): void {
  for (const row of hashRows.value) {
    if (row.status === 'pending' || row.status === 'running') row.status = 'missed';
  }
}

/** 工具栏里的批量入口：作用于勾选的行。 */
function hashChecked(): void {
  const chosen = new Set(checkedKeys.value.map(String));
  openHash(listing.value.entries.filter((entry) => chosen.has(entry.path)));
}

async function runHash(): Promise<void> {
  if (hashing.value || hashScanning.value) return;
  hashing.value = true;
  hashRan.value = true;
  hashCursor = 0;
  hashDone.value = 0;
  // 清单已经在手上时只清值、不动行：重新算一遍不该让表格整片消失再长回来。
  for (const row of hashRows.value) {
    row.hash = '';
    row.error = '';
    row.algo = '';
    row.status = 'pending';
    row.percent = null;
  }
  if (hashRows.value.length === 0) hashTotal.value = 0;
  markRunning();
  hashLastAt = Date.now();
  startHashTicker();

  try {
    const outcome = await collectHashes();
    if (outcome === 'aborted') {
      // 中止不是失败：已经算完的那些仍然有效，界面上也留着。
      message.info('已中止计算，已经算完的摘要保留');
      return;
    }
    if (outcome !== 'done') return;
    // 有文件没算出结果时要说出来：只报"完成"会让用户以为清单是完整的。
    const missed = hashTotal.value - hashDone.value;
    if (missed > 0) message.warning(`已完成，但有 ${missed} 个文件未能计算`);
    else message.success(`已计算 ${hashRows.value.length} 个文件的哈希`);
  } finally {
    hashing.value = false;
    stopHashTicker();
  }
}

/**
 * 中止正在跑的这次计算。
 *
 * 断开的连接会同时终止目标机上的那条命令，所以不会留下一个还在读盘的进程；
 * 已经算完的摘要不会作废（表格与任务记录里都保留）。
 */
function stopHash(): void {
  if (!hashing.value) return;
  hashAbort?.abort();
}

/**
 * 跑一次哈希，把逐帧结果填进表格，并返回终态。
 *
 * 与 runArchive 同形：interrupted 表示流结束了却始终没收到结论 —— 网络断开时
 * 就是这样，不能当成成功。aborted 是用户自己按下了中止，不能报成失败。
 */
async function collectHashes(): Promise<'done' | 'error' | 'interrupted' | 'aborted'> {
  let outcome: 'done' | 'error' | 'aborted' | null = null;
  const ctrl = new AbortController();
  hashAbort = ctrl;
  transfer.value = { kind: '哈希', label: hashRootName.value, loaded: 0, total: 0, unit: 'items' };

  const onEvent = (event: HashEvent): void => {
    switch (event.type) {
      case 'prepare':
        // 目标机要先扫一遍目录才知道有多少文件，这段没有输出，得说清在做什么。
        transfer.value = {
          kind: '哈希',
          label: hashRootName.value,
          loaded: 0,
          total: 0,
          unit: 'items',
          note: '正在扫描文件…',
        };
        break;
      case 'progress':
        // total 可能还没数出来（为 0），此时进度条退回不确定状态。
        hashTotal.value = event.total ?? hashTotal.value;
        hashDone.value = event.done ?? hashDone.value;
        transfer.value = {
          kind: '哈希',
          label: hashRootName.value,
          loaded: hashDone.value,
          total: hashTotal.value,
          unit: 'items',
        };
        break;
      case 'result':
        applyHashResult(event);
        break;
      case 'item':
        // 目标机开始读这个文件了：把"计算中"打到它那一行上，并把百分比归零。
        markHashItem(event.path, event.size);
        break;
      case 'bytes':
        applyHashBytes(event.path, event.bytes, event.bytesTotal);
        break;
      case 'escalating':
        transfer.value = {
          kind: '哈希',
          label: hashRootName.value,
          loaded: 0,
          total: 0,
          unit: 'items',
          note: '权限不足，正在以 root 重做…',
        };
        break;
      case 'done':
        hashTotal.value = event.total ?? hashTotal.value;
        hashDone.value = event.done ?? hashDone.value;
        settlePending();
        outcome = 'done';
        break;
      case 'error':
        outcome = 'error';
        // 整批失败时清单里的文件一个都没算出来，不能让它们停在"等待中"。
        settlePending();
        message.error(event.message);
        break;
    }
  };

  try {
    await api.files.hash(hostId.value, hashPaths.value, hashAlgo.value, onEvent, ctrl.signal);
  } catch (err) {
    // 自己按下了中止：这不是失败，不该弹一条红色的错误。
    if (ctrl.signal.aborted) {
      outcome = 'aborted';
      settlePending();
    } else {
      outcome = 'error';
      settlePending();
      message.error(describe(err, '计算哈希失败'));
    }
  } finally {
    transfer.value = null;
    if (hashAbort === ctrl) hashAbort = null;
  }

  if (outcome === null) {
    settlePending();
    message.error('计算哈希中断：与面板的连接已断开');
    return 'interrupted';
  }
  return outcome;
}

/**
 * 关掉哈希面板。
 *
 * 计算中途不给直接关：结果是一帧一帧进来的，关掉再打开就会清空重来，
 * 用户会以为"算了半天什么都没有"。要停下来有专门的「中止计算」按钮 —— 那是明确的
 * 意图，而 Esc / 点遮罩更可能是误触。
 */
function closeHash(): void {
  if (hashing.value) {
    message.info('计算正在进行：要停下来请点「中止计算」');
    return;
  }
  cancelHashScan();
  showHash.value = false;
}

/** 弹窗自己发起的关闭请求（X、Esc）；计算中途一律驳回。 */
function onHashShow(value: boolean): void {
  if (value) showHash.value = true;
  else closeHash();
}

/**
 * 清单文本：<摘要>  <相对路径>。
 *
 * 这个格式正是 sha256sum -c 认得的，导出的文件可以原样拿到目标机上校验。
 * 失败的行不写进去 —— 带一条错的摘要去校验，只会得到一句莫名其妙的失败。
 */
function hashManifest(): string {
  return hashRows.value
    // 只收真算出了摘要的行：清单里的"等待中 / 未完成"行与失败行都没有摘要，
    // 混进去会拼出一行只有路径的垃圾，sha256sum -c 读到这里就报错。
    .filter((row) => row.hash && !row.error)
    .map((row) => `${row.hash}  ${row.path}`)
    .join('\n');
}

function copyHashes(): void {
  const text = hashManifest();
  if (!text) {
    message.warning('还没有可复制的结果');
    return;
  }
  void writeClipboard(text, `已复制 ${text.split('\n').length} 条摘要`);
}

function exportHashes(): void {
  const text = hashManifest();
  if (!text) {
    message.warning('还没有可导出的结果');
    return;
  }
  // 末尾补一个换行：没有它，文本编辑器里最后一行常被当成没写完。
  const blob = new Blob([`${text}\n`], { type: 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  triggerDownload(url, `${hashAlgo.value}-${hashRootName.value}.txt`);
  // 不能立刻撤销：浏览器可能还没把这段 Blob 读完，那样落盘的是空文件。
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}

function pickFiles(): void {
  fileInput.value?.click();
}

async function upload(files: File[]): Promise<void> {
  if (files.length === 0) return;

  // 远端已存在同名文件时先确认。上传是覆盖写（sftp Create 是 O_TRUNC），
  // 不提示就直接把线上文件替换掉，用户往往来不及反应。
  const existing = new Set(listing.value.entries.map((e) => e.name));
  const clashes = files.filter((f) => existing.has(f.name));
  if (clashes.length > 0) {
    const names = clashes.map((f) => f.name).join('、');
    dialog.warning({
      title: '覆盖同名文件？',
      content: `${listing.value.path} 下已存在同名文件：${names}。继续上传会覆盖它们的内容，且不可恢复。`,
      positiveText: '覆盖上传',
      negativeText: '取消',
      onPositiveClick: () => doUpload(files),
    });
    return;
  }

  await doUpload(files);
}

async function doUpload(files: File[]): Promise<void> {
  if (transfer.value !== null) {
    message.info('已有传输正在进行，请等它结束');
    return;
  }

  uploading.value = true;
  const label = files.length === 1 ? files[0].name : `${files.length} 个文件`;
  // 待上传的总量前端本来就知道，先填上：否则进度条起步时只能显示 "0 B"，
  // 用户看不出这条传输到底有多大、还差多少。
  const total = files.reduce((sum, file) => sum + file.size, 0);
  transfer.value = { kind: '上传', label, loaded: 0, total, unit: 'bytes' };
  try {
    const result = await api.files.upload(
      hostId.value,
      listing.value.path,
      files,
      (loaded, total) => {
        transfer.value = { kind: '上传', label, loaded, total, unit: 'bytes' };
      },
    );
    message.success(`已上传 ${result.saved.length} 个文件`);
    void load();
  } catch (err) {
    message.error(describe(err, '上传失败'));
  } finally {
    uploading.value = false;
    transfer.value = null;
  }
}

async function onFilesPicked(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement;
  const files = Array.from(input.files ?? []);
  // 立即清空，否则连续选择同一个文件不会再次触发 change。
  input.value = '';
  await upload(files);
}

function onDrop(event: DragEvent): void {
  dragActive.value = false;
  dropHint.value = '';
  // 内部拖拽（从另一个文件窗口 / 终端侧栏拖过来的条目）与 OS 文件拖入共用
  // 同一个落点，但后续动作完全不同：前者弹复制/移动菜单，后者是上传。
  if (internalDragPayload()) {
    openDropMenu(event, listing.value.path);
    endFileDrag();
    return;
  }
  const files = Array.from(event.dataTransfer?.files ?? []);
  void upload(files);
}

/**
 * 行的低频操作收进"更多"，让主操作（打开 / 下载）保持突出。
 *
 * 分组按"对这份数据做什么"来排：查看、把路径拿出去用、变更内容、改名、删除。
 * 与条目类型无关的项（改权限、复制、移动）对所有行都给，不适合的项（编辑、
 * 压缩目录、解压包）按类型隐去 —— 灰着一排点了没反应的项比不显示更烦人。
 */
function rowMenuOptions(entry: FileEntry) {
  const previewable = canPreview(entry);
  const editable = isTextKind(previewKindOf(entry.name));
  return [
    entry.isDir || previewable
      ? { label: '打开', key: 'open' }
      : { label: '下载', key: 'download' },
    // 可预览的文件再给一条下载：打开查看器之后想存到本地是紧接着的动作。
    ...(!entry.isDir && previewable ? [{ label: '下载', key: 'download' }] : []),
    ...(editable ? [{ label: '编辑', key: 'edit' }] : []),
    { label: '复制路径', key: 'copyPath' },
    ...(wm ? [{ label: '在终端中打开', key: 'terminal' }] : []),
    ...(!entry.isDir && wm ? [{ label: '运行', key: 'run' }] : []),
    { label: '改权限…', key: 'chmod' },
    { label: '属性…', key: 'attr' },
    { label: '计算哈希…', key: 'hash' },
    { type: 'divider', key: 'divider-copy' },
    { label: '复制', key: 'copy' },
    { label: '移动', key: 'move' },
    ...(entry.isDir ? [{ label: '压缩为 .tar.gz / .zip…', key: 'compress' }] : []),
    ...(canExtract(entry) ? [{ label: '解压到当前目录', key: 'extract' }] : []),
    { type: 'divider', key: 'divider-rename' },
    { label: '重命名', key: 'rename' },
    { type: 'divider', key: 'divider-remove' },
    { label: '删除…', key: 'remove' },
  ];
}

function onRowAction(key: string, entry: FileEntry): void {
  if (key === 'open') openFile(entry);
  else if (key === 'download') void download(entry);
  else if (key === 'edit') editFile(entry);
  else if (key === 'copyPath') void copyPath(entry);
  else if (key === 'terminal') openInTerminal(entry);
  else if (key === 'run') runFile(entry);
  else if (key === 'chmod') openChmod([entry]);
  else if (key === 'attr') void openAttr(entry);
  else if (key === 'hash') openHash([entry]);
  else if (key === 'copy') fillClipboard('copy', [entry.path]);
  else if (key === 'move') fillClipboard('move', [entry.path]);
  else if (key === 'compress') openCompress(entry);
  else if (key === 'extract') void extractArchive(entry);
  else if (key === 'rename') startRename(entry);
  else if (key === 'remove') confirmRemove(entry);
}

const columns = computed<DataTableColumns<FileEntry>>(() => [
  { type: 'selection' },
  {
    // 类型放在名称前：目录与文件的第一眼差别，比权限位更值得先看到。
    title: '类型',
    key: 'kind',
    width: 56,
    render: (row) => h('span', { class: ['fm__ftype', { 'is-dir': row.isDir }] }, row.isDir ? 'DIR' : 'FILE'),
  },
  {
    title: '名称',
    key: 'name',
    minWidth: 200,
    sorter: (a, b) => a.name.localeCompare(b.name, 'zh-Hans-CN'),
    render: (row) =>
      h('div', { class: 'fm__name' }, [
        h(
          row.isDir ? 'a' : 'span',
          { class: row.isDir ? 'fm__link' : 'fm__text', onClick: () => enter(row) },
          row.name,
        ),
        row.isDir ? h(NTag, { size: 'tiny', bordered: false, type: 'info' }, { default: () => '目录' }) : null,
        row.isLink ? h(NTag, { size: 'tiny', bordered: false }, { default: () => '链接' }) : null,
      ]),
  },
  {
    title: '大小',
    key: 'size',
    width: 96,
    sorter: (a, b) => a.size - b.size,
    render: (row) => h('span', { class: 'num' }, row.isDir ? '—' : formatBytes(row.size)),
  },
  {
    title: '修改时间',
    key: 'modTime',
    width: 150,
    sorter: (a, b) => new Date(a.modTime).getTime() - new Date(b.modTime).getTime(),
    render: (row) => h('span', { class: 'num' }, formatTime(row.modTime)),
  },
  {
    title: '权限',
    key: 'mode',
    width: 110,
    render: (row) => h('span', { class: 'num' }, row.mode),
  },
  {
    title: '操作',
    key: 'actions',
    width: 140,
    render: (row) =>
      h(NSpace, { size: 4, wrap: false, align: 'center' }, {
        default: () => [
          h(
            NButton,
            {
              size: 'small',
              quaternary: true,
              type: row.isDir ? 'primary' : 'default',
              onClick: () => (row.isDir ? enter(row) : openFile(row)),
            },
            { default: () => (row.isDir ? '进入' : canPreview(row) ? '打开' : '下载') },
          ),
          h(
            NDropdown,
            {
              trigger: 'click',
              placement: 'bottom-end',
              options: rowMenuOptions(row),
              onSelect: (key: string) => onRowAction(key, row),
            },
            {
              default: () =>
                h(
                  NButton,
                  { size: 'small', quaternary: true, 'aria-label': `更多操作：${row.name}` },
                  { default: () => '更多' },
                ),
            },
          ),
        ],
      }),
  },
]);

// 双击行：目录进入、文件打开查看器（不可预览的类型仍下载）。
// 拖动行：把这一行（或已勾选的一批）交给跨窗口拖拽；落在文件夹行上就是搬进那个目录。
function rowProps(row: FileEntry) {
  return {
    onDblclick: () => openFile(row),
    draggable: true,
    onDragstart: (event: DragEvent) => beginRowDrag(event, row),
    onDragend: () => endFileDrag(),
    onDragover: (event: DragEvent) => onRowDragOver(event, row),
    onDragleave: (event: DragEvent) => onRowDragLeave(event, row),
    onDrop: (event: DragEvent) => onRowDrop(event, row),
  };
}

const kindCount = computed(() => {
  const dirs = listing.value.entries.filter((e) => e.isDir).length;
  return { dirs, files: listing.value.entries.length - dirs };
});

// 选好主机（或从一台换到另一台）之后重新加载根目录。不重置的话，
// 上一台机器的文件会继续列在下面，而用户以为自己看的是新那台。
watch(target, () => {
  listing.value = { path: '/', parent: '', entries: [] };
  loadError.value = '';
  // 剪贴板不因换主机而清空：跨主机粘贴正是这次要支持的能力，
  // 路径属于哪台机器由 clipboard.hostId 记着，粘贴时会走跨主机传输。
  void load('/');
  void loadEscalation();
  void loadCapacity();
});

/** 别的窗口搬走了 / 搬来了本机的文件时，本窗口也要跟着刷新。 */
let stopFileEvents: (() => void) | null = null;

onMounted(() => {
  void load('/');
  // watch 不是 immediate 的，带 hostId 打开时初始那台机器要靠这里补一次。
  void loadEscalation();
  void loadCapacity();
  stopFileEvents = onFilesChanged((ids) => {
    if (target.value !== null && ids.includes(target.value)) void load();
  });
});

// 窗口被关掉时扫描可能还在跑：它不会自己停，得显式中止，
// 否则目标机还在为一个已经关掉的弹窗遍历大目录。计算与计时器同理：
// 窗口都没了，再让它读下去只是白白占着目标机的盘。
onBeforeUnmount(() => {
  stopFileEvents?.();
  stopFileEvents = null;
  cancelHashScan();
  hashAbort?.abort();
  stopHashTicker();
});
</script>

<template>
  <div
    class="fm"
    :class="{ 'fm--drop': dragActive }"
    @dragover.prevent="onDragOver"
    @dragleave="dragActive = false"
    @drop.prevent="onDrop"
  >
    <!--
      路径条：窗口身份由标题栏副标题给出（哪台机器的哪个目录），
      这一行只负责"跳转 / 对当前目录动手"。
      回上级直接点面包屑的上一层，不再占一个独立按钮。
    -->
    <div class="fm__pathbar">
      <!-- 平时是可点的层级路径；点路径空白处切换成输入框（像浏览器地址栏）。
           首段「/」是根目录，点它直接回根。 -->
      <n-input
        v-if="editingPath"
        ref="pathInput"
        v-model:value="pathDraft"
        class="fm__path-edit"
        size="small"
        :input-props="{ 'aria-label': '输入绝对路径', spellcheck: 'false' }"
        placeholder="输入绝对路径后回车跳转，Esc 取消"
        @keyup.enter="submitPathJump"
        @keyup.esc="cancelPathEdit"
        @blur="cancelPathEdit"
      />
      <div v-else class="fm__path" :title="`${listing.path}（点击输入路径）`" @click="beginPathEdit">
        <!-- 主机名就是路径的第一节：桌面上打开的窗口用胶囊（同时可换主机），
             带 hostId 打开的窗口没有选择器，直接写主机名。 -->
        <span v-if="!needsPicker && targetName" class="fm__path-host">{{ targetName }}</span>
        <div v-if="target !== null" class="fm__crumbs">
          <button
            class="fm__crumb"
            :class="{ 'is-current': listing.path === '/' }"
            title="回到根目录"
            @click.stop="load('/')"
          >
            根目录
          </button>
          <template v-for="crumb in pathCrumbs" :key="crumb.path">
            <i class="fm__path-sep" aria-hidden="true">/</i>
            <button
              class="fm__crumb"
              :class="{ 'is-current': crumb.path === listing.path }"
              @click.stop="load(crumb.path)"
            >
              {{ crumb.label }}
            </button>
          </template>
        </div>
      </div>

      <div class="fm__bar-actions">
        <n-button size="small" :disabled="target === null" @click="pickFiles">上传</n-button>
        <n-button size="small" :disabled="target === null" @click="openMkdir">新建目录</n-button>
        <n-button size="small" :loading="loading" :disabled="target === null" @click="refresh">
          刷新
        </n-button>
      </div>

      <!-- 从桌面图标进来时先在窗口内选主机；带 hostId 打开的不会走到这里。
           放在最右端：它和标题回答的是同一个问题（现在看的是哪台机器），
           不打断左侧「路径 + 对当前目录动手」这条主线。
           名字一并收下：「在终端中打开」要拿它拼新窗口的标题。 -->
      <host-picker
        v-if="needsPicker"
        v-model:host-id="target"
        v-model:host-name="pickedName"
        include-local
      />
    </div>

    <!--
      提权说明只在窗口顶部出现一条：写文件、删除、上传都是高频操作，
      每次都弹窗会让人点麻木，反而更容易在真正危险的那次误确认。
    -->
    <n-alert v-if="escalationNotice" :type="escalationNotice.level">
      {{ escalationNotice.text }}
    </n-alert>

    <!-- 读取失败时留在界面上：502 是目标机 SSH/SFTP 连不上，不是"空目录"。 -->
    <div v-if="loadError" class="fm__load-error" role="alert">
      <span class="fm__load-error-text">{{ loadError }}</span>
      <n-button size="tiny" @click="load()">重试</n-button>
    </div>

    <!-- 传输进度：上传走 XHR 的上行进度，下载走 XHR 的接收进度，
         压缩与解压走服务端逐帧回传的条目数 -->
    <div v-if="transfer" class="fm__transfer">
      <span class="fm__transfer-kind">{{ transfer.kind }}</span>
      <span class="fm__transfer-name num" :title="transfer.label">{{ transfer.label }}</span>
      <!-- 没有数字可报的阶段（数条目、提权重做）要说清在做什么，否则进度条就是静止的 -->
      <span v-if="transfer.note" class="fm__transfer-note">{{ transfer.note }}</span>
      <span class="fm__transfer-track">
        <span
          class="fm__transfer-fill"
          :class="{ 'is-unknown': transferPercent === null }"
          :style="{ width: transferPercent === null ? '100%' : `${transferPercent}%` }"
        />
      </span>
      <span class="fm__transfer-size num">{{ transferText }}</span>
    </div>

    <!--
      主栏是文件卡，右栏是"这个目录所在的环境"：容量与常用位置。
      宽窗口并排，窄窗口由容器查询折成一栏（见 global.css 的 .ui-grid）。
    -->
    <div class="ui-grid fm__grid">
      <section class="ui-card ui-card--flush fm__surface">
        <div class="ui-card__head">
          <div class="ui-card__ident">
            <p class="ui-card__eyebrow">DIRECTORY</p>
            <h2 class="ui-card__title">文件列表</h2>
          </div>
          <div class="ui-card__tools">
            <span class="ui-card__meta">
              {{ listing.entries.length }} 项 · {{ kindCount.dirs }} 目录 / {{ kindCount.files }} 文件
            </span>
            <!-- 一个按钮切两种布局：按钮上显示当前视图，点一下换另一个。 -->
            <n-button
              size="small"
              :title="viewMode === 'list' ? '切换到卡片视图' : '切换到列表视图'"
              :aria-label="viewMode === 'list' ? '切换到卡片视图' : '切换到列表视图'"
              @click="toggleView"
            >
              {{ viewMode === 'list' ? '列表' : '卡片' }}
            </n-button>
            <n-dropdown trigger="click" placement="bottom-end" :options="fileActions" @select="onFileAction">
              <n-button size="small" aria-label="文件操作">操作</n-button>
            </n-dropdown>
          </div>
        </div>

      <n-data-table
        v-if="viewMode === 'list'"
        v-model:checked-row-keys="checkedKeys"
        :columns="columns"
        :data="listing.entries"
        :loading="loading"
        :bordered="false"
        :row-key="(row: FileEntry) => row.path"
        :row-props="rowProps"
        size="small"
        flex-height
        :scroll-x="800"
        style="flex: 1; min-height: 0"
      >
        <template #empty>
          <div class="fm__empty">
            <p class="fm__empty-title">
              {{ target === null ? '请先选择主机' : loadError ? '目录读取失败' : '这个目录是空的' }}
            </p>
            <p class="fm__empty-hint">
              <template v-if="target === null">在窗口顶部选择一台主机后即可浏览文件。</template>
              <template v-else-if="loadError">
                检查目标主机是否可达、SSH 凭据是否正确，然后点上方「重试」。
              </template>
              <template v-else>可以把文件拖到窗口里上传，也可以点「新建文件夹」创建目录。</template>
            </p>
          </div>
        </template>
      </n-data-table>

    <!-- 卡片视图：与表格共用 listing.entries、勾选与行操作菜单。 -->
    <div v-else class="fm__cards">
      <div v-if="listing.entries.length === 0" class="fm__empty fm__cards-empty">
        <p class="fm__empty-title">
          {{ target === null ? '请先选择主机' : loadError ? '目录读取失败' : '这个目录是空的' }}
        </p>
        <p class="fm__empty-hint">
          <template v-if="target === null">在窗口顶部选择一台主机后即可浏览文件。</template>
          <template v-else-if="loadError">
            检查目标主机是否可达、SSH 凭据是否正确，然后点上方「重试」。
          </template>
          <template v-else>可以把文件拖到窗口里上传，也可以点「新建文件夹」创建目录。</template>
        </p>
      </div>

      <article
        v-for="entry in listing.entries"
        :key="entry.path"
        class="fm__card"
        :class="{ 'is-checked': isChecked(entry) }"
        draggable="true"
        @dragstart="beginRowDrag($event, entry)"
        @dragend="endFileDrag"
        @dragover="onRowDragOver($event, entry)"
        @dragleave="onRowDragLeave($event, entry)"
        @drop="onRowDrop($event, entry)"
        @dblclick="openFile(entry)"
      >
        <header class="fm__card-head">
          <n-checkbox
            :checked="isChecked(entry)"
            :aria-label="`选择 ${entry.name}`"
            @update:checked="(value) => setChecked(entry, Boolean(value))"
          />
          <span class="fm__card-icon" :class="{ 'is-dir': entry.isDir }" aria-hidden="true">
            <svg><use :href="entry.isDir ? '#glyph-folder' : '#glyph-file'" /></svg>
          </span>
          <button
            class="fm__card-name"
            type="button"
            :title="entry.name"
            @click="openEntry(entry)"
          >
            {{ entry.name }}
          </button>
        </header>

        <p class="fm__card-meta">
          <span class="num">{{ entry.isDir ? '目录' : formatBytes(entry.size) }}</span>
          <span class="num">权限 {{ entry.mode }}</span>
        </p>
        <p class="fm__card-time num">{{ formatTime(entry.modTime) }}</p>

        <footer class="fm__card-foot">
          <n-tag v-if="entry.isLink" size="tiny" :bordered="false">链接</n-tag>
          <span class="fm__card-spacer" />
          <n-button size="tiny" quaternary @click="openFile(entry)">
            {{ entry.isDir ? '进入' : canPreview(entry) ? '打开' : '下载' }}
          </n-button>
          <n-dropdown
            trigger="click"
            placement="bottom-end"
            :options="rowMenuOptions(entry)"
            @select="(key) => onRowAction(String(key), entry)"
          >
            <n-button size="tiny" quaternary :aria-label="`更多操作：${entry.name}`">更多</n-button>
          </n-dropdown>
        </footer>
      </article>
    </div>
    </section>

      <aside class="ui-side">
        <article class="ui-card fm__cap">
          <div class="ui-card__head">
            <div class="ui-card__ident">
              <p class="ui-card__eyebrow">CAPACITY</p>
              <h2 class="ui-card__title">磁盘占用</h2>
            </div>
            <span class="ui-card__meta">{{ currentMount ? formatBytes(currentMount.total) : '—' }}</span>
          </div>

          <template v-if="currentMount">
            <div class="ui-loadrow">
              <div class="ui-loadtop">
                <span>分区已用</span>
                <b>{{ formatBytes(currentMount.used) }}</b>
              </div>
              <div class="ui-bar">
                <i
                  :class="{
                    'is-sig': currentMount.usePercent < 80,
                    'is-warn': currentMount.usePercent >= 80 && currentMount.usePercent < 92,
                    'is-danger': currentMount.usePercent >= 92,
                  }"
                  :style="{ width: `${currentMount.usePercent}%` }"
                />
              </div>
            </div>
            <div class="ui-loadrow">
              <div class="ui-loadtop">
                <span>剩余可用</span>
                <b>{{ formatBytes(currentMount.avail) }}</b>
              </div>
              <div class="ui-bar">
                <i :style="{ width: `${capacityFreePercent}%` }" />
              </div>
            </div>
            <div class="ui-card__foot">
              <span :title="currentMount.source">{{ currentMount.fstype }} · {{ currentMount.source }}</span>
              <span>{{ currentMount.usePercent }}% 已用</span>
            </div>
          </template>
          <p v-else class="fm__side-hint">
            {{
              target === null
                ? '选好主机后读取磁盘容量'
                : capacityLoading
                  ? '正在读取磁盘信息…'
                  : '这台主机没有可用的容量数据'
            }}
          </p>
        </article>

        <article class="ui-card">
          <div class="ui-card__head">
            <div class="ui-card__ident">
              <p class="ui-card__eyebrow">PLACES</p>
              <h2 class="ui-card__title">常用位置</h2>
            </div>
            <span class="ui-card__meta">{{ PLACES.length }}</span>
          </div>
          <ul class="fm__places">
            <li v-for="place in PLACES" :key="place.path">
              <button
                class="fm__place"
                :class="{ 'is-active': target !== null && listing.path === place.path }"
                :disabled="target === null"
                @click="load(place.path)"
              >
                <span
                  class="led"
                  :class="{ 'led--ok': target !== null && listing.path === place.path }"
                  aria-hidden="true"
                />
                <span class="fm__place-text">
                  <b>{{ place.path }}</b>
                  <span>{{ place.hint }}</span>
                </span>
              </button>
            </li>
          </ul>
        </article>
      </aside>
    </div>

    <!-- 拖拽落点提示层：只有拖着东西悬在窗口上时才出现，平时不占位置 -->
    <div v-if="dragActive" class="fm__dropzone">
      <span class="fm__dropzone-text">{{ dropHint }}</span>
    </div>

    <!-- 拖拽落下后的动作菜单：复制 / 移动二选一，点外面即取消 -->
    <file-drop-menu
      :show="dropMenu.show"
      :x="dropMenu.x"
      :y="dropMenu.y"
      @update:show="(value) => (dropMenu.show = value)"
      @select="onDropAction"
    />

    <input ref="fileInput" type="file" multiple hidden @change="onFilesPicked" />

    <!-- 新建文件夹 -->
    <n-modal v-model:show="showMkdir" preset="card" title="新建文件夹" style="width: 420px">
      <n-form label-placement="left" label-width="72" :model="{ mkdirName }">
        <n-form-item label="目录名">
          <n-input v-model:value="mkdirName" placeholder="例如 logs" @keyup.enter="submitMkdir" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showMkdir = false">取消</n-button>
          <n-button type="primary" @click="submitMkdir">创建</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 重命名 -->
    <n-modal v-model:show="showRename" preset="card" title="重命名" style="width: 420px">
      <n-form label-placement="left" label-width="72" :model="{ renameName }">
        <n-form-item label="新名称">
          <n-input v-model:value="renameName" @keyup.enter="submitRename" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showRename = false">取消</n-button>
          <n-button type="primary" @click="submitRename">确定</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 改权限：可视化位编辑，八进制输入框保留给熟手 -->
    <n-modal v-model:show="showChmod" preset="card" title="修改权限" style="width: 520px">
      <div class="fm__chmod">
        <!-- 结果先摆出来：左边是八进制与符号形式，右边仍可直接输入 -->
        <div class="fm__chmod-head">
          <div class="fm__chmod-mode">
            <span class="fm__chmod-octal">{{ chmodBits === null ? '—' : chmodMode.trim() }}</span>
            <span class="fm__chmod-symbolic">{{ chmodSymbolic }}</span>
          </div>
          <div class="fm__chmod-entry">
            <span class="fm__chmod-entry-label">八进制</span>
            <n-input
              v-model:value="chmodMode"
              class="fm__chmod-input"
              placeholder="例如 755"
              @keyup.enter="submitChmod"
            />
          </div>
        </div>

        <!-- 3×3 位矩阵：行是作用对象，列是动作，点一格翻转那一位 -->
        <div class="fm__chmod-grid" role="group" aria-label="权限位">
          <span />
          <span v-for="bit in CHMOD_BITS" :key="bit.key" class="fm__chmod-col">{{ bit.label }}</span>
          <template v-for="row in CHMOD_ROWS" :key="row.key">
            <span class="fm__chmod-row">{{ row.label }}</span>
            <button
              v-for="bit in CHMOD_BITS"
              :key="bit.key"
              type="button"
              class="fm__chmod-cell"
              :class="{ 'is-on': hasChmodBit(row.shift, bit.mask) }"
              :aria-pressed="hasChmodBit(row.shift, bit.mask)"
              :aria-label="`${row.label}${bit.label}`"
              @click="toggleChmodBit(row.shift, bit.mask)"
            >
              {{ bit.letter }}
            </button>
          </template>
        </div>

        <!-- 特殊位不常用，单独一行 -->
        <div class="fm__chmod-special">
          <span class="fm__chmod-special-label">特殊位</span>
          <n-checkbox
            v-for="item in CHMOD_SPECIAL"
            :key="item.key"
            :checked="hasChmodSpecial(item.mask)"
            @update:checked="toggleChmodSpecial(item.mask)"
          >
            {{ item.label }}
          </n-checkbox>
        </div>

        <!-- 常用组合：点一下换一组位，矩阵与八进制同步 -->
        <div class="fm__chmod-presets">
          <span class="fm__chmod-presets-label">常用</span>
          <n-button
            v-for="preset in MODE_PRESETS"
            :key="preset.mode"
            size="tiny"
            :title="preset.hint"
            @click="chmodMode = preset.mode"
          >
            {{ preset.mode }}
          </n-button>
        </div>
      </div>

      <p class="fm__hint">
        将对 {{ chmodPaths.length }} 项生效。改目录的权限不会递归作用到它里面的文件。
      </p>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showChmod = false">取消</n-button>
          <n-button type="primary" @click="submitChmod">应用</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 压缩 -->
    <n-modal v-model:show="showCompress" preset="card" title="压缩" style="width: 460px">
      <n-form label-placement="left" label-width="72" :model="{ compressName }">
        <n-form-item label="格式">
          <n-radio-group v-model:value="compressFormat" size="small">
            <n-radio-button value="tar.gz">tar.gz</n-radio-button>
            <n-radio-button value="zip">zip</n-radio-button>
          </n-radio-group>
        </n-form-item>
        <n-form-item label="文件名">
          <n-input v-model:value="compressName" @keyup.enter="submitCompress" />
        </n-form-item>
      </n-form>
      <p class="fm__hint">
        输出到 {{ compressDir }} 下，包内是相对路径（解开不会多套一层目录）。
        zip 需要目标机装有 zip 命令，缺失时会直接报错。
      </p>
      <template #footer>
        <n-space justify="end">
          <n-button :disabled="compressing" @click="showCompress = false">取消</n-button>
          <n-button type="primary" :loading="compressing" @click="submitCompress">压缩</n-button>
        </n-space>
      </template>
    </n-modal>

    <!--
      属性：上半是不可改的元数据（只读，便于核对），下半是可就地修改的几项。
      保存时只提交改动过的项 —— 打开面板本身不该改变任何东西。
    -->
    <n-modal v-model:show="showAttr" preset="card" title="属性" style="width: 640px">
      <div v-if="loadingAttr" class="fm__hint">正在读取属性…</div>
      <template v-else-if="attr">
        <p class="fm__attr-name num" :title="attr.path">{{ attr.path }}</p>
        <div class="fm__attr">
          <span class="fm__attr-key">类型</span>
          <span class="fm__attr-value">
            {{ attrType }}
            <template v-if="attr.isLink"> → {{ attr.linkTarget || '（读不出目标）' }}</template>
          </span>

          <span class="fm__attr-key">大小</span>
          <span class="fm__attr-value num">
            {{ formatBytes(attr.size) }}（占用 {{ formatBytes(attr.blocks) }}）
          </span>

          <span class="fm__attr-key">权限</span>
          <span class="fm__attr-value num">{{ attr.mode }}</span>

          <span class="fm__attr-key">属主 / 属组</span>
          <span class="fm__attr-value num">
            {{ attr.owner || '—' }} / {{ attr.group || '—' }}（{{ attr.uid }}:{{ attr.gid }}）
          </span>

          <span class="fm__attr-key">修改时间</span>
          <span class="fm__attr-value num">{{ formatTime(attr.mtime) }}</span>

          <span class="fm__attr-key">访问时间</span>
          <span class="fm__attr-value num">{{ formatTime(attr.atime) }}</span>

          <!-- ctime 来自目标机的 stat 命令，取不到时后端给零值时间，这一行就不显示 -->
          <template v-if="hasCtime">
            <span class="fm__attr-key">状态变更时间</span>
            <span class="fm__attr-value num">{{ formatTime(attr.ctime) }}</span>
          </template>

          <span class="fm__attr-key">硬链接数</span>
          <span class="fm__attr-value num">{{ attr.nlink || '—' }}</span>

          <span class="fm__attr-key">inode</span>
          <span class="fm__attr-value num">{{ attr.inode || '—' }}</span>
        </div>

        <n-form label-placement="left" label-width="96" :model="attrForm">
          <n-form-item label="权限值">
            <n-input v-model:value="attrForm.mode" placeholder="例如 755" />
          </n-form-item>
          <n-form-item label="属主">
            <n-input v-model:value="attrForm.owner" placeholder="用户名；留空表示不改" />
          </n-form-item>
          <n-form-item label="属组">
            <n-input v-model:value="attrForm.group" placeholder="组名；留空表示不改" />
          </n-form-item>
          <n-form-item label="修改时间">
            <n-date-picker v-model:value="attrForm.mtime" type="datetime" :clearable="false" />
          </n-form-item>
          <n-form-item label="访问时间">
            <n-date-picker v-model:value="attrForm.atime" type="datetime" :clearable="false" />
          </n-form-item>
        </n-form>

        <p class="fm__hint">
          只有改动过的项会被提交。改属主而不填属组时属组保持原值；权限支持 4 位（setuid/setgid/sticky）。
          改这些需要相应的权限，权限不足时面板会自动提权重做。
        </p>
      </template>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showAttr = false">关闭</n-button>
          <n-button type="primary" :loading="savingAttr" :disabled="!attrDirty" @click="submitAttr">
            应用修改
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <!--
      哈希：结果一帧一帧进来，所以列表在计算过程中就在长。
      这里用受控的 :show 而不是 v-model —— 计算中途要能驳回关闭请求。
    -->
    <n-modal
      :show="showHash"
      preset="card"
      :title="`计算哈希 · ${hashRootName}`"
      style="width: 900px"
      :mask-closable="false"
      @update:show="onHashShow"
    >
      <n-space align="center" size="small" style="margin-bottom: 10px">
        <n-radio-group v-model:value="hashAlgo" size="small" :disabled="hashing">
          <n-radio-button v-for="algo in HASH_ALGOS" :key="algo.value" :value="algo.value">
            {{ algo.label }}
          </n-radio-button>
        </n-radio-group>
        <!--
          计算中这个按钮变成「中止计算」：大文件要读很久，不给停就只能干等，
          而重启面板之类的事又会把任务打断。中止是明确的意图，Esc/点遮罩不是，
          所以停下来的入口只放在这里，而不是顺手允许关窗。
        -->
        <n-button
          v-if="hashing"
          size="small"
          type="warning"
          @click="stopHash"
        >
          中止计算
        </n-button>
        <n-button
          v-else
          size="small"
          type="primary"
          :disabled="hashScanning"
          @click="runHash"
        >
          {{ hashRan ? '重新计算' : '开始计算' }}
        </n-button>
        <span v-if="hashScanning" class="fm__hint">正在列出文件…</span>
        <span v-else-if="hashTotal > 0" class="fm__hint num">
          {{ hashing ? `${hashDone}/${hashTotal}` : hashTotal }} 个文件
        </span>
      </n-space>

      <n-data-table
        :columns="hashColumns"
        :data="hashRows"
        :bordered="false"
        :row-key="hashRowKey"
        size="small"
        virtual-scroll
        :max-height="380"
      >
        <template #empty>
          <div class="fm__hint">
            <template v-if="hashScanning">正在列出待计算的文件…</template>
            <template v-else-if="hashScanError">
              {{ hashScanError }}。可以直接点「开始计算」，计算过程会自己处理。
            </template>
            <template v-else>这个范围里没有文件。</template>
          </div>
        </template>
      </n-data-table>

      <template #footer>
        <n-space justify="end">
          <n-button :disabled="hashing || hashRows.length === 0" @click="copyHashes">复制结果</n-button>
          <n-button :disabled="hashing || hashRows.length === 0" @click="exportHashes">导出清单</n-button>
          <n-button @click="closeHash">关闭</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>
