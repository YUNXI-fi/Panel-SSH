<script lang="ts">
/** 多序列模式下的一条曲线。 */
export interface SparkSeries {
  values: number[];
  color: string;
  /** 每个点对应的采样时刻（epoch 毫秒），用于识别时间上的缺口；长度与 values 一致。 */
  times?: number[];
  /** 序列名，只在悬停浮层里显示。多条曲线混在一起时，光靠颜色对不上号。 */
  name?: string;
}
</script>

<script setup lang="ts">
import { computed, ref } from 'vue';

const props = withDefaults(
  defineProps<{
    /** 单序列模式的数值（与 series 二选一）。 */
    values?: number[];
    /** 单序列模式的采样时刻，用于识别缺口。 */
    times?: number[];
    /** 多序列模式：多条曲线共用同一横轴与纵轴。 */
    series?: SparkSeries[];
    /** 纵轴上限；不传则取所有序列的最大值（至少为 1）。 */
    max?: number;
    /** 单序列模式的颜色。 */
    color?: string;
    /**
     * 相邻两点的间隔超过它就在图上断开（毫秒）。0 表示不做缺口判断。
     *
     * 采样是"按需触发"的：窗口被最小化、标签页切到后台、采集失败时都不会有点，
     * 而曲线默认把相邻两点连成直线 —— 跨越几分钟的两点连起来，看上去就是
     * "这段时间一直很平稳"，这是纯粹的误导。
     */
    gapMs?: number;
    /** 是否开启悬停十字线；多序列模式默认开启。 */
    interactive?: boolean;
    /**
     * 是否在背景画刻度网格。
     *
     * 默认关闭：网络、磁盘那几张图被 flex 压到 40px 高，网格线会和 1.5px 的折线
     * 抢注意力；只有合并图那类跨多个网格单元、高度充足的大图才需要刻度参照。
     */
    grid?: boolean;
    /**
     * 悬停时在指针旁浮出各条曲线的读数（含交点圆点）。
     *
     * 默认关闭，与十字线竖线分开：竖线只标"指针停在哪一列"，几乎不占地方，多序列图
     * 一律给；浮层列出数值要占一块面积，只有 180×90 这种小图之外的大家伙才合适。
     */
    readout?: boolean;
    /** 数值单位，拼在浮层的读数后面（如 '%'）。 */
    unit?: string;
    /**
     * 是否绘制 X/Y 坐标轴。
     *
     * 只给高度充足的合并图用：网络/磁盘那几张图被压到 40px，刻度文字会把曲线
     * 挤得没法看。开启后左侧留出 Y 轴刻度、底部留出时间标签的位置。
     */
    axes?: boolean;
  }>(),
  { color: '#38bdf8', gapMs: 0, unit: '' },
);

const emit = defineEmits<{ hover: [index: number] }>();

// 用 0..100 的归一化坐标绘制，再由 preserveAspectRatio="none" 拉伸到容器尺寸。
// 这样不必在 JS 里测量元素宽高，也就避免了 ResizeObserver 与重绘的相互触发。
//
// 绘图区刻意不贴满 viewBox：SVG 默认 overflow:hidden，描边压在视口边缘上会被
// 裁掉一半。零值基线若钉在 y=100，磁盘读写卡上下两图之间就像多了一条分隔线；
// 首尾点压在 x=0/100 也会被左右裁边。故四周留出内边距，填充区域仍延伸到底边
// （纯色填充被裁不可见），只有折线严格画在安全区内。
const PAD_TOP = 5;
/** 没有坐标轴时左右只留 1：描边不至于被视口裁掉即可。 */
const PAD_X_MIN = 1;
/** 有坐标轴时只在左侧留出刻度宽度；右侧没有文字，绘图区直接贴边。 */
const PAD_LEFT_AXES = 8;
const PAD_RIGHT_AXES = 0;
const PAD_BOTTOM_MIN = 6;
/** 有坐标轴时底部放时间标签，略收一点避免图被压得太扁。 */
const PAD_BOTTOM_AXES = 11;

const padLeft = computed(() => (props.axes ? PAD_LEFT_AXES : PAD_X_MIN));
const padRight = computed(() => (props.axes ? PAD_RIGHT_AXES : PAD_X_MIN));
const padBottom = computed(() => (props.axes ? PAD_BOTTOM_AXES : PAD_BOTTOM_MIN));
const plotHeight = computed(() => 100 - PAD_TOP - padBottom.value);
/** 绘图区的实际宽度（两个坐标轴的横向跨度）。 */
const plotWidth = computed(() => 100 - padLeft.value - padRight.value);

