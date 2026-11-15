<script setup lang="ts">
/**
 * 环境管理窗口：环境变量 / 软件目录 / 系统包 / 安装源 / 操作记录五个页签。
 *
 * 包管理的写操作走一条流水线：预检 → 确认 → WebSocket 执行 → 自动刷新，
 * 预检把求解器的完整事务摆给用户看，冲突时禁止执行。
 *
 * 环境变量是另一条：它没有"求解"这一步（改一行变量没有依赖关系），也不需要
 * 长时间执行 —— 走同步 POST，把"写到哪个文件、复核过没过"的结论直接带回来。
 * 两者共用的只有「操作记录」与回滚入口。
 */
import { computed, h, ref, watch } from 'vue';
import {
  NAlert,
  NButton,
  NDataTable,
  NEmpty,
  NInput,
  NModal,
  NRadioButton,
  NRadioGroup,
  NSelect,
  NSpace,
  NSpin,
  NTabPane,
  NTabs,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import EnvPlanDialog from './EnvPlanDialog.vue';
import EnvRunDialog from './EnvRunDialog.vue';
import EnvOpDialog from './EnvOpDialog.vue';
import {
  ApiError,
  api,
  type EnvCatalogItem,
  type EnvOp,
  type EnvOverview,
  type EnvPackage,
  type EnvPlan,
  type EnvRunRequest,
  type EnvSourceFile,
  type EnvUpgrade,
  type EnvVar,
  type EnvVarsView,
  type ScanDir,
  type ScanView,
} from '../api/client';
import { formatTime } from '../api/format';
import { ENV_CATEGORY_ORDER, ENV_CATEGORY_TEXT } from '../envCatalog';

const message = useMessage();
const dialog = useDialog();

const hostId = ref<number | null>(null);
const hostName = ref('');
const tab = ref('catalog');

const overview = ref<EnvOverview | null>(null);
const overviewLoading = ref(false);
const overviewError = ref('');

const catalogQuery = ref('');

// ---- 环境变量 ----
const varsView = ref<EnvVarsView | null>(null);
const varsLoading = ref(false);
const varsLoaded = ref(false);
const varQuery = ref('');
/** 筛选项；'ineffective' 是"文件里定义了但当前会话没有"，不是作用域。 */
const varFilter = ref<'all' | 'system' | 'user' | 'session' | 'ineffective'>('all');
const varEditShow = ref(false);
const varSaving = ref(false);
/** 编辑中的变量名；为 null 表示这是一次新增。 */
const varEditing = ref<string | null>(null);
const varForm = ref<{ name: string; value: string; scope: 'user' | 'system'; file: string }>({
  name: '',
  value: '',
  scope: 'user',
  file: '',
});

// ---- 环境扫描 ----
const scanShow = ref(false);
const scanLoading = ref(false);
const scanError = ref('');
const scanView = ref<ScanView | null>(null);
// 「加入 PATH」的二次确认：作用域得在这里选，而遮住现有命令的风险也要在这里说清 ——
// 那一句话是用户唯一能反悔的机会。
const pathAddShow = ref(false);
const pathAddDir = ref<ScanDir | null>(null);
const pathAddScope = ref<'user' | 'system'>('user');
const pathAddSaving = ref(false);

const packages = ref<EnvPackage[]>([]);
const upgrades = ref<EnvUpgrade[]>([]);
const pkgMode = ref<'installed' | 'upgrades'>('installed');
const pkgQuery = ref('');
const packagesLoading = ref(false);
const packagesLoaded = ref(false);
const truncated = ref(false);

const sources = ref<EnvSourceFile[]>([]);
const mirrorOptions = ref<{ label: string; value: string }[]>([]);
const mirrorId = ref('tuna');
const sourcesLoading = ref(false);
const sourcesLoaded = ref(false);

const ops = ref<EnvOp[]>([]);
const opsLoading = ref(false);
const opsLoaded = ref(false);

// 预检 → 执行 对话框状态
const planShow = ref(false);
const planLoading = ref(false);
const plan = ref<EnvPlan | null>(null);
let pendingRequest: EnvRunRequest | null = null;
const runShow = ref(false);
const runRequest = ref<EnvRunRequest | null>(null);

// 目录项版本选择 / 任意包安装
const recipePickShow = ref(false);
const pickedItem = ref<EnvCatalogItem | null>(null);
const pickedVersion = ref<string | null>(null);
const customVersion = ref('');
const pkgInstallShow = ref(false);
const inputPkgName = ref('');
const inputPkgVersion = ref('');

// 操作详情
const opShow = ref(false);
const opDetailId = ref<number | null>(null);

const familyText: Record<string, string> = {
  apt: 'APT（Debian/Ubuntu）',
  dnf: 'DNF（RHEL 8+/Fedora/Rocky/Alma）',
  yum: 'YUM（CentOS 7）',
  apk: 'APK（Alpine）',
};

const actionText: Record<string, string> = {
  install: '安装',
  upgrade: '升级',
  remove: '卸载',
  'upgrade-all': '全部升级',
  mirror: '换源',
  rollback: '回滚',
  'env-set': '设置变量',
  'env-unset': '删除变量',
  'env-restore': '还原变量',
};

const canPrivilege = computed(() => (overview.value?.privilegeMode ?? 'none') !== 'none');

/** 提权方式在画像行上的展示。 */
const privilegeText: Record<string, string> = {
  root: 'root',
  su: 'su（提权口令）',
  'sudo-nopasswd': 'sudo（免密）',
  'sudo-password': 'sudo（登录口令）',
};

const privilegeTag = computed<{ type: 'success' | 'warning' | 'info' | 'error'; text: string }>(() => {
  const mode = overview.value?.privilegeMode ?? 'none';
  const text = privilegeText[mode];
  if (!text) return { type: 'error', text: '无特权' };
  return { type: mode === 'root' ? 'success' : 'info', text };
});

interface CatalogGroup {
  category: string;
  title: string;
  /** 当前过滤条件下的条目数与其中已安装的条目数，用于分组标题的统计。 */
  total: number;
  installed: number;
  items: EnvCatalogItem[];
}

/**
 * 目录按分类分组。过滤同时匹配名称与描述；被过滤空的分组不会出现在结果里。
 */
const catalogGroups = computed<CatalogGroup[]>(() => {
  const q = catalogQuery.value.trim().toLowerCase();
  const groups = new Map<string, CatalogGroup>();
  for (const item of overview.value?.catalog ?? []) {
    if (q && !`${item.name} ${item.description}`.toLowerCase().includes(q)) continue;
    let group = groups.get(item.category);
    if (!group) {
      group = {
        category: item.category,
        title: ENV_CATEGORY_TEXT[item.category] ?? item.category,
        total: 0,
        installed: 0,
        items: [],
      };
      groups.set(item.category, group);
    }
    group.items.push(item);
    group.total += 1;
    if (item.installed) group.installed += 1;
  }
  const rank = (category: string): number => {
    const i = ENV_CATEGORY_ORDER.indexOf(category);
    return i < 0 ? ENV_CATEGORY_ORDER.length : i;
  };
  return [...groups.values()].sort((a, b) => rank(a.category) - rank(b.category));
});

const filteredPackages = computed(() => {
  const q = pkgQuery.value.trim().toLowerCase();
  if (!q) return packages.value;
  return packages.value.filter((p) => p.name.toLowerCase().includes(q));
});

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  return fallback;
}

