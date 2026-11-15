<script setup lang="ts">
import { computed, defineAsyncComponent, nextTick, onBeforeUnmount, onMounted, provide, ref, type Component } from 'vue';
import { NButton, NForm, NFormItem, NInput, NModal, NSlider, NSpace, useMessage } from 'naive-ui';

import NotificationPanel from './NotificationPanel.vue';
import Launcher from './Launcher.vue';
import StatusPill from './StatusPill.vue';
import HostDropBar from './HostDropBar.vue';
import { findFileTarget } from '../windows/fileTargets';
import WindowFrame from './WindowFrame.vue';
import { useWindowManager, WindowManagerKey, windowTitle, type AppWindow, type WindowSize } from './useWindowManager';
import { usePlugins } from './usePlugins';
import { useLaunchApps } from './useLaunchApps';
import { useNotifications } from './useNotifications';
import { securityGlyph } from '../securityApps';
import AppGlyphs from '../components/icons/AppGlyphs.vue';
import IconBell from '../components/icons/IconBell.vue';
import IconChartLine from '../components/icons/IconChartLine.vue';
import IconLogin from '../components/icons/IconLogin.vue';
import IconUser from '../components/icons/IconUser.vue';
import { THEMES, applyTheme, currentThemeId, railGlass, setRailGlass } from './theme';
import {
  WALLPAPER_PRESETS,
  clearWallpaper,
  selectWallpaper,
  useWallpaperFile,
  wallpaperImage,
  wallpaperPreset,
  type WallpaperId,
} from './wallpaper';
import { ApiError, api, DSH_WORKSPACE_NAME, DSH_WORKSPACE_ORG, type DesktopLayout, type PluginInfo, type SessionUser } from '../api/client';
import DshLogo from '../components/icons/DshLogo.vue';

// 窗口内容按需加载：数据表格与终端（xterm）体积较大，
// 不进首屏包，只有真正打开对应窗口时才拉取。
const HostManager = defineAsyncComponent(() => import('../windows/HostManager.vue'));
const TerminalHost = defineAsyncComponent(() => import('../windows/TerminalHost.vue'));
const FileManager = defineAsyncComponent(() => import('../windows/FileManager.vue'));
const MonitorWindow = defineAsyncComponent(() => import('../windows/MonitorWindow.vue'));
const ProcessWindow = defineAsyncComponent(() => import('../windows/ProcessWindow.vue'));
const NetworkWindow = defineAsyncComponent(() => import('../windows/NetworkWindow.vue'));
const StorageWindow = defineAsyncComponent(() => import('../windows/StorageWindow.vue'));
const DockerWindow = defineAsyncComponent(() => import('../windows/DockerWindow.vue'));
const BatchWindow = defineAsyncComponent(() => import('../windows/BatchWindow.vue'));
const EnvManager = defineAsyncComponent(() => import('../windows/EnvManager.vue'));
const SystemSettings = defineAsyncComponent(() => import('../windows/SystemSettings.vue'));
const SnapshotWindow = defineAsyncComponent(() => import('../windows/SnapshotWindow.vue'));
const LogWindow = defineAsyncComponent(() => import('../windows/LogWindow.vue'));
const TaskWindow = defineAsyncComponent(() => import('../windows/TaskWindow.vue'));
const PluginCenter = defineAsyncComponent(() => import('../windows/PluginCenter.vue'));
const PluginFrame = defineAsyncComponent(() => import('../windows/PluginFrame.vue'));
const SecurityAppWindow = defineAsyncComponent(() => import('../windows/SecurityAppWindow.vue'));
const AIChatWindow = defineAsyncComponent(() => import('../windows/AIChatWindow.vue'));
const BrowserWindow = defineAsyncComponent(() => import('../windows/BrowserWindow.vue'));
// 文件查看器只以窗口形态存在（从文件管理里双击打开），桌面图标里没有它。
const FileViewer = defineAsyncComponent(() => import('../windows/FileViewer.vue'));

const props = defineProps<{ user: SessionUser }>();
const emit = defineEmits<{ (e: 'logout'): void }>();

const message = useMessage();

const wm = useWindowManager();
provide(WindowManagerKey, wm);

/**
 * 处理「文件拖到主机上」。
 *
 * 目标目录的规则：那台机器的文件管理窗口开着，就用它当前所在的目录；
 * 没开着就新开一个（默认根目录），再把东西放进去。
 *
 * 新窗口从 open 到登记进 fileTargets 需要一次挂载 + 首次列目录，
 * 所以这里轮询等待登记 —— 比把待办传输塞进 props 再跨组件传要简单，
 * 也不会在窗口复用（已开着）时走两条不同的路径。
 */
async function onDropTransfer(
  payload: { hostId: number; hostName: string; paths: string[] },
  host: { id: number; name: string },
  mode: 'copy' | 'move',
): Promise<void> {
  const focusExisting = (): void => {
    const win = wm.windows.find((w) => w.appKey === 'files' && w.props.hostId === host.id);
    if (win) wm.focus(win.id);
  };

  const existing = findFileTarget(host.id);
  if (existing) {
    focusExisting();
    existing.accept(payload, mode);
    return;
  }

  wm.open(
    'files',
    `文件管理 - ${host.name}`,
    { hostId: host.id, hostName: host.name },
    { w: 1200, h: 680 },
  );

  for (let i = 0; i < 20; i += 1) {
    await new Promise((resolve) => window.setTimeout(resolve, 150));
    const target = findFileTarget(host.id);
    if (target) {
      target.accept(payload, mode);
      return;
    }
  }
  message.warning('文件管理窗口打开超时，请在该窗口里重新拖放');
}

// 应用注册表：appKey -> 组件。
//
// 两类应用的入口不同：
//   - 全局工具（主机管理、批量执行、AI 助手、应用中心）不需要知道在哪台机器上
//     操作，因此直接作为桌面图标启动；
//   - 绑定主机的应用（终端、文件、监控、进程、网络、存储）也各有桌面图标，打开后
//     先在窗口内选目标主机；主机管理那一行的「连接」是终端的快捷入口 ——
//     它已经知道要连哪台机器，省掉在窗口里再选一次。
const registry: Record<string, Component> = {
  hosts: HostManager,
  terminal: TerminalHost,
  files: FileManager,
  monitor: MonitorWindow,
  process: ProcessWindow,
  network: NetworkWindow,
  storage: StorageWindow,
  docker: DockerWindow,
  batch: BatchWindow,
  env: EnvManager,
  settings: SystemSettings,
  snapshot: SnapshotWindow,
  logs: LogWindow,
  tasks: TaskWindow,
  plugins: PluginCenter,
  plugin: PluginFrame,
  // 安全类应用没有独立图形界面：使用带操作栏与内嵌终端的原生窗口。
  security: SecurityAppWindow,
  ai: AIChatWindow,
  browser: BrowserWindow,
  fileviewer: FileViewer,
};

