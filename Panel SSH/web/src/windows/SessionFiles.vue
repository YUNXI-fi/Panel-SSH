<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import {
  NButton,
  NDropdown,
  NInput,
  NModal,
  NSpace,
  useDialog,
  useMessage,
  type DropdownOption,
} from 'naive-ui';

import { ApiError, api, triggerDownload, type FileEntry, type FileListing } from '../api/client';
import { formatBytes } from '../api/format';
import FileDropMenu from '../components/FileDropMenu.vue';
import { beginFileDrag, endFileDrag, internalDragPayload, type FileDragPayload } from './fileDrag';
import { crossHostTransfer, type CrossHostProgress, type CrossHostResult } from './fileTransfer';
import { notifyFilesChanged, onFilesChanged } from './fileEvents';

/**
 * 终端窗口左侧的 SFTP 文件栏（MobaXterm 的标志性布局）。
 *
 * 它是"边敲命令边传文件"用的，不是第二个文件管理器：导航、上传、下载，
 * 外加右键菜单里的重命名/删除/新建目录。后者虽然「文件管理」窗口也有，
 * 但在这里对着目标文件右键是更短的路径 —— 前提是确认框走全局 dialog
 * 而不是挤在 240px 宽的侧栏里，那边宽度不够铺开一句警示。
 */
const props = defineProps<{ hostId: number; hostName: string }>();

const message = useMessage();
const dialog = useDialog();

const path = ref('/');
const listing = ref<FileListing>({ path: '/', parent: '', entries: [] });
const loading = ref(false);
const error = ref('');
const selected = ref<string | null>(null);
const uploading = ref(false);
const transfer = ref<{ label: string; loaded: number; total: number; note?: string } | null>(null);
const fileInput = ref<HTMLInputElement | null>(null);

/**
 * 目录在前、同类按名称排。
 *
 * 排序放在前端而不是依赖后端顺序：这是纯粹的显示偏好，而且列表本来就已经
 * 整份拿回来了，再发一次请求只为换个次序不值当。
 */
const entries = computed(() =>
  [...listing.value.entries].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    return a.name.localeCompare(b.name, 'zh-Hans-CN');
  }),
);

const selectedEntry = computed(
  () => entries.value.find((item) => item.path === selected.value) ?? null,
);

const percent = computed(() => {
  const current = transfer.value;
  if (!current || current.total <= 0) return null;
  return Math.min(100, Math.round((current.loaded / current.total) * 100));
});

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    // 远端未响应是 504，与普通读写失败要给不同的建议。
    if (err.status === 504) return `${err.message}。请检查目标机是否可达`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

async function load(dir?: string): Promise<void> {
  loading.value = true;
  error.value = '';
  try {
    listing.value = await api.files.list(props.hostId, dir ?? path.value);
    path.value = listing.value.path;
    selected.value = null;
  } catch (err) {
    error.value = describe(err, '读取目录失败');
  } finally {
    loading.value = false;
  }
}

// 切换会话时侧栏跟着换机器：旧机器的目录树留在屏上没有意义，
// 而且下一次点击会把它当成新机器的路径发出去。
watch(
  () => props.hostId,
  () => {
    path.value = '/';
    void load('/');
  },
  { immediate: true },
);

function open(entry: FileEntry): void {
  if (entry.isDir) {
    void load(entry.path);
    return;
  }
  selected.value = entry.path;
}

async function download(entry: FileEntry): Promise<void> {
  transfer.value = { label: entry.name, loaded: 0, total: entry.size };
  try {
    const blob = await api.files.download(props.hostId, entry.path, (loaded, total) => {
      transfer.value = { label: entry.name, loaded, total: total || entry.size };
    });
    const url = URL.createObjectURL(blob);
    triggerDownload(url, entry.name);
    // 不能立刻撤销：浏览器可能还没把这段 Blob 读完，那样落盘的是 0 字节文件。
    window.setTimeout(() => URL.revokeObjectURL(url), 60_000);
  } catch (err) {
    error.value = describe(err, '下载失败');
  } finally {
    transfer.value = null;
  }
}

