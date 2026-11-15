<script setup lang="ts">
import { computed, inject, onBeforeUnmount, provide, ref, type Component } from 'vue';

import {
  WindowBeforeCloseKey,
  WindowCloseKey,
  WindowManagerKey,
  WindowSubtitleKey,
  type AppWindow,
} from './useWindowManager';

const props = defineProps<{
  win: AppWindow;
  active: boolean;
  /** 应用身份色，只用于标题栏图标；任务记录窗口这类不属于桌面应用入口的窗口没有。 */
  rail?: string;
  /** 窗口内容组件，以及透传给它的入参。 */
  comp: Component;
  data: Record<string, unknown>;
}>();

const wm = inject(WindowManagerKey);

/** 标题栏图标：安全应用在 props 里带了自己的图形，其余仍按 appKey 取。 */
function windowGlyph(): string {
  const glyph = props.win.props.glyph;
  return typeof glyph === 'string' && glyph !== '' ? glyph : props.win.appKey;
}

/** 应用包自带的图标地址；有值时标题栏图标与桌面图标保持同一枚。 */
const windowIconUrl = computed(() => {
  const icon = props.win.props.iconUrl;
  return typeof icon === 'string' && icon !== '' ? icon : '';
});

const windowIconStyle = computed<Record<string, string>>(() => ({
  color: props.rail ?? 'var(--ink-hi)',
  '--icon-mask': `url(${windowIconUrl.value})`,
}));

/*
 * 把"写标题后缀"的能力递给内容组件。
 *
 * 内容组件必须是本组件的子实例，provide 才能穿透 —— 所以上面用 comp 渲染而不是
 * 收 slot：slot 内容的实例父级是定义 slot 的 Desktop，这里的 provide 看不到，
 * 窗口里的主机选择器就没办法把主机名写进标题。
 */
provide(WindowSubtitleKey, (text: string) => wm?.setSubtitle(props.win.id, text));

const emit = defineEmits<{
  (e: 'close' | 'minimize' | 'maximize' | 'focus', id: number): void;
  (e: 'move', id: number, x: number, y: number): void;
  (e: 'resize', id: number, w: number, h: number): void;
}>();

/**
 * 关闭前拦截。
 *
 * 内容组件（如文件查看器）在挂载时登记一个回调；关窗时先问它，它认为可以关
 * 再回调 closeSelf。没登记就照常关闭 —— 这条路径对绝大多数窗口完全不变。
 */
const beforeClose = ref<((next: () => void) => void) | null>(null);

function registerBeforeClose(handler: (next: () => void) => void): void {
  beforeClose.value = handler;
  // 内容组件卸载时清掉登记：窗口还在但内容换了的场景不该留着旧回调。
  onBeforeUnmount(() => {
    if (beforeClose.value === handler) beforeClose.value = null;
  });
}

function closeSelf(): void {
  emit('close', props.win.id);
}

function requestClose(): void {
  const handler = beforeClose.value;
  if (handler) {
    handler(closeSelf);
    return;
  }
  closeSelf();
}

provide(WindowCloseKey, requestClose);
provide(WindowBeforeCloseKey, registerBeforeClose);

interface DragState {
  mode: 'move' | 'resize';
  /** 缩放方向：右、下、右下角。拖动时对应的边长才跟着变化。 */
  edge: 'corner' | 'right' | 'bottom';
  startX: number;
  startY: number;
  origX: number;
  origY: number;
  origW: number;
  origH: number;
}

const drag = ref<DragState | null>(null);

// 使用 Pointer Events + setPointerCapture：拖拽过程中指针移出元素也
// 能持续收到事件，不必往 document 上挂全局监听再手动解绑。
function startDrag(mode: DragState['mode'], edge: DragState['edge'], event: PointerEvent): void {
  if (mode === 'move' && props.win.maximized) return;

  drag.value = {
    mode,
    edge,
    startX: event.clientX,
    startY: event.clientY,
    origX: props.win.x,
    origY: props.win.y,
    origW: props.win.w,
    origH: props.win.h,
  };
  (event.currentTarget as HTMLElement).setPointerCapture(event.pointerId);
  event.preventDefault();
}

function onPointerMove(event: PointerEvent): void {
  const state = drag.value;
  if (!state) return;

  const dx = event.clientX - state.startX;
  const dy = event.clientY - state.startY;

  if (state.mode === 'move') {
    emit('move', props.win.id, state.origX + dx, state.origY + dy);
    return;
  }

  // 只有被拖动的那条边参与变化：拖右边不该改变高度。
  const w = state.edge === 'bottom' ? state.origW : state.origW + dx;
  const h = state.edge === 'right' ? state.origH : state.origH + dy;
  emit('resize', props.win.id, w, h);
}