/**
 * 背景网格的横线与纵线坐标。
 *
 * 横线取绘图区的 0/25/50/75/100% 五档，与 Y 轴刻度一一对应 —— 刻度文字旁边
 * 必须有一条线，否则"50%"会浮在半空。纵线取同样的四等分点，作为 X 轴时间的参照。
 *
 * 网格必须和折线共用同一套内边距 —— 若让它铺满整个 viewBox，横线的零刻度会画在
 * y=100 而不是数据基线，纵线也会越过首尾数据点，看上去像"网格和曲线没对齐"。
 */
const GRID_RATIOS = [0, 0.25, 0.5, 0.75, 1];
const gridRows = computed(() => GRID_RATIOS.map((ratio) => PAD_TOP + ratio * plotHeight.value));
const gridCols = computed(() =>
  GRID_RATIOS.map((ratio) => padLeft.value + ratio * plotWidth.value),
);

/** 统一成序列数组：单序列只是"只有一个元素的数组"。 */
const list = computed<SparkSeries[]>(() => {
  if (props.series && props.series.length > 0) return props.series;
  return [{ values: props.values ?? [], color: props.color, times: props.times }];
});

/**
 * 横轴步长由最长的那条序列决定。
 *
 * 长度不一致时短序列**右对齐**（贴住"现在"这一端）而不是左对齐：几条曲线是
 * 同一台机器的同一段时间，右端永远是此刻。左对齐会让短的那条整体左移，
 * 在并排显示的单序列卡片里看不出来，一旦叠到同一张图上就是明显的错位。
 */
const maxLen = computed(() => Math.max(0, ...list.value.map((s) => s.values.length)));
const step = computed(() =>
  maxLen.value > 1 ? plotWidth.value / (maxLen.value - 1) : 0,
);

const upper = computed(() => {
  if (props.max && props.max > 0) return props.max;
  const all = list.value.flatMap((s) => s.values.filter((v) => Number.isFinite(v)));
  return Math.max(1, ...all);
});

function xAt(index: number): number {
  return step.value > 0 ? padLeft.value + index * step.value : 50;
}

/** 一段连续可画的折线。缺口与无数据处会被切成多段。 */
interface SparkSegment {
  points: string;
  fromX: number;
  toX: number;
}

function segmentsOf(values: number[], times: number[] | undefined): SparkSegment[] {
  const offset = maxLen.value - values.length;
  const limit = props.gapMs;
  const segments: SparkSegment[] = [];
  let current: string[] = [];
  let fromX = 0;
  let toX = 0;

  // 只有一个点的"段"画不出线，直接丢弃。
  function flush(): void {
    if (current.length > 1) segments.push({ points: current.join(' '), fromX, toX });
    current = [];
  }

  values.forEach((value, index) => {
    // 非有限值表示"这一刻没有数据"（速率基准还没建立、工具没给利用率）。
    // 它既不画点、也必须断开：否则会把缺口两侧连成一根斜线。
    if (!Number.isFinite(value)) {
      flush();
      return;
    }
    if (limit > 0 && times) {
      const previous = times[index - 1];
      const at = times[index];
      if (current.length > 0 && Number.isFinite(previous) && at - previous > limit) flush();
    }

    const x = xAt(offset + index);
    const ratio = Math.min(Math.max(value, 0), upper.value) / upper.value;
    const y = PAD_TOP + (1 - ratio) * plotHeight.value;
    if (current.length === 0) fromX = x;
    toX = x;
    current.push(`${x.toFixed(2)},${y.toFixed(2)}`);
  });

  flush();
  return segments;
}

/**
 * 面积填充只在单序列时画：多条曲线各带一块半透明填充，叠在一起会糊成一片，
 * 反而看不出每条线各自的走势。
 *
 * 每段单独封口（左右两端各自落到基线），而不是把整条序列当一块 ——
 * 否则缺口处会拉出两根竖直的长边，像是凭空出现了一段峰值。
 */
const geometry = computed(() =>
  list.value.map((s) => {
    const segments = segmentsOf(s.values, s.times);
    return {
      color: s.color,
      segments,
      areas:
        list.value.length === 1
          ? segments.map((seg) => `${seg.fromX},100 ${seg.points} ${seg.toX},100`)
          : [],
    };
  }),
);

const canHover = computed(() => props.interactive ?? list.value.length > 1);
const hoverIndex = ref(-1);
/** 指针在容器里的纵向位置（百分比）。浮层靠它跟着指针上下走。 */
const hoverY = ref(0);

