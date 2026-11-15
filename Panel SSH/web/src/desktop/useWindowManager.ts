import { onBeforeUnmount, onMounted, reactive, ref, type InjectionKey } from 'vue';

export interface AppWindow {
  id: number;
  appKey: string;
  title: string;
  /**
   * 标题后缀，用来区分同一应用的多个窗口。
   *
   * 同一个应用现在可以开多个窗口，光看 title（"资源监控"）无法分辨哪个是哪台机器，
   * 所以窗口自己在选定目标主机后把主机名写进来。
   */
  subtitle: string;
  x: number;
  y: number;
  w: number;
  h: number;
  z: number;
  minimized: boolean;
  maximized: boolean;
  props: Record<string, unknown>;
}

export interface WindowSize {
  w: number;
  h: number;
}

export interface WindowManager {
  windows: AppWindow[];
  activeId: number | null;
  open(appKey: string, title: string, props?: Record<string, unknown>, size?: WindowSize): number;
  close(id: number): void;
  focus(id: number): void;
  minimize(id: number): void;
  toggleMaximize(id: number): void;
  move(id: number, x: number, y: number): void;
  resize(id: number, w: number, h: number): void;
  setSubtitle(id: number, text: string): void;
}

/** 通过 provide/inject 共享窗口管理器，避免逐层透传。 */
export const WindowManagerKey: InjectionKey<WindowManager> = Symbol('window-manager');

/**
 * 让窗口内容写自己标题后缀的能力。
 *
 * 由 WindowFrame 提供：只有窗口自己知道当前该显示什么后缀（比如选中的主机名），
 * 而内容组件拿不到"我是哪个窗口"—— 于是把 id 绑在提供方这边，使用方只管给文本。
 */
export const WindowSubtitleKey: InjectionKey<(text: string) => void> = Symbol('window-subtitle');

/**
 * 让窗口内容请求"关闭我自己"。
 *
 * 由 WindowFrame 提供：内容组件拿不到自己的窗口 id，而"关闭"这件事必须经过
 * 窗口那一层 —— 那里要先问内容组件有没有未保存的修改（beforeClose）。
 */
export const WindowCloseKey: InjectionKey<() => void> = Symbol('window-close');

/**
 * 让窗口内容登记"关闭前拦截"。
 *
 * 为什么用登记而不是模板 ref：窗口内容基本都是 defineAsyncComponent，
 * `<component ref>` 拿到的是异步加载包装器，读不到内部组件 defineExpose 的方法。
 * 由内容组件在挂载时把回调交给 WindowFrame，异步与否都不影响。
 */
export const WindowBeforeCloseKey: InjectionKey<
  (handler: (next: () => void) => void) => void
> = Symbol('window-before-close');

/**
 * 窗口的完整标题。
 *
 * 标题栏与竖栏任务项两处都要用，抽出来共用一套拼接规则，
 * 免得一处显示"资源监控 · web-01"、另一处还是"资源监控"。
 */
export function windowTitle(win: AppWindow): string {
  return win.subtitle ? `${win.title} · ${win.subtitle}` : win.title;
}

const MIN_W = 480;
const MIN_H = 300;
// 这些值与 tokens.css 的 --titlebar-h / --canvas-gap 必须一致。
const TITLE_BAR_H = 38;
const CANVAS_GAP = 8;
/**
 * 右上角状态胶囊占用的顶部高度带（--canvas-gap + --pill-h）。
 *
 * 这条带子是窗口的纵向硬下限 —— 窗口既不该默认压住胶囊，也不该能被拖进去。
 * 胶囊是常驻读数（z-index 高于所有窗口），一旦允许窗口进入这条带子，
 * 它就会盖住窗口标题栏右端的三个控制按钮，实测最大化按钮会直接点不动。
 * 把冲突从"靠层级压制"改成"空间上互不相交"，才是根治。
 */
const PILL_BAND = 42;
const DEFAULT_RAIL_W = 60;
/**
 * 桌面图标网格右边缘（--canvas-gap + 16 + 2×96 + 24 = 232，窗口再留 CANVAS_GAP）。
 *
 * 图标网格从原来的 3 列收成 2 列：14 个应用排 7 行，占宽 192px，
 * 比三列更窄、更纵向，窗口的默认落点因此从 308 收到 240。
 * 这里仍保留"窗口默认不压住图标"的约束 —— 图标是启动入口，
 * 打开第一个应用就把它盖住，用户想接着开第二个时只能先拖开窗口。
 */