/**
 * 桌面上的一个应用入口。
 *
 * 图标默认直接用 key 去精灵表取同名 <symbol>；只有共用 key 的安全应用
 * 需要额外指定 glyph，避免十几款工具挤在同一枚图标下。
 */
interface AppEntry {
  key: string;
  name: string;
  /** 应用身份色：桌面图标、窗口标题栏图标与左侧 3px 色码条共用。 */
  rail: string;
  /**
   * 图标名：默认取 key。安全应用共用 appKey='security'，但每款工具有自己的图标，
   * 因此允许单独指定。
   */
  glyph?: string;
  /** 应用包自带的 SVG 图标地址；有值时优先于精灵表图形。 */
  iconUrl?: string;
  /**
   * 是否允许同时开多个窗口。
   *
   * 只给需要主机上下文的那些应用：用户要同时盯两三台机器就得有多个窗口。
   * 主机管理这类全局工具没有"主机"这个区分维度，第二次点出来的窗口内容和
   * 第一次完全相同，多开只会平白占掉一块桌面，因此保持"回到已有窗口"。
   */
  multi?: boolean;
  size?: WindowSize;
  props?: Record<string, unknown>;
}

const builtinApps: AppEntry[] = [
  {
    key: 'hosts',
    name: '主机管理',
    rail: 'var(--app-hosts)',
    // 右侧多了实时监控栏（244px），窗口相应加宽，避免列表被挤到横向溢出。
    size: { w: 1200, h: 640 },
  },
  {
    key: 'terminal',
    name: '终端',
    rail: 'var(--app-terminal)',
    multi: true,
    // 终端要放下一个能用的 shell 视口，默认 940×580 只够二十来行输出。
    size: { w: 1000, h: 640 },
  },
  {
    key: 'files',
    name: '文件管理',
    rail: 'var(--app-files)',
    multi: true,
    // 左栏放权限、大小、时间与操作列，右栏还有容量与常用位置：
    // 1080 只够塞下表格本体，路径条和侧栏会把主列挤到横向滚动。
    size: { w: 1200, h: 680 },
  },
  {
    key: 'monitor',
    name: '资源监控',
    rail: 'var(--app-monitor)',
    multi: true,
    size: { w: 1200, h: 560 },
  },
  {
    key: 'process',
    name: '进程管理',
    rail: 'var(--app-process)',
    multi: true,
    size: { w: 1200, h: 760 },
  },
  {
    key: 'network',
    name: '网络管理',
    rail: 'var(--app-network)',
    multi: true,
    size: { w: 1180, h: 700 },
  },
  {
    key: 'storage',
    name: '存储管理',
    rail: 'var(--app-storage)',
    multi: true,
    size: { w: 1180, h: 700 },
  },
  {
    key: 'docker',
    name: 'Docker',
    rail: 'var(--app-docker)',
    // 与网络/存储同属"按主机操作"的工具：同时管两台机器就要两个窗口。
    multi: true,
    // 五个标签页里容器表最宽，默认尺寸要能放下名称、镜像、状态、端口与操作列。
    size: { w: 1240, h: 760 },
  },
  {
    key: 'batch',
    name: '批量执行',
    rail: 'var(--app-batch)',
    size: { w: 1080, h: 720 },
  },
  {
    key: 'env',
    name: '环境管理',
    rail: 'var(--app-env)',
    multi: true,
    size: { w: 1180, h: 700 },
  },
  {
    key: 'settings',
    name: '系统设置',
    rail: 'var(--app-settings)',
    // 设置是"按主机"的：同时调两台机器就要两个窗口，和终端、环境管理一致。
    multi: true,
    // 左侧分类导航 + 右侧设置行，默认尺寸要能放下较长的说明与来源路径。
    size: { w: 1180, h: 720 },
  },
  {
    key: 'ai',
    name: 'AI 助手',
    rail: 'var(--app-ai)',
    size: { w: 880, h: 720 },
  },
  {
    key: 'snapshot',
    name: '操作快照',
    rail: 'var(--app-snapshot)',
    // 左列操作清单 + 右侧快照内容与差异，横向要放下三列差异表。
    size: { w: 1180, h: 700 },
  },
  {
    key: 'logs',
    name: '系统日志',
    rail: 'var(--app-logs)',
    multi: true,
    // 日志窗口带主机选择，多开可以同时盯两三台机器。
    size: { w: 1180, h: 700 },
  },
  {
    key: 'plugins',
    name: '应用中心',
    rail: 'var(--app-apps)',
    size: { w: 1100, h: 660 },
  },
  {
    key: 'browser',
    name: '浏览器',
    rail: 'var(--app-browser)',
    multi: true,
    // 页面本身要占满可视区，另外还要放下地址栏与代理配置的说明。
    size: { w: 980, h: 660 },
  },
  {
    key: 'theme',
    name: '主题中心',
    rail: 'var(--app-theme)',
  },
];

const { plugins, refresh: refreshPlugins } = usePlugins();
const { launchApps, refresh: refreshLaunchApps } = useLaunchApps();
const notifications = useNotifications();
const unreadCount = notifications.unread;

// 已启用的插件、已安装的安全应用与内置应用并列出现在桌面上。
//
// 每台主机上的插件与安全应用各算一个入口：同一款工具装在两台机器上，
// 就是两个图标、两个窗口。因此图标名里带上主机名 —— 否则桌面上会出现
// 两个一模一样的图标，用户无从分辨。
const apps = computed<AppEntry[]>(() => [
  ...builtinApps,
  ...launchApps.value.map((item) => ({
    key: 'security',
    name: `${item.name} · ${item.hostName}`,
    rail: 'var(--warn)',
    glyph: securityGlyph(item.slug),
    iconUrl: item.iconUrl,
    // 打开的是原生操作窗口，同一款工具可以同时连多台主机。
    multi: true,
    // Fail2Ban 与 GnuPG 改成了专用可视化窗口：Jail 卡片与密钥卡片要横向铺开，
    // 1000×640 会把事实栏挤成单列；其余工具仍用通用壳的默认尺寸。
    size:
      item.slug === 'fail2ban' || item.slug === 'gnupg'
        ? { w: 1120, h: 720 }
        : { w: 1000, h: 640 },
    props: {
      hostId: item.hostId,
      hostName: item.hostName,
      slug: item.slug,
      // 窗口标题栏按 props 取图标，和桌面图标保持同一枚。
      glyph: securityGlyph(item.slug),
      iconUrl: item.iconUrl,
    } as Record<string, unknown>,
  })),
  ...plugins.value
    .filter((item) => item.enabled)
    .map((item) => ({
      key: 'plugin',
      name: item.hostName ? `${item.displayName} · ${item.hostName}` : item.displayName,
      rail: 'var(--app-plugin)',
      // DSH 工作台打开的是管理器而不是一张网页：左侧导航加四页内容，
      // 默认尺寸要能放下三列状态卡与插件行，不再沿用普通插件的 1000×680。
      size:
        item.org === DSH_WORKSPACE_ORG && item.name === DSH_WORKSPACE_NAME
          ? { w: 1180, h: 800 }
          : { w: 1000, h: 680 },
      props: { plugin: item } as Record<string, unknown>,
    })),
]);