function pickFiles(): void {
  fileInput.value?.click();
}

async function onFilesPicked(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement;
  const files = Array.from(input.files ?? []);
  // 立即清空，否则连续选择同一个文件不会再次触发 change。
  input.value = '';
  if (files.length === 0) return;

  uploading.value = true;
  const total = files.reduce((sum, file) => sum + file.size, 0);
  const label = files.length === 1 ? files[0].name : `${files.length} 个文件`;
  transfer.value = { label, loaded: 0, total };
  try {
    await api.files.upload(props.hostId, listing.value.path, files, (loaded, sent) => {
      transfer.value = { label, loaded, total: sent };
    });
    await load();
  } catch (err) {
    error.value = describe(err, '上传失败');
  } finally {
    uploading.value = false;
    transfer.value = null;
  }
}

// ---- 右键菜单 ----

const menu = ref({ show: false, x: 0, y: 0 });
/** 右键点中的条目；为空表示点的是空白处，那就只给"新建目录/刷新"。 */
const menuTarget = ref<FileEntry | null>(null);

const menuOptions = computed<DropdownOption[]>(() => {
  const entry = menuTarget.value;
  const options: DropdownOption[] = [];
  if (entry) {
    // 第一项永远是"对着这东西能干什么"：目录是进去，文件是取回来。
    options.push({ label: entry.isDir ? '进入目录' : '下载', key: 'open' });
    options.push({ label: '重命名', key: 'rename' });
    options.push({ label: '删除', key: 'remove' });
    options.push({ key: 'd1', type: 'divider' });
  }
  options.push({ label: '新建目录', key: 'mkdir' });
  options.push({ label: '刷新', key: 'refresh' });
  return options;
});

function openMenu(entry: FileEntry | null, event: MouseEvent): void {
  event.preventDefault();
  menuTarget.value = entry;
  // 右键顺带选中：菜单项作用于哪一行，屏幕上得看得出来。
  if (entry) selected.value = entry.path;
  menu.value = { show: true, x: event.clientX, y: event.clientY };
}

function onMenuSelect(key: string): void {
  const entry = menuTarget.value;
  menu.value = { ...menu.value, show: false };

  switch (key) {
    case 'open':
      if (entry) open(entry);
      break;
    case 'rename':
      if (entry) startRename(entry);
      break;
    case 'remove':
      if (entry) confirmRemove(entry);
      break;
    case 'mkdir':
      mkdirName.value = '';
      showMkdir.value = true;
      break;
    case 'refresh':
      void load();
      break;
    default:
      break;
  }
}

// ---- 新建目录 / 重命名 / 删除 ----

const showMkdir = ref(false);
const mkdirName = ref('');
const showRename = ref(false);
const renameTarget = ref<FileEntry | null>(null);
const renameName = ref('');

function joinPath(dir: string, name: string): string {
  return dir === '/' ? `/${name}` : `${dir}/${name}`;
}

async function submitMkdir(): Promise<void> {
  const name = mkdirName.value.trim();
  if (!name) {
    message.warning('请输入目录名');
    return;
  }
  if (name.includes('/')) {
    message.warning('目录名不能包含 /');
    return;
  }

  try {
    await api.files.mkdir(props.hostId, joinPath(listing.value.path, name));
    showMkdir.value = false;
    message.success('目录已创建');
    await load();
  } catch (err) {
    message.error(describe(err, '创建目录失败'));
  }
}

function startRename(entry: FileEntry): void {
  renameTarget.value = entry;
  renameName.value = entry.name;
  showRename.value = true;
}

