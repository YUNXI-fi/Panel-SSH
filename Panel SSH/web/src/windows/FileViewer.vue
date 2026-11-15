<script setup lang="ts">
import { computed, inject, onMounted, ref } from 'vue';
import {
  NAlert,
  NButton,
  NRadioButton,
  NRadioGroup,
  useDialog,
  useMessage,
} from 'naive-ui';

import { ApiError, api, triggerDownload, type FileEntry } from '../api/client';
import { formatBytes, formatTime } from '../api/format';
import { WindowBeforeCloseKey, WindowCloseKey, WindowSubtitleKey } from '../desktop/useWindowManager';
import { renderMarkdown } from '../markdown';
import { notifyFilesChanged } from './fileEvents';
import { highlightCode } from './highlight';
import {
  isTextKind,
  languageOf,
  previewKindOf,
  resolveMarkdownAsset,
} from './filePreview';

/**
 * 文件查看器。
 *
 * 一个文件"能怎么打开"由扩展名决定：文本走正文接口（可预览、可编辑），
 * 图片/音视频/PDF 走预览接口（inline + Range），其它类型在列表里就没有入口。
 *
 * 预览与编辑放在同一个查看器里而不是两个弹窗：同一个人对同一个文件的
 * 动作是连续的（看一眼 → 改一下 → 再看一眼），换窗口会丢掉上下文。
 */
type ViewerMode = 'preview' | 'source' | 'edit';

const props = defineProps<{
  hostId: number;
  hostName: string;
  entry: FileEntry;
  initialMode?: ViewerMode;
}>();

const message = useMessage();
const dialog = useDialog();

// 标题后缀写成主机名：窗口标题是文件名，竖栏任务项因此能一眼看出是哪台机器上的文件。
const setSubtitle = inject(WindowSubtitleKey, null);
// 由 WindowFrame 提供：它会先问本组件有没有未保存的修改，再真正关窗。
const closeWindow = inject(WindowCloseKey, null);
// 关窗拦截由内容组件主动登记（异步组件下拿不到模板 ref 的 expose）。
const registerBeforeClose = inject(WindowBeforeCloseKey, null);

const kind = computed(() => previewKindOf(props.entry.name));
const textMode = computed(() => isTextKind(kind.value));

// 防御性处理：图片 / PDF 没有可编辑的正文，即使调用方要求进编辑也只能落到预览。
const requested = props.initialMode ?? 'preview';
const mode = ref<ViewerMode>(
  requested === 'edit' && !isTextKind(kind.value) ? 'preview' : requested,
);

/**
 * 大文件的分块预览。
 *
 * 超过单次读取上限（1 MiB）的文件不让它显示一条错误了事：改为按块读取、
 * 滚动到底继续加载。这样 1–5 MiB 的 CSV / 日志能直接看，而浏览器任何时刻
 * 都不会被一整份大文本拖住。
 *
 * 预览上限 64 MiB 只是防呆：再多就不该在浏览器里读，下载更合适。
 */
const STREAM_SOFT_LIMIT = 64 << 20;
const streaming = ref(false);
const chunks = ref<string[]>([]);
const nextOffset = ref(0);
const streamSize = ref(0);
const streamEOF = ref(false);
const loadingMore = ref(false);
const streamCapped = ref(false);
/** 空串表示"还没识别"：第一次请求交给服务端判断。 */
const streamEncoding = ref('');

const tabs = computed<Array<{ key: ViewerMode; label: string }>>(() => {
  // 分块预览只有纯文本：Markdown / HTML 不能渲染（没有完整正文），也不能编辑。
  if (streaming.value) {
    const label =
      kind.value === 'markdown' || kind.value === 'html' || kind.value === 'code'
        ? '源码'
        : '预览';
    return [{ key: kind.value === 'markdown' || kind.value === 'html' ? 'source' : 'preview', label }];
  }
  switch (kind.value) {
    case 'markdown':
    case 'html':
      return [
        { key: 'preview', label: '预览' },
        { key: 'source', label: '源码' },
        { key: 'edit', label: '编辑' },
      ];
    case 'code':
      return [
        { key: 'preview', label: '源码' },
        { key: 'edit', label: '编辑' },
      ];
    case 'text':
      return [
        { key: 'preview', label: '预览' },
        { key: 'edit', label: '编辑' },
      ];
    default:
      return [];
  }
});