// ---------------- 桌面布局 ----------------

/**
 * 应用在桌面布局里的稳定标识。
 *
 * 内置应用用 app.key；插件与安全应用共用同一个 key，必须再带上安装位置，
 * 否则两台机器上的同名应用会被当成同一个图标 —— 删掉一个等于两个都没了。
 */
function appId(app: AppEntry): string {
  if (app.key === 'security') {
    return `security:${String(app.props?.slug ?? '')}@${String(app.props?.hostId ?? '')}`;
  }
  if (app.key !== 'plugin') return app.key;
  const plugin = app.props?.plugin as PluginInfo | undefined;
  return plugin ? `plugin:${plugin.org}/${plugin.name}@${plugin.hostId}` : 'plugin';
}

/**
 * 桌面布局按用户存在服务端：换浏览器、换设备后图标与文件夹仍然一致。
 *
 * 只记 hidden 与 folders，不记完整可见列表 —— 新装的插件默认就该出现在桌面上。
 */
const layout = ref<DesktopLayout>({ hidden: [], folders: [] });

/**
 * 布局是否已从服务端取回。
 *
 * 取回前不渲染图标网格：否则被用户移出桌面的图标会先闪一下再消失，
 * 看起来像桌面在抖动。
 */
const layoutReady = ref(false);

async function loadLayout(): Promise<void> {
  try {
    const saved = await api.desktop.getLayout();
    layout.value = normalizeLayout({
      hidden: Array.isArray(saved.hidden) ? saved.hidden : [],
      folders: Array.isArray(saved.folders) ? saved.folders : [],
    });
  } catch {
    // 布局读取失败不挡桌面：退回默认布局（全部图标可见）继续用。
  } finally {
    layoutReady.value = true;
  }
}

/**
 * 保证"一个应用最多出现在一处"。
 *
 * 桌面有三处状态：顶层、某个文件夹、已移出。同一个 id 同时出现在两处时，
 * 桌面上就会看到两个一模一样的入口。写入侧已逐条收口，这里再兜一次底，
 * 防止旧数据或手工改库把矛盾状态带进来。
 */
function normalizeLayout(next: DesktopLayout): DesktopLayout {
  const hidden = Array.from(new Set(next.hidden));
  const seen = new Set<string>();
  const folders = next.folders.map((folder) => ({
    ...folder,
    apps: folder.apps.filter((id) => {
      if (seen.has(id)) return false;
      seen.add(id);
      return true;
    }),
  }));
  // 既被移出又在文件夹里时以文件夹为准：图标至少还能在文件夹里看到。
  return { hidden: hidden.filter((id) => !seen.has(id)), folders };
}

/** 把图标从所有文件夹里摘掉，供"添加到桌面 / 拖入某个文件夹"复用。 */
function removeFromAllFolders(id: string): void {
  for (const folder of layout.value.folders) {
    if (folder.apps.includes(id)) {
      folder.apps = folder.apps.filter((item) => item !== id);
    }
  }
}

function persistLayout(): void {
  void api.desktop.saveLayout(layout.value).catch(() => {
    message.warning('桌面布局保存失败');
  });
}

/** 已被某个文件夹收纳的图标 id。 */
const foldedIds = computed(() => new Set(layout.value.folders.flatMap((folder) => folder.apps)));

/** 桌面顶层图标：没被移出、也没被任何文件夹收纳。 */
const desktopApps = computed(() =>
  apps.value.filter((app) => {
    const id = appId(app);
    return !layout.value.hidden.includes(id) && !foldedIds.value.has(id);
  }),
);

/** 桌面上的文件夹，成员已解析成应用条目。 */
interface DesktopFolderView {
  id: string;
  name: string;
  apps: AppEntry[];
}

/** 插件被卸载后，文件夹里残留的 id 找不到对应应用，直接跳过。 */
const desktopFolders = computed<DesktopFolderView[]>(() =>
  layout.value.folders.map((folder) => ({
    id: folder.id,
    name: folder.name,
    apps: folder.apps
      .map((id) => apps.value.find((app) => appId(app) === id))
      .filter((app): app is AppEntry => app !== undefined),
  })),
);

/** 启动器与桌面共用同一批应用；额外带上 appId 供右键菜单定位。 */
const launcherApps = computed(() => apps.value.map((app) => ({ ...app, id: appId(app) })));

// ---- 右键菜单 ----

interface DesktopMenuItem {
  label: string;
  danger?: boolean;
  run: () => void;
}

const contextMenu = ref<{ x: number; y: number; items: DesktopMenuItem[] } | null>(null);

/**
 * 在鼠标位置弹菜单。
 *
 * 先按菜单尺寸把坐标夹回视口：桌面贴着屏幕右下角时，
 * 菜单若照原坐标展开会被视口裁掉一半，看得见却点不到。
 */
function openContextMenu(event: MouseEvent, items: DesktopMenuItem[]): void {
  const width = 176;
  const height = items.length * 32 + 10;
  contextMenu.value = {
    x: Math.max(8, Math.min(event.clientX, window.innerWidth - width - 8)),
    y: Math.max(8, Math.min(event.clientY, window.innerHeight - height - 8)),
    items,
  };
}

function closeContextMenu(): void {
  contextMenu.value = null;
}

function runMenuItem(item: DesktopMenuItem): void {
  contextMenu.value = null;
  item.run();
}

function openDesktopMenu(event: MouseEvent): void {
  openContextMenu(event, [
    { label: '新建文件夹', run: createFolder },
    {
      label: '添加图标…',
      run: () => {
        showLauncher.value = true;
      },
    },
  ]);
}

function openAppMenu(event: MouseEvent, app: AppEntry): void {
  openContextMenu(event, [{ label: '从桌面移除', danger: true, run: () => hideApp(app) }]);
}

function openFolderMenu(event: MouseEvent, folder: DesktopFolderView): void {
  openContextMenu(event, [
    { label: '解散文件夹', danger: true, run: () => dissolveFolder(folder.id) },
  ]);
}

/**
 * 启动器磁贴的右键菜单。
 *
 * 三种状态给三种动作：在文件夹里就移出文件夹，被移出桌面就加回来，
 * 其余才是从桌面移除 —— 否则"添加到桌面"点了没有任何变化。
 */
