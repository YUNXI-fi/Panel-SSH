import { computed, ref } from 'vue';
import type { GlobalThemeOverrides } from 'naive-ui';

/**
 * 主题中心。
 *
 * 面板的视觉变量全部在 tokens.css 的 :root 里有一份"工业仪表盘"默认值；
 * 这里按主题把同一批变量写到 <html> 的行内样式上，实现运行期切换。
 * 这样做的原因：
 *   - naive-ui 的主题是 JS 对象，颜色必须同时给一份 TS 值，放在同一处才不会漂；
 *   - 主题切换只需改一组 CSS 变量 + dataset，不必重新挂载任何组件；
 *   - 主题数量增加时，只需在 PALETTES 里加一项。
 */

export type ThemeId = 'industrial' | 'graphite' | 'paper' | 'frost' | 'midnight';

interface Palette {
  dark: boolean;
  bgVoid: string;
  bgPanel: string;
  bgModule: string;
  bgRaise: string;
  bgSunken: string;
  inkHi: string;
  ink: string;
  inkLo: string;
  inkGhost: string;
  accent: string;
  accentHi: string;
  accentPressed: string;
  ok: string;
  warn: string;
  danger: string;
  info: string;
  data1: string;
  data2: string;
  data3: string;
  data4: string;
  onAccent: string;
  onDanger: string;
  dangerInk: string;
  warnInk: string;
  okInk: string;
  violetInk: string;
  /** 叠层与描边以这个颜色为基准，深色主题是白、浅色主题是墨色。 */
  washBase: string;
  /** 描边的三档透明度：hair / line / strong。 */
  lineAlpha: [number, number, number];
  radius: [number, number, number, number];
  shadowModule: string;
  shadowFloat: string;
  scrim: string;
  vignette: string;
  shadowEdge: string;
  railBg: string;
  floatBg: string;
  syn: [string, string, string, string, string, string];
  /** 浅色主题需要更深的身份色，深色主题留空沿用默认值。 */
  appColors?: Record<string, string>;
}

export interface ThemeDef {
  id: ThemeId;
  name: string;
  tagline: string;
  dark: boolean;
  swatches: [string, string, string, string];
  vars: Record<string, string>;
}

const STORAGE_KEY = 'panelssh.theme';

/**
 * 竖栏毛玻璃参数。
 *
 * 只影响最左侧系统竖栏 —— 它是唯一压在壁纸上的常驻面板。
 * 和主题一样按浏览器保存：模糊半径取决于屏幕与显卡，属于"这台设备"的偏好，
 * 不该跟着账号走到另一台机器上。
 */
const GLASS_KEY = 'panelssh.railglass';

export interface RailGlass {
  /** 背景模糊半径，0 表示不模糊。 */
  blur: number;
  /** 背景浓度（百分比）。基础色本身带透明度，这里再按比例收浓。 */
  alpha: number;
}

export const railGlass = ref<RailGlass>({ blur: 18, alpha: 70 });

function applyRailGlass(): void {
  const root = document.documentElement;
  root.style.setProperty('--rail-blur', `${railGlass.value.blur}px`);
  root.style.setProperty('--rail-alpha', `${railGlass.value.alpha}%`);
}

/** 从 localStorage 读到的原始值写回状态；坏值直接忽略。 */
function readRailGlass(raw: string | null): void {
  if (!raw) return;
  try {
    const saved = JSON.parse(raw) as Partial<RailGlass>;
    if (typeof saved.blur === 'number') railGlass.value.blur = saved.blur;
    if (typeof saved.alpha === 'number') railGlass.value.alpha = saved.alpha;
  } catch {
    // 不是合法 JSON 就沿用默认值。
  }
}

/** 修改毛玻璃参数：立即落到 <html> 并记住，不需要额外的"应用"按钮。 */
export function setRailGlass(next: Partial<RailGlass>): void {
  railGlass.value = { ...railGlass.value, ...next };
  applyRailGlass();
  try {
    localStorage.setItem(GLASS_KEY, JSON.stringify(railGlass.value));
  } catch {
    // 隐私模式下存不了，毛玻璃仍然当场生效。
  }
}