// ---- 环境变量 ----

const varScopeText: Record<EnvVar['scope'], string> = { system: '系统', user: '用户', session: '会话' };
const varScopeType: Record<EnvVar['scope'], 'warning' | 'success' | 'default'> = {
  system: 'warning',
  user: 'success',
  session: 'default',
};

const filteredVars = computed<EnvVar[]>(() => {
  const q = varQuery.value.trim().toLowerCase();
  return (varsView.value?.vars ?? []).filter((v) => {
    if (varFilter.value === 'ineffective') {
      if (v.effective) return false;
    } else if (varFilter.value !== 'all' && v.scope !== varFilter.value) {
      return false;
    }
    return !q || `${v.name} ${v.value}`.toLowerCase().includes(q);
  });
});

/** 这个变量是不是由面板可写的文件定义的 —— 只有这种才谈得上"删除"。 */
function isPanelManaged(v: EnvVar): boolean {
  const view = varsView.value;
  if (!view || !v.file) return false;
  if (v.scope === 'system') return v.file === view.systemFile;
  return view.userFiles.some((name) => v.file === `${view.home}/${name}`);
}

/**
 * 用户级可选文件在界面上的标签：后端给的是家目录相对名（.profile），
 * 下拉里直接显示相对名比显示一整条绝对路径更好认，同时也是写给哪个文件的原文。
 */
function userFileOptions(): { label: string; value: string }[] {
  const view = varsView.value;
  if (!view) return [];
  return view.userFiles.map((name) => ({ label: name, value: `${view.home}/${name}` }));
}

async function loadVars(): Promise<void> {
  if (hostId.value === null) return;
  varsLoading.value = true;
  try {
    varsView.value = await api.env.vars(hostId.value);
    varsLoaded.value = true;
  } catch (err) {
    message.error(describe(err, '读取环境变量失败'));
  } finally {
    varsLoading.value = false;
  }
}

function openVarCreate(): void {
  const view = varsView.value;
  varEditing.value = null;
  varForm.value = {
    name: '',
    value: '',
    scope: 'user',
    file: view ? `${view.home}/${view.userFiles[0] ?? '.profile'}` : '',
  };
  varEditShow.value = true;
}

/**
 * 切换作用域时把落地文件跟着修正：两个作用域的可写文件完全不重叠，
 * 沿用上一个作用域的路径只会在提交时被后端白名单拒掉（或者更糟：改错地方）。
 */
function switchVarScope(scope: 'user' | 'system'): void {
  varForm.value.scope = scope;
  const view = varsView.value;
  if (!view) return;
  if (scope === 'system') {
    varForm.value.file = view.systemFile;
    return;
  }
  const valid = view.userFiles.some((name) => varForm.value.file === `${view.home}/${name}`);
  if (!valid) varForm.value.file = `${view.home}/${view.userFiles[0] ?? '.profile'}`;
}

function openVarEdit(v: EnvVar): void {
  const view = varsView.value;
  // 编辑时作用域与落地文件都取该变量的实际来源：用户在列表里点的是"这一条"，
  // 弹窗里却默认成另一个文件，改完落到别处，是最容易被忽略的那类事故。
  // 来自别处的变量（/etc/environment、别人的 profile.d）不可写，只能覆盖 —— 因此
  // 沿用它的作用域，落到面板自己的文件里。
  const scope: 'user' | 'system' = v.scope === 'system' ? 'system' : 'user';
  let file = '';
  if (scope === 'user') file = isPanelManaged(v) && v.file ? v.file : `${view?.home ?? ''}/${view?.userFiles[0] ?? '.profile'}`;
  else file = view?.systemFile ?? '';
  varEditing.value = v.name;
  varForm.value = { name: v.name, value: v.value, scope, file };
  varEditShow.value = true;
}

async function submitVar(): Promise<void> {
  if (hostId.value === null) return;
  const name = varForm.value.name.trim();
  if (!name) {
    message.warning('请填写变量名');
    return;
  }
  varSaving.value = true;
  try {
    const res = await api.env.setVar(hostId.value, {
      name,
      value: varForm.value.value,
      scope: varForm.value.scope,
      file: varForm.value.file || undefined,
    });
    varEditShow.value = false;
    // 复核没过不能包装成成功：它意味着"文件写了，但登录会话里读到的不是这个值"，
    // 用户必须知道，否则下一次登录发现变量没生效时会以为是面板没写进去。
    if (res.verified) message.success(res.summary);
    else message.warning(`${res.summary}${res.note ? `：${res.note}` : ''}`);
    await reloadVars();
    await loadOps();
  } catch (err) {
    message.error(describe(err, '设置环境变量失败'));
  } finally {
    varSaving.value = false;
  }
}

function removeVar(v: EnvVar): void {
  const id = hostId.value;
  if (id === null || !v.file) return;
  // 系统级要 root，用户级不用 —— 与后端 varWrapProfile 的判据一致。
  if (v.scope === 'system' && !canPrivilege.value) {
    message.warning('该主机无法提权，不能改动系统级变量');
    return;
  }
  const scope: 'user' | 'system' = v.scope === 'system' ? 'system' : 'user';
  const file = v.file;
  dialog.warning({
    title: `删除变量 ${v.name}？`,
    content: `只会从 ${file} 里删掉它的定义。如果它在别的文件里也定义了，删完仍然会生效 —— 面板不会去改那些文件。`,
    positiveText: '继续',
    onPositiveClick: async () => {
      try {
        const res = await api.env.unsetVar(id, { name: v.name, scope });
        if (res.verified) message.success(res.summary);
        else message.warning(`${res.summary}${res.note ? `：${res.note}` : ''}`);
        await reloadVars();
        await loadOps();
      } catch (err) {
        message.error(describe(err, '删除环境变量失败'));
      }
    },
  });
}

/** 改完变量后刷新列表：采集本身要起目标机的登录 shell，必须重置加载态。 */
async function reloadVars(): Promise<void> {
  varsLoaded.value = false;
  varsView.value = null;
  await loadVars();
}

// ---- 环境扫描 ----

/**
 * 扫描结果的排序主张：先把"没进 PATH 的"排前面。
 *
 * 这个面板回答的是"我装的东西为什么用不到"，已经在 PATH 里的那些只是背景信息 ——
 * 按路径字母序排会让真正要看的那几行散在中间。
 */