/**
 * 指针位置换算成序列下标。
 *
 * 用绘图区的实际几何反推（而不是按整幅宽度线性取整），十字线才会正好落在
 * 数据点上 —— 左右各有 PAD_X 的内边距，按整幅宽度算会差一个点。
 */
function onMove(event: PointerEvent): void {
  if (!canHover.value || step.value <= 0) return;
  const rect = (event.currentTarget as SVGSVGElement).getBoundingClientRect();
  if (rect.width <= 0) return;
  const xPercent = ((event.clientX - rect.left) / rect.width) * 100;
  const index = Math.round((xPercent - padLeft.value) / step.value);
  hoverIndex.value = Math.min(Math.max(index, 0), maxLen.value - 1);
  hoverY.value = rect.height > 0 ? ((event.clientY - rect.top) / rect.height) * 100 : 0;
  emit('hover', hoverIndex.value);
}

function onLeave(): void {
  if (hoverIndex.value < 0) return;
  hoverIndex.value = -1;
  emit('hover', -1);
}

/** 某条序列在悬停下标处的局部下标；右对齐的短序列在左端会算出负值。 */
function localIndex(series: SparkSeries, index: number): number {
  return index - (maxLen.value - series.values.length);
}

/**
 * 十字准线与各条曲线的交点。
 *
 * 画成"零长度的线 + 圆头线帽"而不是 <circle>：视图被 preserveAspectRatio="none"
 * 非等比拉伸，几何图形会被压成椭圆，而 vector-effect 只保得住描边宽度 ——
 * 但非缩放描边下的圆头线帽恰好就是一个正圆，直径等于 stroke-width。
 */
const hoverMarks = computed(() => {
  const index = hoverIndex.value;
  if (!props.readout || index < 0 || step.value <= 0) return [];
  return list.value.flatMap((s) => {
    const value = s.values[localIndex(s, index)];
    if (!Number.isFinite(value)) return [];
    const ratio = Math.min(Math.max(value, 0), upper.value) / upper.value;
    return [
      {
        color: s.color,
        x: xAt(index),
        y: PAD_TOP + (1 - ratio) * plotHeight.value,
      },
    ];
  });
});