function rgba(hex: string, alpha: number): string {
  const value = hex.replace('#', '');
  const full =
    value.length === 3
      ? value
          .split('')
          .map((c) => c + c)
          .join('')
      : value;
  const num = Number.parseInt(full, 16);
  const r = (num >> 16) & 255;
  const g = (num >> 8) & 255;
  const b = num & 255;
  return `rgba(${r}, ${g}, ${b}, ${alpha})`;
}

/** 浅色主题下把应用身份色整体压深，保证图标在白底上仍然可辨。 */
const LIGHT_APP_COLORS: Record<string, string> = {
  '--app-hosts': '#1f6fa8',
  '--app-files': '#4a5f78',
  '--app-batch': '#b45309',
  '--app-ai': '#0f766e',
  '--app-apps': '#5f6b16',
  '--app-plugin': '#16794f',
  '--app-monitor': '#6d4bc4',
  '--app-process': '#b83a68',
  '--app-network': '#4553c4',
  '--app-storage': '#9a6516',
  '--app-env': '#5a6673',
  '--app-terminal': '#a21caf',
  '--app-snapshot': '#3d6577',
  '--app-logs': '#7a6a52',
  '--app-browser': '#0e7490',
  '--app-docker': '#1d63b8',
  '--app-settings': '#4d6b5b',
  '--app-theme': '#8a4fb0',
};

function buildVars(p: Palette): Record<string, string> {
  const w = (alpha: number) => rgba(p.washBase, alpha);
  const [hair, line, strong] = p.lineAlpha;
  const [r1, r2, r3, r4] = p.radius;
  const [synKeyword, synString, synTitle, synType, synAttr, synTag] = p.syn;

  const vars: Record<string, string> = {
    '--bg-void': p.bgVoid,
    '--bg-panel': p.bgPanel,
    '--bg-module': p.bgModule,
    '--bg-raise': p.bgRaise,
    '--bg-sunken': p.bgSunken,
    '--line-hair': rgba(p.washBase, hair),
    '--line': rgba(p.washBase, line),
    '--line-strong': rgba(p.washBase, strong),
    '--ink-hi': p.inkHi,
    '--ink': p.ink,
    '--ink-lo': p.inkLo,
    '--ink-ghost': p.inkGhost,
    '--sig': p.accent,
    '--sig-hi': p.accentHi,
    '--sig-pressed': p.accentPressed,
    '--sig-wash': rgba(p.accent, p.dark ? 0.1 : 0.12),
    '--sig-wash-soft': rgba(p.accent, p.dark ? 0.06 : 0.055),
    '--sig-line': rgba(p.accent, p.dark ? 0.42 : 0.5),
    '--ok': p.ok,
    '--warn': p.warn,
    '--danger': p.danger,
    '--info': p.info,
    '--ok-wash': rgba(p.ok, p.dark ? 0.12 : 0.1),
    '--ok-line': rgba(p.ok, p.dark ? 0.4 : 0.45),
    '--warn-wash': rgba(p.warn, p.dark ? 0.14 : 0.12),
    '--warn-line': rgba(p.warn, p.dark ? 0.42 : 0.5),
    '--danger-wash': rgba(p.danger, p.dark ? 0.1 : 0.08),
    '--danger-wash-strong': rgba(p.danger, p.dark ? 0.16 : 0.14),
    '--danger-line': rgba(p.danger, p.dark ? 0.4 : 0.45),
    '--info-wash': rgba(p.info, p.dark ? 0.12 : 0.1),
    '--info-line': rgba(p.info, p.dark ? 0.4 : 0.45),
    '--glow-warm': rgba(p.accent, p.dark ? 0.06 : 0.05),
    '--glow-cool': rgba(p.data2, p.dark ? 0.05 : 0.04),
    '--data-1': p.data1,
    '--data-2': p.data2,
    '--data-3': p.data3,
    '--data-4': p.data4,
    '--on-sig': p.onAccent,
    '--on-danger': p.onDanger,
    '--danger-ink': p.dangerInk,
    '--warn-ink': p.warnInk,
    '--ok-ink': p.okInk,
    '--violet-ink': p.violetInk,
    '--wash-base': p.washBase,
    '--wash-2': w(0.02),
    '--wash-3': w(0.03),
    '--wash-4': w(0.04),
    '--wash-5': w(0.05),
    '--wash-6': w(0.06),
    '--wash-7': w(0.07),
    '--wash-10': w(0.1),
    '--wash-16': w(0.16),
    '--wash-26': w(0.26),
    '--grid-line': w(0.022),
    '--grid-dot': w(0.085),
    '--scrim': p.scrim,
    '--vignette': p.vignette,
    '--shadow-edge': p.shadowEdge,
    '--rail-bg': p.railBg,
    '--float-bg': p.floatBg,
    '--shadow-module': p.shadowModule,
    '--shadow-float': p.shadowFloat,
    '--r-1': `${r1}px`,
    '--r-2': `${r2}px`,
    '--r-3': `${r3}px`,
    '--r-4': `${r4}px`,
    '--syn-keyword': synKeyword,
    '--syn-string': synString,
    '--syn-title': synTitle,
    '--syn-type': synType,
    '--syn-attr': synAttr,
    '--syn-tag': synTag,
  };
  if (p.appColors) Object.assign(vars, p.appColors);
  return vars;
}

