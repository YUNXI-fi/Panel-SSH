<script setup lang="ts">
/**
 * 应用启动器。
 *
 * 桌面图标是常用应用的直达入口；启动器是可选入口，解决两件事：
 * 1. 应用多到图标网格放不下时，仍能在一个可搜索的列表里找到；
 * 2. 窗口盖住桌面后，不必先最小化窗口才能启动下一个应用。
 *
 * 两个入口共用 Desktop.vue 里同一份应用表与运行态，不在组件内部另建一套。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue';

interface LauncherApp {
  key: string;
  name: string;
  rail: string;
  /** 图标名：默认取 key；安全应用共用 key，但每款工具有自己的图标。 */
  glyph?: string;
  /** 桌面布局里的稳定标识，右键菜单据此定位要增删的图标。 */
  id: string;
}

const props = defineProps<{
  apps: LauncherApp[];
  /** 已打开应用的桌面 id；插件与安全应用共用 key，不能只比 appKey。 */
  runningKeys: string[];
}>();

const emit = defineEmits<{
  (e: 'launch', app: LauncherApp): void;
  (e: 'menu', payload: { app: LauncherApp; event: MouseEvent }): void;
  (e: 'close'): void;
}>();

const query = ref('');
const searchInput = ref<HTMLInputElement | null>(null);

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase();
  if (!q) return props.apps;
  return props.apps.filter(
    (app) => app.name.toLowerCase().includes(q) || app.key.toLowerCase().includes(q),
  );
});

function isRunning(app: LauncherApp): boolean {
  return props.runningKeys.includes(app.id);
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') emit('close');
}

onMounted(() => {
  searchInput.value?.focus();
  document.addEventListener('keydown', onKeydown);
});

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown);
});
</script>

<template>
  <div class="launcher" @click.self="emit('close')">
    <section class="launcher__panel" role="dialog" aria-modal="true" aria-label="应用启动器">
      <header class="launcher__head">
        <span class="launcher__title">APP LAUNCHER</span>
        <label class="launcher__search">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true">
            <circle cx="11" cy="11" r="7" />
            <path d="m20 20-3.5-3.5" />
          </svg>
          <input
            ref="searchInput"
            v-model="query"
            type="search"
            placeholder="搜索应用…"
            aria-label="搜索应用"
          />
        </label>
        <button class="launcher__close" type="button" title="关闭" aria-label="关闭启动器" @click="emit('close')">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
            <path d="M6 6l12 12M18 6 6 18" />
          </svg>
        </button>
      </header>

      <div class="launcher__grid">
        <button
          v-for="app in filtered"
          :key="`${app.key}-${app.name}`"
          type="button"
          class="launcher-tile"
          :class="{ 'is-running': isRunning(app) }"
          :style="{ '--rail': app.rail }"
          :title="app.name"
          :aria-label="`打开${app.name}`"
          @click="emit('launch', app)"
          @contextmenu.prevent.stop="emit('menu', { app, event: $event })"
        >
          <span class="launcher-tile__icon" aria-hidden="true">
            <svg><use :href="`#glyph-${app.glyph ?? app.key}`" /></svg>
          </span>
          <span class="launcher-tile__label">{{ app.name }}</span>
        </button>

        <p v-if="filtered.length === 0" class="launcher__empty">没有匹配的应用</p>
      </div>
    </section>
  </div>
</template>