function openLauncherMenu(payload: { app: { id: string }; event: MouseEvent }): void {
  const app = apps.value.find((item) => appId(item) === payload.app.id);
  if (!app) return;

  const id = appId(app);
  const folder = layout.value.folders.find((item) => item.apps.includes(id));
  if (folder) {
    openContextMenu(payload.event, [
      { label: '移出文件夹', run: () => removeFromFolder(folder.id, app) },
    ]);
    return;
  }
  if (layout.value.hidden.includes(id)) {
    openContextMenu(payload.event, [{ label: '添加到桌面', run: () => showApp(app) }]);
    return;
  }
  openContextMenu(payload.event, [{ label: '从桌面移除', danger: true, run: () => hideApp(app) }]);
}

// ---- 布局修改 ----

function hideApp(app: AppEntry): void {
  const id = appId(app);
  // 先从文件夹里摘掉：一个图标只允许存在一处，移除后不该残留在文件夹里。
  removeFromAllFolders(id);
  if (!layout.value.hidden.includes(id)) layout.value.hidden.push(id);
  persistLayout();
}

function showApp(app: AppEntry): void {
  const id = appId(app);
  // 添加到桌面等同于"放回顶层"：从文件夹与移出名单里都摘掉，
  // 否则连点两次或先放进文件夹再点添加，就会留下两个相同入口。
  removeFromAllFolders(id);
  layout.value.hidden = layout.value.hidden.filter((item) => item !== id);
  persistLayout();
}

function createFolder(): void {
  layout.value.folders.push({
    id: `folder-${Date.now().toString(36)}`,
    name: `文件夹 ${layout.value.folders.length + 1}`,
    apps: [],
  });
  persistLayout();
}

/** 解散文件夹：成员回到桌面顶层，不跟着消失。 */
function dissolveFolder(folderId: string): void {
  layout.value.folders = layout.value.folders.filter((folder) => folder.id !== folderId);
  if (openFolderId.value === folderId) closeFolderPanel();
  persistLayout();
}

function removeFromFolder(folderId: string, app: AppEntry): void {
  const folder = layout.value.folders.find((item) => item.id === folderId);
  if (!folder) return;
  const id = appId(app);
  folder.apps = folder.apps.filter((item) => item !== id);
  persistLayout();
}

// ---- 拖图标进文件夹 ----

// HTML5 拖放在 dragover 阶段读不到 dataTransfer 的内容，
// 因此用这个 ref 记住正在拖的图标 id，drop 时直接用。
const draggingId = ref('');
const dragOverFolderId = ref('');

function onIconDragStart(app: AppEntry, event: DragEvent): void {
  const id = appId(app);
  draggingId.value = id;
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move';
    event.dataTransfer.setData('text/plain', id);
  }
}

function onIconDragEnd(): void {
  draggingId.value = '';
  dragOverFolderId.value = '';
}

function onFolderDragOver(folder: DesktopFolderView, event: DragEvent): void {
  if (!draggingId.value) return;
  // 必须阻止默认行为，否则浏览器不会把 drop 交给这个元素。
  event.preventDefault();
  dragOverFolderId.value = folder.id;
}

function onFolderDrop(folder: DesktopFolderView, event: DragEvent): void {
  event.preventDefault();
  const id = draggingId.value || event.dataTransfer?.getData('text/plain') || '';
  draggingId.value = '';
  dragOverFolderId.value = '';
  if (!id) return;

  const target = layout.value.folders.find((item) => item.id === folder.id);
  if (!target) return;
  // 同一图标只归一个文件夹所有：先摘掉它在其它文件夹的位置，
  // 再从"已移出"名单里恢复 —— 拖进来就意味着要能在这里看到。
  removeFromAllFolders(id);
  layout.value.hidden = layout.value.hidden.filter((item) => item !== id);
  if (!target.apps.includes(id)) target.apps.push(id);
  persistLayout();
}

// ---- 文件夹面板 ----

const openFolderId = ref('');
const folderPanelPos = ref({ x: 0, y: 0 });

const openFolderView = computed(
  () => desktopFolders.value.find((folder) => folder.id === openFolderId.value) ?? null,
);

/**
 * 打开文件夹面板。
 *
 * 面板按 300×260 的常用尺寸向视口内收，保证贴着屏幕边缘的文件夹
 * 也能完整展开，而不是被裁掉一半。
 */
function openFolderPanel(folder: DesktopFolderView, event: MouseEvent): void {
  openFolderId.value = folder.id;
  folderPanelPos.value = {
    x: Math.max(8, Math.min(event.clientX, window.innerWidth - 312)),
    y: Math.max(8, Math.min(event.clientY, window.innerHeight - 272)),
  };
}

function closeFolderPanel(): void {
  openFolderId.value = '';
  cancelFolderRename();
}

// ---- 文件夹重命名 ----

const renamingFolderId = ref('');
const renameValue = ref('');
const renameInput = ref<HTMLInputElement | null>(null);

/** 双击标题进入改名；下一次渲染后聚焦并全选，省掉用户再点一次。 */
function startFolderRename(folder: DesktopFolderView): void {
  if (renamingFolderId.value === folder.id) return;
  renamingFolderId.value = folder.id;
  renameValue.value = folder.name;
  void nextTick(() => {
    renameInput.value?.focus();
    renameInput.value?.select();
  });
}

function commitFolderRename(): void {
  const id = renamingFolderId.value;
  if (!id) return;
  const folder = layout.value.folders.find((item) => item.id === id);
  const name = renameValue.value.trim();
  // 空名会让文件夹在桌面和面板里都无法辨认，直接保留原名。
  if (folder && name) {
    folder.name = name.slice(0, 24);
    persistLayout();
  }
  cancelFolderRename();
}

function cancelFolderRename(): void {
  renamingFolderId.value = '';
  renameValue.value = '';
}

function openFolderAppMenu(event: MouseEvent, folder: DesktopFolderView, app: AppEntry): void {
  openContextMenu(event, [
    { label: '移出文件夹', run: () => removeFromFolder(folder.id, app) },
  ]);
}

/**
 * 启动应用。
 *
 * 分两类：
 *   - 需要主机上下文的应用（multi）每次点击都是一次新的启动，不去理会已经开着的
 *     那些窗口 —— 同时看两三台机器本来就要多个窗口。回到已开的窗口交给竖栏任务项，
 *     那里按窗口列出，本来就是干这件事的。
 *   - 其余全局工具保持"已有就还原并聚焦"：它们没有主机这个区分维度，
 *     再点一次得到的窗口和已开的那个内容完全相同。
 */
function launch(app: AppEntry): void {
  // 主题中心是弹窗而不是窗口：它是本机外观设置，没有任务上下文，
  // 也不该在竖栏任务里占一项。
  if (app.key === 'theme') {
    showTheme.value = true;
    return;
  }

  if (app.multi) {
    wm.open(app.key, app.name, app.props ?? {}, app.size);
    return;
  }

  const opened = wm.windows.filter((win) => windowBelongsTo(win, app));
  if (opened.length === 0) {
    wm.open(app.key, app.name, app.props ?? {}, app.size);
    return;
  }

  // 优先还原"用户看得见"的那个；全被最小化时取层级最高的那个还原。
  // wm.focus 会把 minimized 置回 false，所以还原与聚焦是一步完成的。
  const shown = opened.filter((win) => !win.minimized).sort((a, b) => b.z - a.z);
  const target = shown[0] ?? [...opened].sort((a, b) => b.z - a.z)[0];
  wm.focus(target.id);
}

