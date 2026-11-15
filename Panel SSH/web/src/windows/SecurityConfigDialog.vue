<script setup lang="ts">
/**
 * 安全应用的配置文件弹窗。
 *
 * Fail2Ban 与 GnuPG 的专用窗口不再复用通用安全窗口，但配置文件仍然是
 * "点路径 → 读内容 → 保存并备份"这一件事，因此抽成独立弹窗给两者共用。
 *
 * 只接受后端声明的配置路径（SecurityConfigEntry），前端不提供任意路径输入。
 */
import { computed, ref, watch } from 'vue';
import { NButton, NInput, NModal, NSpace, NTag, useDialog, useMessage } from 'naive-ui';

import { ApiError, api, type SecurityConfigEntry } from '../api/client';

const props = defineProps<{
  show: boolean;
  hostId: number;
  slug: string;
  entries: SecurityConfigEntry[];
}>();

const emit = defineEmits<{ (e: 'update:show', value: boolean): void }>();

const dialog = useDialog();
const message = useMessage();

const visible = computed({
  get: () => props.show,
  set: (value) => emit('update:show', value),
});

const activePath = ref('');
const activeEntry = computed(
  () => props.entries.find((entry) => entry.path === activePath.value) ?? null,
);

const content = ref('');
const original = ref('');
const exists = ref(true);
const backup = ref('');
const dirOutput = ref('');
const loading = ref(false);
const saving = ref(false);
const error = ref('');

/** 只有普通文件有"未保存"概念；目录浏览是只读的。 */
const dirty = computed(
  () => activeEntry.value !== null && !activeEntry.value.isDir && content.value !== original.value,
);

function describe(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  return err instanceof Error ? err.message : '读取配置失败';
}

async function loadEntry(entry: SecurityConfigEntry): Promise<void> {
  activePath.value = entry.path;
  content.value = '';
  original.value = '';
  dirOutput.value = '';
  error.value = '';
  exists.value = true;
  loading.value = true;
  try {
    if (entry.isDir) {
      dirOutput.value = (await api.securityApps.browse(props.hostId, props.slug, entry.path)).output;
    } else {
      const file = await api.securityApps.readConfig(props.hostId, props.slug, entry.path);
      content.value = file.content;
      original.value = file.content;
      exists.value = file.exists;
      backup.value = file.backup;
    }
  } catch (err) {
    error.value = describe(err);
  } finally {
    loading.value = false;
  }
}

/** 切换文件前先问一句：直接丢掉用户刚写的内容不是"顺手清理"。 */
function selectEntry(entry: SecurityConfigEntry): void {
  if (entry.path === activePath.value) return;
  if (!dirty.value) {
    void loadEntry(entry);
    return;
  }
  dialog.warning({
    title: '放弃未保存的修改？',
    content: '当前文件有未保存的修改，切换后会丢失这些修改。',
    positiveText: '放弃并切换',
    negativeText: '继续编辑',
    onPositiveClick: () => void loadEntry(entry),
  });
}

function reload(): void {
  const entry = activeEntry.value;
  if (!entry) return;
  if (!dirty.value) {
    void loadEntry(entry);
    return;
  }
  dialog.warning({
    title: '放弃未保存的修改？',
    content: '重新读取会用磁盘上的内容覆盖当前编辑。',
    positiveText: '放弃并重读',
    negativeText: '继续编辑',
    onPositiveClick: () => void loadEntry(entry),
  });
}

async function save(): Promise<void> {
  const entry = activeEntry.value;
  if (!entry || entry.isDir || saving.value) return;
  saving.value = true;
  error.value = '';
  try {
    const saved = await api.securityApps.saveConfig(
      props.hostId,
      props.slug,
      entry.path,
      content.value,
    );
    original.value = content.value;
    exists.value = true;
    backup.value = saved.backup;
    message.success('配置已保存，原文件已备份');
  } catch (err) {
    error.value = describe(err);
    message.error(error.value);
  } finally {
    saving.value = false;
  }
}

function close(): void {
  if (!dirty.value) {
    visible.value = false;
    return;
  }
  dialog.warning({
    title: '放弃未保存的修改？',
    content: '关闭后当前编辑的内容不会保留。',
    positiveText: '放弃并关闭',
    negativeText: '继续编辑',
    onPositiveClick: () => {
      visible.value = false;
    },
  });
}

watch(
  () => props.show,
  (on) => {
    if (!on) return;
    const first = props.entries[0];
    if (!first) {
      activePath.value = '';
      error.value = props.entries.length === 0 ? '这个应用没有声明配置文件' : '';
      return;
    }
    void loadEntry(first);
  },
);
</script>