async function submitRename(): Promise<void> {
  const entry = renameTarget.value;
  if (!entry) return;

  const name = renameName.value.trim();
  // 名字没改就当用户放弃：不必为一个没发生的改动去打扰远端。
  if (!name || name === entry.name) {
    showRename.value = false;
    return;
  }
  if (name.includes('/')) {
    message.warning('名称不能包含 /');
    return;
  }

  try {
    await api.files.rename(props.hostId, entry.path, joinPath(listing.value.path, name));
    showRename.value = false;
    message.success('已重命名');
    await load();
  } catch (err) {
    message.error(describe(err, '重命名失败'));
  }
}

/** 删除不可恢复，先问清楚；目录还要点明是"连内容一起删"。 */
function confirmRemove(entry: FileEntry): void {
  dialog.warning({
    title: '删除确认',
    content: entry.isDir
      ? `「${entry.name}」是目录，其中的内容会一并删除，且不可恢复。`
      : `「${entry.name}」将被删除，且不可恢复。`,
    positiveText: '删除',
    negativeText: '取消',
    // 返回 Promise：naive-ui 会在删除期间给按钮上 loading，用户知道它正在做。
    onPositiveClick: () => removeEntry(entry),
  });
}

async function removeEntry(entry: FileEntry): Promise<void> {
  try {
    await api.files.remove(props.hostId, [entry.path]);
    message.success('已删除');
    await load();
  } catch (err) {
    message.error(describe(err, '删除失败'));
  }
}

// ---- 跨主机拖拽搬运 ----

/** 侧栏里的行不是目录就是文件，拖出去时只带这一行。 */
function beginRowDrag(event: DragEvent, entry: FileEntry): void {
  beginFileDrag(event.dataTransfer, {
    hostId: props.hostId,
    hostName: props.hostName,
    paths: [entry.path],
  });
}

/** 正在被拖到其上的目录；用来把落点画出来。 */
const dropDir = ref<string | null>(null);
/** 拖到了列表空白处：落点是当前目录。 */
const listDrop = ref(false);

const dropMenu = ref<{
  show: boolean;
  x: number;
  y: number;
  dest: string;
  payload: FileDragPayload | null;
}>({ show: false, x: 0, y: 0, dest: '', payload: null });

function parentOf(p: string): string {
  const index = p.lastIndexOf('/');
  return index <= 0 ? '/' : p.slice(0, index);
}

function openDropMenu(event: DragEvent, dest: string): void {
  const payload = internalDragPayload();
  if (!payload) return;
  // 同一台机器、同一个目录：搬过去等于什么都没做，直接说清楚。
  if (payload.hostId === props.hostId && payload.paths.every((item) => parentOf(item) === dest)) {
    message.info('目标目录与来源相同');
    return;
  }
  dropMenu.value = { show: true, x: event.clientX, y: event.clientY, dest, payload };
}

function onRowDragOver(event: DragEvent, entry: FileEntry): void {
  if (!entry.isDir || !internalDragPayload()) return;
  event.preventDefault();
  event.stopPropagation();
  dropDir.value = entry.path;
  listDrop.value = false;
}

function onRowDragLeave(entry: FileEntry): void {
  if (dropDir.value === entry.path) dropDir.value = null;
}

function onRowDrop(event: DragEvent, entry: FileEntry): void {
  if (!entry.isDir) return;
  event.preventDefault();
  event.stopPropagation();
  dropDir.value = null;
  listDrop.value = false;
  openDropMenu(event, entry.path);
  endFileDrag();
}

function onListDragOver(event: DragEvent): void {
  if (!internalDragPayload()) return;
  event.preventDefault();
  listDrop.value = true;
}

function onListDrop(event: DragEvent): void {
  if (!internalDragPayload()) return;
  event.preventDefault();
  listDrop.value = false;
  dropDir.value = null;
  openDropMenu(event, listing.value.path);
  endFileDrag();
}

