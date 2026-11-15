<script setup lang="ts">
/**
 * 网络拓扑图。
 *
 * 按归属关系铺成一座金字塔：没有归属方的设备（网桥、独立网口、回环）在最上面一层，
 * 它们的成员落到下一层，再往下继续展开 —— 上窄下宽，一眼看得出这台机器的网络
 * 是围绕哪几个设备组织起来的。
 *
 * 为什么不按设备类别分三列摊开：类别只回答"这是个什么设备"，回答不了"谁挂在谁身上"。
 * 而排查断网先要看的恰恰是后者 —— 上游那座桥没起来时，挂在它下面的口会同时失去地址，
 * 这种"一整支子枝一起出问题"的形状，只有按归属排布才看得出来。
 *
 * 画法上刻意不用 SVG 手绘：节点里要放名字、类型、地址、链路速率这些会折行的文本，
 * 交给 HTML 排版，再用一层 SVG 覆盖层沿着量出来的节点边缘连线，比在 SVG 里自己算
 * 文本换行省事得多。连线在挂载后量一次，之后由 ResizeObserver 盯着容器 ——
 * 折行、字体回退都会让节点高度变，量过一次不代表一直准。
 *
 * 容器做成画布（滚轮缩放、按住拖动）而不是带滚动条的框：一台跑着几十个容器的机器，
 * 节点铺开能有十几列宽，靠滚动条看这种图得像读报纸一样来回挪。缩放让人先看清
 * 谁挂在谁身上这层结构，再放大去读某个口上的地址。
 */
import { computed, nextTick, onBeforeUnmount, onMounted, ref, type ComponentPublicInstance } from 'vue';
import { NButton } from 'naive-ui';

import type { NetInterface, OvsState } from '../api/client';
import { ledClass, ownerLabel, stateClass, stateLabel, typeLabel } from './netLabels';

const props = defineProps<{
  interfaces: NetInterface[];
  /**
   * OVS 现状，目标机没装 OVS 时为 null。
   *
   * OVS 端口的归属只能从这里读：网口一旦被 OVS 接管，转发就交给了用户态，
   * 内核里那个 master 字段是空的。
   */
  ovs: OvsState | null;
  /** 默认路由的出口设备名。 */
  defaultRouteDevs: string[];
}>();

/**
 * 每个接口的归属方（子 -> 父）。
 *
 * 两个来源：内核的 master 字段，以及 OVS 自己的端口表。一个接口只留一个父用来
 * 定位层次，其余关系仍然画线 —— 被两处同时引用的口本来就少见，为它把图形改成
 * 网状图得不偿失。
 */
const parentOf = computed(() => {
  const known = new Set(props.interfaces.map((iface) => iface.name));
  const map = new Map<string, string>();
  for (const iface of props.interfaces) {
    if (iface.master !== '' && known.has(iface.master)) map.set(iface.name, iface.master);
  }
  for (const bridge of props.ovs?.bridges ?? []) {
    if (!known.has(bridge.name)) continue;
    for (const port of bridge.ports) {
      if (port.name === bridge.name || !known.has(port.name)) continue;
      if (!map.has(port.name)) map.set(port.name, bridge.name);
    }
  }
  return map;
});

interface Wire {
  from: string;
  to: string;
  /** 由 OVS 维护的端口关系：内核里查不到，画成虚线。 */
  ovs: boolean;
}

const wires = computed<Wire[]>(() => {
  const known = new Set(props.interfaces.map((iface) => iface.name));
  const seen = new Set<string>();
  const list: Wire[] = [];
  const link = (from: string, to: string, ovs: boolean): void => {
    // 对不上的名字直接跳过：快照里可能只有 OVS 网桥而没有它那个内部口，
    // 端口名又是从 OVS 的库读来的，两边不一定对得上。
    if (from === to || !known.has(from) || !known.has(to)) return;
    const key = `${from}>${to}`;
    if (seen.has(key)) return;
    seen.add(key);
    list.push({ from, to, ovs });
  };

  for (const iface of props.interfaces) {
    if (iface.master !== '') link(iface.master, iface.name, false);
  }
  for (const bridge of props.ovs?.bridges ?? []) {
    for (const port of bridge.ports) link(bridge.name, port.name, true);
  }
  return list;
});

