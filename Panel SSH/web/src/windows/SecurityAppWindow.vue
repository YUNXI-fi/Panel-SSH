<script setup lang="ts">
/**
 * 安全应用原生窗口。
 *
 * 目标：所有操作都在界面里完成，不出现命令行。
 * - 左侧是后端声明的白名单操作与配置文件；
 * - 中间是表单、执行结果与配置编辑器；
 * - 命令由后端拼装并走既有提权链路，前端只提交 action 与字段。
 */
import { computed, onMounted, ref } from 'vue';
import {
  NButton,
  NForm,
  NFormItem,
  NInput,
  NSelect,
  NSpace,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui';

import {
  ApiError,
  api,
  type SecurityAction,
  type SecurityAppView,
  type SecurityConfigEntry,
  type SecurityField,
  type SecurityRunResult,
} from '../api/client';
import { securityGlyph } from '../securityApps';
import Fail2BanWindow from './Fail2BanWindow.vue';
import GnuPgpWindow from './GnuPgpWindow.vue';

const props = defineProps<{
  hostId: number;
  hostName: string;
  slug: string;
  name: string;
  /** 窗口图标名，与桌面图标保持一致。 */
  glyph?: string;
  /** 应用仓库安装的包带自己的图标；为空时用精灵表图形。 */
  iconUrl?: string;
}>();

const dialog = useDialog();
const message = useMessage();

const app = ref<SecurityAppView | null>(null);
const loading = ref(false);
const loadError = ref('');

const mode = ref<'action' | 'config'>('action');
const activeAction = ref<SecurityAction | null>(null);
const form = ref<Record<string, string>>({});
const running = ref(false);
const result = ref<SecurityRunResult | null>(null);
const resultTitle = ref('');

const configLoading = ref(false);
const configSaving = ref(false);
const configError = ref('');
const configFile = ref<{ path: string; content: string; exists: boolean; backup: string } | null>(
  null,
);
const configDir = ref<{ path: string; output: string } | null>(null);
/** 打开编辑器时的原始内容：用来判断是否有未保存修改。 */
const configOriginal = ref('');

const glyph = computed(() => props.glyph || securityGlyph(props.slug));
const appName = computed(() => app.value?.name || props.name);
const inspectActions = computed(() =>
  (app.value?.actions ?? []).filter(
    (action) => action.group !== 'manage' && !action.confirm && !action.danger,
  ),
);
const manageActions = computed(() =>
  (app.value?.actions ?? []).filter(
    (action) => action.group === 'manage' || action.confirm || action.danger,
  ),
);

/**
 * 侧栏分组：应用包的 ui.json 指定了分组就用它，否则沿用内置的两段式。
 *
 * 这样同一份通用窗口既能渲染远程包的界面配置，也能在没装包时直接
 * 打开编译期的安全应用，不需要两套模板。
 */
const actionGroups = computed<{ title: string; actions: SecurityAction[] }[]>(() => {
  const all = app.value?.actions ?? [];
  const sections = app.value?.ui?.sections;
  if (sections && sections.length > 0) {
    const byId = new Map(all.map((action) => [action.id, action]));
    const groups = sections
      .map((section) => ({
        title: section.title,
        actions: section.actions
          .map((id) => byId.get(id))
          .filter((action): action is SecurityAction => action !== undefined),
      }))
      .filter((group) => group.actions.length > 0);
    if (groups.length > 0) return groups;
  }
  return [
    { title: '查看与诊断', actions: inspectActions.value },
    { title: '管理操作', actions: manageActions.value },
  ].filter((group) => group.actions.length > 0);
});
const configDirty = computed(
  () => configFile.value !== null && configFile.value.content !== configOriginal.value,
);

function describe(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  return err instanceof Error ? err.message : '操作失败';
}

function defaultsFor(action: SecurityAction): Record<string, string> {
  const values: Record<string, string> = {};
  for (const field of action.fields ?? []) values[field.key] = field.default ?? '';
  return values;
}

async function load(): Promise<void> {
  loading.value = true;
  loadError.value = '';
  try {
    app.value = await api.securityApps.get(props.hostId, props.slug);
    const initial = app.value.actions.find((item) => item.id === app.value?.defaultAction);
    if (initial) void execute(initial, {});
  } catch (err) {
    loadError.value = describe(err);
  } finally {
    loading.value = false;
  }
}

function onAction(action: SecurityAction): void {
  confirmDiscard(() => {
    mode.value = 'action';
    if ((action.fields ?? []).length > 0) {
      activeAction.value = action;
      form.value = defaultsFor(action);
      return;
    }
    confirmAndRun(action, {});
  });
}

/** 切换操作或配置文件前，先确认是否丢弃未保存的配置修改。 */
function confirmDiscard(next: () => void): void {
  if (!configDirty.value) {
    next();
    return;
  }
  dialog.warning({
    title: '放弃未保存的配置？',
    content: '当前配置文件有未保存的修改，继续操作会丢失这些修改。',
    positiveText: '放弃并继续',
    negativeText: '继续编辑',
    onPositiveClick: next,
  });
}

function confirmAndRun(action: SecurityAction, params: Record<string, string>): void {
  if (!action.confirm) {
    void execute(action, params);
    return;
  }
  dialog.warning({
    title: action.label,
    content: action.confirm,
    positiveText: '执行',
    negativeText: '取消',
    onPositiveClick: () => {
      void execute(action, params);
    },
  });
}

async function execute(action: SecurityAction, params: Record<string, string>): Promise<void> {
  if (running.value) return;
  running.value = true;
  mode.value = 'action';
  resultTitle.value = action.label;
  try {
    result.value = await api.securityApps.run(props.hostId, props.slug, action.id, params);
    if (!result.value.ok) {
      message.error(`${action.label}失败（退出码 ${result.value.exitCode}）`);
    }
  } catch (err) {
    const text = describe(err);
    result.value = { ok: false, exitCode: -1, output: text, durationMs: 0 };
    message.error(text);
  } finally {
    running.value = false;
  }
}

function submitForm(): void {
  const action = activeAction.value;
  if (!action) return;
  for (const field of action.fields ?? []) {
    if (field.required && (form.value[field.key] ?? '').trim() === '') {
      message.warning(`${field.label}不能为空`);
      return;
    }
  }
  confirmAndRun(action, { ...form.value });
}

function cancelForm(): void {
  activeAction.value = null;
  form.value = {};
}

function selectOptions(field: SecurityField): Array<{ label: string; value: string }> {
  return (field.options ?? []).map((option) => ({ label: option, value: option }));
}

async function copyResult(): Promise<void> {
  if (!result.value) return;
  try {
    await navigator.clipboard.writeText(result.value.output);
    message.success('结果已复制');
  } catch {
    message.error('复制失败，请手动选择内容');
  }
}

async function openConfig(entry: SecurityConfigEntry): Promise<void> {
  confirmDiscard(() => {
    void doOpenConfig(entry);
  });
}

async function doOpenConfig(entry: SecurityConfigEntry): Promise<void> {
  mode.value = 'config';
  activeAction.value = null;
  configError.value = '';
  configFile.value = null;
  configDir.value = null;
  configLoading.value = true;
  try {
    if (entry.isDir) {
      configDir.value = await api.securityApps.browse(props.hostId, props.slug, entry.path);
    } else {
      configFile.value = await api.securityApps.readConfig(props.hostId, props.slug, entry.path);
      configOriginal.value = configFile.value.content;
    }
  } catch (err) {
    configError.value = describe(err);
  } finally {
    configLoading.value = false;
  }
}

async function reloadConfig(): Promise<void> {
  if (!configFile.value) return;
  await doOpenConfig({ path: configFile.value.path, isDir: false });
}

async function saveConfig(): Promise<void> {
  if (!configFile.value || configSaving.value) return;
  configSaving.value = true;
  try {
    const saved = await api.securityApps.saveConfig(
      props.hostId,
      props.slug,
      configFile.value.path,
      configFile.value.content,
    );
    configFile.value.backup = saved.backup;
    configOriginal.value = configFile.value.content;
    message.success('配置已保存，原文件已备份');
  } catch (err) {
    message.error(describe(err));
  } finally {
    configSaving.value = false;
  }
}

onMounted(() => {
  // Fail2Ban 与 GnuPG 由各自的专用窗口接管：通用窗口的默认动作既不展示，
  // 也不该在后台多跑一次。
  if (props.slug === 'fail2ban' || props.slug === 'gnupg') return;
  void load();
});
</script>

<template>
  <!-- Fail2Ban 与 GnuPG 有专用可视化窗口；其余工具仍用通用壳。 -->
  <Fail2BanWindow
    v-if="props.slug === 'fail2ban'"
    :host-id="props.hostId"
    :host-name="props.hostName"
    :slug="props.slug"
    :name="props.name"
    :glyph="props.glyph"
  />
  <GnuPgpWindow
    v-else-if="props.slug === 'gnupg'"
    :host-id="props.hostId"
    :host-name="props.hostName"
    :slug="props.slug"
    :name="props.name"
    :glyph="props.glyph"
  />
  <div v-else class="secui">
    <header class="secui__head">
      <span class="secui__icon" aria-hidden="true">
        <span
          v-if="props.iconUrl"
          class="icon-mask secui__icon-img"
          :style="{ '--icon-mask': `url(${props.iconUrl})` }"
        />
        <svg v-else><use :href="`#glyph-${glyph}`" /></svg>
      </span>
      <div class="secui__title">
        <span class="secui__name">{{ appName }}</span>
        <span class="row-sub">
          {{ hostName }} · {{ app?.description || '应用' }}
          <template v-if="app?.version"> · v{{ app.version }}</template>
        </span>
      </div>
      <n-button size="tiny" quaternary :loading="loading" @click="load">刷新</n-button>
      <n-tag size="tiny" :bordered="false" :type="running ? 'warning' : 'success'">
        {{ running ? '执行中' : '就绪' }}
      </n-tag>
    </header>

    <div class="secui__body">
      <aside class="secui__side">
        <template v-for="group in actionGroups" :key="group.title">
          <p class="secui__section">{{ group.title }}</p>
          <div class="secui__actions">
            <n-button
              v-for="action in group.actions"
              :key="action.id"
              size="small"
              block
              quaternary
              class="secui__action"
              :type="action.danger ? 'error' : 'default'"
              :loading="running && resultTitle === action.label"
              @click="onAction(action)"
            >
              {{ action.label }}
              <span v-if="action.fields?.length" class="secui__form-mark">表单</span>
            </n-button>
          </div>
        </template>
        <p v-if="loading" class="secui__hint">正在读取操作目录…</p>

        <template v-if="app?.config?.length">
          <p class="secui__section">配置文件</p>
          <div class="secui__configs">
            <button
              v-for="entry in app.config"
              :key="entry.path"
              type="button"
              class="secui__config"
              :aria-label="`${entry.isDir ? '浏览' : '编辑'} ${entry.path}`"
              :title="entry.isDir ? `浏览 ${entry.path}` : `编辑 ${entry.path}`"
              @click="openConfig(entry)"
            >
              <code>{{ entry.path }}</code>
              <span>{{ entry.isDir ? '浏览' : '编辑' }}</span>
            </button>
          </div>
        </template>

        <p class="secui__hint">
          {{ app?.ui?.hint || '所有操作由面板按白名单执行；需要 root 的动作会复用主机已配置的提权凭据。' }}
        </p>
      </aside>

      <main class="secui__main" :aria-busy="loading || running">
        <div v-if="loading" class="secui__skeleton" aria-label="正在读取应用信息">
          <span v-for="line in 4" :key="line" class="secui__sk-line" />
        </div>
        <div v-else-if="loadError && !app" class="secui__empty secui__empty--error">
          <span>{{ loadError }}</span>
          <n-button size="small" @click="load">重试</n-button>
        </div>

        <template v-else-if="mode === 'config'">
          <div class="secui__panel-head">
            <span class="secui__panel-title">
              {{ configFile?.path || configDir?.path || '配置文件' }}
            </span>
            <n-tag v-if="configDirty" size="tiny" :bordered="false" type="warning">未保存</n-tag>
            <span v-if="configFile" class="row-sub">{{ configFile.content.length }} 字符</span>
            <span class="secui__spacer" />
            <template v-if="configFile">
              <n-button size="small" :loading="configLoading" @click="reloadConfig">
                {{ configDirty ? '放弃修改' : '重新读取' }}
              </n-button>
              <n-button
                size="small"
                type="primary"
                :loading="configSaving"
                :disabled="!configDirty"
                @click="saveConfig"
              >
                保存
              </n-button>
            </template>
          </div>

          <div v-if="configError" class="secui__empty secui__empty--error">{{ configError }}</div>
          <div v-else-if="configLoading" class="secui__empty">正在读取…</div>
          <n-input
            v-else-if="configFile"
            v-model:value="configFile.content"
            type="textarea"
            class="secui__editor"
            placeholder="在此编辑配置文件内容"
            :autosize="{ minRows: 16, maxRows: 30 }"
          />
          <pre v-else-if="configDir" class="secui__output">{{ configDir.output }}</pre>
          <p v-if="configFile" class="secui__hint">
            <template v-if="!configFile.exists">文件尚不存在，保存后会创建。</template>
            <template v-else>保存前会自动备份为 {{ configFile.backup }}。</template>
            目录型配置只读。
          </p>
        </template>

        <template v-else>
          <section v-if="activeAction" class="secui__form">
            <div class="secui__panel-head">
              <span class="secui__panel-title">{{ activeAction.label }}</span>
              <span v-if="activeAction.description" class="row-sub">
                {{ activeAction.description }}
              </span>
            </div>
            <n-form class="secui__fields" label-placement="top" :show-feedback="true">
              <n-form-item
                v-for="field in activeAction.fields"
                :key="field.key"
                :label="field.label"
                :required="field.required"
              >
                <n-select
                  v-if="field.type === 'select'"
                  v-model:value="form[field.key]"
                  :options="selectOptions(field)"
                />
                <n-input
                  v-else-if="field.type === 'textarea'"
                  v-model:value="form[field.key]"
                  type="textarea"
                  :autosize="{ minRows: 4, maxRows: 12 }"
                  :placeholder="field.placeholder"
                />
                <n-input
                  v-else
                  v-model:value="form[field.key]"
                  :type="field.type === 'password' ? 'password' : 'text'"
                  :show-password-on="field.type === 'password' ? 'click' : undefined"
                  :placeholder="field.placeholder"
                />
                <template v-if="field.help" #feedback>
                  <span class="secui__field-help">{{ field.help }}</span>
                </template>
              </n-form-item>
            </n-form>
            <n-space justify="end">
              <n-button size="small" @click="cancelForm">取消</n-button>
              <n-button size="small" type="primary" :loading="running" @click="submitForm">
                执行
              </n-button>
            </n-space>
          </section>

          <section v-if="result" class="secui__result" aria-live="polite">
            <div class="secui__panel-head">
              <span class="secui__led" :class="result.ok ? 'is-ok' : 'is-error'" aria-hidden="true" />
              <span class="secui__panel-title">{{ resultTitle }}</span>
              <n-tag size="tiny" :bordered="false" :type="result.ok ? 'success' : 'error'">
                {{ result.ok ? '成功' : '失败' }}
              </n-tag>
              <span class="row-sub">{{ result.durationMs }} ms</span>
              <span class="secui__spacer" />
              <n-button size="tiny" quaternary @click="copyResult">复制结果</n-button>
            </div>
            <pre class="secui__output">{{ result.output }}</pre>
          </section>

          <div v-else-if="!activeAction" class="secui__empty">
            从左侧选择一项操作，或打开配置文件。
          </div>
        </template>
      </main>
    </div>
  </div>
</template>

<style scoped>
.secui {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  background: var(--bg-module);
  container-type: inline-size;
}

.secui__head {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 12px;
  border-bottom: 1px solid var(--line-hair);
}

.secui__icon {
  flex: none;
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  border: 1px solid var(--warn-line);
  border-radius: var(--r-2);
  background: var(--warn-wash);
  color: var(--warn);
}

.secui__icon svg {
  width: 17px;
  height: 17px;
}

.secui__icon-img {
  width: 17px;
  height: 17px;
  color: var(--warn);
}

.secui__title {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.secui__name {
  overflow: hidden;
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  color: var(--ink-hi);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.secui__body {
  display: flex;
  flex: 1;
  min-height: 0;
}

.secui__side {
  display: flex;
  flex: none;
  flex-direction: column;
  gap: 10px;
  width: 236px;
  padding: 12px;
  overflow-y: auto;
  border-right: 1px solid var(--line-hair);
}

.secui__section {
  margin: 0;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  font-weight: 600;
  letter-spacing: 0.14em;
  color: var(--ink-lo);
}

.secui__actions {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

/* 操作按钮左对齐：侧栏是工具清单，不是一组居中的卡片。 */
.secui__action :deep(.n-button__content) {
  justify-content: flex-start;
  width: 100%;
}

.secui__form-mark {
  margin-left: 4px;
  padding: 0 4px;
  border-radius: var(--r-1);
  background: var(--bg-raise);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.secui__configs {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.secui__config {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 5px 8px;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  color: var(--ink);
  cursor: pointer;
  text-align: left;
  transition: border-color var(--dur-1) var(--ease);
}

.secui__config:hover {
  border-color: var(--line-strong);
}

.secui__config:focus-visible {
  outline: 2px solid var(--sig);
  outline-offset: 2px;
}

.secui__config code {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  color: var(--ink-hi);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.secui__config span {
  flex: none;
  font-size: var(--fs-label);
  color: var(--warn);
}

.secui__hint {
  margin: 0;
  font-size: var(--fs-label);
  line-height: 1.6;
  color: var(--ink-lo);
}

.secui__main {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: 10px;
  min-width: 0;
  min-height: 0;
  padding: 12px;
  overflow: auto;
}

.secui__panel-head {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 26px;
}

.secui__panel-title {
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  color: var(--ink-hi);
}

.secui__spacer {
  flex: 1;
}

.secui__led {
  flex: none;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--ink-lo);
}

.secui__led.is-ok {
  background: var(--ok);
}

.secui__led.is-error {
  background: var(--danger);
}

.secui__skeleton {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 8px 0;
}

.secui__sk-line {
  height: 14px;
  border-radius: var(--r-2);
  background: linear-gradient(90deg, var(--bg-sunken), var(--bg-raise), var(--bg-sunken));
  background-size: 200% 100%;
  animation: secui-shimmer 1.2s linear infinite;
}

.secui__sk-line:nth-child(2) {
  width: 72%;
}

.secui__sk-line:nth-child(3) {
  width: 88%;
}

.secui__sk-line:nth-child(4) {
  width: 54%;
}

@keyframes secui-shimmer {
  from {
    background-position: 200% 0;
  }
  to {
    background-position: -200% 0;
  }
}

.secui__empty {
  display: flex;
  flex: 1;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 32px 16px;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
  text-align: center;
}

.secui__empty--error {
  color: var(--danger);
}

.secui__form {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-bottom: 10px;
  border-bottom: 1px solid var(--line-hair);
}

.secui__field-help {
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

/* 短字段并排，长表单不用滚一屏；单字段时仍占满整行。 */
.secui__fields {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 4px 12px;
}

.secui__result {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: 8px;
  min-height: 0;
}

.secui__output {
  flex: 1;
  min-height: 120px;
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

.secui__editor {
  flex: 1;
  min-height: 0;
}

.secui__editor :deep(.n-input__textarea-el) {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  line-height: 1.65;
}

/* 窄窗口：侧栏变成顶部工具带，主区仍保留完整宽度。 */
@container (max-width: 720px) {
  .secui__body {
    flex-direction: column;
  }

  .secui__side {
    width: auto;
    max-height: 220px;
    border-right: 0;
    border-bottom: 1px solid var(--line-hair);
  }

  .secui__actions {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
  }
}

@media (prefers-reduced-motion: reduce) {
  .secui__config {
    transition: none;
  }

  .secui__sk-line {
    animation: none;
  }
}
</style>