const scanDirs = computed<ScanDir[]>(() => {
  const dirs = scanView.value?.dirs ?? [];
  return [...dirs].sort((a, b) => {
    if (a.inPath !== b.inPath) return a.inPath ? 1 : -1;
    return a.path.localeCompare(b.path);
  });
});

const scanMissing = computed(() => scanDirs.value.filter((d) => !d.inPath).length);

async function openScan(): Promise<void> {
  scanShow.value = true;
  await loadScan();
}

async function loadScan(): Promise<void> {
  const id = hostId.value;
  if (id === null) return;
  scanLoading.value = true;
  scanError.value = '';
  scanView.value = null;
  try {
    scanView.value = await api.env.scanEnv(id);
  } catch (err) {
    scanError.value = describe(err, '扫描环境失败');
  } finally {
    scanLoading.value = false;
  }
}

/**
 * 默认作用域取"用户级"：它不需要提权，改动范围也只到这个人自己的登录环境，
 * 是这台机器上一定能做成的那件事。要影响全机由用户自己在这里改。
 */
function askAddPath(dir: ScanDir): void {
  pathAddDir.value = dir;
  pathAddScope.value = 'user';
  pathAddShow.value = true;
}

async function submitPathAdd(): Promise<void> {
  const id = hostId.value;
  const dir = pathAddDir.value;
  if (id === null || !dir) return;
  if (pathAddScope.value === 'system' && !canPrivilege.value) {
    message.warning('该主机无法提权，不能改系统级变量');
    return;
  }
  pathAddSaving.value = true;
  try {
    const res = await api.env.addPath(id, { dir: dir.path, scope: pathAddScope.value });
    pathAddShow.value = false;
    if (res.verified) message.success(res.summary);
    else message.warning(`${res.summary}${res.note ? `：${res.note}` : ''}`);
    // 重新扫一遍而不是就地改状态：加完之后"这个目录里的命令解析到哪"已经变了，
    // 重扫拿回来的才是当前事实（也能立刻看出它是不是真生效了）。
    await loadScan();
    await reloadVars();
    await loadOps();
  } catch (err) {
    message.error(describe(err, '加入 PATH 失败'));
  } finally {
    pathAddSaving.value = false;
  }
}

async function loadOverview(): Promise<void> {
  if (hostId.value === null) return;
  overviewLoading.value = true;
  overviewError.value = '';
  overview.value = null;
  // 主机切换后所有 tab 缓存作废。
  packagesLoaded.value = false;
  sourcesLoaded.value = false;
  opsLoaded.value = false;
  varsLoaded.value = false;
  varsView.value = null;
  try {
    overview.value = await api.env.overview(hostId.value);
  } catch (err) {
    overviewError.value = describe(err, '读取系统环境信息失败');
  } finally {
    overviewLoading.value = false;
  }
}

watch(hostId, () => {
  if (hostId.value !== null) void loadOverview();
});

async function onTabChange(name: string): Promise<void> {
  tab.value = name;
  if (hostId.value === null) return;
  if (name === 'vars' && !varsLoaded.value) await loadVars();
  if (name === 'packages' && !packagesLoaded.value) await loadPackages();
  if (name === 'sources' && !sourcesLoaded.value) await loadSources();
  if (name === 'ops' || (name === 'packages' && packagesLoaded.value)) {
    // 切回操作记录时刷新一次，拿最新状态。
  }
  if (name === 'ops') await loadOps();
}

async function loadPackages(): Promise<void> {
  if (hostId.value === null) return;
  packagesLoading.value = true;
  try {
    if (pkgMode.value === 'upgrades') {
      const res = await api.env.upgrades(hostId.value);
      upgrades.value = res.upgrades;
    } else {
      const res = await api.env.packages(hostId.value);
      packages.value = res.packages;
      truncated.value = res.truncated;
    }
    packagesLoaded.value = true;
  } catch (err) {
    message.error(describe(err, '读取软件包列表失败'));
  } finally {
    packagesLoading.value = false;
  }
}

async function switchPkgMode(mode: 'installed' | 'upgrades'): Promise<void> {
  pkgMode.value = mode;
  await loadPackages();
}

async function loadSources(): Promise<void> {
  if (hostId.value === null) return;
  sourcesLoading.value = true;
  try {
    const res = await api.env.sources(hostId.value);
    sources.value = res.files;
    mirrorOptions.value = res.mirrors.map((m) => ({ label: m.name, value: m.id }));
    sourcesLoaded.value = true;
  } catch (err) {
    message.error(describe(err, '读取安装源配置失败'));
  } finally {
    sourcesLoading.value = false;
  }
}

async function loadOps(): Promise<void> {
  if (hostId.value === null) return;
  opsLoading.value = true;
  try {
    const res = await api.env.ops(hostId.value);
    ops.value = res.ops;
    opsLoaded.value = true;
  } catch (err) {
    message.error(describe(err, '读取操作记录失败'));
  } finally {
    opsLoading.value = false;
  }
}

// ---- 变更流水线 ----

async function submitPlan(req: EnvRunRequest): Promise<void> {
  if (hostId.value === null) return;
  plan.value = null;
  planShow.value = true;
  planLoading.value = true;
  try {
    plan.value = await api.env.plan(hostId.value, req);
    pendingRequest = req;
  } catch (err) {
    planShow.value = false;
    message.error(describe(err, '变更预检失败'));
  } finally {
    planLoading.value = false;
  }
}

function confirmPlan(): void {
  if (!pendingRequest) return;
  runRequest.value = pendingRequest;
  planShow.value = false;
  runShow.value = true;
}

async function afterRun(): Promise<void> {
  runShow.value = false;
  packagesLoaded.value = false;
  sourcesLoaded.value = false;
  await loadOverview();
  if (tab.value === 'packages') await loadPackages();
  if (tab.value === 'sources') await loadSources();
  await loadOps();
}

/**
 * 顶栏"刷新"：概览刷新会作废所有页签缓存（系统画像可能已经变了），
 * 因此当前页签必须跟着重新拉一次 —— 只刷概览的话，用户点了刷新反而看到
 * 自己的页签变空。
 */
async function refreshCurrent(): Promise<void> {
  await loadOverview();
  await onTabChange(tab.value);
}

// ---- 目录软件 ----

/** 已安装项：有可选版本时是"换版本"，否则只是重装。 */
function catalogActionText(item: EnvCatalogItem): string {
  if (!item.installed) return '安装';
  return item.versions?.length ? '安装其他版本' : '重新安装';
}

function clickRecipe(item: EnvCatalogItem): void {
  if (!canPrivilege.value) {
    message.warning('该主机无法提权，不能变更环境（需要 root 登录，或目标机有可用的 sudo/su）');
    return;
  }
  if (!item.supported) {
    message.warning(`${item.name} 不支持该系统家族`);
    return;
  }
  if (item.versions && item.versions.length > 0) {
    pickedItem.value = item;
    pickedVersion.value = item.versions[0]?.value ?? null;
    customVersion.value = '';
    recipePickShow.value = true;
    return;
  }
  void submitPlan({ action: 'install', recipeSlug: item.slug });
}