const PALETTES: Record<ThemeId, Palette> = {
  industrial: {
    dark: true,
    bgVoid: '#060709',
    bgPanel: '#0c0e11',
    bgModule: '#12151a',
    bgRaise: '#181c22',
    bgSunken: '#08090c',
    inkHi: '#eef1f5',
    ink: '#b6bcc5',
    inkLo: '#868d97',
    inkGhost: '#565c66',
    accent: '#ffb02e',
    accentHi: '#ffc968',
    accentPressed: '#e09a24',
    ok: '#57d98c',
    warn: '#ffb02e',
    danger: '#ff6b6b',
    info: '#5fb8e8',
    data1: '#ffab2e',
    data2: '#45c7c0',
    data3: '#5eb8e8',
    data4: '#cfd95c',
    onAccent: '#14100a',
    onDanger: '#1a0505',
    dangerInk: '#ffb3b3',
    warnInk: '#ffdf9e',
    okInk: '#9ff0c2',
    violetInk: '#a9b6ff',
    washBase: '#ffffff',
    lineAlpha: [0.055, 0.1, 0.18],
    radius: [2, 3, 4, 6],
    shadowModule: '0 1px 0 rgba(255, 255, 255, 0.03) inset, 0 10px 28px rgba(0, 0, 0, 0.45)',
    shadowFloat: '0 24px 64px rgba(0, 0, 0, 0.62)',
    scrim: 'rgba(0, 0, 0, 0.45)',
    vignette: 'rgba(0, 0, 0, 0.55)',
    shadowEdge: 'rgba(0, 0, 0, 0.5)',
    railBg: 'rgba(10, 12, 15, 0.78)',
    floatBg: 'rgba(8, 10, 13, 0.82)',
    syn: ['#c9a0ff', '#9fd88a', '#7fc1ff', '#5fd3c8', '#e6c07b', '#ff9d6e'],
  },
  graphite: {
    dark: true,
    bgVoid: '#08090b',
    bgPanel: '#0c0e12',
    bgModule: '#111419',
    bgRaise: '#171b21',
    bgSunken: '#050608',
    inkHi: '#eef1f5',
    ink: '#aeb6c1',
    inkLo: '#7b838e',
    inkGhost: '#565c66',
    accent: '#2dd4bf',
    accentHi: '#5eead4',
    accentPressed: '#14b8a6',
    ok: '#4ade80',
    warn: '#f5a623',
    danger: '#ff6157',
    info: '#5fb8e8',
    data1: '#2dd4bf',
    data2: '#8b7cf6',
    data3: '#f5a623',
    data4: '#cfd95c',
    onAccent: '#04110f',
    onDanger: '#1a0505',
    dangerInk: '#ffb3b3',
    warnInk: '#ffdf9e',
    okInk: '#9ff0c2',
    violetInk: '#a9b6ff',
    washBase: '#ffffff',
    lineAlpha: [0.06, 0.1, 0.16],
    radius: [2, 2, 3, 4],
    shadowModule: '0 1px 0 rgba(255, 255, 255, 0.03) inset, 0 18px 50px rgba(0, 0, 0, 0.45)',
    shadowFloat: '0 24px 64px rgba(0, 0, 0, 0.62)',
    scrim: 'rgba(0, 0, 0, 0.45)',
    vignette: 'rgba(0, 0, 0, 0.55)',
    shadowEdge: 'rgba(0, 0, 0, 0.5)',
    railBg: 'rgba(10, 11, 14, 0.82)',
    floatBg: 'rgba(8, 9, 11, 0.86)',
    syn: ['#c9a0ff', '#9fd88a', '#7fc1ff', '#5fd3c8', '#e6c07b', '#ff9d6e'],
  },
  paper: {
    dark: false,
    bgVoid: '#e7dfd2',
    bgPanel: '#f2ece1',
    bgModule: '#fbf8f1',
    bgRaise: '#f6f1e7',
    bgSunken: '#efe7d8',
    inkHi: '#211d17',
    ink: '#4a4238',
    inkLo: '#6f6557',
    inkGhost: '#9a8f80',
    accent: '#b8462a',
    accentHi: '#cf5a3c',
    accentPressed: '#9d3a22',
    ok: '#3f7d52',
    warn: '#a86a12',
    danger: '#a32d2d',
    info: '#2f6ea5',
    data1: '#b8462a',
    data2: '#3f7d52',
    data3: '#2f6ea5',
    data4: '#8a7a2f',
    onAccent: '#fbf8f1',
    onDanger: '#fbf8f1',
    dangerInk: '#8f1f1f',
    warnInk: '#7a4d08',
    okInk: '#245c3a',
    violetInk: '#4a3f8f',
    washBase: '#211d17',
    lineAlpha: [0.1, 0.18, 0.34],
    radius: [0, 0, 0, 0],
    shadowModule: '0 1px 0 rgba(33, 29, 23, 0.04)',
    shadowFloat: '0 18px 48px rgba(33, 29, 23, 0.12)',
    scrim: 'rgba(33, 29, 23, 0.28)',
    vignette: 'rgba(33, 29, 23, 0.12)',
    shadowEdge: 'rgba(33, 29, 23, 0.25)',
    railBg: 'rgba(242, 236, 225, 0.86)',
    floatBg: 'rgba(251, 248, 241, 0.9)',
    syn: ['#7c3aed', '#2f7d32', '#1d4ed8', '#0f766e', '#a16207', '#c2410c'],
    appColors: LIGHT_APP_COLORS,
  },
  frost: {
    dark: false,
    bgVoid: '#e9eef4',
    bgPanel: '#ffffff',
    bgModule: '#ffffff',
    bgRaise: '#f2f6fb',
    bgSunken: '#eef3f9',
    inkHi: '#132033',
    ink: '#43536a',
    inkLo: '#55677d',
    inkGhost: '#8b98a9',
    accent: '#2456d6',
    accentHi: '#3b6bea',
    accentPressed: '#1f4ac0',
    ok: '#0f8a5f',
    warn: '#b26a05',
    danger: '#c93a3a',
    info: '#2f6ea5',
    data1: '#2456d6',
    data2: '#0f8a5f',
    data3: '#b26a05',
    data4: '#7c5cd6',
    onAccent: '#ffffff',
    onDanger: '#ffffff',
    dangerInk: '#a52828',
    warnInk: '#8a5004',
    okInk: '#0b6647',
    violetInk: '#3f4fa8',
    washBase: '#132033',
    lineAlpha: [0.1, 0.18, 0.28],
    // 与 frost.css 的 r-sm / r-md / r-lg 对齐：控件 7、卡片 10、窗口 14。
    radius: [4, 7, 10, 14],
    shadowModule: '0 1px 2px rgba(16, 32, 64, 0.04), 0 10px 30px rgba(16, 32, 64, 0.06)',
    shadowFloat: '0 24px 70px rgba(16, 32, 64, 0.16)',
    scrim: 'rgba(16, 32, 64, 0.3)',
    vignette: 'rgba(16, 32, 64, 0.12)',
    shadowEdge: 'rgba(16, 32, 64, 0.18)',
    railBg: 'rgba(255, 255, 255, 0.72)',
    floatBg: 'rgba(255, 255, 255, 0.9)',
    syn: ['#7c3aed', '#2f7d32', '#1d4ed8', '#0f766e', '#a16207', '#c2410c'],
    appColors: LIGHT_APP_COLORS,
  },
  midnight: {
    dark: true,
    bgVoid: '#02050a',
    bgPanel: '#04070e',
    bgModule: '#0a1322',
    bgRaise: '#0f1c30',
    bgSunken: '#03060b',
    inkHi: '#e8f1ff',
    ink: '#9fb1cb',
    inkLo: '#6f83a3',
    inkGhost: '#4a5c78',
    accent: '#38bdf8',
    accentHi: '#7dd3fc',
    accentPressed: '#0ea5e9',
    ok: '#34d399',
    warn: '#fbbf24',
    danger: '#fb7185',
    info: '#60a5fa',
    data1: '#38bdf8',
    data2: '#8b7cf6',
    data3: '#34d399',
    data4: '#fbbf24',
    onAccent: '#03131e',
    onDanger: '#1a0505',
    dangerInk: '#ffb3c1',
    warnInk: '#ffe08a',
    okInk: '#9ff0c2',
    violetInk: '#b9b0ff',
    washBase: '#78b4ff',
    lineAlpha: [0.12, 0.24, 0.36],
    radius: [3, 4, 5, 6],
    shadowModule: '0 1px 0 rgba(120, 180, 255, 0.05) inset, 0 30px 80px rgba(0, 0, 0, 0.55)',
    shadowFloat: '0 30px 90px rgba(0, 0, 0, 0.65)',
    scrim: 'rgba(0, 0, 0, 0.5)',
    vignette: 'rgba(0, 0, 0, 0.6)',
    shadowEdge: 'rgba(0, 0, 0, 0.55)',
    railBg: 'rgba(4, 7, 14, 0.82)',
    floatBg: 'rgba(4, 7, 14, 0.86)',
    syn: ['#c9a0ff', '#9fd88a', '#7fc1ff', '#5fd3c8', '#e6c07b', '#ff9d6e'],
  },
};