/**
 * 层次上限。
 *
 * 正常配置里 master 链不会成环，但真成环时递归就会打转 —— 拿一个上限兜住，
 * 代价只是极深的树被压平几层，而卡死是整个窗口的事。
 */
const MAX_LEVEL = 6;

function levelOf(name: string, cache: Map<string, number>): number {
  const cached = cache.get(name);
  if (cached !== undefined) return cached;
  // 先占位再往下算，遇到环也不会无限递归。
  cache.set(name, 0);
  const parent = parentOf.value.get(name);
  const level = parent === undefined ? 0 : Math.min(levelOf(parent, cache) + 1, MAX_LEVEL);
  cache.set(name, level);
  return level;
}

interface Row {
  level: number;
  items: NetInterface[];
}

const rows = computed<Row[]>(() => {
  const cache = new Map<string, number>();
  const buckets: NetInterface[][] = [];
  for (const iface of props.interfaces) {
    const level = levelOf(iface.name, cache);
    while (buckets.length <= level) buckets.push([]);
    buckets[level].push(iface);
  }

  /**
   * 层内按父节点在上一层的次序排。
   *
   * 同一个父的成员挨在一起，连线才会短而平行；不排的话（物理口和一堆 veth 交错），
   * 每条线都要横跨大半层去够自己的父节点，图上会缠成一团乱麻。
   */
  const index: Map<string, number>[] = [];
  return buckets.map((items, level) => {
    const previous = index[level - 1];
    const ordered =
      previous === undefined
        ? items
        : [...items].sort((a, b) => rankOf(a, previous) - rankOf(b, previous));
    index.push(new Map(ordered.map((iface, position) => [iface.name, position])));
    return { level, items: ordered };
  });
});

/** 父在上一层的位置；没有父或父不在上一层时排到最前。 */
function rankOf(iface: NetInterface, previous: Map<string, number>): number {
  const parent = parentOf.value.get(iface.name);
  return parent === undefined ? -1 : (previous.get(parent) ?? -1);
}

function isExit(name: string): boolean {
  return props.defaultRouteDevs.includes(name);
}

const viewport = ref<HTMLElement | null>(null);
const stage = ref<HTMLElement | null>(null);
/** 接口名 -> 节点元素。按名字索引而不是按下标：连线的两端就是这么找的。 */
const nodeEls = new Map<string, HTMLElement>();
const drawn = ref<{ d: string; ovs: boolean }[]>([]);

// ---- 画布：缩放与拖动 ----

/** 内容层的变换：先平移再缩放。原点钉在左上角，锚点缩放的算式才简单。 */
const scale = ref(1);
const offset = ref({ x: 0, y: 0 });
const dragging = ref(false);

const canvasStyle = computed(() => ({
  transform: `translate(${offset.value.x}px, ${offset.value.y}px) scale(${scale.value})`,
}));

/**
 * 缩放范围。
 *
 * 下限再小，节点上的地址就糊成一团灰；上限再大，一屏只看得到一个网口，
 * 反而不知道它在整张图的什么位置。
 */
const MIN_SCALE = 0.25;
const MAX_SCALE = 2.5;

/** 适应窗口时四周留的余量（像素）。 */
const FIT_PADDING = 24;

function clampScale(value: number): number {
  return Math.min(MAX_SCALE, Math.max(MIN_SCALE, value));
}

function bindNode(name: string, el: Element | ComponentPublicInstance | null): void {
  if (el instanceof HTMLElement) nodeEls.set(name, el);
  else nodeEls.delete(name);
}

/**
 * 量出父节点底边与子节点顶边的中点，连成一条竖向的曲线。
 *
 * 坐标一律取 offset 系列（布局值）而不是 getBoundingClientRect：后者给的是
 * 变换之后的屏幕坐标，画布一旦缩放平移，量出来的位置就跟着一起变 —— 于是
 * 缩放一次就得重测一次，而拖动时每帧都在变。offset 系列与画布变换无关，
 * 量一次可以一直用。
 */