function confirmRecipePick(): void {
  const item = pickedItem.value;
  if (!item) return;
  // Go 允许手填精确版本（在线列表拉取失败时的兜底）；其他目录项只能选给定大版本。
  const version =
    item.slug === 'golang' ? customVersion.value.trim() || pickedVersion.value || '' : pickedVersion.value || '';
  recipePickShow.value = false;
  void submitPlan({ action: 'install', recipeSlug: item.slug, recipeVersion: version || undefined });
}

// ---- 系统包 ----

function clickInstallPkg(): void {
  inputPkgName.value = '';
  inputPkgVersion.value = '';
  pkgInstallShow.value = true;
}

function confirmInstallPkg(): void {
  const name = inputPkgName.value.trim();
  if (!name) {
    message.warning('请填写软件包名');
    return;
  }
  // 允许一次填多个包（空格/逗号分隔），版本约束只对单包输入生效。
  const names = name.split(/[\s,]+/).filter(Boolean);
  const version = inputPkgVersion.value.trim();
  if (names.length > 1 && version) {
    message.warning('多个软件包时不能统一指定版本，请逐个安装');
    return;
  }
  pkgInstallShow.value = false;
  void submitPlan({
    action: 'install',
    specs: names.map((n) => ({ name: n, version: names.length === 1 ? version || undefined : undefined })),
  });
}

function upgradePkg(row: EnvUpgrade): void {
  void submitPlan({ action: 'upgrade', specs: [{ name: row.name }] });
}

function removePkg(row: EnvPackage): void {
  dialog.warning({
    title: `卸载 ${row.name}？`,
    content: '系统将先进行依赖求解并展示完整变更（含可能被连带卸载的包），确认后才会执行。',
    positiveText: '继续',
    onPositiveClick: () => void submitPlan({ action: 'remove', specs: [{ name: row.name }] }),
  });
}

function upgradeAll(): void {
  dialog.warning({
    title: '升级全部软件包？',
    content: '将先展示求解器的完整升级事务（含可能的连带卸载），确认后才会执行。',
    positiveText: '预检',
    onPositiveClick: () => void submitPlan({ action: 'upgrade-all' }),
  });
}

// ---- 安装源 ----

function switchMirror(): void {
  const mirror = mirrorOptions.value.find((m) => m.value === mirrorId.value);
  dialog.warning({
    title: `切换到${mirror?.label ?? ''}镜像源？`,
    content: '当前源配置会先备份到快照，刷新索引失败会自动还原；事后可在操作记录里一键回滚。',
    positiveText: '预检',
    onPositiveClick: () => void submitPlan({ action: 'mirror', mirror: mirrorId.value }),
  });
}

// ---- 操作记录 ----

function openOp(row: EnvOp): void {
  opDetailId.value = row.id;
  opShow.value = true;
}

function rollbackOp(op: EnvOp): void {
  opShow.value = false;
  // 环境变量的回滚是"把这个文件还原成那次操作之前的样子"，走的是另一条同步接口：
  // 它没有包管理器事务，也走不了 WebSocket 那条按动作分发的路由（见后端 validEnvAction）。
  if (op.action.startsWith('env-')) {
    restoreVarOp(op);
    return;
  }
  void submitPlan({ action: 'rollback', originOpId: op.id });
}

function restoreVarOp(op: EnvOp): void {
  dialog.warning({
    title: '还原这次变量变更？',
    content: `将把 ${op.target} 所在的文件还原成这次操作之前的内容，并核对文件内容与备份是否一致。`,
    positiveText: '还原',
    onPositiveClick: async () => {
      try {
        const res = await api.env.restoreVars(op.hostId, op.id);
        if (res.verified) message.success(res.summary);
        else message.warning(`${res.summary}${res.note ? `：${res.note}` : ''}`);
        if (op.hostId === hostId.value) await reloadVars();
        await loadOps();
      } catch (err) {
        message.error(describe(err, '还原变量变更失败'));
      }
    },
  });
}

const OP_STATUS_TYPE: Record<EnvOp['status'], 'info' | 'success' | 'error' | 'warning'> = {
  running: 'info',
  success: 'success',
  failed: 'error',
  interrupted: 'warning',
};
function opStatusType(status: EnvOp['status']) {
  return OP_STATUS_TYPE[status];
}
function opStatusLabel(status: EnvOp['status']): string {
  return { running: '执行中', success: '成功', failed: '失败', interrupted: '已中断' }[status];
}

// ---- 表格定义 ----

const varColumns = [
  {
    title: '变量名',
    key: 'name',
    width: 200,
    render: (row: EnvVar) => h('span', { class: 'var-name' }, row.name),
  },
  {
    title: '值',
    key: 'value',
    ellipsis: { tooltip: true },
    render: (row: EnvVar) =>
      h('span', { class: 'var-value' }, [
        h('span', { class: 'var-value__text' }, row.value),
        // "文件里写了、当前登录会话里却没有"是最容易被误判成配好了的情形，
        // 必须与生效值在视觉上分开，而不是靠备注那行小字。
        row.effective ? null : h(NTag, { size: 'tiny', type: 'warning' }, { default: () => '未生效' }),
      ]),
  },
  {
    title: '作用域',
    key: 'scope',
    width: 82,
    render: (row: EnvVar) =>
      h(NTag, { size: 'small', type: varScopeType[row.scope] }, { default: () => varScopeText[row.scope] }),
  },
  {
    title: '来源',
    key: 'file',
    width: 250,
    ellipsis: { tooltip: true },
    render: (row: EnvVar) =>
      row.file
        ? h('span', { class: 'var-src', title: row.raw ?? row.file }, `${row.file}:${row.line}`)
        : h('span', { class: 'var-src var-src--none' }, '会话注入，无配置文件来源'),
  },
  {
    title: '备注',
    key: 'note',
    ellipsis: { tooltip: true },
    render: (row: EnvVar) => (row.note ? h('span', { class: 'var-note' }, row.note) : ''),
  },
  {
    title: '',
    key: 'actions',
    width: 128,
    align: 'right' as const,
    render: (row: EnvVar) => {
      // PATH 这一行不给"编辑 / 删除"：写它会把当前的 PATH 冻结成字面量，删它会连
      // 用户写在 if 判断里的累加行一起删掉（后端 ValidateVarWritable 也会拒）。
      // 用户对 PATH 真正想做的事是"往里加目录"，所以直接把他送去扫描页。
      if (row.name === 'PATH') {
        return h('span', { class: 'var-ops' }, [
          h(NButton, { size: 'tiny', quaternary: true, onClick: () => void openScan() }, { default: () => '加入目录' }),
        ]);
      }
      return h('span', { class: 'var-ops' }, [
        h(NButton, { size: 'tiny', quaternary: true, onClick: () => openVarEdit(row) }, { default: () => '编辑' }),
        // 只有面板可写的文件才谈得上删除：别处的定义（/etc/environment、
        // 别人的 profile.d）面板不去动，给一个删不掉的按钮是误导。
        isPanelManaged(row)
          ? h(
              NButton,
              {
                size: 'tiny',
                quaternary: true,
                type: 'error',
                disabled: row.scope === 'system' && !canPrivilege.value,
                onClick: () => removeVar(row),
              },
              { default: () => '删除' },
            )
          : null,
      ]);
    },
  },
];

