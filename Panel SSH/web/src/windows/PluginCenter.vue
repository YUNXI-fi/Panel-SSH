<script setup lang="ts">
import { computed, h, inject, onMounted, ref, watch } from 'vue';
import {
  NButton,
  NDropdown,
  NInput,
  NModal,
  NSpace,
  NSwitch,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui';

import { ApiError, api, type AICLIApp, type AICLIPlatform, type BuiltinPlugin, type EnvCatalogItem, type EnvRunRequest, type PluginInfo, type PrivilegeMode, type StoreAppEntry } from '../api/client';
import HostPicker from '../components/HostPicker.vue';
import DshLogo from '../components/icons/DshLogo.vue';
import { WindowManagerKey } from '../desktop/useWindowManager';
import { usePlugins } from '../desktop/usePlugins';
import { useLaunchApps } from '../desktop/useLaunchApps';
import { ENV_CATEGORY_TEXT } from '../envCatalog';
import { securityGlyph } from '../securityApps';
import EnvRunDialog from './EnvRunDialog.vue';

/**
 * DSH 工作台内置插件的标识。
 *
 * 前端只需要它判断一件事：装完之后要不要回填 dsh 的启动命令。
 * 入口地址不在这里拼 —— 后端按主机算好（见 internal/api/plugins.go 的
 * builtinEntry），前端因此不必知道任何插件的路径长什么样。
 */
const DSH_ORG = 'panelssh';
const DSH_NAME = 'dsh-workspace';

/** dsh 的安装配方。它内部会把 Node.js 一起带上，因此只需跑这一次。 */
const DSH_PROVISION_RECIPE = 'dsh';

/**
 * 预检清单：先有 Node.js，才有 dsh 本身。
 *
 * 顺序与安装顺序一致，用户看到的缺件清单因此跟随后面真正执行的事情对得上。
 */
const DSH_PROVISION_RECIPES = ['nodejs', 'dsh'];

/** 配方 slug 到人话名称的对照，只用于安装流程里的文案。 */
const RECIPE_LABELS: Record<string, string> = {
  nodejs: 'Node.js',
  dsh: 'DeepSeek Harness',
};

/**
 * 插件图标。
 *
 * 插件包目前不强制携带图标，因此这里只对已知内置插件给专用图形，
 * 其余（包括后续没有提供图标的三方插件）统一回退到默认拼图图标。
 * 若将来插件清单增加 icon 字段，只需在这个函数里优先返回该字段即可。
 */
function pluginGlyph(item: { org: string; name: string }): string {
  if (item.org === DSH_ORG && item.name === DSH_NAME) return 'terminal';
  return 'plugin-default';
}

/**
 * AI CLI 与开发工具的品牌图标。
 *
 * 只收录精灵表里拿得到官方单色标的工具；没有的（Pi、OmO、Codex 这类官方没发布
 * 单色 mark 的）回退到通用终端图标 —— 宁可统一，也不要一块空白格子。
 */
const AI_GLYPHS: Record<string, string> = {
  claude: 'brand-claude',
  gemini: 'brand-gemini',
  qwen: 'brand-qwen',
  opencode: 'brand-opencode',
  copilot: 'brand-copilot',
  cline: 'brand-cline',
  codebuddy: 'brand-codebuddy',
  atomcode: 'brand-atomgit',
  pm2: 'brand-pm2',
  nodemon: 'brand-nodemon',
  wrangler: 'brand-cloudflare',
  firebase: 'brand-firebase',
  vercel: 'brand-vercel',
  netlify: 'brand-netlify',
  railway: 'brand-railway',
  pnpm: 'brand-pnpm',
  tsx: 'brand-typescript',
  prettier: 'brand-prettier',
  eslint: 'brand-eslint',
  nest: 'brand-nestjs',
  mongosh: 'brand-mongodb',
  'mcp-inspector': 'brand-mcp',
};

function aiGlyph(item: { id: string }): string {
  return AI_GLYPHS[item.id] ?? 'terminal';
}

const message = useMessage();
const dialog = useDialog();
const wm = inject(WindowManagerKey);

// 与桌面图标共用同一份插件状态，装卸后两边同步变化。
const { builtins, capabilities, refresh, onHost } = usePlugins();
// 安全应用装好后要立刻出现在桌面，这里只负责触发刷新。
const { refresh: refreshLaunchApps } = useLaunchApps();

/**
 * 当前选中的目标主机。
 *
 * 插件是「每台主机一份」的：同一份包装到不同机器上，各有自己的版本、
 * 授权与启停状态。因此整个界面都要挂在这个选择上 —— 列表、内置插件的
 * "已安装"判定、以及装/卸/授权/启停这些动作，全部针对选中的那一台。
 *
 * includeLocal 打开：宿主机（hostId=0）和远端主机一样能装插件，
 * 它只是省掉了 SSH 那一段。
 */
const hostId = ref<number | null>(null);

/** 选中主机的显示名，用于提示语与二次确认，避免用户看错目标机。 */
const hostName = ref('');

/** 选中主机上的安装记录，表格与计数都看它。 */
const hostPlugins = computed(() => (hostId.value === null ? [] : onHost(hostId.value)));

/**
 * 内置插件目录与它在选中主机上的安装记录配对。
 *
 * 目录是面板自带的（与主机无关），安装记录才是按主机切片的；配对之后
 * 一张卡片就能同时表达两件事：还没装（给安装按钮）和已装（给开关、打开、更多）。
 */
const builtinRows = computed(() =>
  builtins.value.map((item) => ({
    item,
    installed:
      hostPlugins.value.find((row) => row.org === item.org && row.name === item.name) ?? null,
  })),
);

/** 已安装页签只汇总，不重复列出目录；这里只用来区分来源与排序。 */
const thirdParty = computed(() => hostPlugins.value.filter((item) => item.source !== 'embedded'));

const loading = ref(false);
const installing = ref(false);
const busyBuiltin = ref('');
const fileInput = ref<HTMLInputElement | null>(null);

/** 应用中心的六类内容：面板插件、AI CLI、开发工具、系统组件、网络安全、应用仓库。 */
const tab = ref<'plugins' | 'aicli' | 'dev' | 'system' | 'security' | 'store'>('plugins');

const aiApps = ref<AICLIApp[]>([]);
// 目标机平台：工具"装不了"时要能说清是本机不在它的支持范围内，而不是按钮莫名置灰。
const aiPlatform = ref<AICLIPlatform>({ os: '', arch: '' });
const aiLoading = ref(false);
const aiBusy = ref('');
const aiOutputShow = ref(false);
const aiOutputTitle = ref('');
const aiOutput = ref('');

/**
 * 页头搜索：只过滤界面上已有的数据，不再多跑一次目标机命令。
 * 匹配名称、包名/来源、描述 —— 用户记得哪一个都能搜到。
 */
const query = ref('');

function matchesQuery(...fields: string[]): boolean {
  const q = query.value.trim().toLowerCase();
  if (q === '') return true;
  return fields.some((field) => field.toLowerCase().includes(q));
}

/**
 * 当前页签要展示的工具清单。
 *
 * 后端一次探测把两个分类都算出来了（一台机器只需要一次 SSH），这里只做切片，
 * 切页签因此不会再跑一遍目标机命令。
 */
const visibleApps = computed(() =>
  aiApps.value
    .filter((item) => item.group === (tab.value === 'dev' ? 'dev' : 'ai'))
    .filter((item) => matchesQuery(item.name, item.pkg, item.note ?? '')),
);

/**
 * AI 工具页签里的内置插件卡片。
 *
 * 和工具卡片放在同一个网格里 —— 否则内置区固定在顶部、工具列表自己滚动，
 * 滚动条还会让两段的列宽对不齐。开发工具页签不带内置插件。
 */
const aiBuiltinRows = computed(() =>
  (tab.value === 'aicli' ? builtinRows.value : []).filter((row) =>
    matchesQuery(row.item.displayName, row.item.description),
  ),
);

/**
 * 已安装总览里的一条。
 *
 * 四类来源的卡片长得一样，但底部能做的事不同（插件是启停/打开，npm 工具是升级/卸载，
 * 系统组件是重装），因此把"来源"和"原始对象"一起带在条目里，模板据此分支。
 */
interface InstalledEntry {
  key: string;
  kind: 'plugin' | 'ai' | 'dev' | 'system';
  name: string;
  subtitle: string;
  desc: string;
  glyph: string;
  dsh?: boolean;
  plugin?: PluginInfo;
  app?: AICLIApp;
  env?: EnvCatalogItem;
}

/** 已安装页签的分组：只列真正装上了的东西，目录页签才列全部。 */
const installedGroups = computed<{ key: string; title: string; items: InstalledEntry[] }[]>(() => {
  const pluginItems: InstalledEntry[] = [];
  for (const row of builtinRows.value) {
    if (!row.installed) continue;
    pluginItems.push({
      key: `plugin:${row.item.org}/${row.item.name}`,
      kind: 'plugin',
      name: row.item.displayName,
      subtitle: `${row.item.org}/${row.item.name} · v${row.item.version}`,
      desc: row.item.description,
      glyph: pluginGlyph(row.item),
      dsh: row.item.org === DSH_ORG && row.item.name === DSH_NAME,
      plugin: row.installed,
    });
  }
  for (const item of thirdParty.value) {
    pluginItems.push({
      key: `plugin:${item.id}`,
      kind: 'plugin',
      name: item.displayName,
      subtitle: `${item.org}/${item.name} · v${item.version}`,
      desc: item.description || '—',
      glyph: pluginGlyph(item),
      plugin: item,
    });
  }

  const appItems = (group: 'ai' | 'dev'): InstalledEntry[] =>
    aiApps.value
      .filter((app) => app.group === group && app.installed)
      .map((app) => ({
        key: `app:${app.id}`,
        kind: group,
        name: app.name,
        subtitle: `${app.pkg} · ${app.version || '已安装'}`,
        desc: app.path,
        glyph: aiGlyph(app),
        app,
      }));

  const systemItems: InstalledEntry[] = envCatalog.value
    .filter((item) => item.installed)
    .map((item) => ({
      key: `sys:${item.slug}`,
      kind: 'system',
      name: item.name,
      subtitle: `${ENV_CATEGORY_TEXT[item.category] ?? item.category} · ${item.version || '已安装'}`,
      desc: item.description,
      glyph: 'env',
      env: item,
    }));

  return [
    { key: 'plugin', title: '插件', items: pluginItems },
    { key: 'ai', title: 'AI 工具', items: appItems('ai') },
    { key: 'dev', title: '开发工具', items: appItems('dev') },
    { key: 'system', title: '系统组件', items: systemItems },
  ]
    .map((group) => ({
      ...group,
      items: group.items.filter((entry) =>
        matchesQuery(entry.name, entry.subtitle, entry.desc),
      ),
    }))
    .filter((group) => group.items.length > 0);
});

/** 系统组件页签的列表：与已安装总览一样，搜索只做本地过滤。 */
const visibleEnv = computed(() =>
  envCatalog.value.filter((item) =>
    matchesQuery(item.name, item.description, ENV_CATEGORY_TEXT[item.category] ?? item.category),
  ),
);

/**
 * 网络安全页签的列表。
 *
 * 与「系统组件」共用同一份目录和同一条安装流水线，只是按 security 分类切片。
 * 不定制排序：后端目录里的书写顺序就是使用频率顺序（扫描与防火墙在前，
 * 审计与隐私工具在后），前端再排一次只会让两处顺序互相打架。
 */
const visibleSecurity = computed(() =>
  envCatalog.value
    .filter((item) => item.category === 'security')
    .filter((item) => matchesQuery(item.name, item.description, item.useCase ?? '', item.note ?? '')),
);

const installedCount = computed(() =>
  installedGroups.value.reduce((sum, group) => sum + group.items.length, 0),
);

const pageEyebrow = computed(() => {
  if (tab.value === 'plugins') return 'APPLICATIONS';
  if (tab.value === 'dev') return 'DEVELOPMENT';
  if (tab.value === 'system') return 'SYSTEM';
  if (tab.value === 'security') return 'SECURITY';
  if (tab.value === 'store') return 'APP STORE';
  return 'AI CLI';
});

const pageTitle = computed(() => {
  if (tab.value === 'plugins') return '应用中心';
  if (tab.value === 'dev') return '开发工具';
  if (tab.value === 'system') return '系统组件';
  if (tab.value === 'security') return '网络安全';
  if (tab.value === 'store') return '应用仓库';
  return 'AI 工具';
});

// 系统组件页签的数据：来自软件环境的同一份目录（探测脚本也已合并成一次 SSH 往返）。
const envCatalog = ref<EnvCatalogItem[]>([]);
const envPrivilege = ref<PrivilegeMode>('');
const envLoading = ref(false);
const sysRunShow = ref(false);
const sysRunRequest = ref<EnvRunRequest | null>(null);

// 网络安全页签的详情弹窗。应用中心是窗口式界面，没有路由栈，
// 详情因此沿用现有的弹窗形态，而不是为它单开一个页面。
const securityDetail = ref<EnvCatalogItem | null>(null);
const securityDetailShow = ref(false);

// 已安装总览要同时看三份数据：插件、npm 工具、系统组件。
// 记下每份是按哪台主机取的，切页签时就不必反复跑目标机命令。
const aiLoadedFor = ref<number | null>(null);
const envLoadedFor = ref<number | null>(null);

/** 已安装页签需要的数据，按主机懒加载。 */
function ensureOverview(): void {
  const target = hostId.value;
  if (target === null) return;
  if (aiLoadedFor.value !== target) void loadAI();
  if (envLoadedFor.value !== target) void loadEnv();
}

// 环境补装的执行弹窗。dsh 与它依赖的 Node.js 都可能不在，补装走环境模块
// 那条已经验证过的流水线（提权、快照、逐步执行、补偿、复核都在那边），
// 而不是在这里另写一套执行逻辑。
const dshRunShow = ref(false);
const dshRunRequest = ref<EnvRunRequest | null>(null);

const showGrant = ref(false);
const grantTarget = ref<PluginInfo | null>(null);
const grantSelection = ref<string[]>([]);
const saving = ref(false);

/**
 * 内置插件的「设置」弹窗。
 *
 * 内置插件有配置页，三项操作（设置页 / 授权 / 卸载）都挂在它下面；
 * 用弹窗而不是下拉菜单，是因为这三项里有两项会再弹一个确认框，
 * 菜单和对话框层层叠着容易点错。
 */
const showSettings = ref(false);
const settingsTarget = ref<PluginInfo | null>(null);

function openSettings(item: PluginInfo): void {
  settingsTarget.value = item;
  showSettings.value = true;
}

/** 设置弹窗里的动作：先关弹窗，再做各自的事。 */
function onSettingsAction(key: 'config' | 'grant' | 'uninstall'): void {
  const target = settingsTarget.value;
  showSettings.value = false;
  if (!target) return;
  if (key === 'config') openConfig(target);
  else if (key === 'grant') openGrant(target);
  else askUninstall(target);
}

const capabilityLabel = computed(() => {
  const map = new Map<string, string>();
  for (const item of capabilities.value) map.set(item.name, item.label);
  return map;
});

const capabilityRisk = computed(() => {
  const map = new Map<string, string>();
  for (const item of capabilities.value) map.set(item.name, item.risk);
  return map;
});

/**
 * 授权列表的展示顺序。
 *
 * 用面板给的能力清单顺序（风险升序），而不是插件清单里的声明顺序：
 * 后者由插件作者决定，"什么都想要"的插件完全可以靠调整顺序把高危能力排到最前。
 */
const grantCapabilities = computed(() => {
  const order = new Map(capabilities.value.map((item, index) => [item.name, index]));
  return [...(grantTarget.value?.capabilities ?? [])].sort(
    (a, b) => (order.get(a) ?? 0) - (order.get(b) ?? 0),
  );
});

function riskOf(name: string): string {
  return capabilityRisk.value.get(name) ?? '';
}

function riskLabel(name: string): string {
  const risk = riskOf(name);
  if (risk === 'high') return '高风险';
  if (risk === 'medium') return '可写';
  return '';
}

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

function labelOf(name: string): string {
  return capabilityLabel.value.get(name) ?? name;
}

async function load(): Promise<void> {
  loading.value = true;
  try {
    await refresh();
  } catch (err) {
    message.error(describe(err, '加载插件列表失败'));
  } finally {
    loading.value = false;
  }
}

/** 读取选中主机上的 AI CLI 安装状态。 */
async function loadAI(): Promise<void> {
  const target = hostId.value;
  if (target === null) return;
  aiLoading.value = true;
  try {
    const res = await api.aiCLI.list(target);
    aiApps.value = res.apps;
    aiPlatform.value = res.platform;
    aiLoadedFor.value = target;
  } catch (err) {
    message.error(describe(err, '读取 AI CLI 状态失败'));
  } finally {
    aiLoading.value = false;
  }
}

/** 读取选中主机上的系统组件目录与安装状态（与软件环境同一份数据）。 */
async function loadEnv(): Promise<void> {
  const target = hostId.value;
  if (target === null) return;
  envLoading.value = true;
  try {
    const res = await api.env.overview(target);
    envCatalog.value = res.catalog;
    envPrivilege.value = res.privilegeMode;
    envLoadedFor.value = target;
    // 目录探测会同步后端的可打开应用记录，这里顺手刷新桌面共用的那份列表。
    void refreshLaunchApps().catch(() => {
      // 桌面图标刷新失败不影响目录本身，下一次安装或打开应用中心会再同步。
    });
  } catch (err) {
    message.error(describe(err, '读取系统组件失败'));
  } finally {
    envLoading.value = false;
  }
}

// ---------------- 应用仓库 ----------------

const storeApps = ref<StoreAppEntry[]>([]);
const storeLoading = ref(false);
const storeBusy = ref('');
const storeSource = ref('');
const storeFetchedAt = ref('');
const storeCached = ref(false);
const storeLoadedFor = ref<number | null>(null);

async function loadStore(force = false): Promise<void> {
  const target = hostId.value;
  if (target === null) return;
  storeLoading.value = true;
  try {
    const res = await api.store.list(target, force);
    storeApps.value = res.apps;
    storeSource.value = res.source;
    storeFetchedAt.value = res.fetchedAt;
    storeCached.value = res.cached;
    storeLoadedFor.value = target;
  } catch (err) {
    message.error(describe(err, '读取应用仓库失败'));
  } finally {
    storeLoading.value = false;
  }
}

const visibleStoreApps = computed(() =>
  storeApps.value.filter((app) => matchesQuery(app.name, app.slug, app.description)),
);

const installedStoreCount = computed(
  () => storeApps.value.filter((app) => app.onHost).length,
);

async function installStoreApp(app: StoreAppEntry): Promise<void> {
  const target = hostId.value;
  if (target === null) {
    message.error('请先选择要安装到哪台主机');
    return;
  }
  if (storeBusy.value) return;
  storeBusy.value = app.slug;
  try {
    await api.store.install(app.slug, target);
    message.success(`${app.name} 已安装到 ${hostName.value || '本机'}`);
    await loadStore(true);
    await refreshLaunchApps();
  } catch (err) {
    message.error(describe(err, '安装应用失败'));
  } finally {
    storeBusy.value = '';
  }
}

function askStoreUninstall(app: StoreAppEntry): void {
  dialog.warning({
    title: '卸载应用？',
    content:
      `会从所有主机桌面移除「${app.name}」入口，并删除面板本地的应用包；` +
      '目标机上的软件与配置文件不受影响。',
    positiveText: '卸载',
    negativeText: '取消',
    onPositiveClick: async () => {
      storeBusy.value = app.slug;
      try {
        await api.store.uninstall(app.slug);
        message.success('应用已卸载');
        await loadStore(true);
        await refreshLaunchApps();
      } catch (err) {
        message.error(describe(err, '卸载应用失败'));
      } finally {
        storeBusy.value = '';
      }
    },
  });
}

/**
 * 安装一个系统组件。
 *
 * 不在这里另写执行逻辑：直接交给环境模块的流水线（提权、快照、逐步执行、
 * 失败补偿、事后复核都在那边），与本窗口里 DSH 的补装是同一条路。
 */
function installSystem(item: EnvCatalogItem): void {
  sysRunRequest.value = { action: 'install', recipeSlug: item.slug };
  sysRunShow.value = true;
}

function openSecurityDetail(item: EnvCatalogItem): void {
  securityDetail.value = item;
  securityDetailShow.value = true;
}

/**
 * 在 Panel SSH 里打开一个安全应用。
 *
 * 这些工具没有自己的图形界面，打开方式是在目标主机上开一个终端窗口，
 * 并自动执行一条只读状态命令；后续操作与改配置都留在同一个终端里完成。
 * 没有 launch 元数据的目录项不会走到这里 —— 它们不显示「打开」。
 */
function openSecurity(item: EnvCatalogItem): void {
  const target = hostId.value;
  if (target === null || !item.launch) return;
  wm?.open(
    'security',
    `${item.name} · ${hostName.value || '宿主机'}`,
    {
      hostId: target,
      hostName: hostName.value,
      slug: item.slug,
      glyph: securityGlyph(item.slug),
    },
    { w: 1000, h: 640 },
  );
}

/** 详情里的「打开」：先收起详情，再开终端窗口。 */
function openSecurityFromDetail(): void {
  const item = securityDetail.value;
  if (!item) return;
  securityDetailShow.value = false;
  openSecurity(item);
}

/** 详情弹窗里的安装入口：先收起详情，再交给同一条环境流水线。 */
function installSecurityDetail(): void {
  const item = securityDetail.value;
  if (!item) return;
  securityDetailShow.value = false;
  installSystem(item);
}

/** 系统组件安装流程结束：关闭弹窗并回读一次状态。 */
async function onSysDone(): Promise<void> {
  sysRunShow.value = false;
  await loadEnv();
}

/** 一键安装/升级：用户级 npm 前缀，不需要 root。 */
async function installAI(app: AICLIApp): Promise<void> {
  const target = hostId.value;
  if (target === null) {
    message.error('请先选择要安装到哪台主机');
    return;
  }
  if (aiBusy.value !== '') return;

  aiBusy.value = app.id;
  aiOutputTitle.value = `安装 ${app.name}`;
  aiOutput.value = '正在准备 Node 运行时并安装，请稍候…';
  aiOutputShow.value = true;
  try {
    const res = await api.aiCLI.install(target, app.id);
    aiOutputTitle.value = `${app.name} 安装完成`;
    aiOutput.value = res.output || '安装完成';
    message.success(`${app.name} 已安装到 ${hostName.value}`);
    await loadAI();
  } catch (err) {
    aiOutputTitle.value = `${app.name} 安装失败`;
    aiOutput.value = describe(err, '安装失败');
    message.error(describe(err, '安装失败'));
  } finally {
    aiBusy.value = '';
  }
}

/**
 * 是否是面板自己装的那一份。
 *
 * 探测会连系统安装（/usr/local/bin）和 npm 全局前缀一起认，但卸载只清
 * ~/.panelssh/ai-cli 与自己建的 ~/.local/bin 软链。不先拦住的话，
 * 点了卸载会重读一遍状态、发现还是"已安装"，看起来就是"没反应"。
 */
function isManagedAIPath(path: string): boolean {
  return path.includes('/.panelssh/ai-cli') || path.includes('/.local/bin/');
}

async function uninstallAI(app: AICLIApp): Promise<void> {
  const target = hostId.value;
  if (target === null || aiBusy.value !== '') return;

  if (isManagedAIPath(app.path)) {
    await doUninstall(app, false);
    return;
  }

  // 装在系统前缀（/usr/local、/usr/bin）里的那一份需要 root 才能删。
  // 不直接尝试：先问一句，失败时用户才知道是自己点了提权，而不是面板乱动系统文件。
  dialog.warning({
    title: '这个工具不在面板的管理目录里',
    content:
      `「${app.name}」目前在 ${app.path || '系统路径'}，属于系统级安装。` +
      `可以用提权方式卸载（需要目标机有 root 或 sudo），` +
      `也可以到终端手动执行：sudo npm uninstall -g ${app.pkg}`,
    positiveText: '提权卸载',
    negativeText: '取消',
    onPositiveClick: () => {
      void doUninstall(app, true);
    },
  });
}

/** 真正发起卸载；elevated 决定走用户级还是 root 那套脚本。 */
async function doUninstall(app: AICLIApp, elevated: boolean): Promise<void> {
  const target = hostId.value;
  if (target === null || aiBusy.value !== '') return;
  aiBusy.value = app.id;

  // 和安装一样走输出窗口：卸载失败的原因通常在命令输出里，
  // 一个几秒就消失的 toast 根本读不完。
  aiOutputTitle.value = `卸载 ${app.name}`;
  aiOutput.value = elevated ? '正在以 root 身份执行卸载…' : '正在卸载…';
  aiOutputShow.value = true;

  try {
    const res = await api.aiCLI.uninstall(target, app.id, elevated, app.path);
    aiOutputTitle.value = `${app.name} 已卸载`;
    aiOutput.value = res.output || '卸载完成';
    message.success(`已卸载 ${app.name}`);
    await loadAI();
  } catch (err) {
    const detail = describe(err, '卸载失败');
    aiOutputTitle.value = `卸载 ${app.name} 失败`;
    aiOutput.value = detail;
    message.error(detail);

    // 用户级卸载只认面板自己的目录。若探测到的包其实装在系统前缀或由发行版包提供，
    // 这里直接给出第二条路，不用让用户自己再猜一遍该点什么。
    if (!elevated && detail.includes('不在面板管理的目录')) {
      dialog.warning({
        title: '改用提权卸载？',
        content: `${detail}\n\n可以尝试以 root 身份卸载（需要目标机有 root 或 sudo）。`,
        positiveText: '提权卸载',
        negativeText: '取消',
        onPositiveClick: () => {
          void doUninstall(app, true);
        },
      });
    }
  } finally {
    aiBusy.value = '';
  }
}

/** 复制面板实际执行的安装命令，便于到终端里手动重试。 */
async function copyInstall(app: AICLIApp): Promise<void> {
  const cmd = `npm install -g --prefix ~/.panelssh/ai-cli --allow-scripts=${app.pkg} ${app.pkg}@latest`;
  try {
    await navigator.clipboard.writeText(cmd);
    message.success('安装命令已复制');
  } catch {
    message.error('复制失败，请手动选择命令');
  }
}

// 主机或页签切换时按需拉取：两类目录都要跑一次目标机命令，
// 不在插件页签下做无谓的探测。AI CLI 与开发工具共用同一份结果。
watch([hostId, tab], () => {
  if (hostId.value === null) return;
  if (tab.value === 'store') {
    if (storeLoadedFor.value !== hostId.value) void loadStore();
    return;
  }
  if (tab.value === 'plugins') {
    ensureOverview();
    return;
  }
  if (tab.value === 'system' || tab.value === 'security') {
    if (envLoadedFor.value !== hostId.value) void loadEnv();
    return;
  }
  if (aiLoadedFor.value !== hostId.value) void loadAI();
});

function pickArchive(): void {
  fileInput.value?.click();
}

async function onArchivePicked(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  input.value = ''; // 允许重复选择同一个文件
  if (!file) return;

  const target = hostId.value;
  if (target === null) {
    message.error('请先选择要安装到哪台主机');
    return;
  }

  installing.value = true;
  try {
    // 安装时不带任何授权：能力一律走"默认拒绝 + 管理员显式勾选"。
    const installed = await api.plugins.install(target, file);
    message.success(`已安装到 ${hostName.value}：${installed.displayName}，请确认要授予的能力`);
    await load();

    // 同名插件可能同时装在多台主机上，必须用 hostId 一起找这条新记录，
    // 否则会拿到别的主机那一份，把它的授权改掉。
    const fresh = hostPlugins.value.find(
      (item) => item.org === installed.org && item.name === installed.name,
    );
    if (fresh) openGrant(fresh);
  } catch (err) {
    message.error(describe(err, '安装失败'));
  } finally {
    installing.value = false;
  }
}

function openGrant(item: PluginInfo): void {
  grantTarget.value = item;
  grantSelection.value = [...item.granted];
  showGrant.value = true;
}

function selectAllCapabilities(): void {
  grantSelection.value = [...(grantTarget.value?.capabilities ?? [])];
}

function clearCapabilities(): void {
  grantSelection.value = [];
}

async function saveGrant(): Promise<void> {
  const target = grantTarget.value;
  if (!target) return;

  saving.value = true;
  try {
    await api.plugins.setCapabilities(target.hostId, target.org, target.name, grantSelection.value);
    showGrant.value = false;
    message.success('授权已更新');
    await load();
  } catch (err) {
    message.error(describe(err, '更新授权失败'));
  } finally {
    saving.value = false;
  }
}

async function toggleEnabled(item: PluginInfo, enabled: boolean): Promise<void> {
  try {
    await api.plugins.setEnabled(item.hostId, item.org, item.name, enabled);
    await load();
  } catch (err) {
    message.error(describe(err, '更新状态失败'));
    // 失败时回读一次，避免开关停在错误的位置。
    await load();
  }
}

/**
 * 装一个内置插件。
 *
 * 内置插件不是"写一条记录"那么轻：DSH 工作台要在目标机上真正跑起来，
 * 得先有 Node.js，再有 dsh 本体。因此这里是一段编排 ——
 * 确认目标主机 → 预检查缺什么 → 缺件再问一次 → 补装 → 回填启动命令 → 写记录。
 *
 * 补装那一步交给环境模块的流水线（EnvRunDialog + env 配方），不在这里另造：
 * 提权、快照、逐步执行、失败补偿、事后复核都已经被那条链路处理过了。
 */
async function installBuiltin(item: BuiltinPlugin): Promise<void> {
  const target = hostId.value;
  if (target === null) {
    message.error('请先选择要安装到哪台主机');
    return;
  }
  // 编排期间不接受第二次点击：naive-ui 的 loading 只挡视觉，不挡点击。
  if (busyBuiltin.value !== '') return;

  busyBuiltin.value = item.org + '/' + item.name;
  try {
    if (!(await confirmInstall(item))) return;

    const inspection = await inspectEnvironment(target);
    if (inspection === null) return; // 已给过中止原因

    // 拿不到 root 的机器改走用户级安装：Node 24 与 dsh 都不需要系统包。
    if (inspection.userLevel) {
      if (!(await confirmUserProvision())) return;
      message.info('正在下载 Node 24 并安装 dsh，可能需要几分钟…');
      await api.dsh.provision(target);
      await finishDshInstall(target);
      // finishDshInstall 里的探测会 SetCommand，而 SetCommand 会停掉实例；
      // 用户级安装刚拉起来的那一份因此要再启动一次。
      await api.dsh.start(target);
      return;
    }

    if (inspection.missing.length > 0) {
      if (!(await confirmProvision(inspection.missing))) return;
      // 补装交给环境流水线，收尾回到 onDshEnvDone —— 那一步会解开 busyBuiltin。
      dshRunRequest.value = { action: 'install', recipeSlug: DSH_PROVISION_RECIPE };
      dshRunShow.value = true;
      return;
    }

    await finishDshInstall(target);
  } catch (err) {
    message.error(describe(err, '安装失败'));
  } finally {
    // 执行弹窗还开着时不解锁：流程没走完，解锁会让用户以为可以再点一次。
    if (!dshRunShow.value) busyBuiltin.value = '';
  }
}

/**
 * 第一次确认：装到哪台主机。
 *
 * 目标主机是上方下拉框选出来的，看错一行就会装到另一台机器上，
 * 而安装会在那边落文件、拉包，所以要把主机名摆到用户眼前问一次。
 */
function confirmInstall(item: BuiltinPlugin): Promise<boolean> {
  return confirmDialog({
    title: '安装内置插件',
    lines: [
      `将把「${item.displayName}」安装到「${hostName.value}」。`,
      item.description,
      '安装会在目标主机上准备运行环境，可能需要联网下载。',
    ],
    positiveText: '继续',
  });
}

/**
 * 第二次确认：由面板补装缺失的环境。
 *
 * 只出一个弹窗，而不是按缺件逐项问：dsh 的安装配方本来就把 Node.js 一起带上，
 * 拆成两次询问会让用户以为要做两件事，而"同意装 Node、不同意装 dsh"这个
 * 中间态没有任何可交付的结果。
 */
function confirmProvision(missing: string[]): Promise<boolean> {
  const names = missing.map((slug) => RECIPE_LABELS[slug] ?? slug).join('、');
  return confirmDialog({
    title: '目标主机缺少运行环境',
    lines: [
      `「${hostName.value}」上还没装：${names}。`,
      '继续将由面板在这台主机上安装它们，需要联网下载，可能持续几分钟。',
      '上游当前把预发布版标为 latest，因此 dsh 装到的是 developer preview 版本。',
    ],
    positiveText: '安装',
  });
}

/** 一个只问"继续/取消"的确认框；取消的三条路径（按钮/关闭/点遮罩）都算取消。 */
function confirmDialog(options: {
  title: string;
  lines: string[];
  positiveText: string;
}): Promise<boolean> {
  return new Promise((resolve) => {
    dialog.warning({
      title: options.title,
      content: () =>
        h(
          'div',
          options.lines.map((line) => h('p', { style: 'margin:0 0 8px' }, line)),
        ),
      positiveText: options.positiveText,
      negativeText: '取消',
      onPositiveClick: () => resolve(true),
      onNegativeClick: () => resolve(false),
      onClose: () => resolve(false),
      onMaskClick: () => resolve(false),
    });
  });
}

/**
 * 预检目标机上的环境，回答"还缺什么"。
 *
 * 返回 null 表示流程应当中止（原因已经展示给用户）。
 * userLevel 为 true 表示这台机器提不了权，改用用户级安装。
 */
async function inspectEnvironment(target: number): Promise<{ missing: string[]; userLevel: boolean } | null> {
  const overview = await api.env.overview(target);

  // 补装 Node.js 要走系统包管理器，拿不到 root 就换一条不需要 root 的路：
  // 把 Node 24 与 dsh 装进用户目录（见 internal/api 的 dshProvisionScript）。
  if (overview.privilegeMode === 'none') {
    return { missing: [], userLevel: true };
  }

  const missing = DSH_PROVISION_RECIPES.filter((slug) => {
    const item = overview.catalog.find((row) => row.slug === slug);
    // 目录里没有这个条目就算"不缺"：那说明当前面板根本不认识它，
    // 报成缺件只会把用户推向一个这里装不了的东西。
    return item ? !item.installed : false;
  });
  return { missing, userLevel: false };
}

/** 用户级安装前的确认：说明装到哪、不需要 root。 */
function confirmUserProvision(): Promise<boolean> {
  return confirmDialog({
    title: '使用用户级安装',
    lines: [
      `「${hostName.value}」上拿不到 root，无法安装系统包。`,
      '将把 Node.js 24 下载到 ~/.panelssh/node，并把 dsh 装到 ~/.panelssh/dsh-runtime。',
      '全程以当前登录用户运行，不需要提权；首次安装需要联网下载，可能持续几分钟。',
    ],
    positiveText: '开始安装',
  });
}

/** 补装（或环境本来就齐）之后，把 dsh 真正接上。 */
async function onDshEnvDone(): Promise<void> {
  dshRunShow.value = false;
  const target = hostId.value;
  if (target === null) {
    busyBuiltin.value = '';
    return;
  }

  try {
    await finishDshInstall(target);
  } catch (err) {
    message.error(describe(err, '安装未完成'));
  } finally {
    busyBuiltin.value = '';
  }
}

/**
 * 收尾：回填启动命令，再写出插件记录。
 *
 * 顺序是刻意的 —— 插件记录是 /dsh/{id}/ 入口的开关，先写会出现"入口已经存在、
 * 命令却还没配"的窗口期，用户此时点开会看到一个未配置的工作台。
 * 反过来失败无副作用：只多了一条命令记录，卸载时一并清掉。
 *
 * 回填用后端探测而不是前端拼路径：dsh 装在目标机的哪里由那边的 npm 前缀决定，
 * 面板在本地猜不出来。探测成功同时回答了"装上了"与"找得到"，因此它也被当作
 * 补装成功的判据 —— 装完但定位不到，本来就没法回填。
 */
async function finishDshInstall(target: number): Promise<void> {
  await api.dsh.detectCommand(target);
  await api.plugins.installBuiltin(target, DSH_ORG, DSH_NAME);
  message.success(`已安装到 ${hostName.value}：DSH 工作台`);
  await load();
}

/**
 * 卸载是不可逆操作：第三方插件的文件会被删除，内置插件占用的后台进程会被停掉。
 * 因此走对话框二次确认，而不是一个可撤销的轻量动作。
 */
function askUninstall(item: PluginInfo): void {
  dialog.warning({
    title: '卸载插件',
    content:
      item.source === 'embedded'
        ? `确认从「${hostName.value}」卸载「${item.displayName}」？它占用的后台进程会被停掉。`
        : `确认从「${hostName.value}」卸载「${item.displayName}」？其文件会被删除，且不可恢复。`,
    positiveText: '卸载',
    negativeText: '取消',
    onPositiveClick: () => uninstall(item),
  });
}

async function uninstall(item: PluginInfo): Promise<void> {
  try {
    await api.plugins.uninstall(item.hostId, item.org, item.name);
    message.success(`已从 ${hostName.value} 卸载 ${item.displayName}`);
    await load();
  } catch (err) {
    message.error(describe(err, '卸载失败'));
  }
}

function open(item: PluginInfo): void {
  // DSH 工作台的管理器需要更宽的窗口，其余插件维持原尺寸。
  const size =
    item.org === DSH_ORG && item.name === DSH_NAME ? { w: 1180, h: 800 } : { w: 1000, h: 680 };
  wm?.open('plugin', item.displayName, { plugin: item }, size);
}

/**
 * 打开内置插件的设置页。
 *
 * 不能复用「打开」：内置插件跑起来后，它的入口路径会被自身界面接管，
 * 只带 config=1 才能进到设置页（见 PluginFrame 的 frameSrc）。
 */
function openConfig(item: PluginInfo): void {
  wm?.open('plugin', `${item.displayName} · 设置`, { plugin: item, variant: 'config' }, { w: 860, h: 620 });
}

/** 插件卡片的「更多」菜单：与原来的表格行菜单保持一致。 */
function pluginMenu(item: PluginInfo): Array<Record<string, unknown>> {
  return [
    ...(item.source === 'embedded' ? [{ label: '设置', key: 'config' }] : []),
    { label: '授权…', key: 'grant' },
    { type: 'divider', key: 'divider' },
    { label: '卸载…', key: 'uninstall' },
  ];
}

function onCardAction(key: string, item: PluginInfo): void {
  if (key === 'config') openConfig(item);
  else if (key === 'grant') openGrant(item);
  else if (key === 'uninstall') askUninstall(item);
}

onMounted(load);
</script>

<template>
  <div class="plugins">
    <!--
      页头四个页签共用一行：标题与右侧操作回答的是同一个问题（现在看的是哪台机器、能对它做什么），
      页签因此排在页头之后，作为对这一台主机的几种查看方式。
    -->
    <div class="plugins__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">{{ pageEyebrow }}</p>
        <span class="page-title">
          {{ pageTitle }}
          <span class="plugins__title page-count">
            <template v-if="tab === 'plugins'">
              {{ hostName || '本机' }}上共 {{ installedCount }} 个应用
            </template>
            <template v-else-if="tab === 'system'">
              {{ hostName || '本机' }}
              <template v-if="envCatalog.length > 0">
                · 已安装 {{ envCatalog.filter((item) => item.installed).length }} /
                {{ envCatalog.length }}
              </template>
            </template>
            <template v-else-if="tab === 'security'">
              {{ hostName || '本机' }}
              <template v-if="visibleSecurity.length > 0">
                · 已安装 {{ visibleSecurity.filter((item) => item.installed).length }} /
                {{ visibleSecurity.length }}
              </template>
            </template>
            <template v-else-if="tab === 'store'">
              {{ hostName || '本机' }} · 已添加 {{ installedStoreCount }} /
              {{ storeApps.length }}
            </template>
            <template v-else>
              {{ hostName || '本机' }}
              <template v-if="visibleApps.length > 0">
                · 已安装 {{ visibleApps.filter((item) => item.installed).length }} /
                {{ visibleApps.length }}
              </template>
            </template>
          </span>
        </span>
      </div>
      <n-space size="small">
        <!-- 搜索只过滤当前页签已有的数据，不触发目标机探测。 -->
        <n-input
          v-model:value="query"
          size="small"
          clearable
          placeholder="搜索名称、包名或说明"
          style="width: 200px"
        />
        <n-button
          v-if="tab === 'plugins'"
          size="small"
          type="primary"
          :loading="installing"
          :disabled="hostId === null"
          @click="pickArchive"
        >
          安装插件
        </n-button>
        <n-button
          v-if="tab === 'store'"
          size="small"
          :loading="storeLoading"
          :disabled="hostId === null"
          @click="loadStore(true)"
        >
          刷新仓库
        </n-button>
        <!-- 主机胶囊放页头右端：它和标题回答的是同一个问题（现在看的是哪台机器）。 -->
        <HostPicker v-model:host-id="hostId" v-model:host-name="hostName" include-local />
      </n-space>
    </div>

    <div class="plugins__tabs" role="tablist" aria-label="应用中心分类">
      <button
        class="plugins__tab"
        :class="{ 'plugins__tab--active': tab === 'plugins' }"
        role="tab"
        :aria-selected="tab === 'plugins'"
        @click="tab = 'plugins'"
      >
        已安装
      </button>
      <button
        class="plugins__tab"
        :class="{ 'plugins__tab--active': tab === 'aicli' }"
        role="tab"
        :aria-selected="tab === 'aicli'"
        @click="tab = 'aicli'"
      >
        AI 工具
      </button>
      <button
        class="plugins__tab"
        :class="{ 'plugins__tab--active': tab === 'dev' }"
        role="tab"
        :aria-selected="tab === 'dev'"
        @click="tab = 'dev'"
      >
        开发工具
      </button>
      <button
        class="plugins__tab"
        :class="{ 'plugins__tab--active': tab === 'system' }"
        role="tab"
        :aria-selected="tab === 'system'"
        @click="tab = 'system'"
      >
        系统组件
      </button>
      <button
        class="plugins__tab"
        :class="{ 'plugins__tab--active': tab === 'security' }"
        role="tab"
        :aria-selected="tab === 'security'"
        @click="tab = 'security'"
      >
        网络安全
      </button>
      <button
        class="plugins__tab"
        :class="{ 'plugins__tab--active': tab === 'store' }"
        role="tab"
        :aria-selected="tab === 'store'"
        @click="tab = 'store'"
      >
        应用仓库
      </button>
    </div>

    <template v-if="tab === 'plugins'">
      <!--
        已安装：这台主机上装了什么的汇总，按来源分组。
        上面几个页签是"能装什么"的目录，这里是"已经装了什么"，两边不混。
      -->
      <div class="plugins__scroll">
      <template v-for="group in installedGroups" :key="group.key">
        <p class="plugins__section">{{ group.title }}</p>
        <div class="plugins__builtins">
          <article v-for="entry in group.items" :key="entry.key" class="plugins__card">
            <header class="plugins__card-head">
              <span class="plugins__icon" aria-hidden="true">
                <DshLogo v-if="entry.dsh" />
                <svg v-else><use :href="`#glyph-${entry.glyph}`" /></svg>
              </span>
              <div class="plugins__card-title">
                <span class="plugins__card-name" :title="entry.name">{{ entry.name }}</span>
                <span class="row-sub">{{ entry.subtitle }}</span>
              </div>
              <n-tag
                v-if="entry.kind === 'plugin'"
                size="tiny"
                :bordered="false"
                :type="entry.plugin?.source === 'embedded' ? 'info' : 'default'"
              >
                {{ entry.plugin?.source === 'embedded' ? '内置' : '第三方' }}
              </n-tag>
            </header>

            <p class="plugins__card-desc">{{ entry.desc }}</p>

            <footer class="plugins__card-foot">
              <template v-if="entry.plugin">
                <n-switch
                  size="small"
                  :value="entry.plugin.enabled"
                  :aria-label="`启用或停用 ${entry.name}`"
                  @update:value="(value) => entry.plugin && toggleEnabled(entry.plugin, value)"
                />
                <span class="row-sub">{{ entry.plugin.enabled ? '已启用' : '已停用' }}</span>
                <span class="plugins__card-spacer" />
                <n-button
                  size="small"
                  type="primary"
                  :disabled="!entry.plugin.enabled"
                  @click="entry.plugin && open(entry.plugin)"
                >
                  打开
                </n-button>
                <!-- 内置插件有配置页，统一用「设置」弹窗；第三方插件只有授权与卸载。 -->
                <n-button
                  v-if="entry.dsh"
                  size="small"
                  :aria-label="`设置：${entry.name}`"
                  @click="entry.plugin && openSettings(entry.plugin)"
                >
                  设置
                </n-button>
                <n-dropdown
                  v-else
                  trigger="click"
                  placement="bottom-end"
                  :options="pluginMenu(entry.plugin)"
                  @select="(key) => entry.plugin && onCardAction(String(key), entry.plugin)"
                >
                  <n-button size="small" quaternary :aria-label="`更多操作：${entry.name}`">
                    更多
                  </n-button>
                </n-dropdown>
              </template>
              <template v-else-if="entry.app">
                <span class="row-sub">已安装</span>
                <span class="plugins__card-spacer" />
                <n-button size="small" type="primary" @click="installAI(entry.app)">升级</n-button>
                <n-button size="small" @click="uninstallAI(entry.app)">卸载</n-button>
              </template>
              <template v-else-if="entry.env">
                <span class="row-sub">已安装</span>
                <span class="plugins__card-spacer" />
                <n-button size="small" type="primary" @click="installSystem(entry.env)">
                  重新安装
                </n-button>
              </template>
            </footer>
          </article>
        </div>
      </template>

      <p v-if="installedGroups.length > 0 && (aiLoading || envLoading)" class="row-sub">
        正在读取工具与系统组件的安装状态…
      </p>

      <div v-if="loading || (installedGroups.length === 0 && (aiLoading || envLoading))" class="plugins__empty">
        <p class="plugins__empty-title">正在读取已安装应用…</p>
      </div>

      <div v-else-if="installedGroups.length === 0" class="plugins__empty">
        <p class="plugins__empty-title">
          {{ query ? `没有匹配「${query}」的应用` : '这台主机上还没有安装任何应用' }}
        </p>
        <template v-if="!query">
          <p class="plugins__empty-hint">
            可以到「AI 工具」「开发工具」「系统组件」页签安装，或上传第三方插件 zip 包（包内需要包含 plugin.json）。
          </p>
          <n-button
            size="small"
            type="primary"
            :loading="installing"
            :disabled="hostId === null"
            @click="pickArchive"
          >
            安装插件
          </n-button>
        </template>
      </div>
      </div>
    </template>

    <template v-else-if="tab === 'system'">
      <div v-if="envLoading && envCatalog.length === 0" class="plugins__empty">
        <p class="plugins__empty-title">正在探测系统组件…</p>
      </div>

      <div v-else-if="visibleEnv.length === 0 && query" class="plugins__empty">
        <p class="plugins__empty-title">没有匹配「{{ query }}」的系统组件</p>
      </div>

      <div v-else class="plugins__grid">
        <article v-for="item in visibleEnv" :key="item.slug" class="plugins__card">
          <header class="plugins__card-head">
            <div class="plugins__card-title">
              <span class="plugins__card-name" :title="item.name">{{ item.name }}</span>
              <span class="row-sub">
                {{ ENV_CATEGORY_TEXT[item.category] ?? item.category }}
                <template v-if="item.version"> · {{ item.version }}</template>
              </span>
            </div>
            <n-tag size="tiny" :bordered="false" :type="item.installed ? 'success' : 'default'">
              {{ item.installed ? '已安装' : '未安装' }}
            </n-tag>
          </header>

          <p class="plugins__card-desc">
            {{ item.description }}
            <span v-if="item.note"><br />{{ item.note }}</span>
          </p>

          <div v-if="!item.supported || envPrivilege === 'none'" class="plugins__card-caps">
            <n-tag v-if="!item.supported" size="tiny" :bordered="false" type="warning">
              该发行版不提供
            </n-tag>
            <n-tag v-if="envPrivilege === 'none'" size="tiny" :bordered="false" type="warning">
              目标机没有可用的提权方式
            </n-tag>
          </div>

          <footer class="plugins__card-foot">
            <span class="row-sub">{{ item.installed ? '已安装' : '未安装' }}</span>
            <span class="plugins__card-spacer" />
            <n-button
              size="small"
              type="primary"
              :disabled="hostId === null || !item.supported || envPrivilege === 'none'"
              @click="installSystem(item)"
            >
              {{ item.installed ? '重新安装' : '安装' }}
            </n-button>
          </footer>
        </article>
      </div>
    </template>

    <!--
      网络安全：与「系统组件」同源（一次探测、同一条安装流水线），
      只是按分类切片，并额外展示适用场景。点卡片进详情，安装按钮仍留在卡片上，
      常用操作不必先开一次详情。
    -->
    <template v-else-if="tab === 'security'">
      <div v-if="envLoading && envCatalog.length === 0" class="plugins__empty">
        <p class="plugins__empty-title">正在探测网络安全应用…</p>
      </div>

      <div v-else-if="visibleSecurity.length === 0" class="plugins__empty">
        <p class="plugins__empty-title">
          {{ query ? `没有匹配「${query}」的安全应用` : '这台主机上没有可用的网络安全应用' }}
        </p>
        <p v-if="!query" class="plugins__empty-hint">
          当前系统的包管理器可能不受支持；可以切换到其他主机后再试。
        </p>
      </div>

      <div v-else class="plugins__grid">
        <article
          v-for="item in visibleSecurity"
          :key="item.slug"
          class="plugins__card plugins__card--clickable"
          role="button"
          tabindex="0"
          :aria-label="`查看 ${item.name} 详情`"
          @click="openSecurityDetail(item)"
          @keydown.enter.prevent="openSecurityDetail(item)"
          @keydown.space.prevent="openSecurityDetail(item)"
        >
          <header class="plugins__card-head">
            <span class="plugins__icon plugins__icon--security" aria-hidden="true">
              <svg><use :href="`#glyph-${securityGlyph(item.slug)}`" /></svg>
            </span>
            <div class="plugins__card-title">
              <span class="plugins__card-name" :title="item.name">{{ item.name }}</span>
              <span class="row-sub">
                {{ ENV_CATEGORY_TEXT[item.category] ?? item.category }}
                <template v-if="item.version"> · {{ item.version }}</template>
              </span>
            </div>
            <n-tag size="tiny" :bordered="false" :type="item.installed ? 'success' : 'default'">
              {{ item.installed ? '已安装' : '未安装' }}
            </n-tag>
          </header>

          <p class="plugins__card-desc">{{ item.description }}</p>
          <p v-if="item.useCase" class="plugins__card-usecase">适用场景：{{ item.useCase }}</p>

          <div v-if="!item.supported || envPrivilege === 'none'" class="plugins__card-caps">
            <n-tag v-if="!item.supported" size="tiny" :bordered="false" type="warning">
              该发行版不提供
            </n-tag>
            <n-tag v-if="envPrivilege === 'none'" size="tiny" :bordered="false" type="warning">
              目标机没有可用的提权方式
            </n-tag>
          </div>

          <footer class="plugins__card-foot">
            <span class="row-sub">{{ item.installed ? '已安装' : '未安装' }}</span>
            <span class="plugins__card-spacer" />
            <n-button size="small" quaternary @click.stop="openSecurityDetail(item)">
              详情
            </n-button>
            <n-button
              v-if="item.installed && item.launch"
              size="small"
              type="primary"
              @click.stop="openSecurity(item)"
            >
              打开
            </n-button>
            <n-button
              size="small"
              :type="item.installed && item.launch ? 'default' : 'primary'"
              :disabled="hostId === null || !item.supported || envPrivilege === 'none'"
              @click.stop="installSystem(item)"
            >
              {{ item.installed ? '重新安装' : '安装' }}
            </n-button>
          </footer>
        </article>
      </div>
    </template>

    <template v-else-if="tab === 'store'">
      <div class="plugins__scroll">
        <p v-if="storeSource" class="plugins__source">
          来源 {{ storeSource }}
          <template v-if="storeFetchedAt"> · 索引生成于 {{ storeFetchedAt }}</template>
          <n-tag v-if="storeCached" size="tiny" :bordered="false" type="warning">本地缓存</n-tag>
        </p>

        <div v-if="storeLoading && storeApps.length === 0" class="plugins__empty">
          <p class="plugins__empty-title">正在读取应用仓库…</p>
        </div>
        <div v-else-if="query && visibleStoreApps.length === 0" class="plugins__empty">
          <p class="plugins__empty-title">没有匹配「{{ query }}」的应用</p>
        </div>
        <div v-else-if="visibleStoreApps.length === 0" class="plugins__empty">
          <p class="plugins__empty-title">应用仓库暂时没有可用的应用</p>
          <p class="row-sub">检查网络后点右上角「刷新仓库」。</p>
        </div>
        <div v-else class="plugins__grid">
          <article v-for="app in visibleStoreApps" :key="app.slug" class="plugins__card">
            <header class="plugins__card-head">
              <span class="plugins__icon" aria-hidden="true">
                <span
                  class="icon-mask plugins__store-icon"
                  :style="{ '--icon-mask': `url(${api.store.iconUrl(app.slug)})` }"
                />
              </span>
              <div class="plugins__card-title">
                <span class="plugins__card-name" :title="app.name">{{ app.name }}</span>
                <span class="row-sub">
                  {{ app.slug }} · v{{ app.version }} ·
                  {{ ENV_CATEGORY_TEXT[app.category] ?? app.category }}
                </span>
              </div>
              <n-tag v-if="app.updateAvailable" size="tiny" :bordered="false" type="warning">
                可更新
              </n-tag>
              <n-tag v-else-if="app.onHost" size="tiny" :bordered="false" type="success">
                已添加
              </n-tag>
            </header>

            <p class="plugins__card-desc">{{ app.description }}</p>

            <footer class="plugins__card-foot">
              <span class="row-sub">
                {{ app.installed ? `已安装 v${app.installedVersion}` : '未安装' }}
              </span>
              <span class="plugins__card-spacer" />
              <n-button
                v-if="app.installed"
                size="small"
                quaternary
                :disabled="storeBusy !== ''"
                @click="askStoreUninstall(app)"
              >
                卸载
              </n-button>
              <n-button
                size="small"
                type="primary"
                :loading="storeBusy === app.slug"
                @click="installStoreApp(app)"
              >
                {{
                  app.updateAvailable
                    ? '更新'
                    : app.onHost
                      ? '重新安装'
                      : app.installed
                        ? '添加到桌面'
                        : '安装'
                }}
              </n-button>
            </footer>
          </article>
        </div>
      </div>
    </template>

    <template v-else>

      <div
        v-if="aiLoading && aiApps.length === 0 && aiBuiltinRows.length === 0"
        class="plugins__empty"
      >
        <p class="plugins__empty-title">正在探测 {{ pageTitle }}…</p>
      </div>

      <div
        v-else-if="query && visibleApps.length === 0 && aiBuiltinRows.length === 0"
        class="plugins__empty"
      >
        <p class="plugins__empty-title">没有匹配「{{ query }}」的工具</p>
      </div>

      <div v-else class="plugins__grid">
        <article
          v-for="row in aiBuiltinRows"
          :key="row.item.org + '/' + row.item.name"
          class="plugins__card"
        >
          <header class="plugins__card-head">
            <span class="plugins__icon" aria-hidden="true">
              <!-- DSH 有官方品牌标，不用通用终端图形。 -->
              <DshLogo v-if="row.item.org === DSH_ORG && row.item.name === DSH_NAME" />
              <svg v-else><use :href="`#glyph-${pluginGlyph(row.item)}`" /></svg>
            </span>
            <div class="plugins__card-title">
              <span class="plugins__card-name" :title="row.item.displayName">
                {{ row.item.displayName }}
              </span>
              <span class="row-sub">
                {{ row.item.org }}/{{ row.item.name }} · v{{ row.item.version }}
              </span>
            </div>
            <n-tag v-if="row.installed" size="tiny" :bordered="false" type="success">
              已安装
            </n-tag>
            <n-button
              v-else
              size="small"
              type="primary"
              :disabled="hostId === null || busyBuiltin !== ''"
              :loading="busyBuiltin === row.item.org + '/' + row.item.name"
              @click="installBuiltin(row.item)"
            >
              安装
            </n-button>
          </header>

          <p class="plugins__card-desc">{{ row.item.description }}</p>

          <!-- 装好之后，卡片底部操作的就是这台主机上的那份安装记录。 -->
          <footer v-if="row.installed" class="plugins__card-foot">
            <n-switch
              size="small"
              :value="row.installed.enabled"
              :aria-label="`启用或停用 ${row.item.displayName}`"
              @update:value="(value) => row.installed && toggleEnabled(row.installed, value)"
            />
            <span class="row-sub">{{ row.installed.enabled ? '已启用' : '已停用' }}</span>
            <span class="plugins__card-spacer" />
            <n-button
              size="small"
              type="primary"
              :disabled="!row.installed.enabled"
              @click="row.installed && open(row.installed)"
            >
              打开
            </n-button>
            <n-button
              size="small"
              :aria-label="`设置：${row.item.displayName}`"
              @click="row.installed && openSettings(row.installed)"
            >
              设置
            </n-button>
          </footer>
        </article>
        <article v-for="app in visibleApps" :key="app.id" class="plugins__card">
          <header class="plugins__card-head">
            <span class="plugins__icon" aria-hidden="true">
              <svg><use :href="`#glyph-${aiGlyph(app)}`" /></svg>
            </span>
            <div class="plugins__card-title">
              <span class="plugins__card-name" :title="app.name">{{ app.name }}</span>
              <span class="row-sub">
                {{ app.pkg }} · Node {{ app.nodeMajor > 0 ? '≥' + app.nodeMajor : '不限' }}
              </span>
            </div>
            <n-tag size="tiny" :bordered="false" :type="app.installed ? 'success' : 'default'">
              {{ app.installed ? '已安装' : '未安装' }}
            </n-tag>
          </header>

          <p class="plugins__card-desc">
            <span v-if="app.note">{{ app.note }}<br /></span>
            <span v-if="app.installed">{{ app.version || '已安装' }} · {{ app.path }}</span>
            <span v-if="!app.note && !app.installed">—</span>
          </p>

          <!--
            平台约束只写在产物随平台分发的工具上：这类包装到别的架构不会"少个功能"，
            而是装完直接跑不起来，所以必须在下发安装命令之前就说清楚。
          -->
          <div v-if="app.os.length > 0 || app.arch.length > 0" class="plugins__card-caps">
            <n-tag size="tiny" :bordered="false">
              {{ app.os.length > 0 ? app.os.join(' / ') : '任意系统' }}
              ·
              {{ app.arch.length > 0 ? app.arch.join(' / ') : '任意处理器' }}
            </n-tag>
            <n-tag v-if="!app.supported" size="tiny" :bordered="false" type="warning">
              不支持 {{ aiPlatform.os }}/{{ aiPlatform.arch }}
            </n-tag>
          </div>

          <footer class="plugins__card-foot">
            <n-button size="small" quaternary @click="copyInstall(app)">复制命令</n-button>
            <span class="plugins__card-spacer" />
            <n-button
              size="small"
              type="primary"
              :loading="aiBusy === app.id"
              :disabled="
                hostId === null || !app.supported || (aiBusy !== '' && aiBusy !== app.id)
              "
              @click="installAI(app)"
            >
              {{ app.installed ? '升级' : '安装' }}
            </n-button>
            <n-button
              v-if="app.installed"
              size="small"
              :disabled="aiBusy !== ''"
              @click="uninstallAI(app)"
            >
              卸载
            </n-button>
          </footer>
        </article>
      </div>
    </template>

    <input ref="fileInput" type="file" accept=".zip,application/zip" hidden @change="onArchivePicked" />

    <!-- 内置插件的设置入口：三个操作都从这里进，不再用下拉菜单。 -->
    <n-modal v-model:show="showSettings" preset="card" title="设置" style="width: 380px">
      <p class="plugins__dialog-title">
        {{ settingsTarget?.displayName }}
        <span class="row-sub">{{ settingsTarget?.org }}/{{ settingsTarget?.name }}</span>
      </p>
      <n-space vertical :size="8">
        <n-button block type="primary" @click="onSettingsAction('config')">打开设置页</n-button>
        <n-button block @click="onSettingsAction('grant')">授权…</n-button>
        <n-button block type="error" @click="onSettingsAction('uninstall')">卸载…</n-button>
      </n-space>
    </n-modal>

    <!-- 网络安全应用的详情：窗口式界面没有路由栈，详情沿用弹窗形态。 -->
    <n-modal v-model:show="securityDetailShow" preset="card" title="应用详情" style="width: 520px">
      <div v-if="securityDetail" class="plugins__detail">
        <header class="plugins__detail-head">
          <span class="plugins__icon plugins__icon--security" aria-hidden="true">
            <svg><use :href="`#glyph-${securityGlyph(securityDetail.slug)}`" /></svg>
          </span>
          <div class="plugins__detail-title">
            <span class="plugins__detail-name">{{ securityDetail.name }}</span>
            <span class="row-sub">
              {{ ENV_CATEGORY_TEXT[securityDetail.category] ?? securityDetail.category }}
            </span>
          </div>
          <n-tag
            size="tiny"
            :bordered="false"
            :type="securityDetail.installed ? 'success' : 'default'"
          >
            {{ securityDetail.installed ? '已安装' : '未安装' }}
          </n-tag>
        </header>

        <div class="plugins__detail-row">
          <span class="plugins__detail-label">功能</span>
          <p>{{ securityDetail.description }}</p>
        </div>
        <div v-if="securityDetail.useCase" class="plugins__detail-row">
          <span class="plugins__detail-label">适用场景</span>
          <p>{{ securityDetail.useCase }}</p>
        </div>
        <div v-if="securityDetail.note" class="plugins__detail-row">
          <span class="plugins__detail-label">注意事项</span>
          <p>{{ securityDetail.note }}</p>
        </div>
        <div v-if="securityDetail.launch" class="plugins__detail-row">
          <span class="plugins__detail-label">打开方式</span>
          <p>在目标机终端中执行 <code>{{ securityDetail.launch.command }}</code></p>
        </div>
        <div
          v-if="securityDetail.launch?.configPaths?.length"
          class="plugins__detail-row"
        >
          <span class="plugins__detail-label">配置文件</span>
          <div class="plugins__detail-paths">
            <code v-for="path in securityDetail.launch.configPaths" :key="path">{{ path }}</code>
          </div>
        </div>
        <div class="plugins__detail-row">
          <span class="plugins__detail-label">当前状态</span>
          <p>
            <template v-if="securityDetail.installed">
              已安装<template v-if="securityDetail.version"> · {{ securityDetail.version }}</template>
            </template>
            <template v-else>未安装</template>
            <template v-if="!securityDetail.supported"> · 当前发行版不提供</template>
            <template v-if="envPrivilege === 'none'"> · 目标机没有可用的提权方式</template>
          </p>
        </div>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button @click="securityDetailShow = false">关闭</n-button>
          <n-button
            v-if="securityDetail?.installed && securityDetail?.launch"
            type="primary"
            @click="openSecurityFromDetail"
          >
            打开
          </n-button>
          <n-button
            :type="securityDetail?.installed && securityDetail?.launch ? 'default' : 'primary'"
            :disabled="
              hostId === null || !securityDetail?.supported || envPrivilege === 'none'
            "
            @click="installSecurityDetail"
          >
            {{ securityDetail?.installed ? '重新安装' : '安装' }}
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <n-modal v-model:show="showGrant" preset="card" title="授权插件能力" style="width: 560px">
      <p class="plugins__dialog-title">
        {{ grantTarget?.displayName }}
        <span class="row-sub">{{ grantTarget?.org }}/{{ grantTarget?.name }}</span>
      </p>
      <p class="plugins__hint">
        勾选即代表你把对应能力开放给该插件；不勾选的会被能力桥拒绝。
        升级插件时，这里的选择只会保留「已勾选 且 新版本仍申请」的项，
        插件无法靠升级悄悄扩权。
      </p>

      <div class="plugins__caps">
        <div class="plugins__caps-actions">
          <n-button size="tiny" @click="selectAllCapabilities">全选</n-button>
          <n-button size="tiny" :disabled="grantSelection.length === 0" @click="clearCapabilities">
            全不选
          </n-button>
          <span class="row-sub">已选 {{ grantSelection.length }} / {{ grantTarget?.capabilities.length ?? 0 }}</span>
        </div>

        <label v-for="name in grantCapabilities" :key="name" class="plugins__cap">
          <input
            type="checkbox"
            :value="name"
            :checked="grantSelection.includes(name)"
            @change="
              (event: Event) => {
                const checked = (event.target as HTMLInputElement).checked;
                grantSelection = checked
                  ? [...grantSelection, name]
                  : grantSelection.filter((item) => item !== name);
              }
            "
          />
          <span class="plugins__cap-name">{{ labelOf(name) }}</span>
          <span
            v-if="riskLabel(name)"
            class="plugins__cap-risk"
            :class="`plugins__cap-risk--${riskOf(name)}`"
          >
            {{ riskLabel(name) }}
          </span>
          <span class="row-sub">{{ name }}</span>
        </label>
        <p v-if="grantCapabilities.length === 0" class="plugins__hint">
          该插件未申请任何能力。
        </p>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button @click="showGrant = false">取消</n-button>
          <n-button type="primary" :loading="saving" @click="saveGrant">保存授权</n-button>
        </n-space>
      </template>
    </n-modal>

    <!--
      环境补装的执行弹窗。它自带 WebSocket 步骤流，因此安装过程中的每一步、
      每一条输出、失败原因都能就地看到，不必让用户切到环境管理窗口去猜进度。
    -->
    <EnvRunDialog
      v-model:show="dshRunShow"
      :host-id="hostId ?? 0"
      :host-name="hostName"
      :request="dshRunRequest"
      @done="onDshEnvDone"
    />

    <!-- 系统组件的安装：同一条环境流水线，只是请求换成了对应的配方。 -->
    <EnvRunDialog
      v-model:show="sysRunShow"
      :host-id="hostId ?? 0"
      :host-name="hostName"
      :request="sysRunRequest"
      @done="onSysDone"
    />

    <!-- AI CLI 安装输出：npm 与 postinstall 的报错原文最有用，不裁剪。 -->
    <n-modal v-model:show="aiOutputShow" preset="card" :title="aiOutputTitle" style="width: 720px">
      <pre class="plugins__ai-output">{{ aiOutput }}</pre>
      <template #footer>
        <n-space justify="end">
          <n-button @click="aiOutputShow = false">关闭</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>