const content = ref('');
const original = ref('');
const draft = ref('');
const loading = ref(false);
const error = ref('');
const saving = ref(false);
/**
 * 文件的原始编码。
 *
 * 服务端已经把正文转成 UTF-8 再下发，这里记下原编码，保存时原样带回去。
 * 把 GBK 的 CSV 改一行再保存，文件不能悄悄变成 UTF-8 —— 用户的其他系统
 * 可能一直按 GBK 读它。
 */
const encoding = ref('utf-8');

const ENCODING_LABELS: Record<string, string> = {
  'utf-8': 'UTF-8',
  'utf-8-bom': 'UTF-8（带 BOM）',
  'utf-16le': 'UTF-16 LE',
  'utf-16be': 'UTF-16 BE',
  gb18030: 'GB18030 / GBK',
};

const encodingLabel = computed(() => ENCODING_LABELS[encoding.value] ?? encoding.value);
/** UTF-8 与 UTF-8 BOM 不需要额外提示；其余编码要让用户知道"看到的是转码后的"。 */
const convertedEncoding = computed(
  () => encoding.value !== 'utf-8' && encoding.value !== 'utf-8-bom',
);

const language = computed(() => languageOf(props.entry.name));
const highlighted = computed(() => highlightCode(content.value, language.value));
const lineCount = computed(() => (content.value === '' ? 1 : content.value.split('\n').length));
/** 行号只在前几千行时渲染：几万行数字本身就会把滚动条拖垮，而那种文件读起来靠搜索。 */
const showGutter = computed(() => lineCount.value <= 4000);
const gutter = computed(() => {
  if (!showGutter.value) return '';
  const parts: string[] = [];
  for (let i = 1; i <= lineCount.value; i += 1) parts.push(String(i));
  return parts.join('\n');
});

const dirty = computed(() => mode.value === 'edit' && draft.value !== original.value);

const previewSrc = computed(() => api.files.previewUrl(props.hostId, props.entry.path));

const markdownHtml = computed(() =>
  renderMarkdown(content.value, {
    highlight: highlightCode,
    imageUrl: (src) => {
      const target = resolveMarkdownAsset(props.entry.path, src);
      if (!target) return null;
      // 外链与 data: 图片原样交给浏览器；远端相对路径改写成预览接口。
      if (/^(https?:\/\/|data:image\/)/i.test(target)) return target;
      return api.files.previewUrl(props.hostId, target);
    },
  }),
);

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 504) return `${err.message}。请检查目标机是否可达`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

async function loadText(): Promise<void> {
  loading.value = true;
  error.value = '';
  try {
    const file = await api.files.readText(props.hostId, props.entry.path);
    content.value = file.content;
    original.value = file.content;
    draft.value = file.content;
    encoding.value = file.encoding || 'utf-8';
  } catch (err) {
    // 413 是"超过单次读取上限"：改用分块预览，而不是把用户挡在门外。
    // 415（疑似二进制）仍然如实报错。
    if (err instanceof ApiError && err.status === 413) {
      await startStreaming();
      return;
    }
    error.value = describe(err, '读取文件失败');
  } finally {
    loading.value = false;
  }
}

/** 切到分块预览：先把状态重置，再取第一块。 */
async function startStreaming(): Promise<void> {
  streaming.value = true;
  chunks.value = [];
  nextOffset.value = 0;
  streamSize.value = props.entry.size;
  streamEOF.value = false;
  streamCapped.value = false;
  streamEncoding.value = '';
  // Markdown / HTML 没有完整正文，只能看源码（纯文本块）。
  mode.value = kind.value === 'markdown' || kind.value === 'html' ? 'source' : 'preview';
  await loadMore();
}

/** 取下一块。滚动到底时由 onStreamScroll 触发。 */
async function loadMore(): Promise<void> {
  if (loadingMore.value || streamEOF.value || streamCapped.value) return;
  if (nextOffset.value >= STREAM_SOFT_LIMIT) {
    streamCapped.value = true;
    return;
  }

  loadingMore.value = true;
  try {
    const result = await api.files.textChunk(
      props.hostId,
      props.entry.path,
      nextOffset.value,
      // 第一次不传，让服务端识别；之后必须带上，否则变长编码无法对齐。
      streamEncoding.value || undefined,
    );
    streamEncoding.value = result.encoding;
    encoding.value = result.encoding;
    streamSize.value = result.size;
    if (result.chunk) chunks.value.push(result.chunk);
    nextOffset.value = result.nextOffset;
    streamEOF.value = result.eof;
  } catch (err) {
    error.value = describe(err, '读取文件失败');
    streamEOF.value = true;
  } finally {
    loadingMore.value = false;
  }
}