function clockOf(epochMs: number): string {
  const at = new Date(epochMs);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${pad(at.getHours())}:${pad(at.getMinutes())}:${pad(at.getSeconds())}`;
}

/** 悬停时刻。各序列共用一条时间轴，取第一条带时刻的即可。 */
const tipTime = computed(() => {
  const index = hoverIndex.value;
  if (index < 0) return '';
  for (const s of list.value) {
    const at = s.times?.[localIndex(s, index)];
    if (Number.isFinite(at)) return clockOf(at as number);
  }
  return '';
});

/**
 * Y 轴刻度：与五条横向网格线一一对应（0 / 25 / 50 / 75 / 100%）。
 *
 * 不能按 0..100 自己取整：绘图区顶部从 PAD_TOP 起、底部到 padBottom 止，
 * 刻度必须落在这套几何上，否则文字会与网格线错开。
 */
const yTicks = computed(() => {
  if (!props.axes) return [];
  const max = upper.value;
  return GRID_RATIOS.slice()
    .reverse()
    .map((ratio) => ({
      key: ratio,
      top: PAD_TOP + (1 - ratio) * plotHeight.value,
      text: `${Math.round(max * ratio)}${props.unit}`,
    }));
});

/**
 * X 轴刻度：与纵向网格同位置取时间戳（0/25/50/75/100%）。
 *
 * 时间取最长的那条序列 —— 短序列是右对齐的，用它换算会把首尾时刻标错。
 */
const xTicks = computed(() => {
  if (!props.axes) return [];
  const series = list.value.find((s) => s.values.length === maxLen.value) ?? list.value[0];
  const times = series?.times;
  if (!times || times.length < 2) return [];
  return GRID_RATIOS.map((ratio) => ({
    key: ratio,
    left: padLeft.value + ratio * plotWidth.value,
    text: clockOf(times[Math.round(ratio * (times.length - 1))]),
  }));
});

/**
 * 浮层里的读数。
 *
 * 那一刻没有数据的序列照样列出来（显示「—」）而不是悄悄跳过：三条曲线里少一条，
 * 看的人第一反应是"这条没显示"还是"这条是 0"，跳过会让答案模棱两可。
 */
const tipRows = computed(() => {
  const index = hoverIndex.value;
  if (index < 0) return [];
  return list.value.map((s) => {
    const value = s.values[localIndex(s, index)];
    return {
      name: s.name ?? '',
      color: s.color,
      text: Number.isFinite(value) ? `${value.toFixed(1)}${props.unit}` : '—',
    };
  });
});

/**
 * 浮层定位。
 *
 * 用百分比而不是像素：viewBox 经 preserveAspectRatio="none" 拉伸后，viewBox 坐标
 * 与容器尺寸是严格线性的，因此百分比定位不需要测量元素宽高（组件刻意不做测量，
 * 避免 ResizeObserver 与重绘互相触发）。
 *
 * 靠近右/左边缘时改锚点，靠近下半区时翻到指针上方 —— 否则浮层会被卡片裁掉。
 */
const tipStyle = computed(() => {
  const x = xAt(hoverIndex.value);
  const tx = x > 70 ? 'calc(-100% - 8px)' : x < 30 ? '8px' : '-50%';
  const ty = hoverY.value > 50 ? 'calc(-100% - 8px)' : '8px';
  return { left: `${x}%`, top: `${hoverY.value}%`, transform: `translate(${tx}, ${ty})` };
});
</script>

<template>
  <div class="sparkbox">
    <svg
      class="spark"
      viewBox="0 0 100 100"
      preserveAspectRatio="none"
      aria-hidden="true"
      @pointermove="onMove"
      @pointerleave="onLeave"
    >
      <!--
        背景网格画在最底层：填充区域（opacity 0.16）盖在它上面仍能透出来，
        折线则始终压在网格之上，不会被刻度线切开视觉连续性。
      -->
      <g v-if="grid" class="spark__grid">
        <line
          v-for="y in gridRows"
          :key="`h-${y}`"
          :x1="padLeft"
          :x2="100 - padRight"
          :y1="y"
          :y2="y"
          vector-effect="non-scaling-stroke"
        />
        <line
          v-for="x in gridCols"
          :key="`v-${x}`"
          class="spark__grid-col"
          :x1="x"
          :x2="x"
          :y1="PAD_TOP"
          :y2="100 - padBottom"
          vector-effect="non-scaling-stroke"
        />
      </g>
      <template v-for="(item, index) in geometry" :key="index">
        <polygon
          v-for="(area, areaIndex) in item.areas"
          :key="`area-${areaIndex}`"
          :points="area"
          :fill="item.color"
          opacity="0.16"
        />
        <polyline
          v-for="(segment, segmentIndex) in item.segments"
          :key="`line-${segmentIndex}`"
          :points="segment.points"
          fill="none"
          :stroke="item.color"
          stroke-width="1.5"
          stroke-linejoin="round"
          stroke-linecap="round"
          vector-effect="non-scaling-stroke"
        />
      </template>
      <!--
        十字准线的竖线。它不画圆点：视图被非等比拉伸，<circle> 会被压成椭圆，
        而 vector-effect 只保得住描边宽度、救不了几何形状 —— 交点上的圆点改由
        下面那条"零长度 + 圆头线帽"的线来画，它不受拉伸影响。
      -->
      <line
        v-if="hoverIndex >= 0 && step > 0"
        class="spark__cursor"
        :x1="xAt(hoverIndex)"
        :x2="xAt(hoverIndex)"
        y1="0"
        y2="100"
        vector-effect="non-scaling-stroke"
      />
      <line
        v-for="(mark, markIndex) in hoverMarks"
        :key="`mark-${markIndex}`"
        class="spark__mark"
        :x1="mark.x - 0.01"
        :x2="mark.x + 0.01"
        :y1="mark.y"
        :y2="mark.y"
        :stroke="mark.color"
        stroke-width="5"
        stroke-linecap="round"
        vector-effect="non-scaling-stroke"
      />
    </svg>
    <!--
      坐标轴标签用 HTML：svg 被 preserveAspectRatio="none" 非等比拉伸，
      <text> 会跟着变形。Y 轴刻度与横向网格线一一对应，X 轴时间与纵向网格线对齐。
    -->
    <template v-if="axes">
      <div class="spark__yaxis" :style="{ width: `${padLeft}%` }" aria-hidden="true">
        <span v-for="tick in yTicks" :key="tick.key" :style="{ top: `${tick.top}%` }">
          {{ tick.text }}
        </span>
      </div>
      <div v-if="xTicks.length" class="spark__xaxis" aria-hidden="true">
        <span v-for="tick in xTicks" :key="tick.key" :style="{ left: `${tick.left}%` }">
          {{ tick.text }}
        </span>
      </div>
    </template>
    <!--
      读数浮层用 HTML 而不是 <text>：SVG 文本同样会被非等比拉伸压变形，
      HTML 文字在浮层里保持正常字形。pointer-events 必须关掉，
      否则浮层压在 svg 上会吃掉 pointermove，十字线一动就抖。
    -->
    <div v-if="readout && hoverIndex >= 0" class="spark__tip" :style="tipStyle">
      <span v-if="tipTime" class="spark__tip-time">{{ tipTime }}</span>
      <template v-for="(row, rowIndex) in tipRows" :key="rowIndex">
        <span class="spark__tip-dot" :style="{ background: row.color }" />
        <span class="spark__tip-name">{{ row.name }}</span>
        <span class="spark__tip-value">{{ row.text }}</span>
      </template>
    </div>
  </div>
</template>

<style scoped>
/* 尺寸与定位锚点见 global.css 的 .sparkbox：它得和父容器的 flex 布局规则放在一起，
   写在组件里会被调用方的 .ovscard__chart 这类规则按源码顺序盖掉。 */

/* 触摸设备上横向拖动要能触发 pointermove，纵向仍交给窗口滚动 —— 监视窗口
   本身是可滚动的，把整块曲线区变成"吞掉滚动手势"的区域会很难受。 */
.spark {
  display: block;
  width: 100%;
  height: 100%;
  /* 有坐标轴时右端直接贴边：描边会越过 viewBox 半个像素，放行才不会缺一角。 */
  overflow: visible;
  touch-action: pan-y;
}

/* 网格取最淡的一档描边色：它只是背景刻度，不许跟高饱和的曲线抢注意力。
   线宽用 non-scaling-stroke 钉在 1 个设备像素，视图被非等比拉伸也不会变粗。 */
.spark__grid line {
  stroke: var(--line-hair);
  stroke-width: 1;
}

/* 纵线再淡一半：读量级靠横线，纵线只是让人对齐"这两条曲线的峰是不是同一时刻"。
   令牌里没有更淡的一档，直接写字面量；选择器必须带上 .spark__grid line，
   否则 specificity 输给上面那条，颜色改不动。 */
.spark__grid line.spark__grid-col {
  stroke: rgba(255, 255, 255, 0.03);
}

.spark__cursor {
  stroke: var(--ink-lo);
  stroke-width: 1;
  stroke-dasharray: 3 3;
}

/* 坐标轴刻度：等宽 9px，颜色取最淡的一档，不与曲线抢注意力。 */
.spark__yaxis {
  position: absolute;
  top: 0;
  bottom: 0;
  left: 0;
  pointer-events: none;
}

.spark__yaxis span {
  position: absolute;
  right: 5px;
  color: var(--ink-lo);
  font-family: var(--font-mono);
  font-size: 10px;
  line-height: 1;
  white-space: nowrap;
  transform: translateY(-50%);
}

/* X 轴刻度按百分比定位，与纵向网格线共用同一组坐标。 */
.spark__xaxis {
  position: absolute;
  right: 0;
  bottom: 1px;
  left: 0;
  height: 11px;
  color: var(--ink-lo);
  font-family: var(--font-mono);
  font-size: 10px;
  line-height: 1;
  pointer-events: none;
}

.spark__xaxis span {
  position: absolute;
  white-space: nowrap;
  transform: translateX(-50%);
}

/* 首尾贴边：居中会让文字越出绘图区，被卡片边缘裁掉。 */
.spark__xaxis span:first-child {
  transform: translateX(0);
}

.spark__xaxis span:last-child {
  transform: translateX(-100%);
}

/* 交点圆点的颜色由 :stroke 内联给（跟随各自曲线的颜色），这里只管尺寸语义：
   线段是零长度的，圆点直径完全由 stroke-width 决定，见模板里的说明。 */
.spark__mark {
  fill: none;
}

.spark__tip {
  position: absolute;
  z-index: 1;
  display: grid;
  grid-template-columns: auto minmax(0, auto) auto;
  gap: 2px var(--sp-2);
  align-items: center;
  padding: var(--sp-1) var(--sp-2);
  border: 1px solid var(--line);
  border-radius: var(--r-2);
  background: var(--bg-raise);
  box-shadow: var(--shadow-module);
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1.4;
  white-space: nowrap;
  /* 浮层压在 svg 上，必须让指针事件穿过去，否则十字线自己在抖。 */
  pointer-events: none;
}

/* 时刻占满整行，与下面的读数分开一层。 */
.spark__tip-time {
  grid-column: 1 / -1;
  color: var(--ink-lo);
}

.spark__tip-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
}

.spark__tip-name {
  color: var(--ink-lo);
}

.spark__tip-value {
  color: var(--ink-hi);
  text-align: right;
}
</style>