/** 扫描结果里的命令预览：列出前几个，多的用总数带过。 */
function commandPreview(dir: ScanDir): string {
  if (dir.denied) return '没有读取权限，列不出内容';
  if (dir.commands.length === 0) return '目录里没有可执行文件';
  const names = dir.commands.map((c) => c.name);
  const head = names.slice(0, 6).join(' ');
  return names.length > 6 ? `${head} 等 ${dir.total} 个` : `${head}（共 ${dir.total} 个）`;
}

const scanColumns = [
  {
    title: '目录',
    key: 'path',
    ellipsis: { tooltip: true },
    render: (row: ScanDir) => h('span', { class: 'var-src' }, row.path),
  },
  {
    title: '来源',
    key: 'source',
    width: 110,
    render: (row: ScanDir) => h(NTag, { size: 'small' }, { default: () => row.label }),
  },
  {
    title: '里面的命令',
    key: 'commands',
    render: (row: ScanDir) => {
      const parts = [h('div', { class: 'scan__cmds' }, commandPreview(row))];
      // 抢名字的风险必须与"命令清单"挨着显示：用户看的是"这里面有 node"，
      // 而要提醒的正是"它同时会顶掉系统的 python"。
      if (row.shadows?.length) {
        parts.push(
          h(
            'div',
            { class: 'scan__shadow' },
            `加入后会遮住同名的 ${row.shadows.join('、')} —— 它们现在来自 PATH 里的其它目录`,
          ),
        );
      }
      return h('div', null, parts);
    },
  },
  {
    title: 'PATH',
    key: 'inPath',
    width: 96,
    render: (row: ScanDir) =>
      row.inPath
        ? h(NTag, { size: 'small', type: 'success' }, { default: () => '已经在' })
        : h(NTag, { size: 'small', type: 'warning' }, { default: () => '不在' }),
  },
  {
    title: '',
    key: 'actions',
    width: 110,
    align: 'right' as const,
    render: (row: ScanDir) =>
      row.inPath || row.denied
        ? null
        : h(
            NButton,
            { size: 'tiny', quaternary: true, type: 'primary', onClick: () => askAddPath(row) },
            { default: () => '加入 PATH' },
          ),
  },
];

const pkgColumns = [
  { title: '软件包', key: 'name', ellipsis: { tooltip: true } },
  { title: '版本', key: 'version', ellipsis: { tooltip: true } },
  { title: '架构', key: 'arch', width: 100 },
  {
    title: '操作',
    key: 'actions',
    width: 90,
    align: 'right' as const,
    render: (row: EnvPackage) =>
      h(
        NButton,
        { size: 'tiny', quaternary: true, type: 'error', disabled: !canPrivilege.value, onClick: () => removePkg(row) },
        { default: () => '卸载' },
      ),
  },
];

const upgradeColumns = [
  { title: '软件包', key: 'name', ellipsis: { tooltip: true } },
  { title: '当前版本', key: 'current', ellipsis: { tooltip: true } },
  { title: '候选版本', key: 'candidate', ellipsis: { tooltip: true } },
  { title: '来源', key: 'repo', width: 130, ellipsis: { tooltip: true } },
  {
    title: '操作',
    key: 'actions',
    width: 90,
    align: 'right' as const,
    render: (row: EnvUpgrade) =>
      h(
        NButton,
        { size: 'tiny', quaternary: true, type: 'primary', disabled: !canPrivilege.value, onClick: () => upgradePkg(row) },
        { default: () => '升级' },
      ),
  },
];

const opsColumns = [
  {
    title: '时间',
    key: 'createdAt',
    width: 150,
    render: (row: EnvOp) => formatTime(row.createdAt),
  },
  { title: '主机', key: 'hostName', width: 110, ellipsis: { tooltip: true } },
  {
    title: '动作',
    key: 'action',
    width: 76,
    render: (row: EnvOp) => h(NTag, { size: 'small' }, { default: () => actionText[row.action] ?? row.action }),
  },
  { title: '目标', key: 'target', ellipsis: { tooltip: true } },
  {
    title: '状态',
    key: 'status',
    width: 150,
    render: (row: EnvOp) =>
      h('span', { class: 'ops-status' }, [
        h(NTag, { size: 'small', type: opStatusType(row.status) }, { default: () => opStatusLabel(row.status) }),
        row.compensated ? h(NTag, { size: 'small', type: 'warning' }, { default: () => '已补偿' }) : null,
        row.verified ? h(NTag, { size: 'small', type: 'success' }, { default: () => '已复核' }) : null,
      ]),
  },
  {
    title: '',
    key: 'open',
    width: 70,
    align: 'right' as const,
    render: (row: EnvOp) =>
      h(NButton, { size: 'tiny', quaternary: true, onClick: () => openOp(row) }, { default: () => '详情' }),
  },
];

</script>