const ICON_AREA_W = 232;

/**
 * 最左侧系统竖栏的宽度。
 *
 * 窗口画布从竖栏右边缘开始，横向计算都要减掉它。这里读 CSS 变量而不是写死常量：
 * 竖栏宽度曾出现 CSS 40px 与 JS 44px 各写一份导致最大化窗口算错的情况，
 * 让 tokens.css 做唯一真相源可以避免重演。
 */
function railWidth(): number {
  const raw = getComputedStyle(document.documentElement).getPropertyValue('--railbar-w');
  const n = Number.parseFloat(raw);
  return Number.isFinite(n) ? n : DEFAULT_RAIL_W;
}

/** 画布可用宽度 = 视口宽 - 最左侧系统竖栏。 */
function canvasWidth(): number {
  return window.innerWidth - railWidth();
}
// 默认窗口尺寸。内容密度高的应用（如资源监控）由调用方显式指定更大尺寸，
// 否则内容会被挤到只剩一两行可见。
const DEFAULT_W = 940;
const DEFAULT_H = 580;

export function useWindowManager(): WindowManager {
  const windows = reactive<AppWindow[]>([]);
  const activeId = ref<number | null>(null);

  let seq = 0;
  let zSeq = 10;

  /**
   * 用户为每个窗口摆放的"期望几何"。
   *
   * 视口变窄时窗口必须临时收敛，否则标题栏会被裁到看不见；但视口恢复后
   * 应该回到用户原来的摆放，而不是永远停在收敛后的位置。move/resize 会
   * 更新这里，clampAll 只读不写。
   */
  const desired = new Map<number, { x: number; y: number; w: number; h: number }>();

  function remember(win: AppWindow): void {
    desired.set(win.id, { x: win.x, y: win.y, w: win.w, h: win.h });
  }

  function find(id: number): AppWindow | undefined {
    return windows.find((w) => w.id === id);
  }

  function open(
    appKey: string,
    title: string,
    props: Record<string, unknown> = {},
    size?: WindowSize,
  ): number {
    const id = ++seq;
    // 层叠偏移只作用于纵向：横向必须整体待在图标区右侧，
    // 再叠加横向偏移会把窗口右缘推出画布。
    const offset = (windows.length % 6) * 28;

    // 宽度同时在「图标区右侧」与「画布右缘」之间收敛，小屏上也不会超出可视区域。
    const width = Math.min(size?.w ?? DEFAULT_W, canvasWidth() - ICON_AREA_W - CANVAS_GAP * 2);
    // 高度避让右上角胶囊与画布下边缘。原先还要额外扣掉底部任务栏占的 64px，
    // 任务栏收进竖栏后这笔扣除取消，窗口因此多出这一截可用高度。
    const height = Math.min(size?.h ?? DEFAULT_H, window.innerHeight - PILL_BAND - CANVAS_GAP);
    const winW = Math.max(MIN_W, width);
    const winH = Math.max(MIN_H, height);

    // 默认落点是图标区右侧；视口比"图标区 + 窗口"还窄时先贴左，
    // 否则新窗口一出生就有一半在画布外。
    const x = Math.min(ICON_AREA_W + CANVAS_GAP, Math.max(0, canvasWidth() - CANVAS_GAP - winW));

    const win: AppWindow = {
      id,
      appKey,
      title,
      subtitle: '',
      x,
      y: PILL_BAND + CANVAS_GAP + offset,
      w: winW,
      h: winH,
      z: ++zSeq,
      minimized: false,
      maximized: false,
      props,
    };
    windows.push(win);
    remember(win);
    activeId.value = id;
    return id;
  }

  function close(id: number): void {
    const idx = windows.findIndex((w) => w.id === id);
    if (idx >= 0) windows.splice(idx, 1);
    desired.delete(id);

    if (activeId.value === id) {
      const top = windows.filter((w) => !w.minimized).sort((a, b) => b.z - a.z)[0];
      activeId.value = top ? top.id : null;
    }
  }

  function focus(id: number): void {
    const win = find(id);
    if (!win) return;
    win.z = ++zSeq;
    win.minimized = false;
    activeId.value = id;
  }

  /** 由窗口内容（如选好目标主机的选择器）写标题后缀。 */
  function setSubtitle(id: number, text: string): void {
    const win = find(id);
    if (win) win.subtitle = text;
  }

  function minimize(id: number): void {
    const win = find(id);
    if (!win) return;
    win.minimized = true;
    if (activeId.value === id) {
      const top = windows.filter((w) => !w.minimized).sort((a, b) => b.z - a.z)[0];
      activeId.value = top ? top.id : null;
    }
  }

  function toggleMaximize(id: number): void {
    const win = find(id);
    if (!win) return;
    win.maximized = !win.maximized;
    focus(id);
  }

  function move(id: number, x: number, y: number): void {
    const win = find(id);
    if (!win) return;
    // 横向以画布为界（原点在左侧竖栏右边缘）；x 允许探出左边一段，
    // 但要保留部分标题栏可见，防止窗口被拖到再也抓不回来的位置。
    win.x = Math.max(-win.w + 160, Math.min(x, canvasWidth() - 80));
    // 纵向下限是胶囊条带，不是画布边缘：见 PILL_BAND 的说明。
    win.y = Math.max(PILL_BAND, Math.min(y, window.innerHeight - TITLE_BAR_H - CANVAS_GAP));
    remember(win);
  }

  function resize(id: number, w: number, h: number): void {
    const win = find(id);
    if (!win) return;
    win.w = Math.max(MIN_W, Math.min(w, canvasWidth() - win.x - CANVAS_GAP));
    win.h = Math.max(MIN_H, Math.min(h, window.innerHeight - win.y - CANVAS_GAP));
    remember(win);
  }

  /**
   * 视口变化时把窗口收敛回可见区域。
   *
   * 不做这一步的话，把浏览器窗口从 1440 缩到 820 时，1120 宽的窗口会被
   * .desktop 的 overflow:hidden 直接裁掉右半边，而标题栏也跟着跑到视野外，
   * 用户既看不到内容也拖不动它。缩放浏览器窗口是日常操作，不是边界情况。
   *
   * 收敛的基准是 desired（用户最后一次摆放的位置与尺寸），而不是当前值：
   * 视口恢复后要能回到原处。旧实现直接拿当前值往回收，收到画布外之后
   * 就再也回不来了 —— 切换页面导致 iframe 短暂变窄，窗口就会永久停在左边。
   */
  function clampAll(): void {
    const maxW = canvasWidth() - CANVAS_GAP * 2;
    const maxH = window.innerHeight - PILL_BAND - CANVAS_GAP;
    // 右/下边界的口径与 move/resize 一致：留出画布间隙。
    const rightLimit = canvasWidth() - CANVAS_GAP;
    const bottomLimit = window.innerHeight - TITLE_BAR_H - CANVAS_GAP;

    for (const win of windows) {
      // 最大化窗口的尺寸由 CSS 按视口计算，不需要（也不能）写回几何。
      if (win.maximized) continue;

      const want = desired.get(win.id) ?? { x: win.x, y: win.y, w: win.w, h: win.h };
      const width = Math.max(MIN_W, Math.min(want.w, maxW));
      const height = Math.max(MIN_H, Math.min(want.h, maxH));

      // 视口比窗口还窄时 maxX 会算成负数；此时宁可贴住左边、裁掉右侧，
      // 也不要把窗口推出画布 —— 那正是"窗口跑到屏幕外"的来源。
      const maxX = Math.max(0, rightLimit - width);
      const maxY = Math.max(PILL_BAND, bottomLimit - height);

      win.w = width;
      win.h = height;
      win.x = Math.min(Math.max(0, want.x), maxX);
      win.y = Math.min(Math.max(PILL_BAND, want.y), maxY);
    }
  }

  onMounted(() => window.addEventListener('resize', clampAll));
  onBeforeUnmount(() => window.removeEventListener('resize', clampAll));

  // 必须用 reactive 包一层：直接在普通对象里返回 activeId.value 只会拿到
  // 调用瞬间的快照，后续变更不会触发视图更新。reactive 会自动解包 ref。
  return reactive({
    windows,
    activeId,
    open,
    close,
    focus,
    minimize,
    toggleMaximize,
    move,
    resize,
    setSubtitle,
  });
}