<template>
  <n-modal
    v-model:show="visible"
    preset="card"
    title="配置文件"
    style="width: 920px"
    :closable="false"
    :mask-closable="false"
    :close-on-esc="false"
  >
    <div class="seccfg">
      <aside class="seccfg__list" aria-label="配置文件清单">
        <button
          v-for="entry in entries"
          :key="entry.path"
          type="button"
          class="seccfg__item"
          :class="{ 'is-on': entry.path === activePath }"
          :title="entry.path"
          @click="selectEntry(entry)"
        >
          <code>{{ entry.path }}</code>
          <span>{{ entry.isDir ? '目录' : '文件' }}</span>
        </button>
        <p v-if="entries.length === 0" class="seccfg__hint">这个应用没有声明配置文件。</p>
      </aside>

      <main class="seccfg__main">
        <div class="seccfg__head">
          <span class="seccfg__path num">{{ activePath || '选择左侧文件' }}</span>
          <n-tag v-if="dirty" size="tiny" :bordered="false" type="warning">未保存</n-tag>
          <span class="seccfg__grow" />
          <n-button size="small" :loading="loading" :disabled="!activeEntry" @click="reload">
            {{ dirty ? '放弃修改' : '重新读取' }}
          </n-button>
          <n-button
            size="small"
            type="primary"
            :loading="saving"
            :disabled="!dirty"
            @click="save"
          >
            保存
          </n-button>
        </div>

        <div v-if="error" class="seccfg__error">{{ error }}</div>
        <div v-else-if="loading" class="seccfg__empty">正在读取…</div>
        <n-input
          v-else-if="activeEntry && !activeEntry.isDir"
          v-model:value="content"
          type="textarea"
          class="seccfg__editor"
          placeholder="在此编辑配置文件内容"
          :autosize="{ minRows: 16, maxRows: 28 }"
        />
        <pre v-else-if="activeEntry?.isDir" class="seccfg__output">{{ dirOutput }}</pre>

        <p v-if="activeEntry && !activeEntry.isDir && !error" class="seccfg__hint">
          <template v-if="!exists">文件尚不存在，保存后会创建。</template>
          <template v-else>保存前会自动备份为 {{ backup }}。</template>
          目录型配置只读。
        </p>
      </main>
    </div>

    <template #footer>
      <n-space justify="end">
        <n-button @click="close">关闭</n-button>
      </n-space>
    </template>
  </n-modal>
</template>

<style scoped>
.seccfg {
  display: grid;
  grid-template-columns: 228px minmax(0, 1fr);
  gap: 14px;
  height: 58vh;
  min-height: 320px;
}

.seccfg__list {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-height: 0;
  padding-right: 4px;
  overflow-y: auto;
}

.seccfg__item {
  display: flex;
  flex-direction: column;
  gap: 2px;
  width: 100%;
  padding: 7px 9px;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  color: var(--ink);
  text-align: left;
  cursor: pointer;
  transition: border-color var(--dur-1) var(--ease), background var(--dur-1) var(--ease);
}

.seccfg__item:hover {
  border-color: var(--line-strong);
}

.seccfg__item.is-on {
  border-color: var(--sig-line);
  background: var(--sig-wash-soft);
}

.seccfg__item:focus-visible {
  outline: 2px solid var(--sig);
  outline-offset: 2px;
}

.seccfg__item code {
  overflow: hidden;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  color: var(--ink-hi);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.seccfg__item span {
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.seccfg__main {
  display: flex;
  flex-direction: column;
  gap: 10px;
  min-width: 0;
  min-height: 0;
}

.seccfg__head {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 26px;
}

.seccfg__path {
  overflow: hidden;
  font-size: var(--fs-label);
  color: var(--ink-hi);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.seccfg__grow {
  flex: 1;
}

.seccfg__editor {
  flex: 1;
  min-height: 0;
}

.seccfg__editor :deep(.n-input__textarea-el) {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  line-height: 1.65;
}

.seccfg__output {
  flex: 1;
  min-height: 0;
  margin: 0;
  padding: 10px 12px;
  overflow: auto;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  line-height: 1.65;
  color: var(--ink);
  white-space: pre-wrap;
  word-break: break-word;
}

.seccfg__error {
  padding: 8px 10px;
  border: 1px solid var(--danger-line);
  border-radius: var(--r-2);
  background: var(--danger-wash);
  font-size: var(--fs-sm);
  color: var(--danger-ink);
  white-space: pre-wrap;
}

.seccfg__empty {
  display: flex;
  flex: 1;
  align-items: center;
  justify-content: center;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
}

.seccfg__hint {
  margin: 0;
  font-size: var(--fs-label);
  line-height: 1.6;
  color: var(--ink-lo);
}
</style>