function onStreamScroll(event: Event): void {
  const el = event.target as HTMLElement | null;
  if (!el) return;
  // 提前 400px 开始取下一块，滚动过程里不会出现明显的空白。
  if (el.scrollTop + el.clientHeight >= el.scrollHeight - 400) void loadMore();
}

// ---- 图片缩放 / 拖动 ----
//
// 缩放不改变布局尺寸，只做 transform：否则每滚一格都要重新布局整张图，
// 大图会明显掉帧。offset 以"适应窗口后的图片中心"为原点。

const MIN_SCALE = 0.1;
const MAX_SCALE = 8;

const scale = ref(1);
const offset = ref({ x: 0, y: 0 });
const dragging = ref(false);

const zoomText = computed(() => `${Math.round(scale.value * 100)}%`);

const imageStyle = computed(() => ({
  transform: `translate(-50%, -50%) translate(${offset.value.x}px, ${offset.value.y}px) scale(${scale.value})`,
}));

function clampScale(value: number): number {
  return Math.min(MAX_SCALE, Math.max(MIN_SCALE, value));
}

/**
 * 以容器内的某一点为中心缩放。
 *
 * px / py 是光标相对容器中心的坐标。缩放前后光标下的那个像素必须停在同一
 * 位置上，公式就是让这一点在变换里保持不动。
 */
function zoomAt(next: number, px = 0, py = 0): void {
  const current = scale.value;
  const target = clampScale(next);
  if (target === current) return;
  const ratio = target / current;
  offset.value = {
    x: px - (px - offset.value.x) * ratio,
    y: py - (py - offset.value.y) * ratio,
  };
  scale.value = target;
}

function zoomBy(factor: number): void {
  zoomAt(scale.value * factor);
}

function resetZoom(): void {
  scale.value = 1;
  offset.value = { x: 0, y: 0 };
}

function onImageWheel(event: WheelEvent): void {
  const el = event.currentTarget as HTMLElement | null;
  if (!el) return;
  const rect = el.getBoundingClientRect();
  const px = event.clientX - (rect.left + rect.width / 2);
  const py = event.clientY - (rect.top + rect.height / 2);
  // 触摸板双指捏合在浏览器里也是 wheel（带 ctrlKey），一并按缩放处理。
  const factor = event.deltaY < 0 ? 1.12 : 1 / 1.12;
  zoomAt(scale.value * factor, px, py);
}

let dragOrigin = { x: 0, y: 0, offsetX: 0, offsetY: 0 };

function onImagePointerDown(event: PointerEvent): void {
  if (event.button !== 0) return;
  const el = event.currentTarget as HTMLElement | null;
  if (!el) return;
  dragging.value = true;
  dragOrigin = {
    x: event.clientX,
    y: event.clientY,
    offsetX: offset.value.x,
    offsetY: offset.value.y,
  };
  el.setPointerCapture(event.pointerId);
}

function onImagePointerMove(event: PointerEvent): void {
  if (!dragging.value) return;
  offset.value = {
    x: dragOrigin.offsetX + (event.clientX - dragOrigin.x),
    y: dragOrigin.offsetY + (event.clientY - dragOrigin.y),
  };
}

function endImageDrag(event: PointerEvent): void {
  if (!dragging.value) return;
  dragging.value = false;
  const el = event.currentTarget as HTMLElement | null;
  if (el?.hasPointerCapture(event.pointerId)) el.releasePointerCapture(event.pointerId);
}

// ---- 模式切换 / 关闭 / 保存 ----

function requestMode(next: string): void {
  const target = next as ViewerMode;
  if (target === mode.value) return;
  if (target === 'edit') {
    // 每次进编辑都以磁盘上的最新内容为起点：上次保存后 content 已经更新，
    // 继续用旧草稿会让"改了一半又保存过"的修改被悄悄带回。
    draft.value = content.value;
    mode.value = target;
    return;
  }
  if (dirty.value) {
    dialog.warning({
      title: '放弃未保存的修改？',
      content: `${props.entry.path} 有未保存的内容，切换后这些修改会丢失。`,
      positiveText: '放弃修改',
      negativeText: '继续编辑',
      onPositiveClick: () => {
        mode.value = target;
      },
    });
    return;
  }
  mode.value = target;
}

/**
 * 关闭前的拦截。
 *
 * 窗口标题栏的 × 与查看器里的「关闭」都会先走到这里：有未保存的修改就确认，
 * 确认后执行 next（真正关闭窗口）。没有改动则直接放行。
 */