function applyTransferProgress(progress: CrossHostProgress, label: string): void {
  if (progress.mode === 'direct') {
    transfer.value = { label, loaded: progress.done, total: progress.total, note: '直连传输中' };
    return;
  }
  if (progress.mode === 'relay' && progress.bytesTotal > 0) {
    transfer.value = { label, loaded: progress.bytes, total: progress.bytesTotal, note: '经面板中转' };
    return;
  }
  transfer.value = {
    label,
    loaded: progress.done,
    total: progress.total,
    note: progress.note,
  };
}

async function transferAcross(
  mode: 'copy' | 'move',
  payload: FileDragPayload,
  dest: string,
): Promise<void> {
  const action = mode === 'copy' ? '复制' : '移动';

  if (payload.hostId === props.hostId) {
    try {
      if (mode === 'copy') await api.files.copy(payload.hostId, payload.paths, dest);
      else await api.files.move(payload.hostId, payload.paths, dest);
      message.success(`已${action} ${payload.paths.length} 项`);
      notifyFilesChanged([payload.hostId]);
    } catch (err) {
      message.error(describe(err, mode === 'copy' ? '复制失败' : '移动失败'));
    }
    return;
  }

  const label = `${payload.hostName || '源主机'} → ${props.hostName}`;
  let result: CrossHostResult;
  try {
    result = await crossHostTransfer(
      {
        fromHostId: payload.hostId,
        toHostId: props.hostId,
        paths: payload.paths,
        target: dest,
        move: mode === 'move',
      },
      {
        onProgress: (progress) => applyTransferProgress(progress, label),
        onNotice: (text) => message.warning(text),
      },
    );
  } finally {
    transfer.value = null;
  }

  if (result.outcome === 'done') {
    if (result.failed > 0) message.warning(`已${action}，但有 ${result.failed} 个文件未能传输`);
    else message.success(`已${action} ${payload.paths.length} 项`);
    notifyFilesChanged([payload.hostId, props.hostId]);
    return;
  }
  message.error(result.outcome === 'interrupted' ? `传输中断：${result.message}` : result.message);
}

function onDropAction(action: 'copy' | 'move'): void {
  const menu = dropMenu.value;
  dropMenu.value = { ...menu, show: false };
  if (!menu.payload) return;
  void transferAcross(action, menu.payload, menu.dest);
}

let stopFileEvents: (() => void) | null = null;
onMounted(() => {
  // 另一个窗口把东西搬进 / 搬出这台机器时，侧栏也要跟着变。
  stopFileEvents = onFilesChanged((ids) => {
    if (ids.includes(props.hostId)) void load();
  });
});
onBeforeUnmount(() => {
  stopFileEvents?.();
  stopFileEvents = null;
});
</script>

