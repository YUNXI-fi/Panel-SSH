<script setup lang="ts">
/**
 * GnuPG 专用窗口。
 *
 * 通用安全窗口只会把 gpg 的输出贴成一段文本。这里把钥匙环画成密钥卡片：
 * 公钥/私钥分段切换，算法、有效期、指纹、用途一眼可读；生成、导入、导出、
 * 删除都在界面上完成，用户不需要输入命令，也不需要看 gpg 的输出格式。
 *
 * 数据来自后端解析好的 /security-apps/gnupg/keys（--with-colons）。
 */
import { computed, onMounted, ref } from 'vue';
import {
  NButton,
  NForm,
  NFormItem,
  NInput,
  NModal,
  NSelect,
  NSpace,
  NSpin,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui';

import {
  ApiError,
  api,
  type GnuPgpKey,
  type SecurityAction,
  type SecurityAppView,
  type SecurityConfigEntry,
  type SecurityRunResult,
} from '../api/client';
import { securityGlyph } from '../securityApps';
import SecurityConfigDialog from './SecurityConfigDialog.vue';

const props = defineProps<{
  hostId: number;
  hostName: string;
  slug: string;
  name: string;
  glyph?: string;
}>();

const dialog = useDialog();
const message = useMessage();

const view = ref<SecurityAppView | null>(null);
const publicKeys = ref<GnuPgpKey[]>([]);
const secretKeys = ref<GnuPgpKey[]>([]);
const loading = ref(false);
const loaded = ref(false);
const error = ref('');
const available = ref(true);
const checkedAt = ref('');
const tab = ref<'public' | 'secret'>('public');
const busy = ref('');
const notice = ref<{ title: string; text: string } | null>(null);
const showConfig = ref(false);

const showGenerate = ref(false);
const genForm = ref({ name: '', email: '', algo: 'rsa2048', expire: '2y', passphrase: '' });

const showImport = ref(false);
const importText = ref('');

const showExport = ref(false);
const exportTarget = ref<GnuPgpKey | null>(null);
const exportText = ref('');

const algoOptions = [
  { label: 'RSA 2048', value: 'rsa2048' },
  { label: 'RSA 4096', value: 'rsa4096' },
  { label: 'Ed25519', value: 'ed25519' },
];
const expireOptions = [
  { label: '1 年', value: '1y' },
  { label: '2 年', value: '2y' },
  { label: '5 年', value: '5y' },
  { label: '永久', value: '0' },
];

const glyph = computed(() => props.glyph || securityGlyph(props.slug));
const configs = computed<SecurityConfigEntry[]>(() => view.value?.config ?? []);
const secretFingerprints = computed(() => new Set(secretKeys.value.map((key) => key.fingerprint)));
const activeKeys = computed(() => (tab.value === 'secret' ? secretKeys.value : publicKeys.value));
const sortedKeys = computed(() => [...activeKeys.value].sort((a, b) => b.created - a.created));
const statusText = computed(
  () => `${publicKeys.value.length} 个公钥 · ${secretKeys.value.length} 个私钥`,
);
const checkedAtText = computed(() => formatClock(checkedAt.value));

function formatClock(value: string): string {
  const date = new Date(value);
  if (!value || Number.isNaN(date.getTime())) return '';
  return date.toLocaleTimeString('zh-CN', { hour12: false });
}

function formatDate(epoch: number): string {
  if (!epoch) return '—';
  return new Date(epoch * 1000).toLocaleDateString('zh-CN');
}

function describe(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  return err instanceof Error ? err.message : '读取密钥失败';
}

function findAction(id: string): SecurityAction | undefined {
  return view.value?.actions.find((action) => action.id === id);
}

const CAP_LABELS: Record<string, string> = {
  sign: '签名',
  certify: '证书',
  encrypt: '加密',
  authenticate: '认证',
};

function capLabel(capability: string): string {
  return CAP_LABELS[capability] ?? capability;
}

/** RSA/DSA 的长度是选型关键，曲线算法（Ed25519 等）报 255 位只会误导。 */
function algorithmLabel(key: GnuPgpKey): string {
  if (key.algorithm === 'RSA' || key.algorithm === 'DSA') {
    return `${key.algorithm} ${key.bits}`;
  }
  return key.algorithm;
}

function subkeySummary(key: GnuPgpKey): string {
  const first = key.subkeys[0];
  if (!first) return '';
  const name = first.algorithm === 'RSA' ? `RSA ${first.bits}` : first.algorithm;
  const caps = first.capabilities.map(capLabel).join('/');
  const more = key.subkeys.length > 1 ? ` 等 ${key.subkeys.length} 把` : '';
  return `${name}${caps ? ` · ${caps}` : ''}${more}`;
}

async function load(silent = false): Promise<void> {
  if (loading.value) return;
  loading.value = true;
  if (!silent) error.value = '';
  try {
    const [detail, pub, sec] = await Promise.all([
      api.securityApps.get(props.hostId, props.slug),
      api.securityApps.gnupgKeys(props.hostId, 'public'),
      api.securityApps.gnupgKeys(props.hostId, 'secret'),
    ]);
    view.value = detail;
    available.value = pub.available;
    publicKeys.value = pub.available ? pub.keys : [];
    secretKeys.value = sec.available ? sec.keys : [];
    checkedAt.value = pub.checkedAt;
    if (!pub.available) {
      error.value = pub.error || 'gpg 不可用';
    } else if (!sec.available) {
      error.value = sec.error || '读取私钥列表失败';
    } else {
      error.value = '';
    }
  } catch (err) {
    error.value = describe(err);
  } finally {
    loading.value = false;
    loaded.value = true;
  }
}

/** 变更动作统一走这里：成功由调用方决定是否关弹窗与回读，失败把输出留在页面上。 */
async function execute(
  actionId: string,
  busyKey: string,
  params: Record<string, string>,
): Promise<SecurityRunResult | null> {
  const action = findAction(actionId);
  if (!action) {
    message.error('当前版本没有提供这个操作');
    return null;
  }
  if (busy.value) return null;
  busy.value = busyKey;
  notice.value = null;
  try {
    const res = await api.securityApps.run(props.hostId, props.slug, action.id, params);
    if (!res.ok) {
      notice.value = { title: `${action.label}失败`, text: res.output };
    }
    return res;
  } catch (err) {
    notice.value = { title: `${action.label}失败`, text: describe(err) };
    return null;
  } finally {
    busy.value = '';
  }
}

function openGenerate(): void {
  genForm.value = { name: '', email: '', algo: 'rsa2048', expire: '2y', passphrase: '' };
  showGenerate.value = true;
}

async function submitGenerate(): Promise<void> {
  if (!genForm.value.name.trim()) {
    message.warning('请填写姓名');
    return;
  }
  const res = await execute('generate', 'generate', { ...genForm.value });
  if (!res?.ok) return;
  showGenerate.value = false;
  message.success('密钥已生成');
  await load(true);
}

function openImport(): void {
  importText.value = '';
  showImport.value = true;
}

async function submitImport(): Promise<void> {
  if (!importText.value.trim()) {
    message.warning('请粘贴公钥文本');
    return;
  }
  const res = await execute('import', 'import', { armored: importText.value });
  if (!res?.ok) return;
  showImport.value = false;
  message.success('公钥已导入');
  await load(true);
}

async function copyFingerprint(key: GnuPgpKey): Promise<void> {
  try {
    await navigator.clipboard.writeText(key.fingerprint);
    message.success('指纹已复制');
  } catch {
    message.error('复制失败，请手动选择指纹');
  }
}

async function startExport(key: GnuPgpKey): Promise<void> {
  if (busy.value) return;
  exportTarget.value = key;
  exportText.value = '';
  showExport.value = true;
  const res = await execute('export', 'export', { key: key.fingerprint });
  if (!res) {
    showExport.value = false;
    return;
  }
  if (!res.ok) {
    showExport.value = false;
    return;
  }
  // 只取 stdout：stderr 里的警告混进来会破坏 ASCII 公钥块。
  exportText.value = (res.stdout ?? res.output).trim();
}

async function copyExport(): Promise<void> {
  try {
    await navigator.clipboard.writeText(exportText.value);
    message.success('公钥已复制');
  } catch {
    message.error('复制失败，请手动选择内容');
  }
}

function downloadExport(): void {
  const key = exportTarget.value;
  if (!key || !exportText.value) return;
  const blob = new Blob([exportText.value], { type: 'application/pgp-keys' });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = `gnupg-${key.keyId}.asc`;
  anchor.click();
  URL.revokeObjectURL(url);
}

/** 公钥页上的"删除"要区分是否持有私钥：gpg 不允许单独删掉有私钥的公钥。 */
function confirmDelete(key: GnuPgpKey): void {
  const withSecret = tab.value === 'secret' || secretFingerprints.value.has(key.fingerprint);
  dialog.warning({
    title: withSecret ? '删除密钥对？' : '删除公钥？',
    content: withSecret
      ? `会同时删除私钥与公钥，且无法恢复。指纹 ${key.fingerprint}`
      : `公钥删除后需要重新导入才能使用。指纹 ${key.fingerprint}`,
    positiveText: withSecret ? '删除密钥对' : '删除公钥',
    negativeText: '取消',
    onPositiveClick: async () => {
      const res = await execute('delete', 'delete', {
        scope: withSecret ? 'secret' : 'public',
        fingerprint: key.fingerprint,
      });
      if (!res?.ok) return;
      message.success('密钥已删除');
      await load(true);
    },
  });
}

onMounted(() => {
  void load();
});
</script>

<template>
  <div class="gpg">
    <div class="gpg__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">HOST SECURITY / GNUPG</p>
        <span class="page-title">
          <span
            class="led"
            :class="!available ? 'led--danger' : sortedKeys.length ? 'led--ok' : 'led--warn'"
            aria-hidden="true"
          />
          <svg class="gpg__glyph" aria-hidden="true">
            <use :href="`#glyph-${glyph}`" />
          </svg>
          {{ view?.name || name }}
        </span>
        <span class="page-meta">
          {{ hostName }} · {{ statusText }}
          <template v-if="checkedAtText"> · 更新于 {{ checkedAtText }}</template>
        </span>
      </div>

      <span class="gpg__grow" />

      <div class="gpg__tools">
        <n-button v-if="configs.length" size="small" quaternary @click="showConfig = true">
          配置文件
        </n-button>
        <n-button
          v-if="findAction('import')"
          size="small"
          :disabled="!available || busy !== ''"
          @click="openImport"
        >
          导入公钥
        </n-button>
        <n-button
          v-if="findAction('generate')"
          size="small"
          :disabled="!available || busy !== ''"
          @click="openGenerate"
        >
          生成密钥
        </n-button>
        <n-button size="small" type="primary" class="refresh-btn" @click="load()">
          <template #icon v-if="loading">
            <n-spin :size="16" />
          </template>
          <template v-if="!loading">刷新</template>
        </n-button>
      </div>
    </div>

    <div v-if="error" class="gpg__banner is-error">
      <span class="led led--danger" aria-hidden="true" />
      <span>{{ error }}</span>
      <span class="gpg__grow" />
      <n-button size="tiny" quaternary @click="load()">重试</n-button>
    </div>

    <div v-if="notice" class="gpg__banner is-error">
      <span class="led led--danger" aria-hidden="true" />
      <div class="gpg__notice">
        <b>{{ notice.title }}</b>
        <pre>{{ notice.text }}</pre>
      </div>
      <span class="gpg__grow" />
      <n-button size="tiny" quaternary @click="notice = null">关闭</n-button>
    </div>

    <div class="gpg__tabs">
      <div class="gpg__seg" role="tablist" aria-label="密钥类型">
        <button
          type="button"
          role="tab"
          :aria-selected="tab === 'public'"
          :class="{ 'is-on': tab === 'public' }"
          @click="tab = 'public'"
        >
          公钥 <span class="num">{{ publicKeys.length }}</span>
        </button>
        <button
          type="button"
          role="tab"
          :aria-selected="tab === 'secret'"
          :class="{ 'is-on': tab === 'secret' }"
          @click="tab = 'secret'"
        >
          私钥 <span class="num">{{ secretKeys.length }}</span>
        </button>
      </div>
      <span class="gpg__hint">密钥属于登录用户的 ~/.gnupg 钥匙环</span>
      <span class="gpg__grow" />
    </div>

    <div v-if="loaded && !available" class="gpg__empty ui-card">
      <span class="gpg__empty-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6">
          <circle cx="8" cy="12" r="4" />
          <path d="M12 12h9M18 12v3M15 12v2" />
        </svg>
      </span>
      <h3>GnuPG 不可用</h3>
      <p>目标机上找不到可用的 gpg，或当前用户没有可读的钥匙环。</p>
      <n-button :loading="loading" @click="load()">重新检测</n-button>
    </div>

    <div
      v-else-if="!loaded || (loading && sortedKeys.length === 0)"
      class="gpg__placeholder"
    >
      <n-spin :size="20" />
      <span>正在读取钥匙环…</span>
    </div>

    <div v-else-if="error && sortedKeys.length === 0" class="gpg__empty ui-card">
      <span class="gpg__empty-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6">
          <circle cx="8" cy="12" r="4" />
          <path d="M12 12h9M18 12v3M15 12v2" />
        </svg>
      </span>
      <h3>读取密钥失败</h3>
      <p>{{ error }}</p>
      <n-button :loading="loading" @click="load()">重新读取</n-button>
    </div>

    <div v-else-if="sortedKeys.length === 0" class="gpg__empty ui-card">
      <span class="gpg__empty-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6">
          <circle cx="8" cy="12" r="4" />
          <path d="M12 12h9M18 12v3M15 12v2" />
        </svg>
      </span>
      <h3>{{ tab === 'secret' ? '还没有私钥' : '还没有公钥' }}</h3>
      <p>
        {{
          tab === 'secret'
            ? '生成一对新密钥后，私钥会出现在这里。'
            : '生成一对新密钥，或导入别人发来的 ASCII 公钥。'
        }}
      </p>
      <div class="gpg__empty-actions">
        <n-button type="primary" :disabled="!findAction('generate')" @click="openGenerate">
          生成密钥
        </n-button>
        <n-button
          v-if="tab === 'public'"
          :disabled="!findAction('import')"
          @click="openImport"
        >
          导入公钥
        </n-button>
      </div>
    </div>

    <div v-else class="gpg__keys">
      <article v-for="key in sortedKeys" :key="key.fingerprint" class="gpg-key ui-card">
        <header class="ui-card__head">
          <div class="ui-card__ident">
            <p class="ui-card__eyebrow">{{ tab === 'secret' ? 'SECRET KEY' : 'PUBLIC KEY' }}</p>
            <h3 class="ui-card__title">{{ key.name || key.uid }}</h3>
            <p class="gpg-key__sub">
              <span v-if="key.email">{{ key.email }} · </span>
              <span class="num">{{ key.keyId }}</span>
            </p>
          </div>
          <div class="ui-card__tools gpg-key__tools">
            <n-tag v-if="key.validity === 'r'" size="tiny" :bordered="false" type="error">
              已吊销
            </n-tag>
            <n-tag v-else-if="key.validity === 'e'" size="tiny" :bordered="false" type="warning">
              已过期
            </n-tag>
            <n-tag
              v-if="tab === 'public' && secretFingerprints.has(key.fingerprint)"
              size="tiny"
              :bordered="false"
              type="info"
            >
              含私钥
            </n-tag>
            <n-button size="tiny" quaternary @click="copyFingerprint(key)">复制指纹</n-button>
            <n-button
              size="tiny"
              quaternary
              :loading="busy === 'export' && exportTarget?.fingerprint === key.fingerprint"
              @click="startExport(key)"
            >
              导出
            </n-button>
            <n-button
              size="tiny"
              quaternary
              type="error"
              :disabled="busy !== ''"
              @click="confirmDelete(key)"
            >
              删除
            </n-button>
          </div>
        </header>

        <dl class="gpg-key__facts">
          <div>
            <dt>算法</dt>
            <dd>{{ algorithmLabel(key) }}</dd>
          </div>
          <div>
            <dt>创建</dt>
            <dd class="num">{{ formatDate(key.created) }}</dd>
          </div>
          <div>
            <dt>到期</dt>
            <dd class="num">{{ key.expires ? formatDate(key.expires) : '永久' }}</dd>
          </div>
        </dl>

        <div class="gpg-key__fpr">
          <span class="silk">指纹</span>
          <code class="num">{{ key.fingerprint }}</code>
        </div>

        <footer class="gpg-key__foot">
          <div class="gpg-key__caps">
            <span v-for="cap in key.capabilities" :key="cap" class="gpg-key__cap">
              {{ capLabel(cap) }}
            </span>
            <span v-if="key.capabilities.length === 0" class="gpg-key__cap is-muted">无用途标记</span>
          </div>
          <span v-if="key.subkeys.length" class="gpg-key__subs">
            子钥 {{ key.subkeys.length }} · {{ subkeySummary(key) }}
          </span>
        </footer>
      </article>
    </div>

    <!-- 生成密钥 -->
    <n-modal v-model:show="showGenerate" preset="card" title="生成密钥" style="width: 540px">
      <n-form label-placement="top" :model="genForm">
        <n-form-item label="姓名" required>
          <n-input
            v-model:value="genForm.name"
            maxlength="64"
            placeholder="密钥用户标识里的姓名，如 Zhang San"
          />
        </n-form-item>
        <n-form-item label="邮箱">
          <n-input
            v-model:value="genForm.email"
            maxlength="190"
            placeholder="选填，如 zhang@example.com"
          />
        </n-form-item>
        <div class="gpg__form-row">
          <n-form-item label="算法">
            <n-select v-model:value="genForm.algo" :options="algoOptions" />
          </n-form-item>
          <n-form-item label="有效期">
            <n-select v-model:value="genForm.expire" :options="expireOptions" />
          </n-form-item>
        </div>
        <n-form-item label="口令">
          <n-input
            v-model:value="genForm.passphrase"
            type="password"
            show-password-on="click"
            maxlength="128"
            placeholder="留空表示不设口令"
          />
          <template #feedback>
            <span class="gpg__help">建议设置口令保护私钥；留空则私钥不加保护。</span>
          </template>
        </n-form-item>
      </n-form>
      <p class="gpg__help">生成过程可能持续数十秒，期间保持窗口打开即可。</p>
      <template #footer>
        <n-space justify="end">
          <n-button :disabled="busy !== ''" @click="showGenerate = false">取消</n-button>
          <n-button type="primary" :loading="busy === 'generate'" @click="submitGenerate">
            生成密钥
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 导入公钥 -->
    <n-modal v-model:show="showImport" preset="card" title="导入公钥" style="width: 620px">
      <n-input
        v-model:value="importText"
        type="textarea"
        class="gpg__import"
        placeholder="粘贴以 -----BEGIN PGP PUBLIC KEY BLOCK----- 开头的 ASCII 公钥"
        :autosize="{ minRows: 10, maxRows: 18 }"
      />
      <p class="gpg__help">
        只接受 ASCII 公钥；私钥块不会被接受，私钥请留在生成它的机器上。
      </p>
      <template #footer>
        <n-space justify="end">
          <n-button :disabled="busy !== ''" @click="showImport = false">取消</n-button>
          <n-button type="primary" :loading="busy === 'import'" @click="submitImport">
            导入
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 导出公钥 -->
    <n-modal
      v-model:show="showExport"
      preset="card"
      :title="`导出公钥${exportTarget?.name ? ` · ${exportTarget.name}` : ''}`"
      style="width: 660px"
    >
      <div v-if="busy === 'export'" class="gpg__placeholder is-compact">
        <n-spin :size="18" />
        <span>正在导出…</span>
      </div>
      <pre v-else class="gpg__export">{{ exportText }}</pre>
      <template #footer>
        <n-space justify="end">
          <n-button :disabled="!exportText" @click="copyExport">复制</n-button>
          <n-button :disabled="!exportText" @click="downloadExport">下载 .asc</n-button>
          <n-button type="primary" @click="showExport = false">关闭</n-button>
        </n-space>
      </template>
    </n-modal>

    <SecurityConfigDialog
      v-model:show="showConfig"
      :host-id="hostId"
      :slug="slug"
      :entries="configs"
    />
  </div>
</template>

<style scoped>
.gpg {
  display: flex;
  flex-direction: column;
  gap: 14px;
  height: 100%;
  padding: 16px 18px 18px;
  overflow: hidden;
  container-type: inline-size;
}

.gpg__bar {
  display: flex;
  flex: none;
  gap: var(--sp-3);
}

.gpg__grow {
  flex: 1;
}

.gpg__glyph {
  align-self: center;
  width: 17px;
  height: 17px;
  color: var(--warn);
}

.gpg__tools {
  display: flex;
  flex: none;
  align-items: center;
  gap: 8px;
}

.gpg__banner {
  display: flex;
  flex: none;
  align-items: flex-start;
  gap: var(--sp-2);
  padding: 8px 10px;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  font-size: var(--fs-sm);
  color: var(--ink);
}

.gpg__banner.is-error {
  border-color: var(--danger-line);
  background: var(--danger-wash);
  color: var(--danger-ink);
}

.gpg__notice {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

.gpg__notice pre {
  max-height: 140px;
  margin: 0;
  overflow: auto;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

.gpg__tabs {
  display: flex;
  flex: none;
  align-items: center;
  gap: 12px;
}

.gpg__seg {
  display: inline-flex;
  flex: none;
  padding: 3px;
  border: 1px solid var(--line);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
}

.gpg__seg button {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 5px 12px;
  border: 0;
  border-radius: var(--r-1);
  background: transparent;
  font: inherit;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
  cursor: pointer;
}

.gpg__seg button:hover {
  color: var(--ink);
}

.gpg__seg button.is-on {
  background: var(--bg-raise);
  color: var(--ink-hi);
  box-shadow: 0 1px 0 var(--wash-4) inset;
}

.gpg__seg button:focus-visible {
  outline: 2px solid var(--sig);
  outline-offset: 2px;
}

.gpg__seg .num {
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.gpg__hint {
  overflow: hidden;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  color: var(--ink-lo);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.gpg__placeholder {
  display: flex;
  flex: 1;
  align-items: center;
  justify-content: center;
  gap: 10px;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
}

.gpg__placeholder.is-compact {
  flex: none;
  padding: 24px 0;
}

.gpg__empty {
  flex: 1;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 32px;
  text-align: center;
}

.gpg__empty-icon {
  display: grid;
  place-items: center;
  width: 52px;
  height: 52px;
  border: 1px solid var(--line);
  border-radius: 50%;
  background: var(--bg-sunken);
  color: var(--ink-lo);
}

.gpg__empty-icon svg {
  width: 26px;
  height: 26px;
}

.gpg__empty h3 {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--ink-hi);
}

.gpg__empty p {
  max-width: 420px;
  margin: 0;
  font-size: var(--fs-sm);
  line-height: 1.7;
  color: var(--ink-lo);
}

.gpg__empty-actions {
  display: flex;
  gap: 8px;
  margin-top: 4px;
}

.gpg__keys {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: 14px;
  min-height: 0;
  padding-bottom: 2px;
  overflow-y: auto;
}

.gpg-key__sub {
  margin: 2px 0 0;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.gpg-key__tools {
  flex-wrap: wrap;
  justify-content: flex-end;
}

.gpg-key__facts {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
  margin: 0 0 10px;
}

.gpg-key__facts div {
  padding: 8px 10px;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
}

.gpg-key__facts dt {
  margin: 0 0 2px;
  font-family: var(--font-mono);
  font-size: 9px;
  font-weight: 600;
  letter-spacing: 0.16em;
  color: var(--ink-lo);
  text-transform: uppercase;
}

.gpg-key__facts dd {
  margin: 0;
  font-size: var(--fs-sm);
  color: var(--ink-hi);
}

.gpg-key__fpr {
  display: flex;
  align-items: baseline;
  gap: 10px;
  padding: 8px 10px;
  border: 1px dashed var(--line);
  border-radius: var(--r-2);
}

.gpg-key__fpr code {
  flex: 1;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--ink);
  letter-spacing: 0.04em;
  overflow-wrap: anywhere;
}

.gpg-key__foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin-top: 10px;
  padding-top: 9px;
  border-top: 1px solid var(--line-hair);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.gpg-key__caps {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}

.gpg-key__cap {
  padding: 1px 6px;
  border: 1px solid var(--line);
  border-radius: var(--r-1);
  color: var(--ink);
}

.gpg-key__cap.is-muted {
  border-color: var(--line-hair);
  color: var(--ink-lo);
}

.gpg-key__subs {
  flex: none;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.gpg__form-row {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 12px;
}

.gpg__help {
  margin: 6px 0 0;
  font-size: var(--fs-label);
  line-height: 1.6;
  color: var(--ink-lo);
}

.gpg__import :deep(.n-input__textarea-el) {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  line-height: 1.6;
}

.gpg__export {
  max-height: 46vh;
  margin: 0;
  padding: 10px 12px;
  overflow: auto;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--ink);
  white-space: pre-wrap;
  word-break: break-word;
}

@container (max-width: 700px) {
  .gpg__bar {
    flex-direction: column;
    align-items: stretch;
  }

  .gpg__tools {
    flex-wrap: wrap;
  }

  .gpg-key__facts {
    grid-template-columns: minmax(0, 1fr);
  }

  .gpg-key__foot {
    flex-direction: column;
    align-items: flex-start;
  }
}
</style>