function beforeClose(next: () => void): void {
  if (!dirty.value) {
    next();
    return;
  }
  dialog.warning({
    title: '放弃未保存的修改？',
    content: `${props.entry.path} 有未保存的内容，关闭后这些修改会丢失。`,
    positiveText: '放弃修改',
    negativeText: '继续编辑',
    onPositiveClick: next,
  });
}

async function save(): Promise<void> {
  if (!dirty.value || saving.value) return;
  saving.value = true;
  try {
    const result = await api.files.writeText(
      props.hostId,
      props.entry.path,
      draft.value,
      encoding.value,
    );
    content.value = draft.value;
    original.value = draft.value;
    // 权限位没能还原时后端会回一条 warning：不能报"已保存"就完事。
    if (result.warning) message.warning(result.warning);
    else message.success('已保存');
    // 文件窗口是本组件之外的另一个窗口，靠这条广播让它刷新列表。
    notifyFilesChanged([props.hostId]);
    mode.value = 'preview';
  } catch (err) {
    message.error(describe(err, '保存失败'));
  } finally {
    saving.value = false;
  }
}

/** Tab 在编辑器里应当缩进，而不是把焦点跳到别处。 */
function onEditorKeydown(event: KeyboardEvent): void {
  if (event.key !== 'Tab') return;
  event.preventDefault();
  const el = event.target as HTMLTextAreaElement | null;
  if (!el) return;
  const start = el.selectionStart;
  const end = el.selectionEnd;
  draft.value = `${draft.value.slice(0, start)}  ${draft.value.slice(end)}`;
  window.requestAnimationFrame(() => {
    el.selectionStart = start + 2;
    el.selectionEnd = start + 2;
  });
}

async function download(): Promise<void> {
  try {
    const blob = await api.files.download(props.hostId, props.entry.path);
    const url = URL.createObjectURL(blob);
    triggerDownload(url, props.entry.name);
    // 不能立刻撤销：浏览器可能还没读完这段 Blob，那样落盘的是 0 字节文件。
    window.setTimeout(() => URL.revokeObjectURL(url), 60_000);
  } catch (err) {
    message.error(describe(err, '下载失败'));
  }
}

onMounted(() => {
  // 把"关闭前确认"交给窗口：点标题栏的 × 也会走同一条检查。
  registerBeforeClose?.(beforeClose);
  // 窗口标题是文件名，主机名放到副标题里；WindowFrame 负责把它拼进任务栏标题。
  if (props.hostName) setSubtitle?.(props.hostName);
  if (textMode.value) void loadText();
});
</script>

