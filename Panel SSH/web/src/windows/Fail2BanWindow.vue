<script setup lang="ts">
/**
 * Fail2Ban 专用窗口。
 *
 * 通用安全窗口把 fail2ban-client 的输出整段贴出来，用户看到的还是命令行。
 * 这里改成"态势 + 卡片 + 解封按钮"：Jail 用卡片列出，封禁地址逐行展示，
 * 解封直接在地址行上完成；重载、校验、启动放在页头，配置文件走弹窗。
 *
 * 数据来自后端解析好的 /security-apps/fail2ban/overview，前端不拆命令文本。
 */
import { computed, onMounted, ref } from 'vue';
import { NButton, NSpin, useMessage } from 'naive-ui';

import {
  ApiError,
  api,
  type Fail2BanJail,
  type Fail2BanOverview,
  type SecurityAction,
  type SecurityAppView,
  type SecurityConfigEntry,
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

const message = useMessage();

const view = ref<SecurityAppView | null>(null);
const overview = ref<Fail2BanOverview | null>(null);
const loading = ref(false);
const error = ref('');
const busy = ref('');
const notice = ref<{ title: string; text: string } | null>(null);
const showConfig = ref(false);

const glyph = computed(() => props.glyph || securityGlyph(props.slug));
const jails = computed(() => overview.value?.jails ?? []);
const running = computed(() => overview.value?.running === true);
const available = computed(() => overview.value?.available !== false);
const configs = computed<SecurityConfigEntry[]>(() => view.value?.config ?? []);

const totals = computed(() =>
  jails.value.reduce(
    (acc, jail) => ({
      failed: acc.failed + jail.currentlyFailed,
      totalFailed: acc.totalFailed + jail.totalFailed,
      banned: acc.banned + jail.currentlyBanned,
      totalBanned: acc.totalBanned + jail.totalBanned,
    }),
    { failed: 0, totalFailed: 0, banned: 0, totalBanned: 0 },
  ),
);

const statCards = computed(() => [
  { key: 'jails', label: 'JAIL 数', value: jails.value.length, foot: '已启用的过滤器' },
  { key: 'banned', label: '当前封禁', value: totals.value.banned, foot: '所有 Jail 合计' },
  { key: 'totalBanned', label: '累计封禁', value: totals.value.totalBanned, foot: '自服务启动以来' },
  { key: 'failed', label: '当前失败', value: totals.value.failed, foot: '正在观察的来源' },
]);

const statusText = computed(() => {
  if (!overview.value) return loading.value ? '正在读取状态' : '状态未知';
  if (!available.value) return '未安装或命令不可用';
  if (!running.value) return '服务未运行';
  return `${jails.value.length} 个 Jail · 当前封禁 ${totals.value.banned}`;
});

const checkedAt = computed(() => formatClock(overview.value?.checkedAt ?? ''));

function formatClock(value: string): string {
  const date = new Date(value);
  if (!value || Number.isNaN(date.getTime())) return '';
  return date.toLocaleTimeString('zh-CN', { hour12: false });
}

function describe(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  return err instanceof Error ? err.message : '读取 Fail2Ban 状态失败';
}

function findAction(id: string): SecurityAction | undefined {
  return view.value?.actions.find((action) => action.id === id);
}

async function load(silent = false): Promise<void> {
  if (loading.value) return;
  loading.value = true;
  if (!silent) error.value = '';
  try {
    const [detail, data] = await Promise.all([
      api.securityApps.get(props.hostId, props.slug),
      api.securityApps.fail2banOverview(props.hostId),
    ]);
    view.value = detail;
    overview.value = data;
    error.value = '';
  } catch (err) {
    error.value = describe(err);
  } finally {
    loading.value = false;
  }
}

/** 所有变更动作共用一条执行路径：成功后回读状态，失败时把输出留在页面上。 */
async function execute(
  actionId: string,
  busyKey: string,
  params: Record<string, string>,
  successText: string,
): Promise<void> {
  const action = findAction(actionId);
  if (!action) {
    message.error('当前版本没有提供这个操作');
    return;
  }
  if (busy.value) return;
  busy.value = busyKey;
  notice.value = null;
  try {
    const res = await api.securityApps.run(props.hostId, props.slug, action.id, params);
    if (res.ok) {
      message.success(successText);
      await load(true);
      return;
    }
    notice.value = { title: `${action.label}失败`, text: res.output };
  } catch (err) {
    notice.value = { title: `${action.label}失败`, text: describe(err) };
  } finally {
    busy.value = '';
  }
}

function startService(): void {
  void execute('start', 'start', {}, 'Fail2Ban 已启动');
}

function reloadConfig(): void {
  void execute('reload', 'reload', {}, '配置已重载');
}

function validateConfig(): void {
  void execute('test', 'test', {}, '配置校验通过');
}

function unbanKey(jail: Fail2BanJail, ip: string): string {
  return `unban:${jail.name}:${ip}`;
}

function unban(jail: Fail2BanJail, ip: string): void {
  void execute('unban', unbanKey(jail, ip), { jail: jail.name, ip }, `已解封 ${ip}`);
}

onMounted(() => {
  void load();
});
</script>

<template>
  <div class="f2b">
    <div class="f2b__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">HOST SECURITY / FAIL2BAN</p>
        <span class="page-title">
          <span
            class="led"
            :class="running ? 'led--ok' : available ? 'led--warn' : 'led--danger'"
            aria-hidden="true"
          />
          <svg class="f2b__glyph" aria-hidden="true">
            <use :href="`#glyph-${glyph}`" />
          </svg>
          {{ view?.name || name }}
        </span>
        <span class="page-meta f2b__meta">
          {{ hostName }} · {{ statusText }}
          <template v-if="checkedAt"> · 更新于 {{ checkedAt }}</template>
        </span>
      </div>

      <span class="f2b__grow" />

      <div class="f2b__tools">
        <n-button v-if="configs.length" size="small" quaternary @click="showConfig = true">
          配置文件
        </n-button>
        <n-button
          v-if="findAction('test')"
          size="small"
          quaternary
          :loading="busy === 'test'"
          @click="validateConfig"
        >
          校验配置
        </n-button>
        <n-button
          v-if="findAction('reload') && running"
          size="small"
          :loading="busy === 'reload'"
          @click="reloadConfig"
        >
          重载配置
        </n-button>
        <n-button size="small" type="primary" class="refresh-btn" @click="load()">
          <template #icon v-if="loading">
            <n-spin :size="16" />
          </template>
          <template v-if="!loading">刷新</template>
        </n-button>
      </div>
    </div>

    <div v-if="error" class="f2b__banner is-error">
      <span class="led led--danger" aria-hidden="true" />
      <span>{{ error }}</span>
      <span class="f2b__grow" />
      <n-button size="tiny" quaternary @click="load()">重试</n-button>
    </div>

    <div v-if="notice" class="f2b__banner is-error">
      <span class="led led--danger" aria-hidden="true" />
      <div class="f2b__notice">
        <b>{{ notice.title }}</b>
        <pre>{{ notice.text }}</pre>
      </div>
      <span class="f2b__grow" />
      <n-button size="tiny" quaternary @click="notice = null">关闭</n-button>
    </div>

    <div v-if="!overview" class="f2b__placeholder">
      <n-spin v-if="loading" :size="20" />
      <span>{{ loading ? '正在读取 Fail2Ban 状态…' : error ? '读取失败，请重试' : '暂无状态' }}</span>
    </div>

    <!-- 服务未运行 / 未安装：不摆一堆空卡片，直接给出下一步动作。 -->
    <section v-else-if="!running" class="f2b__stopped ui-card">
      <span class="f2b__stopped-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6">
          <path d="M12 3l7 3v5c0 4.4-2.9 8.4-7 9.6C7.9 19.4 5 15.4 5 11V6l7-3z" />
          <path d="M9 10l6 6M15 10l-6 6" />
        </svg>
      </span>
      <h3>{{ available ? 'Fail2Ban 服务未运行' : 'Fail2Ban 不可用' }}</h3>
      <p v-if="overview.error" class="f2b__stopped-error">{{ overview.error }}</p>
      <p v-else class="f2b__stopped-error">
        {{ available ? 'fail2ban-client 连接不到服务。' : '目标机上找不到 fail2ban-client。' }}
      </p>
      <div class="f2b__stopped-actions">
        <n-button
          v-if="available && findAction('start')"
          type="primary"
          :loading="busy === 'start'"
          @click="startService"
        >
          启动服务
        </n-button>
        <n-button :loading="loading" @click="load()">重新检测</n-button>
      </div>
    </section>

    <template v-else>
      <div class="ui-stats">
        <article v-for="card in statCards" :key="card.key" class="ui-card ui-stat">
          <span class="ui-stat__label">{{ card.label }}</span>
          <b class="ui-stat__value">{{ card.value }}</b>
          <span class="ui-stat__foot">{{ card.foot }}</span>
        </article>
      </div>

      <div class="f2b__jails">
        <article v-for="jail in jails" :key="jail.name" class="f2b-jail ui-card">
          <header class="ui-card__head">
            <div class="ui-card__ident">
              <p class="ui-card__eyebrow">JAIL</p>
              <h3 class="ui-card__title num">{{ jail.name }}</h3>
            </div>
            <span class="f2b-jail__badge num">
              封禁 <b>{{ jail.currentlyBanned }}</b>
            </span>
          </header>

          <div class="f2b-jail__facts">
            <div>
              <span>当前失败</span>
              <b class="num">{{ jail.currentlyFailed }}</b>
            </div>
            <div>
              <span>累计失败</span>
              <b class="num">{{ jail.totalFailed }}</b>
            </div>
            <div>
              <span>当前封禁</span>
              <b class="num">{{ jail.currentlyBanned }}</b>
            </div>
            <div>
              <span>累计封禁</span>
              <b class="num">{{ jail.totalBanned }}</b>
            </div>
          </div>

          <div class="f2b-jail__bans">
            <div class="f2b-jail__bans-head">
              <span class="silk">封禁地址</span>
              <span class="num">{{ jail.bannedIps.length }}</span>
            </div>

            <p v-if="jail.error" class="f2b-jail__err" :title="jail.error">{{ jail.error }}</p>
            <ul v-else-if="jail.bannedIps.length" class="f2b-jail__ips">
              <li v-for="ip in jail.bannedIps" :key="ip">
                <code class="num">{{ ip }}</code>
                <n-button
                  size="tiny"
                  quaternary
                  :loading="busy === unbanKey(jail, ip)"
                  :disabled="busy !== '' && busy !== unbanKey(jail, ip)"
                  @click="unban(jail, ip)"
                >
                  解封
                </n-button>
              </li>
            </ul>
            <p v-else class="f2b-jail__none">
              <span class="led led--ok" aria-hidden="true" />
              当前没有封禁地址
            </p>
          </div>
        </article>

        <p v-if="jails.length === 0" class="f2b__no-jail">
          服务在运行，但还没有启用任何 Jail：先在配置文件里声明要监控的日志与规则。
        </p>
      </div>
    </template>

    <SecurityConfigDialog
      v-model:show="showConfig"
      :host-id="hostId"
      :slug="slug"
      :entries="configs"
    />
  </div>
</template>

<style scoped>
.f2b {
  display: flex;
  flex-direction: column;
  gap: 14px;
  height: 100%;
  padding: 16px 18px 18px;
  overflow: hidden;
  container-type: inline-size;
}

.f2b__bar {
  display: flex;
  flex: none;
  gap: var(--sp-3);
}

.f2b__meta {
  display: block;
  max-width: 100%;
}

.f2b__glyph {
  align-self: center;
  width: 17px;
  height: 17px;
  color: var(--warn);
}

.f2b__grow {
  flex: 1;
}

.f2b__tools {
  display: flex;
  flex: none;
  align-items: center;
  gap: 8px;
}

.f2b__banner {
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

.f2b__banner.is-error {
  border-color: var(--danger-line);
  background: var(--danger-wash);
  color: var(--danger-ink);
}

.f2b__notice {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

.f2b__notice pre {
  max-height: 140px;
  margin: 0;
  overflow: auto;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

.f2b__placeholder {
  display: flex;
  flex: 1;
  align-items: center;
  justify-content: center;
  gap: 10px;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
}

.f2b__stopped {
  flex: 1;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 32px;
  text-align: center;
}

.f2b__stopped-icon {
  display: grid;
  place-items: center;
  width: 52px;
  height: 52px;
  border: 1px solid var(--warn-line);
  border-radius: 50%;
  background: var(--warn-wash);
  color: var(--warn);
}

.f2b__stopped-icon svg {
  width: 26px;
  height: 26px;
}

.f2b__stopped h3 {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--ink-hi);
}

.f2b__stopped-error {
  max-width: 560px;
  margin: 0;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  line-height: 1.7;
  color: var(--ink-lo);
  word-break: break-word;
}

.f2b__stopped-actions {
  display: flex;
  gap: 8px;
  margin-top: 4px;
}

.f2b__jails {
  display: grid;
  flex: 1;
  grid-template-columns: repeat(auto-fill, minmax(330px, 1fr));
  gap: 14px;
  min-height: 0;
  padding-bottom: 2px;
  overflow-y: auto;
  align-content: start;
}

.f2b__no-jail {
  grid-column: 1 / -1;
  margin: 0;
  padding: 24px;
  border: 1px dashed var(--line);
  border-radius: var(--r-3);
  font-size: var(--fs-sm);
  color: var(--ink-lo);
  text-align: center;
}

.f2b-jail__badge {
  flex: none;
  padding: 2px 7px;
  border: 1px solid var(--warn-line);
  border-radius: var(--r-1);
  background: var(--warn-wash);
  font-size: var(--fs-label);
  color: var(--warn-ink);
}

.f2b-jail__badge b {
  font-weight: 600;
  color: var(--ink-hi);
}

.f2b-jail__facts {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px;
  margin-bottom: 12px;
}

.f2b-jail__facts div {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
  padding: 7px 9px;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
}

.f2b-jail__facts span {
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.f2b-jail__facts b {
  font-size: 16px;
  font-weight: 600;
  color: var(--ink-hi);
}

.f2b-jail__bans {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: 6px;
  min-height: 0;
}

.f2b-jail__bans-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.f2b-jail__ips {
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 188px;
  margin: 0;
  padding: 0;
  overflow-y: auto;
  list-style: none;
}

.f2b-jail__ips li {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 3px 6px 3px 9px;
  border-radius: var(--r-1);
  background: var(--bg-sunken);
}

.f2b-jail__ips li:hover {
  background: var(--bg-raise);
}

.f2b-jail__ips code {
  overflow: hidden;
  font-size: var(--fs-xs);
  color: var(--ink-hi);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.f2b-jail__none {
  display: flex;
  align-items: center;
  gap: 6px;
  margin: 0;
  padding: 6px 0;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

.f2b-jail__err {
  margin: 0;
  overflow: hidden;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  line-height: 1.6;
  color: var(--danger-ink);
  text-overflow: ellipsis;
  white-space: nowrap;
}

@container (max-width: 640px) {
  .f2b__bar {
    flex-direction: column;
    align-items: stretch;
  }

  .f2b__tools {
    flex-wrap: wrap;
  }
}
</style>