const META: Array<{ id: ThemeId; name: string; tagline: string }> = [
  { id: 'industrial', name: '工业仪表盘', tagline: '深炭灰 + 琥珀信号 · 当前默认' },
  { id: 'graphite', name: 'Graphite', tagline: '石墨黑 + 青色信号 · 精密克制' },
  { id: 'paper', name: 'Paper Ops', tagline: '暖白纸面 + 陶土色 · 账本气质' },
  { id: 'frost', name: 'Frost', tagline: '冷白底 + 钴蓝 · 明亮通用' },
  { id: 'midnight', name: 'Midnight', tagline: '深潜蓝 + 青色数据 · 任务控制' },
];

export const THEMES: ThemeDef[] = META.map(({ id, name, tagline }) => {
  const p = PALETTES[id];
  return {
    id,
    name,
    tagline,
    dark: p.dark,
    swatches: [p.bgVoid, p.bgModule, p.accent, p.inkHi],
    vars: buildVars(p),
  };
});

export const currentThemeId = ref<ThemeId>('industrial');
export const currentTheme = computed(
  () => THEMES.find((item) => item.id === currentThemeId.value) ?? THEMES[0],
);

/** 把主题变量写到 <html> 上，并记住选择。 */
export function applyTheme(id: ThemeId): void {
  const theme = THEMES.find((item) => item.id === id) ?? THEMES[0];
  const root = document.documentElement;
  for (const [key, value] of Object.entries(theme.vars)) {
    root.style.setProperty(key, value);
  }
  root.dataset.theme = theme.id;
  root.style.colorScheme = theme.dark ? 'dark' : 'light';
  currentThemeId.value = theme.id;
  try {
    localStorage.setItem(STORAGE_KEY, theme.id);
  } catch {
    // 隐私模式下 localStorage 可能抛错：主题仍然生效，只是不记住。
  }
}

