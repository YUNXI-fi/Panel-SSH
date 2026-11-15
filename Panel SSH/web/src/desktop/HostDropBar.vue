<script setup lang="ts">
/**
 * 拖拽文件时浮出的主机投放条。
 *
 * 为什么要有它：文件管理的行拖拽只在本窗口内有意义，而"把文件送到另一台机器"
 * 缺少一个可见的落点 —— 用户得先打开对方窗口，再找目录。这条浮层在拖拽期间
 * 把可选主机直接摆出来，松手就落到对方当前目录。
 *
 * 只在面板内部拖拽时出现：OS 文件拖进来是"上传到当前目录"，与跨主机无关。
 */
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { NDropdown } from 'naive-ui';

import { endFileDrag, internalDragPayload, onFileDragChange, type FileDragPayload } from '../windows/fileDrag';
import { api, type Host } from '../api/client';

const emit = defineEmits<{
  (event: 'transfer', payload: FileDragPayload, host: Host, mode: 'copy' | 'move'): void;
}>();

/** 拖拽中的载荷；松开后仍保留在 pending 里，等用户选完动作再清。 */
const dragging = ref<FileDragPayload | null>(null);
const pending = ref<FileDragPayload | null>(null);
const hosts = ref<Host[]>([]);
const overHostId = ref<number | null>(null);
const menu = ref({ show: false, x: 0, y: 0, host: null as Host | null });

let off: (() => void) | null = null;

onMounted(() => {
  off = onFileDragChange((payload) => {
    if (payload) {
      dragging.value = payload;
      void ensureHosts();
      return;
    }
    // dragend 在 drop 之后还会再触发一次：这时候菜单可能正等着用户选动作，
    // 不能把 pending 一起清掉，否则浮层闪一下就没了。
    dragging.value = null;
    if (!menu.value.show) pending.value = null;
  });
});

onBeforeUnmount(() => off?.());

async function ensureHosts(): Promise<void> {
  if (hosts.value.length > 0) return;
  try {
    hosts.value = await api.hosts.list();
  } catch {
    // 静默：取不到就只剩"没有其它主机"的提示，拖拽本身不受影响。
  }
}

/** 源主机不能作为目标：拖到自己身上没有意义。 */
function candidates(): Host[] {
  const source = dragging.value?.hostId ?? pending.value?.hostId;
  return hosts.value.filter((host) => host.id !== source);
}

function onDragOver(event: DragEvent, host: Host): void {
  event.preventDefault();
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'copy';
  overHostId.value = host.id;
}

function onDragLeave(host: Host): void {
  if (overHostId.value === host.id) overHostId.value = null;
}

function onDrop(event: DragEvent, host: Host): void {
  event.preventDefault();
  event.stopPropagation();
  overHostId.value = null;

  // dragend 会紧接着把模块状态清空，所以先把载荷存下来。
  const payload = internalDragPayload() ?? dragging.value;
  if (!payload) return;
  pending.value = payload;
  dragging.value = null;
  menu.value = { show: true, x: event.clientX, y: event.clientY, host };
}

function choose(action: 'copy' | 'move'): void {
  const host = menu.value.host;
  const payload = pending.value;
  menu.value = { ...menu.value, show: false };
  pending.value = null;
  if (!host || !payload) return;
  emit('transfer', payload, host, action);
  endFileDrag();
}
</script>

<template>
  <div v-if="dragging || pending" class="dropbar">
    <span class="dropbar__title">
      拖到主机上 · {{ (dragging ?? pending)?.paths.length }} 项
    </span>
    <div class="dropbar__hosts">
      <button
        v-for="host in candidates()"
        :key="host.id"
        class="dropbar__host"
        :class="{ 'is-over': overHostId === host.id }"
        type="button"
        @dragover="onDragOver($event, host)"
        @dragleave="onDragLeave(host)"
        @drop="onDrop($event, host)"
      >
        <span class="dropbar__name">{{ host.name }}</span>
        <span class="dropbar__addr">{{ host.addr }}</span>
      </button>
      <span v-if="candidates().length === 0" class="dropbar__empty">没有其它主机</span>
    </div>

    <n-dropdown
      :show="menu.show"
      :x="menu.x"
      :y="menu.y"
      :options="[
        { label: '复制到该主机', key: 'copy' },
        { label: '移动到该主机', key: 'move' },
      ]"
      trigger="manual"
      placement="bottom-start"
      @select="(key) => choose(key as 'copy' | 'move')"
      @clickoutside="menu.show = false"
    />
  </div>
</template>