function stopDrag(): void {
  drag.value = null;
}

const frameStyle = computed(() => {
  if (props.win.maximized) {
    // 坐标原点在画布（左侧系统竖栏右侧）内，所以最大化就是铺满整个画布。
    // 右上角状态胶囊在存在最大化窗口时会被隐藏，这里不需要为它留避让带。
    return {
      left: '0px',
      top: '0px',
      width: '100%',
      height: '100%',
      zIndex: props.win.z,
    };
  }
  return {
    left: `${props.win.x}px`,
    top: `${props.win.y}px`,
    width: `${props.win.w}px`,
    height: `${props.win.h}px`,
    zIndex: props.win.z,
  };
});
</script>

<template>
  <section
    v-show="!win.minimized"
    class="win"
    :class="{ 'win--active': active }"
    :style="frameStyle"
    @pointerdown="emit('focus', win.id)"
  >
    <header
      class="win__bar"
      @pointerdown="startDrag('move', 'corner', $event)"
      @pointermove="onPointerMove"
      @pointerup="stopDrag"
      @pointercancel="stopDrag"
      @dblclick="emit('maximize', win.id)"
    >
      <!--
        左侧身份色条：常驻但压暗，聚焦时点亮。
        颜色只回答"这是哪个应用"，不承担其它语义。
      -->
      <span class="win__accent" :style="{ background: rail ?? 'var(--ink-ghost)' }" aria-hidden="true" />

      <!--
        图标名直接取窗口的 appKey：每个会开窗的 key 在精灵表里都有同名定义，
        因此不必再查一遍应用表。着色用身份色，与桌面图标、竖栏任务项同一套识别语言；
        查不到身份色的窗口（任务记录）回退到标题栏的默认字色。
      -->
      <span
        v-if="windowIconUrl"
        class="icon-mask win__glyph-img"
        :style="windowIconStyle"
        aria-hidden="true"
      />
      <svg v-else class="win__glyph" :style="{ color: rail }" aria-hidden="true">
        <use :href="`#glyph-${windowGlyph()}`" />
      </svg>

      <span class="win__title">{{ win.title }}</span>
      <span v-if="win.subtitle" class="win__sub">{{ win.subtitle }}</span>
      <span v-else class="win__sub" />
      <span class="win__tick" aria-hidden="true" />
      <div class="win__actions" @pointerdown.stop>
        <!--
          用内联 SVG 而不是 − □ × 三个字符：这三个码位的字形在不同平台
          基线位置与字重都不一致，标题栏按钮会忽高忽低。
        -->
        <button class="win__btn" title="最小化" aria-label="最小化" @click="emit('minimize', win.id)">
          <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
            <line x1="1.5" y1="5" x2="8.5" y2="5" stroke="currentColor" stroke-width="1.2" />
          </svg>
        </button>
        <button
          class="win__btn"
          :title="win.maximized ? '还原' : '最大化'"
          :aria-label="win.maximized ? '还原窗口' : '最大化窗口'"
          @click="emit('maximize', win.id)"
        >
          <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
            <template v-if="win.maximized">
              <rect x="1.5" y="3" width="5.5" height="5.5" fill="none" stroke="currentColor" stroke-width="1.2" />
              <path d="M4 3V1.5H8.5V6H7" fill="none" stroke="currentColor" stroke-width="1.2" />
            </template>
            <rect v-else x="1.8" y="1.8" width="6.4" height="6.4" fill="none" stroke="currentColor" stroke-width="1.2" />
          </svg>
        </button>
        <button
          class="win__btn win__btn--close"
          title="关闭"
          aria-label="关闭窗口"
          @click="requestClose"
        >
          <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
            <path d="M2 2l6 6M8 2l-6 6" stroke="currentColor" stroke-width="1.2" />
          </svg>
        </button>
      </div>
    </header>

    <div class="win__body">
      <component :is="comp" v-bind="data" />
    </div>

    <template v-if="!win.maximized">
      <div
        class="win__resize win__resize--corner"
        @pointerdown="startDrag('resize', 'corner', $event)"
        @pointermove="onPointerMove"
        @pointerup="stopDrag"
        @pointercancel="stopDrag"
      />
      <div
        class="win__resize win__resize--right"
        @pointerdown="startDrag('resize', 'right', $event)"
        @pointermove="onPointerMove"
        @pointerup="stopDrag"
        @pointercancel="stopDrag"
      />
      <div
        class="win__resize win__resize--bottom"
        @pointerdown="startDrag('resize', 'bottom', $event)"
        @pointermove="onPointerMove"
        @pointerup="stopDrag"
        @pointercancel="stopDrag"
      />
    </template>
  </section>
</template>