function measure(): void {
  if (stage.value === null) return;
  const next: { d: string; ovs: boolean }[] = [];
  for (const wire of wires.value) {
    const from = nodeEls.get(wire.from);
    const to = nodeEls.get(wire.to);
    if (from === undefined || to === undefined) continue;
    const x1 = from.offsetLeft + from.offsetWidth / 2;
    const y1 = from.offsetTop + from.offsetHeight;
    const x2 = to.offsetLeft + to.offsetWidth / 2;
    const y2 = to.offsetTop;
    // 控制点先直着往下拉一段再拐向目标：线从父节点底部笔直出来、落到子节点顶部，
    // 只有中段是斜的，几个兄弟节点并排时看着才像从同一条干线上分出去的。
    const bend = Math.max(14, (y2 - y1) * 0.45);
    next.push({
      ovs: wire.ovs,
      d: `M ${x1} ${y1} C ${x1} ${y1 + bend}, ${x2} ${y2 - bend}, ${x2} ${y2}`,
    });
  }
  drawn.value = next;
}

let observer: ResizeObserver | null = null;

/**
 * 把整张图缩到放得下并居中。
 *
 * 打开弹窗和点"适应窗口"都走这里。缩放上限卡在 1：图本来就比窗口小的时候
 * 不该被放大 —— 那只会把字拉糊，并不能让人看得更清楚。
 *
 * 用 offsetWidth/offsetHeight 而不是 getBoundingClientRect：后者带着当前
 * 的缩放，拿它算比例会自我放大，滚一次轮就再也回不到 1。
 */
function fit(): void {
  const view = viewport.value;
  const host = stage.value;
  if (view === null || host === null) return;
  const width = host.offsetWidth;
  const height = host.offsetHeight;
  if (width === 0 || height === 0) return;
  const next = clampScale(
    Math.min(1, (view.clientWidth - FIT_PADDING * 2) / width, (view.clientHeight - FIT_PADDING * 2) / height),
  );
  scale.value = next;
  offset.value = {
    x: (view.clientWidth - width * next) / 2,
    y: (view.clientHeight - height * next) / 2,
  };
}

/**
 * 以指针位置为锚点缩放。
 *
 * 先算出指针底下那个内容坐标，缩完之后再把那个点挪回指针底下 —— 不这么做的话，
 * 放大时图会从左上角往外长，鼠标指着的那座网桥会自己跑掉，越缩越找不到。
 */
function onWheel(event: WheelEvent): void {
  const view = viewport.value;
  if (view === null) return;
  const rect = view.getBoundingClientRect();
  const px = event.clientX - rect.left;
  const py = event.clientY - rect.top;
  // 指数缩放：每格滚轮的倍率恒定，从 1 缩到 0.5 与从 0.5 缩到 0.25 手感一致。
  const next = clampScale(scale.value * Math.exp(-event.deltaY * 0.0015));
  if (next === scale.value) return;
  const contentX = (px - offset.value.x) / scale.value;
  const contentY = (py - offset.value.y) / scale.value;
  scale.value = next;
  offset.value = { x: px - contentX * next, y: py - contentY * next };
}

// 拖动：记下按下那一刻的指针位置与位移基准，之后按增量走。

let dragFrom = { x: 0, y: 0 };
let dragOrigin = { x: 0, y: 0 };

function onPointerDown(event: PointerEvent): void {
  // 只接左键：右键要留给上下文菜单，中键有自己的用途。
  if (event.button !== 0) return;
  const view = viewport.value;
  if (view === null) return;
  dragging.value = true;
  dragFrom = { x: event.clientX, y: event.clientY };
  dragOrigin = { ...offset.value };
  // 把指针扣在这块元素上：拖出画布边界、掠过别的元素时事件仍然回到这里，
  // 不会拖到一半因为指针离开而断掉。
  view.setPointerCapture(event.pointerId);
}

function onPointerMove(event: PointerEvent): void {
  if (!dragging.value) return;
  offset.value = {
    x: dragOrigin.x + (event.clientX - dragFrom.x),
    y: dragOrigin.y + (event.clientY - dragFrom.y),
  };
}

function onPointerUp(event: PointerEvent): void {
  if (!dragging.value) return;
  dragging.value = false;
  const view = viewport.value;
  if (view?.hasPointerCapture(event.pointerId)) view.releasePointerCapture(event.pointerId);
}