<template>
  <div class="env">
    <div class="env__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">ENVIRONMENT</p>
        <span class="page-title">软件环境</span>
      </div>
      <span class="env__spacer" />
      <n-button size="small" quaternary :loading="overviewLoading" @click="refreshCurrent">刷新</n-button>
      <!-- 主机胶囊放页头右端：它和标题回答的是同一个问题（现在看的是哪台机器）。 -->
      <HostPicker v-model:host-id="hostId" v-model:host-name="hostName" include-local />
    </div>

    <n-spin :show="overviewLoading">
      <n-alert
        v-if="overviewError"
        type="error"
        :title="overviewError"
        class="env__alert"
      />

      <template v-else-if="overview">
        <div class="env__profile">
          <span class="env__os">{{ overview.profile.prettyName || overview.profile.os }}</span>
          <n-tag size="small" type="info">{{ familyText[overview.profile.family] ?? overview.profile.family }}</n-tag>
          <n-tag size="small">{{ overview.profile.arch }}</n-tag>
          <n-tag size="small" :type="privilegeTag.type">
            {{ privilegeTag.text }}
          </n-tag>
        </div>
        <n-alert
          v-if="!canPrivilege"
          type="warning"
          class="env__alert"
          title="该主机当前为只读模式：需要 root 登录，或目标机能提权（su 需要主机配置里的提权口令；sudo 需要这个账号能免密，或能凭登录口令通过密码认证）"
        />

        <n-tabs v-model:value="tab" type="line" class="env__tabs" @update:value="onTabChange">
          <!-- 环境变量 -->
          <n-tab-pane name="vars" tab="环境变量">
            <div class="pkgs__bar">
              <n-input
                v-model:value="varQuery"
                size="small"
                placeholder="过滤变量名或值"
                class="pkgs__search"
                clearable
              />
              <n-radio-group v-model:value="varFilter" size="small">
                <n-radio-button value="all">全部</n-radio-button>
                <n-radio-button value="system">系统</n-radio-button>
                <n-radio-button value="user">用户</n-radio-button>
                <n-radio-button value="session">会话</n-radio-button>
                <n-radio-button value="ineffective">未生效</n-radio-button>
              </n-radio-group>
              <span class="pkgs__spacer" />
              <n-button size="small" quaternary :loading="varsLoading" @click="loadVars">刷新</n-button>
              <!-- 扫描：回答"这台机器上还装着哪些当前 shell 用不到的环境" -->
              <n-button size="small" quaternary @click="openScan">
                <template #icon>
                  <svg class="btn-glyph" viewBox="0 0 48 48" aria-hidden="true">
                    <use href="#glyph-scan" />
                  </svg>
                </template>
                扫描环境
              </n-button>
              <n-button size="small" type="primary" @click="openVarCreate">新增变量</n-button>
            </div>

            <div v-if="varsView" class="var__meta">
              <span>登录 shell：{{ varsView.shell || '未知' }}</span>
              <span>家目录：{{ varsView.home || '未知' }}</span>
              <span>共 {{ varsView.vars.length }} 个变量</span>
            </div>
            <n-alert
              v-if="varsView?.truncated"
              type="warning"
              class="env__alert"
              title="有配置文件超过体积上限或本轮读取配额用尽，内容没取回来 —— 下面这些变量的来源推断可能不完整"
            />

            <n-spin :show="varsLoading">
              <n-data-table
                :columns="varColumns"
                :data="filteredVars"
                :row-key="(row: EnvVar) => row.name"
                size="small"
                :max-height="480"
              />
              <n-empty
                v-if="!varsLoading && varsView && filteredVars.length === 0"
                description="没有匹配的变量"
                class="pkgs__empty"
              />
            </n-spin>
          </n-tab-pane>

          <!-- 软件目录 -->
          <n-tab-pane name="catalog" tab="软件目录">
            <div class="pkgs__bar">
              <n-input
                v-model:value="catalogQuery"
                size="small"
                placeholder="过滤运行环境与工具链"
                class="pkgs__search"
                clearable
              />
            </div>
            <!-- 分组由数据驱动：分类的标题、数量和条目都来自 overview.catalog。 -->
            <template v-for="group in catalogGroups" :key="group.category">
              <div class="catalog__group-title">
                {{ group.title }} · {{ group.total }} 项 · 已装 {{ group.installed }}
              </div>
              <div class="catalog__grid">
                <div v-for="item in group.items" :key="item.slug" class="cat" :class="{ 'cat--off': !item.supported }">
                  <div class="cat__head">
                    <span class="cat__name">{{ item.name }}</span>
                    <n-tag v-if="!item.supported" size="tiny">不支持</n-tag>
                    <n-tag v-else-if="item.installed" size="tiny" type="success">已装 {{ item.version }}</n-tag>
                    <n-tag v-else size="tiny" type="warning">未安装</n-tag>
                  </div>
                  <div class="cat__desc" :title="item.description">{{ item.description }}</div>
                  <div v-if="!item.installed && item.versions?.length" class="cat__meta">
                    可选版本 {{ item.versions.length }} 个
                  </div>
                  <div v-if="item.note" class="cat__note">{{ item.note }}</div>
                  <!-- 在线版本列表没取到时说明原因，用户仍可在弹窗里手输版本。 -->
                  <div v-if="item.versionError" class="cat__note">{{ item.versionError }}</div>
                  <div class="cat__foot">
                    <n-button
                      size="tiny"
                      type="primary"
                      :disabled="!item.supported || !canPrivilege"
                      @click="clickRecipe(item)"
                    >
                      {{ catalogActionText(item) }}
                    </n-button>
                  </div>
                </div>
              </div>
            </template>
            <n-empty v-if="catalogGroups.length === 0" description="没有匹配的目录项" class="catalog__empty" />
          </n-tab-pane>

          <!-- 系统包 -->
          <n-tab-pane name="packages" tab="系统包">
            <div class="pkgs__bar">
              <n-radio-group :value="pkgMode" size="small" @update:value="switchPkgMode">
                <n-radio-button value="installed">已安装</n-radio-button>
                <n-radio-button value="upgrades">可升级（{{ upgrades.length }}）</n-radio-button>
              </n-radio-group>
              <n-input
                v-if="pkgMode === 'installed'"
                v-model:value="pkgQuery"
                size="small"
                placeholder="过滤软件包名"
                class="pkgs__search"
                clearable
              />
              <span class="pkgs__spacer" />
              <n-button size="small" quaternary :loading="packagesLoading" @click="loadPackages">刷新</n-button>
              <n-button
                v-if="pkgMode === 'upgrades'"
                size="small"
                type="primary"
                :disabled="!canPrivilege || upgrades.length === 0"
                @click="upgradeAll"
              >
                全部升级（{{ upgrades.length }}）
              </n-button>
              <n-button
                v-else
                size="small"
                type="primary"
                :disabled="!canPrivilege"
                @click="clickInstallPkg"
              >
                安装软件包
              </n-button>
            </div>
            <div v-if="truncated && pkgMode === 'installed'" class="pkgs__hint">
              包数量超过上限，仅显示前 5000 个，可用搜索缩小范围
            </div>
            <n-spin :show="packagesLoading">
              <n-data-table
                v-if="pkgMode === 'installed'"
                :columns="pkgColumns"
                :data="filteredPackages"
                :row-key="(row: EnvPackage) => row.name"
                size="small"
                :max-height="480"
              />
              <n-data-table
                v-else
                :columns="upgradeColumns"
                :data="upgrades"
                :row-key="(row: EnvUpgrade) => row.name"
                size="small"
                :max-height="480"
              />
              <n-empty
                v-if="!packagesLoading && pkgMode === 'upgrades' && upgrades.length === 0"
                description="没有可升级的包（可能需要先刷新索引，升级动作会自动刷新）"
                class="pkgs__empty"
              />
            </n-spin>
          </n-tab-pane>

          <!-- 安装源 -->
          <n-tab-pane name="sources" tab="安装源">
            <div class="src__bar">
              <span class="src__label">镜像站一键切换：</span>
              <n-select
                v-model:value="mirrorId"
                :options="mirrorOptions"
                size="small"
                class="src__select"
              />
              <n-button size="small" type="primary" :disabled="!canPrivilege" @click="switchMirror">
                切换并刷新索引
              </n-button>
            </div>
            <n-spin :show="sourcesLoading">
              <div v-for="file in sources" :key="file.path" class="src__file">
                <div class="src__path">{{ file.path }}</div>
                <pre class="src__content">{{ file.content }}</pre>
              </div>
              <n-empty v-if="!sourcesLoading && sources.length === 0" description="没有读到源配置文件" />
            </n-spin>
          </n-tab-pane>

          <!-- 操作记录 -->
          <n-tab-pane name="ops" tab="操作记录">
            <div class="pkgs__bar">
              <span class="src__label">本机的环境变更历史（含步骤级日志与回滚）</span>
              <span class="pkgs__spacer" />
              <n-button size="small" quaternary :loading="opsLoading" @click="loadOps">刷新</n-button>
            </div>
            <n-spin :show="opsLoading">
              <n-data-table
                :columns="opsColumns"
                :data="ops"
                :row-key="(row: EnvOp) => row.id"
                size="small"
                :max-height="500"
              />
            </n-spin>
          </n-tab-pane>
        </n-tabs>
      </template>

      <n-empty v-else-if="!overviewLoading && hostId === null" description="请先选择一台目标主机（含本机）" class="env__empty" />
    </n-spin>

    <!-- 目录软件版本选择 -->
    <n-modal v-model:show="recipePickShow" preset="card" :title="`安装 ${pickedItem?.name ?? ''}`" style="width: 460px">
      <div class="pick">
        <div v-if="pickedItem?.version" class="pick__current">当前版本：{{ pickedItem.version }}</div>
        <n-select
          v-if="pickedItem?.versions?.length"
          v-model:value="pickedVersion"
          :options="pickedItem.versions"
          placeholder="选择版本"
        />
        <n-input
          v-if="pickedItem?.slug === 'golang'"
          v-model:value="customVersion"
          placeholder="或手动输入精确版本，如 1.26.1"
          class="pick__custom"
        />
        <div v-if="pickedItem?.versionError" class="pick__note">{{ pickedItem.versionError }}</div>
      </div>
      <template #footer>
        <n-space justify="end">
          <n-button @click="recipePickShow = false">取消</n-button>
          <n-button
            type="primary"
            :disabled="!pickedVersion && !customVersion.trim()"
            @click="confirmRecipePick"
          >
            下一步（预检）
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 任意包安装 -->
    <n-modal v-model:show="pkgInstallShow" preset="card" title="安装软件包" style="width: 460px">
      <div class="pick">
        <n-input v-model:value="inputPkgName" placeholder="软件包名（多个用空格或逗号分隔）" />
        <n-input
          v-model:value="inputPkgVersion"
          placeholder="版本约束（可选，apt 用 =1.2.3，留空取候选版本）"
          class="pick__custom"
        />
      </div>
      <template #footer>
        <n-space justify="end">
          <n-button @click="pkgInstallShow = false">取消</n-button>
          <n-button type="primary" :disabled="!inputPkgName.trim()" @click="confirmInstallPkg">下一步（预检）</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 环境变量：新增 / 编辑 -->
    <n-modal
      v-model:show="varEditShow"
      preset="card"
      :title="varEditing ? `编辑 ${varEditing}` : '新增环境变量'"
      style="width: 540px"
    >
      <div class="pick">
        <n-input
          v-model:value="varForm.name"
          :disabled="!!varEditing"
          placeholder="变量名，如 HTTP_PROXY"
        />
        <n-input
          v-model:value="varForm.value"
          type="textarea"
          :rows="3"
          placeholder="变量值（留空即设为空值）"
        />
        <div class="pick__note">
          值会原样写进文件：$ 与反引号不会被展开，写成 $PATH:/opt 就是这几个字符本身。
        </div>
        <n-radio-group :value="varForm.scope" size="small" @update:value="switchVarScope">
          <n-radio-button value="user">用户级（只对这个登录用户生效）</n-radio-button>
          <n-radio-button value="system">系统级（所有用户生效）</n-radio-button>
        </n-radio-group>
        <n-select
          v-if="varForm.scope === 'user'"
          v-model:value="varForm.file"
          :options="userFileOptions()"
          placeholder="写入哪个文件"
        />
        <div v-else class="pick__note">
          系统级固定写入 {{ varsView?.systemFile }} —— 面板自己的文件，不去改别人的配置。
        </div>
        <div v-if="varForm.scope === 'system' && !canPrivilege" class="pick__note">
          该主机无法提权，改不了系统级变量。
        </div>
      </div>
      <template #footer>
        <n-space justify="end">
          <n-button @click="varEditShow = false">取消</n-button>
          <n-button
            type="primary"
            :loading="varSaving"
            :disabled="!varForm.name.trim() || (varForm.scope === 'system' && !canPrivilege)"
            @click="submitVar"
          >
            保存
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 环境扫描结果 -->
    <n-modal
      v-model:show="scanShow"
      preset="card"
      title="扫描环境"
      style="width: 900px"
      :bordered="false"
    >
      <n-spin :show="scanLoading">
        <div class="scan__hint">
          扫描 /var/apps、/opt、/usr/local 下的应用目录与家目录里的几个常见位置，
          看这台机器上还有哪些命令行环境是"装好了、但当前登录 shell 用不到"的。
        </div>
        <n-alert v-if="scanError" type="error" class="env__alert" :title="scanError" />
        <template v-else-if="scanView">
          <div class="var__meta">
            <span>家目录：{{ scanView.home || '未知' }}</span>
            <span>登录 PATH 里已有 {{ scanView.pathDirs.length }} 个目录</span>
            <span v-if="scanMissing > 0">发现 {{ scanDirs.length }} 个环境，其中 {{ scanMissing }} 个不在 PATH 里</span>
            <span v-else>发现 {{ scanDirs.length }} 个环境，都已在 PATH 里</span>
          </div>
          <n-data-table
            :columns="scanColumns"
            :data="scanDirs"
            :row-key="(row: ScanDir) => row.path"
            size="small"
            :max-height="440"
          />
          <n-empty
            v-if="scanDirs.length === 0"
            description="这些约定位置里没有找到命令行环境"
            class="pkgs__empty"
          />
        </template>
      </n-spin>
      <template #footer>
        <n-space justify="end">
          <n-button :loading="scanLoading" @click="loadScan">重新扫描</n-button>
          <n-button @click="scanShow = false">关闭</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 加入 PATH 确认 -->
    <n-modal
      v-model:show="pathAddShow"
      preset="card"
      title="把这个目录加入 PATH"
      style="width: 560px"
    >
      <div class="pick">
        <div class="scan__path">{{ pathAddDir?.path }}</div>
        <div class="pick__note">
          面板会写下 <span class="var-src">export PATH="目录:…:$PATH"</span>，
          由目标机自己展开 PATH —— 不是把当前的 PATH 抄下来存死，所以以后装的工具照样能用。
          同一个文件里重复执行只会保留一份，不会越加越长。
        </div>
        <n-alert
          v-if="pathAddDir?.shadows?.length"
          type="warning"
          title="这个目录排在 PATH 最前，会遮住同名的现有命令"
        >
          {{ pathAddDir.shadows.join('、') }} 现在来自 PATH 里的其它目录，加进去之后会用这个目录里的版本。
        </n-alert>
        <n-radio-group v-model:value="pathAddScope" size="small">
          <n-radio-button value="user">用户级（只对这个登录用户生效）</n-radio-button>
          <n-radio-button value="system">系统级（所有用户生效）</n-radio-button>
        </n-radio-group>
        <div v-if="pathAddScope === 'user'" class="pick__note">
          写入 {{ varsView?.home }}/{{ varsView?.userFiles[0] ?? '.profile' }}，不需要提权。
        </div>
        <div v-else class="pick__note">
          写入 {{ varsView?.systemFile }}，需要这台机器能提权。
          <template v-if="!canPrivilege">这台机器目前提不了权，选不了的。</template>
        </div>
      </div>
      <template #footer>
        <n-space justify="end">
          <n-button @click="pathAddShow = false">取消</n-button>
          <n-button
            type="primary"
            :loading="pathAddSaving"
            :disabled="pathAddScope === 'system' && !canPrivilege"
            @click="submitPathAdd"
          >
            加入
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 预检 / 执行 / 详情 -->
    <EnvPlanDialog v-model:show="planShow" :plan="plan" :loading="planLoading" @confirm="confirmPlan" />
    <EnvRunDialog
      v-model:show="runShow"
      :host-id="hostId ?? 0"
      :host-name="hostName"
      :request="runRequest"
      @done="afterRun"
    />
    <EnvOpDialog v-model:show="opShow" :op-id="opDetailId" @rollback="rollbackOp" />
  </div>
