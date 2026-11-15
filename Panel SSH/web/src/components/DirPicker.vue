<script setup lang="ts">
/**
 * 远端目录选择器。
 *
 * 只做一件事：在目标主机上逐层浏览目录，交回一个绝对路径。
 * 与文件管理窗口的区别是"只能选目录"——Compose 项目目录、数据目录这类输入
 * 需要的是一个可写路径，混进文件列表只会让点击目标变得含糊。
 */
import { computed, ref, watch } from 'vue';
import { NButton, NInput, NModal, NSpace } from 'naive-ui';

import { ApiError, api, type FileEntry } from '../api/client';

const props = defineProps<{
  show: boolean;
  hostId: number | null;
  /** 打开时优先定位到这里；不是绝对路径则从根目录开始。 */
  initialPath?: string;
}>();

const emit = defineEmits<{
  (e: 'update:show', value: boolean): void;
  (e: 'select', path: string): void;
}>();

const current = ref('/');
const inputPath = ref('/');
const parent = ref('');
const entries = ref<FileEntry[]>([]);
const loading = ref(false);
const error = ref('');

const dirs = computed(() => entries.value.filter((entry) => entry.isDir));

function describe(err: unknown): string {
  if (err instanceof ApiError) {
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return '读取目录失败';
}

async function load(path: string): Promise<void> {
  if (props.hostId === null) return;
  const target = path.trim() || '/';
  loading.value = true;
  error.value = '';
  try {
    const res = await api.files.list(props.hostId, target);
    current.value = res.path;
    parent.value = res.parent;
    entries.value = res.entries;
    inputPath.value = res.path;
  } catch (err) {
    error.value = describe(err);
  } finally {
    loading.value = false;
  }
}

// 每次打开都重新定位：上一次停留的位置对下一个输入框没有意义。
watch(
  () => props.show,
  (open) => {
    if (!open) return;
    const start = props.initialPath?.startsWith('/') ? props.initialPath : '/';
    void load(start);
  },
);

function goUp(): void {
  if (parent.value) void load(parent.value);
}

function enter(entry: FileEntry): void {
  void load(entry.path);
}

function submitPath(): void {
  void load(inputPath.value);
}

function confirm(): void {
  if (error.value) return;
  emit('select', current.value);
  emit('update:show', false);
}

function close(): void {
  emit('update:show', false);
}
</script>

<template>
  <n-modal
    :show="show"
    preset="card"
    title="选择目录"
    style="width: 640px"
    @update:show="emit('update:show', $event)"
  >
    <div class="dirpick">
      <div class="dirpick__bar">
        <n-button size="small" :disabled="!parent || loading" @click="goUp">上一级</n-button>
        <n-input
          v-model:value="inputPath"
          class="dirpick__input"
          size="small"
          placeholder="/（绝对路径）"
          aria-label="目录路径"
          @keyup.enter="submitPath"
        />
        <n-button size="small" :loading="loading" @click="submitPath">前往</n-button>
      </div>

      <p class="dirpick__path num" :title="current">{{ current }}</p>

      <div class="dirpick__list">
        <button
          v-for="dir in dirs"
          :key="dir.path"
          type="button"
          class="dirpick__row"
          @click="enter(dir)"
        >
          <svg class="dirpick__icon" aria-hidden="true"><use href="#glyph-folder" /></svg>
          <span class="dirpick__name num" :title="dir.name">{{ dir.name }}</span>
        </button>
        <p v-if="error" class="dirpick__hint is-error">{{ error }}</p>
        <p v-else-if="!loading && dirs.length === 0" class="dirpick__hint">这个目录下没有子目录</p>
      </div>
    </div>

    <template #footer>
      <n-space justify="end">
        <n-button @click="close">取消</n-button>
        <n-button type="primary" :disabled="loading || !!error" @click="confirm">
          选择此目录
        </n-button>
      </n-space>
    </template>
  </n-modal>
</template>

<style scoped>
.dirpick {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.dirpick__bar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.dirpick__input {
  flex: 1;
  min-width: 0;
}

/* 当前路径单独一行：输入框里的值可能被键盘改成还没生效的路径，这里只显示已加载的。 */
.dirpick__path {
  margin: 0;
  padding: 4px var(--sp-2);
  overflow: hidden;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  font-size: var(--fs-xs);
  color: var(--ink-lo);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dirpick__list {
  min-height: 220px;
  max-height: 320px;
  padding: var(--sp-1);
  overflow: auto;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-module);
}

.dirpick__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  width: 100%;
  padding: 6px var(--sp-2);
  border: 0;
  border-radius: var(--r-2);
  background: transparent;
  color: var(--ink);
  font-family: inherit;
  font-size: var(--fs-sm);
  text-align: left;
  cursor: pointer;
  transition: background var(--dur-1) var(--ease), color var(--dur-1) var(--ease);
}

.dirpick__row:hover {
  background: rgba(255, 255, 255, 0.04);
  color: var(--ink-hi);
}

.dirpick__icon {
  flex: 0 0 auto;
  width: 16px;
  height: 16px;
  color: var(--ink-lo);
}

.dirpick__row:hover .dirpick__icon {
  color: var(--sig);
}

.dirpick__name {
  overflow: hidden;
  min-width: 0;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dirpick__hint {
  margin: var(--sp-4) 0;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
  text-align: center;
}

.dirpick__hint.is-error {
  color: #ffb3b3;
}
</style>