onMounted(() => {
  // 首帧渲染完才有尺寸可量、也才谈得上缩放。
  void nextTick(() => {
    measure();
    fit();
  });
  if (stage.value !== null && typeof ResizeObserver !== 'undefined') {
    observer = new ResizeObserver(() => measure());
    observer.observe(stage.value);
  }
});

onBeforeUnmount(() => {
  observer?.disconnect();
  observer = null;
});
</script>

<template>
  <div class="topo">
    <div class="topo__legend">
      <span class="topo__key"><span class="topo__dash" />Linux 归属</span>
      <span class="topo__key"><span class="topo__dash is-ovs" />OVS 端口</span>
      <span class="topo__key"><span class="led led--ok" />已连接</span>
      <span class="topo__key"><span class="led led--warn" />无载波</span>
      <span class="topo__key"><span class="led" />已停用</span>
      <span class="topo__key topo__hint">滚轮缩放 · 拖动平移</span>
    </div>

    <div
      ref="viewport"
      class="topo__canvas"
      :class="{ 'is-dragging': dragging }"
      @wheel.prevent="onWheel"
      @pointerdown="onPointerDown"
      @pointermove="onPointerMove"
      @pointerup="onPointerUp"
      @pointercancel="onPointerUp"
    >
      <div ref="stage" class="topo__stage" :style="canvasStyle">
        <!-- 连线层压在节点下面，也不接鼠标事件：它只是把归属说清楚，
             不该盖住节点上的字，更不该在划过时抢走指针。 -->
        <svg class="topo__wires" aria-hidden="true">
          <path
            v-for="(wire, index) in drawn"
            :key="index"
            :d="wire.d"
            :class="{ 'is-ovs': wire.ovs }"
          />
        </svg>

        <section v-for="row in rows" :key="row.level" class="topo__row">
          <article
            v-for="iface in row.items"
            :key="iface.name"
            :ref="(el) => bindNode(iface.name, el)"
            class="toponode"
            :class="stateClass(iface)"
          >
            <header class="toponode__head">
              <span class="led" :class="ledClass(iface)" aria-hidden="true" />
              <span class="toponode__name num">{{ iface.name }}</span>
              <span v-if="isExit(iface.name)" class="toponode__exit">默认出口</span>
            </header>

            <div class="toponode__meta">
              <span class="toponode__type">{{ typeLabel(iface.type) }}</span>
              <span v-if="ownerLabel(iface.owner)" class="toponode__owner">
                {{ ownerLabel(iface.owner) }}
              </span>
              <span class="toponode__state">{{ stateLabel(iface) }}</span>
            </div>

            <div class="toponode__addrs">
              <span
                v-for="addr in iface.addrs"
                :key="`${addr.family}-${addr.addr}`"
                class="toponode__addr num"
                :class="{ 'is-v6': addr.family === 'ipv6' }"
              >
                {{ addr.addr }}/{{ addr.prefix }}
              </span>
              <span v-if="iface.addrs.length === 0" class="toponode__addr is-empty">未配置地址</span>
            </div>

            <span v-if="iface.speedMbps > 0" class="toponode__link num">
              {{ iface.speedMbps }} Mb/s
            </span>
          </article>
        </section>
      </div>
    </div>

    <!-- 图会被拖出画布之外，得有个一键回到"全都能看见"的把手，否则手一抖就找不回来了。
         它挂在画布之外：指针被画布捕获之后，click 也会被一并转到画布上，放在里面点不动。 -->
    <n-button class="topo__fit" size="tiny" @click="fit">适应窗口</n-button>
  </div>
</template>

<style scoped>
/*
 * 图例与画布一上一下。
 *
 * 图例不参与缩放拖动：它是读图的钥匙（哪条线是 OVS 的、哪种灯是通的），
 * 跟着图一起跑掉的话，缩小时就没人知道那些颜色在说什么了。
 */
.topo {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  height: 100%;
}

.topo__legend {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-4);
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