function resolve(appKey: string): Component {
  return registry[appKey] ?? HostManager;
}

/**
 * 某个窗口是否属于某个应用。
 *
 * 所有插件窗口共用 appKey='plugin'，必须再比对插件身份，否则打开任意一个插件
 * 会让桌面上每个插件图标都点亮、竖栏任务项也会认错徽标。
 * 桌面图标的运行标记与竖栏任务项的徽标都要用这条判定，因此单独抽出来。
 */
function windowBelongsTo(win: AppWindow, app: AppEntry): boolean {
  if (win.appKey !== app.key) return false;
  if (app.key === 'security') {
    // 同一个 key 下每款工具、每台主机都是独立实例。
    return (
      win.props.slug === app.props?.slug && win.props.hostId === app.props?.hostId
    );
  }
  if (app.key !== 'plugin') return true;

  const self = app.props?.plugin as { org: string; name: string; hostId: number } | undefined;
  const opened = win.props.plugin as { org: string; name: string; hostId: number } | undefined;
  // 换主机等于换了一个插件实例：装在不同机器上的同名插件，窗口内容完全不同，
  // 因此归属判定必须连 hostId 一起比，否则点第二台机器的图标会还原到第一台的窗口。
  return (
    self !== undefined &&
    self.org === opened?.org &&
    self.name === opened?.name &&
    self.hostId === opened?.hostId
  );
}

/**
 * 该应用是否已有窗口打开。
 *
 * 图标据此把边框与徽标点亮成身份色，一眼看出哪些应用正在跑 ——
 * 这是桌面系统里"已打开应用带运行标记"的通行做法。
 */
function isAppOpen(app: AppEntry): boolean {
  return wm.windows.some((win) => windowBelongsTo(win, app));
}

/**
 * 窗口所属应用的身份色，供窗口标题栏图标着色。
 *
 * 查不到应用条目时返回 undefined（任务记录窗口不在桌面应用表里），
 * 标题栏图标随之退回默认字色，而不是硬塞一个不对的颜色。
 *
 * 文件查看器没有桌面入口（它从文件管理里双击打开），但标题栏图标与色条仍应
 * 沿用文件管理的身份色 —— 否则同一个来源的窗口会有两套长相。
 */
const windowOnlyRails: Record<string, string> = {
  fileviewer: 'var(--app-files)',
};

function railOf(win: AppWindow): string | undefined {
  return apps.value.find((item) => windowBelongsTo(win, item))?.rail ?? windowOnlyRails[win.appKey];
}

/**
 * 该桌面图标是否是 DSH 工作台。
 *
 * 插件图标默认共用 #glyph-plugin；只有 DSH 工作台用品牌鲸鱼标，
 * 避免把"所有插件长得一样"这件事沿用到一个有明确品牌的应用上。
 */
function isDshApp(app: AppEntry): boolean {
  if (app.key !== 'plugin') return false;
  const plugin = app.props?.plugin as PluginInfo | undefined;
  return plugin?.org === DSH_WORKSPACE_ORG && plugin.name === DSH_WORKSPACE_NAME;
}

function toggleWindow(win: AppWindow): void {
  // 已聚焦的窗口再次点击 => 最小化；否则聚焦并还原。
  if (!win.minimized && wm.activeId === win.id) {
    wm.minimize(win.id);
  } else {
    wm.focus(win.id);
  }
}

/**
 * 未读角标要跟着服务端变：别的窗口跑完批量任务、主机掉线、插件被改动，
 * 都会在这里多出一条。因此定时刷新，而不是只在打开面板时拉一次。
 *
 * 间隔取 60 秒：它是角标不是实时读数，没必要秒级刷新；而且每次刷新都会
 * 带回通知列表，间隔太短纯属白费流量。用户点开面板时还会再强制刷新一次。
 */
const NOTIFY_POLL_MS = 60000;
let notifyTimer: number | undefined;

onMounted(() => {
  // 插件列表拉取失败不应拖累桌面的其它功能，这里单独吞掉错误。
  void refreshPlugins().catch(() => undefined);
  // 已安装的安全应用同样决定桌面图标，失败时维持上一次结果。
  void refreshLaunchApps().catch(() => undefined);
  void loadLayout();

  void notifications.refresh();
  notifyTimer = window.setInterval(() => void notifications.refresh(), NOTIFY_POLL_MS);
});

onBeforeUnmount(() => {
  if (notifyTimer !== undefined) window.clearInterval(notifyTimer);
});

async function logout(): Promise<void> {
  try {
    await api.auth.logout();
  } catch {
    // 本地状态清理优先于服务端结果，失败不阻断退出。
  }
  emit('logout');
}

// ---- 竖栏的系统级入口 ----

/**
 * 打开任务记录窗口。
 *
 * 先去重再打开：桌面图标带"已打开"标记，用户知道自己点的是什么；
 * 竖栏按钮没有这层反馈，重复点击若每次都新开窗口，桌上很快会堆出一摞。
 *
 * taskId 由通知中心传入，表示"直接看这一条任务"。
 */
function openTasks(taskId?: number): void {
  const opened = wm.windows.find((w) => w.appKey === 'tasks');
  if (opened) {
    // 已经开着就换掉它要展示的任务并聚焦，而不是再开一个窗口。
    if (taskId !== undefined) opened.props = { ...opened.props, taskId };
    wm.focus(opened.id);
    return;
  }
  wm.open('tasks', '任务记录', taskId === undefined ? {} : { taskId }, { w: 1040, h: 640 });
}

/** 通知中心抽屉的开关。 */
const showNotifications = ref(false);

function toggleNotifications(): void {
  showNotifications.value = !showNotifications.value;
}

/**
 * 启动器是可选入口：桌面图标已经覆盖常用应用，这里额外提供搜索与
 * "窗口盖住桌面时仍能启动"的入口。两份入口共用同一个 apps 与 launch()。
 */
const showLauncher = ref(false);

/** 已打开应用的桌面 id，用于启动器里的运行标记。 */
const runningKeys = computed(() => {
  const ids = new Set<string>();
  for (const win of wm.windows) {
    const app = apps.value.find((item) => windowBelongsTo(win, item));
    if (app) ids.add(appId(app));
  }
  return Array.from(ids);
});

function toggleLauncher(): void {
  showLauncher.value = !showLauncher.value;
}

function onLauncherLaunch(app: AppEntry): void {
  launch(app);
  showLauncher.value = false;
}