<template>
  <aside class="sftp" :aria-label="`${hostName} 的文件`">
    <div class="sftp__head">
      <button
        class="sftp__icon"
        :disabled="listing.parent === ''"
        title="上级目录"
        aria-label="上级目录"
        @click="load(listing.parent)"
      >
        <svg width="10" height="10" viewBox="0 0 10 10" aria-hidden="true">
          <path
            d="M5 8V2M5 2 2 5M5 2l3 3"
            fill="none"
            stroke="currentColor"
            stroke-width="1.3"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </button>
      <span class="sftp__path" :title="listing.path">{{ listing.path }}</span>
      <button class="sftp__icon" title="刷新" aria-label="刷新" @click="load()">
        <svg width="10" height="10" viewBox="0 0 12 12" aria-hidden="true">
          <path
            d="M10.5 6a4.5 4.5 0 1 1-4.5-4.5c1.26 0 2.465.5 3.37 1.37L10.5 4"
            fill="none"
            stroke="currentColor"
            stroke-width="1.3"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
          <path d="M10.5 1.5v2.5h-2.5" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
      </button>
    </div>

    <div
      class="sftp__list"
      :class="{ 'is-drop': listDrop }"
      @contextmenu="openMenu(null, $event)"
      @dragover="onListDragOver"
      @dragleave="listDrop = false"
      @drop="onListDrop"
    >
      <p v-if="error" class="sftp__error">{{ error }}</p>
      <p v-else-if="!loading && entries.length === 0" class="sftp__hint">空目录</p>
      <button
        v-for="entry in entries"
        :key="entry.path"
        class="sftp__row"
        :class="{ 'is-dir': entry.isDir, 'is-selected': entry.path === selected, 'is-drop': entry.path === dropDir }"
        :title="entry.name"
        draggable="true"
        @click="open(entry)"
        @contextmenu.stop="openMenu(entry, $event)"
        @dragstart="beginRowDrag($event, entry)"
        @dragend="endFileDrag"
        @dragover="onRowDragOver($event, entry)"
        @dragleave="onRowDragLeave(entry)"
        @drop="onRowDrop($event, entry)"
      >
        <span class="sftp__glyph" aria-hidden="true">{{ entry.isDir ? '▸' : '·' }}</span>
        <span class="sftp__name">{{ entry.name }}</span>
        <span v-if="!entry.isDir" class="sftp__size num">{{ formatBytes(entry.size) }}</span>
      </button>
      <!--
        加载覆盖层：点目录后旧列表还留在屏上，没有反馈就分不清「没反应」
        和「正在读取」。盖住列表同时也挡住这期间对旧条目的误点。
      -->
      <div v-if="loading" class="sftp__loading" role="status" aria-live="polite">
        <span class="sftp__spinner" aria-hidden="true" />
        <span>读取中…</span>
      </div>
    </div>

    <div class="sftp__foot">
      <span v-if="transfer" class="sftp__transfer num">
        {{ transfer.label }} ·
        {{ percent === null ? formatBytes(transfer.loaded) : `${percent}%` }}
        <template v-if="transfer.note">（{{ transfer.note }}）</template>
      </span>
      <span class="sftp__spacer" />
      <button class="sftp__act" :disabled="uploading" @click="pickFiles">上传</button>
      <button
        class="sftp__act"
        :disabled="selectedEntry === null || selectedEntry.isDir"
        @click="selectedEntry && download(selectedEntry)"
      >
        下载
      </button>
      <input ref="fileInput" type="file" multiple hidden @change="onFilesPicked" />
    </div>

    <n-dropdown
      :show="menu.show"
      :x="menu.x"
      :y="menu.y"
      :options="menuOptions"
      trigger="manual"
      placement="bottom-start"
      @select="onMenuSelect"
      @clickoutside="menu.show = false"
    />

    <!-- 拖拽落下后的动作菜单：复制 / 移动二选一，点外面即取消 -->
    <file-drop-menu
      :show="dropMenu.show"
      :x="dropMenu.x"
      :y="dropMenu.y"
      @update:show="(value) => (dropMenu.show = value)"
      @select="onDropAction"
    />

    <!--
      新建与重命名用居中弹窗而不是侧栏内联输入：240px 宽塞不下一个像样的
      输入框加说明，而这两个操作都需要看清自己在改什么。
    -->
    <n-modal v-model:show="showMkdir" preset="card" title="新建目录" style="width: 420px">
      <n-input v-model:value="mkdirName" placeholder="目录名" @keyup.enter="submitMkdir" />
      <template #footer>
        <n-space justify="end">
          <n-button @click="showMkdir = false">取消</n-button>
          <n-button type="primary" @click="submitMkdir">创建</n-button>
        </n-space>
      </template>
    </n-modal>

    <n-modal v-model:show="showRename" preset="card" title="重命名" style="width: 420px">
      <n-input v-model:value="renameName" placeholder="新名称" @keyup.enter="submitRename" />
      <template #footer>
        <n-space justify="end">
          <n-button @click="showRename = false">取消</n-button>
          <n-button type="primary" @click="submitRename">确定</n-button>
        </n-space>
      </template>
    </n-modal>
  </aside>
</template>