/** 首帧之前调用，避免先闪一帧默认主题。 */
export function initTheme(): void {
  let saved: string | null = null;
  try {
    saved = localStorage.getItem(STORAGE_KEY);
    readRailGlass(localStorage.getItem(GLASS_KEY));
  } catch {
    saved = null;
  }
  applyTheme((saved as ThemeId) ?? 'industrial');
  applyRailGlass();

  // 多标签页同步：在 A 标签切换主题后，B 标签也要跟着变。
  // 否则看起来就像"改了主题没生效"——其实改的是另一个标签页。
  window.addEventListener('storage', (event) => {
    if (event.key === GLASS_KEY) {
      readRailGlass(event.newValue);
      applyRailGlass();
      return;
    }
    if (event.key !== STORAGE_KEY || !event.newValue) return;
    if (event.newValue === currentThemeId.value) return;
    applyTheme(event.newValue as ThemeId);
  });
}

/** naive-ui 的主题覆盖：从同一套变量派生，避免两处颜色口径分裂。 */
export function naiveOverridesFor(theme: ThemeDef): GlobalThemeOverrides {
  const v = theme.vars;
  // 设计规范里的控件高度：28 / 32 / 36，全部组件共用同一组档位。
  const h = { tiny: '24px', small: '28px', medium: '32px', large: '36px' };
  return {
    common: {
      fontFamily: 'var(--font-sans)',
      fontFamilyMono: 'var(--font-mono)',
      fontSize: '13px',
      fontSizeSmall: '12px',
      fontSizeTiny: '11px',
      borderRadius: v['--r-2'],
      borderRadiusSmall: v['--r-1'],

      primaryColor: v['--sig'],
      primaryColorHover: v['--sig-hi'],
      primaryColorPressed: v['--sig-pressed'],
      primaryColorSuppl: v['--sig'],
      infoColor: v['--info'],
      successColor: v['--ok'],
      warningColor: v['--warn'],
      errorColor: v['--danger'],

      bodyColor: v['--bg-panel'],
      cardColor: v['--bg-module'],
      modalColor: v['--bg-module'],
      popoverColor: v['--bg-raise'],
      tableColor: v['--bg-module'],
      tableHeaderColor: v['--bg-raise'],
      inputColor: v['--bg-sunken'],
      inputColorDisabled: v['--bg-module'],
      actionColor: v['--bg-module'],
      tagColor: v['--bg-raise'],

      borderColor: v['--line'],
      dividerColor: v['--line-hair'],

      textColorBase: v['--ink-hi'],
      textColor1: v['--ink-hi'],
      textColor2: v['--ink'],
      textColor3: v['--ink-lo'],
      textColorDisabled: v['--ink-ghost'],
      placeholderColor: v['--ink-lo'],
      iconColor: v['--ink-lo'],
      iconColorHover: v['--ink-hi'],

      hoverColor: v['--wash-5'],
      boxShadow1: v['--shadow-module'],
      boxShadow2: v['--shadow-float'],
    },
    Button: {
      heightTiny: h.tiny,
      heightSmall: h.small,
      heightMedium: h.medium,
      heightLarge: h.large,
      paddingTiny: '0 8px',
      paddingSmall: '0 10px',
      paddingMedium: '0 14px',
      paddingLarge: '0 17px',
      fontSizeTiny: '11px',
      fontSizeSmall: '11px',
      fontSizeMedium: '12px',
      fontSizeLarge: '12px',
      fontWeight: '600',
      fontWeightStrong: '600',
      borderRadiusTiny: v['--r-1'],
      borderRadiusSmall: v['--r-2'],
      borderRadiusMedium: v['--r-2'],
      borderRadiusLarge: v['--r-3'],
      textColorPrimary: v['--on-sig'],
      textColorHoverPrimary: v['--on-sig'],
      textColorPressedPrimary: v['--on-sig'],
      textColorFocusPrimary: v['--on-sig'],
      borderPrimary: '1px solid transparent',
      borderHoverPrimary: '1px solid transparent',
      borderFocusPrimary: '1px solid transparent',
    },
    Input: {
      heightSmall: h.small,
      heightMedium: h.medium,
      heightLarge: h.large,
      borderRadius: v['--r-2'],
      color: v['--bg-sunken'],
      colorFocus: v['--bg-sunken'],
      colorDisabled: v['--bg-module'],
      border: `1px solid ${v['--line']}`,
      borderHover: `1px solid ${v['--sig-line']}`,
      borderFocus: `1px solid ${v['--sig']}`,
      // 聚焦只加 1px 环，不改变布局：与组件规范里的焦点环一致。
      boxShadowFocus: `0 0 0 1px ${v['--sig']}`,
      caretColor: v['--sig'],
      iconColor: v['--ink-lo'],
      suffixTextColor: v['--ink-lo'],
      countTextColor: v['--ink-ghost'],
    },
    InternalSelection: {
      heightSmall: h.small,
      heightMedium: h.medium,
      heightLarge: h.large,
      borderRadius: v['--r-2'],
      color: v['--bg-sunken'],
      colorActive: v['--bg-sunken'],
      textColor: v['--ink'],
      placeholderColor: v['--ink-lo'],
      border: `1px solid ${v['--line']}`,
      borderHover: `1px solid ${v['--sig-line']}`,
      borderFocus: `1px solid ${v['--sig']}`,
      boxShadowFocus: `0 0 0 1px ${v['--sig']}`,
      arrowColor: v['--ink-lo'],
    },
    Select: {
      menuBoxShadow: v['--shadow-float'],
    },
    DataTable: {
      borderColor: v['--line'],
      // 表头不再铺一层抬起色：预览稿里的表头与表格同底，只用一条底线分层，
      // 深色主题下满铺的 --bg-raise 会让每一张表看起来都压着一块更亮的板。
      thColor: 'transparent',
      thColorHover: 'transparent',
      thTextColor: v['--ink-lo'],
      thFontWeight: '600',
      thIconColor: v['--ink-lo'],
      sorterSize: '14px',
      tdTextColor: v['--ink'],
      tdColorHover: v['--wash-3'],
      borderRadius: v['--r-2'],
    },
    Card: {
      borderRadius: v['--r-4'],
      color: v['--bg-module'],
      borderColor: v['--line'],
      titleFontSizeMedium: '13px',
      titleFontWeight: '600',
    },
    Tabs: {
      tabFontSizeMedium: '11px',
      tabFontWeight: '600',
      barColor: v['--sig'],
    },
    Alert: {
      borderRadius: v['--r-2'],
      titleTextColor: v['--ink-hi'],
      contentTextColor: v['--ink'],
      padding: '10px 12px',
    },
    Progress: {
      railHeight: '5px',
      fillColor: v['--sig'],
      railColor: v['--wash-10'],
      textColorCircle: v['--ink'],
    },
    Tag: {
      heightSmall: '20px',
      fontSizeSmall: '10px',
      borderRadius: v['--r-1'],
      fontWeightStrong: '600',
      colorBordered: v['--bg-raise'],
      textColor: v['--ink'],
      border: `1px solid ${v['--line']}`,
    },
    Dialog: {
      borderRadius: v['--r-3'],
      color: v['--bg-module'],
      titleFontSize: '14px',
      titleFontWeight: '600',
      titleTextColor: v['--ink-hi'],
      textColor: v['--ink'],
      actionSpace: '8px',
      padding: '16px',
    },
    Modal: {
      color: v['--bg-module'],
      boxShadow: v['--shadow-float'],
    },
    Drawer: {
      borderRadius: '0',
      color: v['--bg-panel'],
      boxShadow: v['--shadow-float'],
    },
    Message: {
      color: v['--bg-raise'],
      borderRadius: v['--r-2'],
      boxShadow: v['--shadow-float'],
      padding: '8px 12px',
      textColor: v['--ink'],
      iconColorInfo: v['--info'],
      iconColorSuccess: v['--ok'],
      iconColorWarning: v['--warn'],
      iconColorError: v['--danger'],
    },
    Notification: {
      color: v['--bg-module'],
      borderRadius: v['--r-3'],
      boxShadow: v['--shadow-float'],
    },
    Dropdown: {
      borderRadius: v['--r-2'],
      optionHeightMedium: '32px',
      optionColorHover: v['--wash-4'],
      optionTextColor: v['--ink'],
      dividerColor: v['--line-hair'],
    },
    Popover: {
      color: v['--bg-raise'],
      borderRadius: v['--r-2'],
      boxShadow: v['--shadow-float'],
      textColor: v['--ink-hi'],
    },
    Tooltip: {
      color: v['--bg-raise'],
      textColor: v['--ink-hi'],
      borderRadius: v['--r-1'],
      boxShadow: v['--shadow-float'],
    },
    Upload: {
      draggerColor: v['--bg-sunken'],
      draggerBorder: `1px dashed ${v['--line-strong']}`,
      draggerBorderHover: `1px dashed ${v['--sig']}`,
      borderRadius: v['--r-2'],
    },
    Descriptions: {
      thColor: v['--bg-raise'],
      tdColor: v['--bg-module'],
      borderColor: v['--line'],
      thTextColor: v['--ink-lo'],
      tdTextColor: v['--ink'],
    },
    Pagination: {
      itemSizeMedium: '28px',
      itemBorderRadius: v['--r-1'],
      itemFontSizeMedium: '11px',
      itemTextColorActive: v['--on-sig'],
    },
    Switch: {
      railHeightMedium: '20px',
      buttonHeightMedium: '14px',
      railColor: v['--wash-16'],
      railColorActive: v['--sig'],
      buttonColor: theme.dark ? '#f4f7fb' : '#ffffff',
    },
    Checkbox: {
      sizeMedium: '16px',
      borderRadius: '2px',
      colorChecked: v['--sig'],
      borderChecked: `1px solid ${v['--sig']}`,
      checkMarkColor: v['--on-sig'],
    },
    Radio: {
      color: v['--sig'],
      dotColorActive: v['--sig'],
      // 分段的单选按钮组：选中项提亮一档，而不是整块涂成主色。
      buttonColor: 'transparent',
      buttonColorActive: v['--bg-raise'],
      buttonTextColor: v['--ink-lo'],
      buttonTextColorActive: v['--ink-hi'],
      buttonBorderColor: v['--line'],
      buttonBorderColorActive: v['--line-strong'],
    },
    Form: {
      labelTextColor: v['--ink-lo'],
      labelFontWeight: '500',
      feedbackTextColorError: v['--danger'],
    },
    Tree: {
      nodeTextColor: v['--ink'],
      nodeColorHover: v['--wash-4'],
    },
    Spin: {
      color: v['--sig'],
      textColor: v['--ink-lo'],
    },
    Skeleton: {
      color: v['--wash-6'],
      colorEnd: v['--wash-10'],
      borderRadius: '999px',
    },
    Slider: {
      railHeight: '4px',
      handleSize: '14px',
      railColor: v['--wash-10'],
      fillColor: v['--sig'],
      fillColorHover: v['--sig-hi'],
      handleColor: v['--sig'],
      handleBoxShadow: `0 0 0 2px ${v['--sig-wash']}`,
    },
    Collapse: {
      titleTextColor: v['--ink'],
      titleFontSize: '12px',
      titleFontWeight: '600',
      titlePadding: '10px 0',
      arrowColor: v['--ink-lo'],
    },
    Menu: {
      itemHeight: '32px',
      itemColorHover: v['--wash-4'],
      itemColorActive: v['--wash-6'],
      itemTextColor: v['--ink-lo'],
      itemTextColorActive: v['--ink-hi'],
      itemIconColor: v['--ink-lo'],
      borderRadius: v['--r-1'],
    },
    Badge: {
      color: v['--sig'],
      fontSize: '9px',
    },
    Divider: {
      color: v['--line-hair'],
      textColor: v['--ink-lo'],
    },
    DatePicker: {
      itemSize: '28px',
      itemFontSize: '12px',
      itemBorderRadius: v['--r-1'],
      itemTextColor: v['--ink'],
      itemTextColorActive: v['--on-sig'],
      itemTextColorCurrent: v['--sig'],
      itemColorActive: v['--sig'],
      itemColorHover: v['--wash-4'],
      itemColorIncluded: v['--wash-6'],
      panelColor: v['--bg-raise'],
      panelTextColor: v['--ink'],
      panelBoxShadow: v['--shadow-float'],
      panelBorderRadius: v['--r-2'],
      calendarTitleTextColor: v['--ink-hi'],
      calendarTitleFontWeight: '600',
      calendarDaysTextColor: v['--ink-lo'],
      panelHeaderDividerColor: v['--line-hair'],
      calendarDividerColor: v['--line-hair'],
      arrowColor: v['--ink-lo'],
      iconColor: v['--ink-lo'],
    },
    Popconfirm: {
      fontSize: '12px',
      iconColor: v['--warn'],
    },
    Empty: {
      fontSizeMedium: '12px',
      textColor: v['--ink-lo'],
      extraTextColor: v['--ink-lo'],
      iconColor: v['--ink-ghost'],
    },
  };
}