const hasWindows = computed(() => wm.windows.length > 0);

// ---- 修改密码 ----

const showPassword = ref(false);
const changingPassword = ref(false);
const passwordForm = ref({ oldPassword: '', newPassword: '', confirmPassword: '' });

// ---- 主题中心 ----

const showTheme = ref(false);

/**
 * 竖栏毛玻璃的两个参数。
 *
 * 用可写 computed 直接接 n-slider 的 v-model：拖动时立刻写回 <html>，
 * 不需要"应用"按钮 —— 效果实时可见，用户是边拖边看。
 */
const railBlur = computed({
  get: () => railGlass.value.blur,
  set: (value: number) => setRailGlass({ blur: value }),
});

const railAlpha = computed({
  get: () => railGlass.value.alpha,
  set: (value: number) => setRailGlass({ alpha: value }),
});

// ---- 壁纸 ----

/** 预设壁纸直接映射到 global.css 的 .wp--* 图层，缩略图与真实桌面共用同一套规则。 */
const wallpaperClass = computed(() =>
  wallpaperImage.value ? 'wp--image' : `wp--${wallpaperPreset.value}`,
);

const wallpaperStyle = computed(() =>
  wallpaperImage.value ? { backgroundImage: `url("${wallpaperImage.value}")` } : undefined,
);

function onSelectWallpaper(id: WallpaperId): void {
  selectWallpaper(id);
}

async function onWallpaperFile(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  // 清空 value：否则连续选同一张图片不会再触发 change。
  input.value = '';
  if (!file) return;
  try {
    await useWallpaperFile(file);
    message.success('壁纸已更新');
  } catch (error) {
    message.error(error instanceof Error ? error.message : '壁纸设置失败');
  }
}

/**
 * 当前页面加载的入口脚本名。
 *
 * 构建产物带内容哈希，所以这串文件名就是"这一页跑的是哪一版"的唯一凭据：
 * 排查"改了样式却没生效"时，先看它是不是最新的那一个。
 */
const buildTag = computed(() => {
  const src = Array.from(document.querySelectorAll('script[type="module"]'))
    .map((node) => node.getAttribute('src') ?? '')
    .find((value) => value.includes('/assets/index-'));
  return src ? (src.split('/').pop() ?? '未知') : '未知';
});

function openPasswordDialog(): void {
  passwordForm.value = { oldPassword: '', newPassword: '', confirmPassword: '' };
  showPassword.value = true;
}

async function submitPassword(): Promise<void> {
  const { oldPassword, newPassword, confirmPassword } = passwordForm.value;
  if (!oldPassword || !newPassword) {
    message.warning('请填写原密码与新密码');
    return;
  }
  if (newPassword !== confirmPassword) {
    message.warning('两次输入的新密码不一致');
    return;
  }

  changingPassword.value = true;
  try {
    await api.auth.changePassword(oldPassword, newPassword);
    showPassword.value = false;
    // 服务端会轮换会话签名密钥，因此其他浏览器上的登录状态已同时失效。
    message.success('密码已更新，其他已登录的会话已全部下线');
  } catch (err) {
    message.error(err instanceof ApiError ? err.message : '修改密码失败');
  } finally {
    changingPassword.value = false;
  }
}
</script>