<template>
  <div class="fv">
    <header class="fv__bar">
      <n-button size="small" quaternary :disabled="!closeWindow" @click="closeWindow?.()">
        关闭
      </n-button>
      <div class="fv__ident">
        <span class="fv__name" :title="entry.path">{{ entry.name }}</span>
        <span class="fv__meta num">
          {{ hostName }} · {{ formatBytes(entry.size) }} · {{ formatTime(entry.modTime) }}
          <template v-if="textMode && !loading && !error && !streaming"> · {{ lineCount }} 行</template>
        </span>
      </div>
      <span class="fv__spacer" />

      <template v-if="kind === 'image' && !error">
        <n-button size="tiny" quaternary :disabled="scale <= MIN_SCALE" @click="zoomBy(1 / 1.25)">
          缩小
        </n-button>
        <n-button size="tiny" quaternary title="点击复位；也可以直接双击图片" @click="resetZoom">
          {{ zoomText }}
        </n-button>
        <n-button size="tiny" quaternary :disabled="scale >= MAX_SCALE" @click="zoomBy(1.25)">
          放大
        </n-button>
      </template>

      <n-radio-group
        v-if="tabs.length"
        :value="mode"
        size="small"
        name="fv-mode"
        @update:value="(value) => requestMode(String(value))"
      >
        <n-radio-button v-for="tab in tabs" :key="tab.key" :value="tab.key">
          {{ tab.label }}
        </n-radio-button>
      </n-radio-group>

      <n-button size="small" @click="download">下载</n-button>
      <n-button
        v-if="mode === 'edit'"
        size="small"
        type="primary"
        :loading="saving"
        :disabled="!dirty"
        @click="save"
      >
        保存
      </n-button>
    </header>

    <div class="fv__body">
      <div v-if="!loading && !error && textMode && convertedEncoding" class="fv__encoding">
        文件编码 {{ encodingLabel }}：正文已转换为 UTF-8 显示，保存时会按原编码写回。
      </div>
      <div v-if="loading" class="fv__hint">正在读取文件…</div>
      <n-alert v-else-if="error" type="error">{{ error }}</n-alert>

      <!-- 超过 1 MiB 的文本：按块读取，滚动到底继续加载 -->
      <div v-else-if="streaming" class="fv__stream" @scroll="onStreamScroll">
        <pre v-for="(chunk, index) in chunks" :key="index" class="fv__stream-block">{{ chunk }}</pre>
        <p class="fv__hint fv__stream-note">
          <template v-if="loadingMore">正在加载…</template>
          <template v-else-if="streamEOF">已到文件末尾 · {{ formatBytes(streamSize) }}</template>
          <template v-else-if="streamCapped">
            已加载 {{ formatBytes(nextOffset) }}，达到预览上限，请下载查看完整内容。
          </template>
          <template v-else>
            已加载 {{ formatBytes(nextOffset) }} / {{ formatBytes(streamSize) }} · 继续滚动加载
          </template>
        </p>
      </div>

      <template v-else-if="mode === 'edit'">
        <textarea
          v-model="draft"
          class="fv__editor"
          spellcheck="false"
          :aria-label="`编辑 ${entry.name}`"
          @keydown="onEditorKeydown"
        />
        <div class="fv__foot">
          <span class="fv__hint">Tab 插入两个空格；保存就地覆盖远端文件，权限位保持不变。</span>
        </div>
      </template>

      <!-- 代码与"源码"视图：前者是代码文件的默认视图，后者是 md/html 的第二个页签 -->
      <template
        v-else-if="mode === 'source' || (kind === 'code' && mode === 'preview')"
      >
        <div class="fv__codewrap">
          <div v-if="showGutter" class="fv__gutter num" aria-hidden="true">{{ gutter }}</div>
          <pre class="fv__code"><code v-html="highlighted" /></pre>
        </div>
      </template>

      <template v-else-if="kind === 'markdown'">
        <div class="fv__md" v-html="markdownHtml" />
      </template>

      <template v-else-if="kind === 'html'">
        <!--
          sandbox 为空字符串即"全部禁用"：脚本、表单、同源访问都不给。
          这个文件来自被管理的机器，内容不可信；一旦允许同源，它就能以面板身份
          调用 API。
        -->
        <div class="fv__stage">
          <iframe
            class="fv__html"
            sandbox=""
            referrerpolicy="no-referrer"
            :srcdoc="content"
            title="HTML 预览"
          />
        </div>
        <div class="fv__foot">
          <span class="fv__hint">
            在隔离沙箱中渲染：不执行脚本，相对路径的样式与图片不可用。要看完整效果请下载后打开。
          </span>
        </div>
      </template>

      <template v-else-if="kind === 'image'">
        <!--
          滚轮缩放、按住拖动平移；双击复位。缩放只改 transform，不触发布局，
          大图拖起来才不会一格一卡。
        -->
        <div
          class="fv__stage fv__stage--image"
          :class="{ 'is-dragging': dragging }"
          @wheel.prevent="onImageWheel"
          @pointerdown="onImagePointerDown"
          @pointermove="onImagePointerMove"
          @pointerup="endImageDrag"
          @pointercancel="endImageDrag"
          @dblclick="resetZoom"
        >
          <img
            class="fv__image"
            :src="previewSrc"
            :style="imageStyle"
            :alt="entry.name"
            draggable="false"
            @dragstart.prevent
          />
        </div>
      </template>

      <template v-else-if="kind === 'audio'">
        <div class="fv__stage">
          <audio class="fv__audio" :src="previewSrc" controls preload="metadata" />
          <p class="fv__hint">浏览器不支持该编码时，请下载后用本地播放器打开。</p>
        </div>
      </template>

      <template v-else-if="kind === 'video'">
        <div class="fv__stage">
          <video class="fv__video" :src="previewSrc" controls preload="metadata" />
          <p class="fv__hint">浏览器不支持该编码时，请下载后用本地播放器打开。</p>
        </div>
      </template>

      <template v-else-if="kind === 'pdf'">
        <div class="fv__stage">
          <iframe class="fv__pdf" :src="previewSrc" title="PDF 预览" />
        </div>
        <div class="fv__foot">
          <span class="fv__hint">PDF 由浏览器内置阅读器显示；不支持时请下载查看。</span>
        </div>
      </template>

      <template v-else>
        <pre class="fv__text">{{ content }}</pre>
      </template>
    </div>
  </div>
</template>