</template>

<style scoped>
.env {
  display: flex;
  flex-direction: column;
  height: 100%;
  padding: 12px 14px 14px;
  box-sizing: border-box;
  gap: 8px;
}
/* n-spin 会在 .env 与 .env__tabs 之间插进 container / content 两层 DOM，而
   flex:1 只对直接子项生效 —— 不把这两层接进高度链，.env__tabs 就退化成按内容
   撑高，下面 .n-tab-pane 的 height:100% 随之失去参照，overflow-y:auto 永远
   不触发（现象：滚动条不出现，超出窗口的部分被 .win__body 直接裁掉）。 */
.env > :deep(.n-spin-container) {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
}
.env > :deep(.n-spin-container > .n-spin-content) {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
}
.env__bar {
  display: flex;
  align-items: center;
  gap: 10px;
}
.env__spacer {
  flex: 1;
}
.env__profile {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.env__os {
  font-weight: 600;
  font-size: 13px;
}
.env__alert {
  margin: 4px 0;
}
.env__tabs {
  flex: 1;
  min-height: 0;
  /* 窗口主体是 overflow:hidden（见 global.css 的 .win__body），滚动得由窗口自己
     实现 —— 页签不做滚动容器的话，软件目录这类长列表会被直接裁掉，用户既看不到
     也滚不到（这一条踩过：目录条目从 6 项涨到 20 多项后，下面的条目全部不可达）。 */
  display: flex;
  flex-direction: column;
}
.env__tabs :deep(.n-tabs-pane-wrapper) {
  flex: 1;
  min-height: 0;
}
.env__tabs :deep(.n-tab-pane) {
  height: 100%;
  overflow-y: auto;
  padding-top: 8px;
  /* 留出滚动条的位置，别让它压住右侧卡片的边框。 */
  padding-right: 6px;
}
.env__empty {
  margin-top: 80px;
}
.catalog__group-title {
  font-size: 12px;
  font-weight: 600;
  color: var(--text-2, #8b919b);
  margin: 8px 0;
}
.catalog__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(250px, 1fr));
  gap: 10px;
  margin-bottom: 16px;
}
.catalog__empty {
  margin-top: 60px;
}
.cat {
  border: 1px solid var(--border-1, rgba(255, 255, 255, 0.1));
  border-radius: 6px;
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 6px;
  background: rgba(255, 255, 255, 0.02);
}
.cat--off {
  opacity: 0.55;
}
.cat__head {
  display: flex;
  align-items: center;
  gap: 8px;
}
.cat__name {
  font-weight: 600;
  font-size: 14px;
}
.cat__desc {
  font-size: 12px;
  color: var(--text-2, #8b919b);
  line-height: 1.5;
  /* 描述长短不一，截断到两行才能让同一行的卡片高度齐平。 */
  display: -webkit-box;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  overflow: hidden;
}
.cat__meta {
  font-size: 11px;
  color: var(--text-2, #8b919b);
}
.cat__note {
  font-size: 11px;
  color: var(--warning-color, #ffab2e);
}
.cat__foot {
  display: flex;
  justify-content: flex-end;
  /* 卡片高度由网格行拉平，按钮贴底才不会因附注行数不同而参差。 */
  margin-top: auto;
}
.pkgs__bar,
.src__bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 10px;
}
.pkgs__search {
  width: 220px;
}
.pkgs__spacer {
  flex: 1;
}
.pkgs__hint {
  font-size: 12px;
  color: var(--warning-color, #ffab2e);
  margin-bottom: 6px;
}
.pkgs__empty {
  margin-top: 60px;
}
.src__label {
  font-size: 13px;
  color: var(--text-2, #b9bec6);
}
.src__select {
  width: 200px;
}
.src__file {
  margin-bottom: 10px;
}
.src__path {
  font-size: 12px;
  font-family: var(--font-mono, monospace);
  color: var(--text-2, #8b919b);
  margin-bottom: 4px;
}
.src__content {
  margin: 0;
  padding: 8px 10px;
  background: rgba(0, 0, 0, 0.3);
  border-radius: 6px;
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  line-height: 1.5;
  max-height: 180px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}
.pick {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.pick__current {
  font-size: 12px;
  color: var(--text-2, #8b919b);
}
.pick__custom {
  margin-top: 4px;
}
.pick__note {
  font-size: 12px;
  color: var(--warning-color, #ffab2e);
}
.var__meta {
  display: flex;
  gap: 16px;
  font-size: 12px;
  color: var(--text-2, #8b919b);
  margin-bottom: 8px;
}
.var-name {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
}
.var-value {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  max-width: 100%;
}
.var-value__text {
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.var-src {
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  color: var(--text-2, #8b919b);
}
.var-src--none {
  font-family: inherit;
  font-style: italic;
}
.var-note {
  font-size: 11px;
  color: var(--warning-color, #ffab2e);
}
.var-ops {
  display: inline-flex;
  gap: 2px;
}
.btn-glyph {
  width: 14px;
  height: 14px;
}
.scan__hint {
  font-size: 12px;
  color: var(--text-2, #8b919b);
  margin-bottom: 8px;
}
.scan__cmds {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.scan__shadow {
  font-size: 11px;
  color: var(--warning-color, #ffab2e);
}
.scan__path {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  word-break: break-all;
}
.ops-status {
  display: inline-flex;
  gap: 4px;
}
</style>