<template>
  <div class="desktop">
    <div class="desktop__wallpaper" :class="wallpaperClass" :style="wallpaperStyle" />

    <!-- 应用图标定义只此一份，桌面图标与竖栏品牌标都用 <use> 引用。 -->
    <AppGlyphs />

    <!--
      最左侧系统竖栏：自上而下是品牌标识、任务栏、账户级操作。
      原来的 44px 顶栏承担了品牌、时钟、用户名、改密码、退出五件事，
      改版后品牌上提为竖栏徽标、时钟并入右上角状态胶囊、
      账户操作下沉到竖栏底部。顶栏那条横带因此被彻底去掉，
      工作区上方不再有一整条被占用、却在水平方向大部分为空的区域。
      任务栏原先常驻画布底部，收进竖栏后画布底部不再被压掉 64px 高度。
    -->
    <aside class="railbar">
      <span class="railbar__brand" title="Panel SSH">
        <span class="railbar__mark" aria-hidden="true">
          <svg class="railbar__mark-glyph" viewBox="0 0 24 24">
            <path
              fill="currentColor"
              d="M20 10H4v9h16zM3 3h18a1 1 0 0 1 1 1v16a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1m2 3v2h2V6zm4 0v2h2V6zm-4 5h3v5H5z"
            />
          </svg>
        </span>
      </span>

      <!-- 启动器是可选入口：桌面图标仍是主入口，这里提供搜索与全量列表。 -->
      <button
        class="railbar__launcher"
        :class="{ 'is-on': showLauncher }"
        title="应用启动器"
        aria-label="应用启动器"
        :aria-expanded="showLauncher"
        @click="toggleLauncher"
      >
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" aria-hidden="true">
          <rect x="3.5" y="3.5" width="6.5" height="6.5" rx="1" />
          <rect x="14" y="3.5" width="6.5" height="6.5" rx="1" />
          <rect x="3.5" y="14" width="6.5" height="6.5" rx="1" />
          <rect x="14" y="14" width="6.5" height="6.5" rx="1" />
        </svg>
      </button>

      <div class="railbar__rule" aria-hidden="true" />

      <!--
        任务栏：只列已打开的窗口，不承担启动职责（启动由桌面图标负责），
        避免把同一批应用在屏幕上陈列两遍。没有窗口时整段隐藏，不留空档。
      -->
      <div v-if="hasWindows" class="railbar__tasks">
        <button
          v-for="win in wm.windows"
          :key="win.id"
          class="railbar__task"
          :class="{ 'is-active': win.id === wm.activeId, 'is-min': win.minimized }"
          :title="windowTitle(win)"
          :aria-label="windowTitle(win)"
          @click="toggleWindow(win)"
        >
          <!-- 图标名同样取 appKey；颜色保持中性，不与琥珀色的聚焦状态抢眼。 -->
          <svg class="railbar__task-glyph" aria-hidden="true">
            <use :href="`#glyph-${win.appKey}`" />
          </svg>
        </button>
      </div>

      <span class="railbar__spacer" />

      <div class="railbar__tools">
        <button
          class="railbar__btn"
          :title="unreadCount > 0 ? `通知（${unreadCount} 条未读）` : '通知'"
          aria-label="通知"
          @click="toggleNotifications"
        >
          <IconBell />
          <!-- 超过两位就不再显示精确值：那个位置放不下，也没有精确到个位的必要 -->
          <span v-if="unreadCount > 0" class="railbar__badge num" aria-hidden="true">
            {{ unreadCount > 99 ? '99+' : unreadCount }}
          </span>
        </button>
        <button class="railbar__btn" title="任务记录" aria-label="任务记录" @click="openTasks()">
          <IconChartLine />
        </button>
        <button class="railbar__btn" :title="`修改密码（${props.user.username}）`" aria-label="修改密码" @click="openPasswordDialog">
          <IconUser />
        </button>
        <button class="railbar__btn" title="退出登录" aria-label="退出登录" @click="logout">
          <IconLogin />
        </button>
      </div>
    </aside>

    <!--
      画布：窗口坐标系的横向原点在这里（竖栏右边缘）。
      useWindowManager 的所有横向计算都会减掉 --railbar-w，与这里保持同一套数字。
    -->
    <div class="desktop__canvas" @contextmenu.self.prevent="openDesktopMenu">
      <StatusPill />

      <!--
        桌面图标网格。图标顺序：应用在前、文件夹在后；
        右键空白处可以新建文件夹或打开启动器添加图标。
      -->
      <div v-if="layoutReady" class="desktop__icons" @contextmenu.self.prevent="openDesktopMenu">
        <button
          v-for="app in desktopApps"
          :key="appId(app)"
          class="desktop-icon"
          :class="{ 'is-open': isAppOpen(app) }"
          :style="{ '--rail': app.rail }"
          :aria-label="`打开${app.name}`"
          draggable="true"
          @click="launch(app)"
          @contextmenu.prevent.stop="openAppMenu($event, app)"
          @dragstart="onIconDragStart(app, $event)"
          @dragend="onIconDragEnd"
        >
          <!--
            图标按 app.key 取对应 <symbol>：内置应用与插件各自的 key
            （插件恒为 plugin）在精灵表里都有同名定义，因此不需要额外的
            图标名字段来维持这份对应关系。
          -->
          <span class="desktop-icon__icon" aria-hidden="true">
            <span
              v-if="app.iconUrl"
              class="icon-mask desktop-icon__img"
              :style="{ '--icon-mask': `url(${app.iconUrl})` }"
            />
            <DshLogo v-else-if="isDshApp(app)" class="desktop-icon__glyph" />
            <svg v-else class="desktop-icon__glyph">
              <use :href="`#glyph-${app.glyph ?? app.key}`" />
            </svg>
          </span>
          <span class="desktop-icon__label">{{ app.name }}</span>
        </button>

        <!-- 文件夹：把应用图标拖进来收纳，点击展开成员 -->
        <button
          v-for="folder in desktopFolders"
          :key="folder.id"
          class="desktop-icon desktop-icon--folder"
          :class="{ 'is-drop': dragOverFolderId === folder.id }"
          :aria-label="`打开${folder.name}`"
          @click="openFolderPanel(folder, $event)"
          @contextmenu.prevent.stop="openFolderMenu($event, folder)"
          @dragover="onFolderDragOver(folder, $event)"
          @drop="onFolderDrop(folder, $event)"
        >
          <span class="desktop-icon__icon" aria-hidden="true">
            <!-- 空文件夹没有内容可展示，退回文件夹图形 -->
            <svg v-if="folder.apps.length === 0" class="desktop-icon__glyph">
              <use href="#glyph-folder" />
            </svg>
            <!-- 有内容时直接摆出内部应用图标，最多四个 -->
            <span
              v-else
              class="desktop-icon__mini"
              :class="{ 'is-single': folder.apps.length === 1 }"
            >
              <span
                v-for="app in folder.apps.slice(0, 4)"
                :key="appId(app)"
                class="desktop-icon__mini-cell"
                :style="{ '--rail': app.rail }"
              >
                <span
                  v-if="app.iconUrl"
                  class="icon-mask desktop-icon__mini-img"
                  :style="{ '--icon-mask': `url(${app.iconUrl})` }"
                />
                <DshLogo v-else-if="isDshApp(app)" class="desktop-icon__mini-glyph" />
                <svg v-else class="desktop-icon__mini-glyph">
                  <use :href="`#glyph-${app.glyph ?? app.key}`" />
                </svg>
              </span>
            </span>
            <!-- 超过四个才标总数：否则徽标只是重复画面上已有的信息 -->
            <span v-if="folder.apps.length > 4" class="desktop-icon__count num">
              {{ folder.apps.length }}
            </span>
          </span>
          <span class="desktop-icon__label">{{ folder.name }}</span>
        </button>
      </div>

      <!-- 文件夹内容：贴在图标旁边展开，点空白处关闭 -->
      <div
        v-if="openFolderView"
        class="desktop-popover-scrim"
        @pointerdown="closeFolderPanel"
        @contextmenu.prevent="closeFolderPanel"
      />
      <section
        v-if="openFolderView"
        class="desktop-folder"
        :style="{ left: `${folderPanelPos.x}px`, top: `${folderPanelPos.y}px` }"
        role="dialog"
        :aria-label="openFolderView.name"
      >
        <header class="desktop-folder__head" @dblclick="startFolderRename(openFolderView)">
          <input
            v-if="renamingFolderId === openFolderView.id"
            ref="renameInput"
            v-model="renameValue"
            class="desktop-folder__rename"
            maxlength="24"
            aria-label="文件夹名称"
            @keydown.enter.prevent="commitFolderRename"
            @keydown.esc.prevent="cancelFolderRename"
            @blur="commitFolderRename"
            @click.stop
            @dblclick.stop
          />
          <span v-else class="desktop-folder__title" title="双击重命名">
            {{ openFolderView.name }}
          </span>
          <span class="desktop-folder__count">{{ openFolderView.apps.length }} 个应用</span>
        </header>
        <div v-if="openFolderView.apps.length > 0" class="desktop-folder__grid">
          <button
            v-for="app in openFolderView.apps"
            :key="appId(app)"
            type="button"
            class="desktop-folder__tile"
            :style="{ '--rail': app.rail }"
            :title="app.name"
            @click="launch(app); closeFolderPanel()"
            @contextmenu.prevent.stop="openFolderAppMenu($event, openFolderView, app)"
          >
            <span class="desktop-folder__icon" aria-hidden="true">
              <span
                v-if="app.iconUrl"
                class="icon-mask desktop-folder__img"
                :style="{ '--icon-mask': `url(${app.iconUrl})` }"
              />
              <DshLogo v-else-if="isDshApp(app)" class="desktop-folder__glyph" />
              <svg v-else class="desktop-folder__glyph"><use :href="`#glyph-${app.glyph ?? app.key}`" /></svg>
            </span>
            <span class="desktop-folder__label">{{ app.name }}</span>
          </button>
        </div>
        <p v-else class="desktop-folder__empty">把桌面图标拖进来</p>
      </section>

      <WindowFrame
        v-for="win in wm.windows"
        :key="win.id"
        :win="win"
        :active="win.id === wm.activeId"
        :rail="railOf(win)"
        :comp="resolve(win.appKey)"
        :data="win.props"
        @close="wm.close"
        @minimize="wm.minimize"
        @maximize="wm.toggleMaximize"
        @focus="wm.focus"
        @move="wm.move"
        @resize="wm.resize"
      />

      <!--
        通知抽屉挂在画布内，而不是 .desktop 上：它只覆盖画布，不盖住左侧竖栏，
        否则铃铛本身会被自己的面板压住，用户无法用同一个按钮关掉它。
      -->
      <NotificationPanel v-model:show="showNotifications" @open-task="openTasks" />

      <!-- 启动器只覆盖画布，不盖住左侧竖栏：竖栏按钮仍可用来关闭它。 -->
      <Launcher
        v-if="showLauncher"
        :apps="launcherApps"
        :running-keys="runningKeys"
        @launch="onLauncherLaunch"
        @menu="openLauncherMenu"
        @close="showLauncher = false"
      />
    </div>

    <!--
      桌面右键菜单。挂在 .desktop 根节点上：菜单既要从桌面图标弹出，
      也要从启动器磁贴弹出，层级必须高于启动器（960）。
    -->
    <div
      v-if="contextMenu"
      class="desktop-popover-scrim is-menu"
      @pointerdown="closeContextMenu"
      @contextmenu.prevent="closeContextMenu"
    />
    <div
      v-if="contextMenu"
      class="desktop-menu"
      :style="{ left: `${contextMenu.x}px`, top: `${contextMenu.y}px` }"
      role="menu"
    >
      <button
        v-for="item in contextMenu.items"
        :key="item.label"
        type="button"
        class="desktop-menu__item"
        :class="{ 'is-danger': item.danger }"
        role="menuitem"
        @click="runMenuItem(item)"
      >
        {{ item.label }}
      </button>
    </div>

    <n-modal v-model:show="showPassword" preset="card" title="修改密码" style="width: 430px">
      <n-form label-placement="left" label-width="86" :model="passwordForm">
        <n-form-item label="原密码">
          <n-input
            v-model:value="passwordForm.oldPassword"
            type="password"
            show-password-on="click"
            placeholder="当前登录密码"
            :input-props="{ autocomplete: 'current-password', 'aria-label': '原密码' }"
          />
        </n-form-item>
        <n-form-item label="新密码">
          <n-input
            v-model:value="passwordForm.newPassword"
            type="password"
            show-password-on="click"
            placeholder="至少 10 个字符"
            :input-props="{ autocomplete: 'new-password', 'aria-label': '新密码' }"
          />
        </n-form-item>
        <n-form-item label="确认新密码">
          <n-input
            v-model:value="passwordForm.confirmPassword"
            type="password"
            show-password-on="click"
            placeholder="再次输入新密码"
            :input-props="{ autocomplete: 'new-password', 'aria-label': '确认新密码' }"
            @keyup.enter="submitPassword"
          />
        </n-form-item>
      </n-form>

      <template #footer>
        <n-space justify="end">
          <n-button @click="showPassword = false">取消</n-button>
          <n-button type="primary" :loading="changingPassword" @click="submitPassword">确定</n-button>
        </n-space>
      </template>
    </n-modal>

    <!--
      主题中心：切换的是同一批 CSS 变量与 naive-ui 的配色。
      选择只保存在本机浏览器（localStorage），不写进服务端配置，
      因为"看着舒服"是每台设备、每个人的事，不该被管理员一刀切。
    -->
    <n-modal v-model:show="showTheme" class="theme-modal" preset="card" title="主题中心" style="width: 680px">
      <p class="theme__hint">选择一套配色，立即生效；选择保存在当前浏览器。</p>
      <div class="theme__grid">
        <button
          v-for="item in THEMES"
          :key="item.id"
          class="theme__card"
          :class="{ 'is-active': item.id === currentThemeId }"
          :aria-pressed="item.id === currentThemeId"
          @click="applyTheme(item.id)"
        >
          <span class="theme__swatches" aria-hidden="true">
            <i v-for="color in item.swatches" :key="color" :style="{ background: color }"></i>
          </span>
          <span class="theme__name">{{ item.name }}</span>
          <span class="theme__tagline">{{ item.tagline }}</span>
          <span v-if="item.id === currentThemeId" class="theme__current">当前</span>
        </button>
      </div>

      <div class="theme__split" role="separator"></div>
      <p class="theme__section">壁纸</p>
      <p class="theme__hint">壁纸独立于主题，和主题一样只保存在当前浏览器。</p>
      <div class="wp__grid">
        <button
          v-for="preset in WALLPAPER_PRESETS"
          :key="preset.id"
          class="wp__card"
          :class="{ 'is-active': !wallpaperImage && wallpaperPreset === preset.id }"
          :aria-pressed="!wallpaperImage && wallpaperPreset === preset.id"
          @click="onSelectWallpaper(preset.id)"
        >
          <span class="wp__thumb" :class="`wp--${preset.id}`" aria-hidden="true"></span>
          <span class="wp__name">{{ preset.name }}</span>
        </button>

        <label class="wp__card" :class="{ 'is-active': !!wallpaperImage }">
          <span
            class="wp__thumb wp__thumb--upload"
            :class="{ 'wp--image': wallpaperImage }"
            :style="wallpaperStyle"
            aria-hidden="true"
          >
            <svg v-if="!wallpaperImage" viewBox="0 0 24 24" width="16" height="16">
              <path
                d="M12 5v14M5 12h14"
                fill="none"
                stroke="currentColor"
                stroke-width="1.6"
                stroke-linecap="round"
              />
            </svg>
          </span>
          <span class="wp__name">上传图片</span>
          <input class="wp__file" type="file" accept="image/*" @change="onWallpaperFile" />
        </label>
      </div>
      <p v-if="wallpaperImage" class="wp__note">
        已使用自定义图片
        <button type="button" class="wp__link" @click="clearWallpaper">移除</button>
      </p>

      <div class="theme__split" role="separator"></div>
      <p class="theme__section">竖栏毛玻璃</p>
      <p class="theme__hint">只影响最左侧系统竖栏，和主题一样保存在当前浏览器。</p>
      <div class="glass">
        <div class="glass__row">
          <span class="glass__label">模糊半径</span>
          <n-slider v-model:value="railBlur" class="glass__slider" :min="0" :max="30" :step="1" />
          <span class="glass__value num">{{ railBlur }}px</span>
        </div>
        <div class="glass__row">
          <span class="glass__label">背景浓度</span>
          <n-slider v-model:value="railAlpha" class="glass__slider" :min="20" :max="100" :step="5" />
          <span class="glass__value num">{{ railAlpha }}%</span>
        </div>
      </div>

      <p class="theme__build">
        当前主题 <b>{{ currentThemeId }}</b> · 界面构建 <b>{{ buildTag }}</b>
      </p>
    </n-modal>

    <!-- 拖文件时浮出：把条目直接送到另一台主机的文件管理窗口 -->
    <HostDropBar @transfer="onDropTransfer" />
  </div>
</template>