.topo__key {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

/* 用法说明推到最右：它是操作提示，不和左侧的状态图例混在一起读。 */
.topo__hint {
  margin-left: auto;
  color: var(--ink-ghost);
}

/*
 * 画布本体。
 *
 * 图比它大就裁掉，交给缩放与拖动去够，外层不再滚动 —— 页面滚动和画布变换
 * 两套叠在一起时，滚轮到底是在滚页面还是缩图全靠运气。
 */
.topo__canvas {
  position: relative;
  flex: 1;
  min-height: 0;
  overflow: hidden;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  cursor: grab;
  /* 不选中：拖动时按住节点会把地址刷成一片高亮。 */
  user-select: none;
  /* 交出触摸手势：不声明的话手指拖的是页面，不是这张图。 */
  touch-action: none;
}

.topo__canvas.is-dragging {
  cursor: grabbing;
}

/*
 * 内容层：它按内容尺寸撑开，再整体缩放平移。
 *
 * 一层一行、逐层往下，行间距要留得下连线，太挤就只剩一堆挨着的方框。
 *
 * 宽度必须写死 max-content：这是绝对定位的元素，auto 宽度会被画布宽度卡住，
 * 而层被压窄之后节点会挤行，量出来的连线和眼睛看到的就对不上了。
 */
.topo__stage {
  position: absolute;
  top: 0;
  left: 0;
  width: max-content;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 44px;
  /* 原点钉在左上角：缩放与平移都基于它算，见 onWheel。 */
  transform-origin: 0 0;
  will-change: transform;
}

/*
 * 浮在画布右下角。
 *
 * 定位基准是 .topo 而不是画布：画布是它最后一行子元素，右下角正好重合，
 * 而挂在画布之外就避开了指针捕获把 click 一起转走的问题。
 */
.topo__fit {
  position: absolute;
  right: var(--sp-2);
  bottom: var(--sp-2);
  z-index: 1;
}

/* 图例里的线段与下面的连线一一对应，改连线得回来一起改。 */
.topo__dash {
  width: 18px;
  height: 0;
  border-top: 1px solid var(--ink-ghost);
}

.topo__dash.is-ovs {
  border-top-color: var(--sig);
  border-top-style: dashed;
}

.topo__wires {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  pointer-events: none;
}

.topo__wires path {
  fill: none;
  stroke: var(--ink-ghost);
  stroke-width: 1;
}

.topo__wires path.is-ovs {
  stroke: var(--sig);
  stroke-dasharray: 4 4;
}

.topo__row {
  display: flex;
  align-items: flex-start;
  gap: var(--sp-3);
}

/*
 * 节点宽度写死。
 *
 * 让内容自己撑的话，同一层里放 IPv6 长地址的节点会明显更宽，几个兄弟节点并排时
 * 连线的落点就参差不齐；写死宽度后每个节点的中线都在等距的格点上，连线是平行的。
 * flex: none 保证它不被压扁 —— 这一层特别宽时该做的是把整张图缩下来看，
 * 不是把节点挤小到读不出名字。
 */
.toponode {
  display: flex;
  flex: none;
  flex-direction: column;
  gap: 4px;
  width: 200px;
  padding: var(--sp-2);
  border: 1px solid var(--line-hair);
  border-left: 2px solid var(--ink-ghost);
  border-radius: var(--r-2);
  background: var(--bg-module);
}

/* 链路状态用左边缘那道竖线表达，与接口卡片上的灯是同一套语义：
   扫一层下来就知道哪几个口是通的，不必逐个去读状态文字。 */
.toponode.is-up {
  border-left-color: var(--ok);
}

.toponode.is-idle {
  border-left-color: var(--warn);
}

.toponode__head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.toponode__name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  font-size: var(--fs-sm);
  color: var(--ink-hi);
  white-space: nowrap;
}

.toponode__exit {
  flex: none;
  font-size: var(--fs-label);
  color: var(--warn);
}

.toponode__meta {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.toponode.is-up .toponode__state {
  color: var(--ok);
}

.toponode.is-idle .toponode__state {
  color: var(--warn);
}

.toponode__owner {
  padding: 0 4px;
  border-radius: var(--r-1);
  background: var(--bg-sunken);
}

.toponode__addrs {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

/* IPv6 比 IPv4 长得多，一行放不下时宁可折行也不省略：地址折了还能读全，
   截断之后只剩前半段，反而没法用来核对。 */
.toponode__addr {
  overflow-wrap: anywhere;
}

.toponode__addr.is-v6,
.toponode__addr.is-empty {
  color: var(--ink-ghost);
}

.toponode__link {
  font-size: var(--fs-label);
  color: var(--ink-ghost);
}
</style>