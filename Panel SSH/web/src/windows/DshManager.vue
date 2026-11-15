﻿<script setup lang="ts">
/**
 * DSH 管理器。
 *
 * 它替代了原来"窗口里直接嵌一层 dsh iframe"的形态：用户先看到的是安装形态、
 * 运行控制、工作区、插件与日志，点「进入 Harness」才切到 dsh 本体。
 *
 * 数据来源分两类，轮询频率因此不同：
 *   - 状态（state/message/pid）走轻量的 /dsh/status，每 2 秒一次；
 *   - 版本、Node、数据目录要执行命令，只在打开页面或动作完成后取一次。
 */
import { computed, h, onBeforeUnmount, onMounted, ref, watch } from 'vue';
import { NModal, useDialog, useMessage } from 'naive-ui';

import {
  ApiError,
  api,
  copyText,
  type DshOverview,
  type DshPlugin,
  type DshWorkspace,
  type PluginInfo,
} from '../api/client';
import DshLogo from '../components/icons/DshLogo.vue';

const props = defineProps<{ plugin: PluginInfo }>();

const hostId = props.plugin.hostId;
const hostName = props.plugin.hostName || '宿主机';

const message = useMessage();
const dialog = useDialog();

type Section = 'overview' | 'workspaces' | 'plugins' | 'logs';
const active = ref<Section>('overview');
const harness = ref(false);

const SECTIONS: { key: Section; label: string; glyph: string }[] = [
  { key: 'overview', label: '概览', glyph: 'circle' },
  { key: 'workspaces', label: '工作区', glyph: 'folder' },
  { key: 'plugins', label: '插件管理', glyph: 'puzzle' },
  { key: 'logs', label: '运行日志', glyph: 'lines' },
];

const overview = ref<DshOverview | null>(null);
const loadingOverview = ref(false);
const actionBusy = ref('');

const workspaces = ref<DshWorkspace[]>([]);
const workspacesLoading = ref(false);
const workspacesLoaded = ref(false);

const plugins = ref<DshPlugin[]>([]);
const pluginsLoading = ref(false);
const pluginsLoaded = ref(false);
const pluginQuery = ref('');
const pluginBusy = ref('');
const installPluginShow = ref(false);
const installPluginSpec = ref('');

const logs = ref<string[]>([]);
const logsLoading = ref(false);
const autoScroll = ref(true);
const logBox = ref<HTMLElement | null>(null);

const now = ref(Date.now());
let statusTimer: number | undefined;
let logTimer: number | undefined;
let tickTimer: number | undefined;

const running = computed(() => overview.value?.state === 'running');
const installed = computed(() => overview.value?.installed ?? false);

const stateText = computed(() => {
  const map: Record<string, string> = {
    running: '运行中',
    starting: '启动中',
    stopped: '已停止',
    failed: '启动失败',
    disabled: '未配置',
  };
  return map[overview.value?.state ?? 'disabled'] ?? overview.value?.state ?? '—';
});

const uptimeText = computed(() => {
  const started = overview.value?.startedAt;
  if (!running.value || !started) return '—';
  const startedMs = new Date(started).getTime();
  // 后端零值时间退化为 "0001-01-01"，不能当成真实的启动时刻。
  if (Number.isNaN(startedMs) || startedMs <= 0) return '—';
  const ms = Math.max(0, now.value - startedMs);
  const total = Math.floor(ms / 1000);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;
  if (hours > 0) return `${hours} 小时 ${minutes} 分`;
  if (minutes > 0) return `${minutes} 分 ${seconds} 秒`;
  return `${seconds} 秒`;
});

const pidText = computed(() => {
  const pid = overview.value?.pid ?? 0;
  return pid > 0 ? String(pid) : '—';
});

const filteredPlugins = computed(() => {
  const query = pluginQuery.value.trim().toLowerCase();
  if (query === '') return plugins.value;
  return plugins.value.filter(
    (item) =>
      item.name.toLowerCase().includes(query) ||
      item.description.toLowerCase().includes(query),
  );
});

interface LogLine {
  time: string;
  level: string;
  text: string;
}

const parsedLogs = computed<LogLine[]>(() =>
  logs.value.map((line) => {
    const match = /^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) \[([A-Z]+)\] (.*)$/.exec(line);
    if (match === null) return { time: '', level: 'INFO', text: line };
    return { time: match[1], level: match[2], text: match[3] };
  }),
);

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error) return err.message;
  return fallback;
}

function formatTime(value: string): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString('zh-CN', { hour12: false });
}

function relativeTime(value: string): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const diff = Date.now() - date.getTime();
  if (diff < 60_000) return '刚刚';
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`;
  return `${Math.floor(diff / 86_400_000)} 天前`;
}

async function refreshOverview(): Promise<void> {
  loadingOverview.value = true;
  try {
    overview.value = await api.dsh.overview(hostId);
  } catch (err) {
    message.error(describe(err, '读取 dsh 状态失败'));
  } finally {
    loadingOverview.value = false;
  }
}

/** 轮询只取轻量状态，避免每两秒在目标机上跑一次 npm/node 探测。 */
async function refreshStatus(): Promise<void> {
  if (loadingOverview.value) return;
  try {
    const status = await api.dsh.status(hostId);
    if (overview.value === null) return;
    overview.value = {
      ...overview.value,
      state: status.state,
      message: status.message,
      url: status.url,
      command: status.command,
      logs: status.logs,
      available: status.available,
      pid: status.pid ?? overview.value.pid,
      startedAt: status.startedAt ?? overview.value.startedAt,
    };
  } catch {
    // 轮询失败不打扰用户：网络抖动时下一次自然会恢复。
  }
}

async function loadWorkspaces(): Promise<void> {
  workspacesLoading.value = true;
  try {
    const res = await api.dsh.workspaces(hostId);
    workspaces.value = res.workspaces;
    workspacesLoaded.value = true;
  } catch (err) {
    message.error(describe(err, '读取工作区失败'));
  } finally {
    workspacesLoading.value = false;
  }
}

async function loadPlugins(): Promise<void> {
  pluginsLoading.value = true;
  try {
    const res = await api.dsh.plugins(hostId);
    plugins.value = res.plugins;
    pluginsLoaded.value = true;
  } catch (err) {
    message.error(describe(err, '读取插件清单失败'));
  } finally {
    pluginsLoading.value = false;
  }
}

async function loadLogs(scroll: boolean): Promise<void> {
  if (logsLoading.value) return;
  logsLoading.value = true;
  try {
    const res = await api.dsh.logs(hostId, 1000);
    logs.value = res.lines;
    if (scroll && autoScroll.value) {
      requestAnimationFrame(() => {
        if (logBox.value !== null) logBox.value.scrollTop = logBox.value.scrollHeight;
      });
    }
  } catch (err) {
    message.error(describe(err, '读取运行日志失败'));
  } finally {
    logsLoading.value = false;
  }
}

/** 统一的动作执行：同一时间只允许一个动作，失败时给出后端返回的原因。 */
async function runAction(key: string, task: () => Promise<void>, done: string): Promise<void> {
  if (actionBusy.value !== '') return;
  actionBusy.value = key;
  try {
    await task();
    message.success(done);
    await refreshOverview();
  } catch (err) {
    message.error(describe(err, `${done}失败`));
  } finally {
    actionBusy.value = '';
  }
}

function startDsh(): void {
  void runAction('start', () => api.dsh.start(hostId).then(() => undefined), '已请求启动');
}

function stopDsh(): void {
  void runAction('stop', () => api.dsh.stop(hostId).then(() => undefined), '已停止 dsh');
}

function restartDsh(): void {
  void runAction('restart', () => api.dsh.restart(hostId).then(() => undefined), '已请求重启');
}

/** 检查更新：npm 全局覆盖安装 latest，装完回填命令并重启。 */
function updateDsh(): void {
  confirmDialog(
    '检查更新',
    [
      `将在「${hostName}」上执行 npm install -g @deepseek-ai/dsh@latest。`,
      '上游当前把预发布版标为 latest，因此更新可能拿到 developer preview 版本。',
      '更新完成后 dsh 会自动重启，正在进行的会话会中断。',
    ],
    '更新',
    () => {
      void runAction('update', () => api.dsh.update(hostId).then(() => undefined), '更新完成');
    },
  );
}

/** 强制重建：先卸载全局包再重新安装，用于包损坏或版本回退。 */
function reinstallDsh(): void {
  confirmDialog(
    '强制重建',
    [
      `将在「${hostName}」上卸载并重新安装 @deepseek-ai/dsh。`,
      '这不会删除 dsh 的数据目录（会话、工作区、插件配置都在那里）。',
      'dsh 配置的插件依赖保留在 profile 中，重建后会继续加载。',
    ],
    '重建',
    () => {
      void runAction('reinstall', () => api.dsh.reinstall(hostId).then(() => undefined), '重建完成');
    },
  );
}

/**
 * 一键安装：用户级安装，不需要 root。
 *
 * 为什么不是环境配方：Node 24 与 dsh 都可以装进用户家目录，而宿主机（以及很多
 * 远端主机）拿不到 sudo 口令。走用户前缀既绕开提权，也让面板保持普通用户运行。
 */
function installDsh(): void {
  confirmDialog(
    '用户级安装 DeepSeek Harness',
    [
      `目标主机：${hostName}`,
      '将把 Node.js 24 下载到 ~/.panelssh/node，并把 dsh 装到 ~/.panelssh/dsh-runtime。',
      '全程以当前登录用户运行，不需要 root，也不会改动系统包。',
      '首次安装需要联网下载，可能持续几分钟。',
    ],
    '开始安装',
    () => {
      message.info('正在下载 Node 24 并安装 dsh，可能需要几分钟…');
      void runAction(
        'provision',
        () => api.dsh.provision(hostId).then(() => undefined),
        '安装完成，dsh 正在启动',
      );
    },
  );
}

/** 已经装好但启动命令丢了（例如从旧版本升级上来）：重新探测并回填。 */
function detectCommand(): void {
  void runAction(
    'detect',
    () => api.dsh.detectCommand(hostId).then(() => undefined),
    '已回填启动命令，正在启动',
  );
}

function confirmDialog(
  title: string,
  lines: string[],
  positiveText: string,
  onConfirm: () => void,
): void {
  dialog.warning({
    title,
    content: () => h('div', lines.map((line) => h('p', { style: 'margin:0 0 8px' }, line))),
    positiveText,
    negativeText: '取消',
    onPositiveClick: onConfirm,
  });
}

async function copyDataDir(): Promise<void> {
  const dir = overview.value?.dataDir;
  if (!dir) return;
  if (await copyText(dir)) message.success('数据目录已复制');
  else message.error('复制失败，请手动选择路径');
}

function openHarness(): void {
  harness.value = true;
}

/**
 * 用浏览器的原生地址打开 dsh。
 *
 * 面板内的 Harness 走 /dsh/{id}/ 子路径反代，dsh 的 DeepSeek 账号登录在
 * 这条路上走不通（它的 OAuth 只接受回环 origin），因此这里另开一个窗口，
 * 让浏览器直接访问 dsh 监听的回环端口。
 *
 * 窗口必须在用户手势的同步调用栈里先开出来：等接口返回再 window.open
 * 会被浏览器当成非用户触发的弹窗拦截。
 */
async function openNative(): Promise<void> {
  if (actionBusy.value !== '') return;
  actionBusy.value = 'native';
  const win = window.open('', '_blank');
  try {
    const res = await api.dsh.native(hostId);
    if (win !== null) {
      win.location.href = res.url;
    } else if (window.open(res.url, '_blank', 'noopener') === null) {
      message.warning('浏览器拦截了新窗口，请允许本站弹窗后重试');
    }
  } catch (err) {
    if (win !== null) win.close();
    message.error(describe(err, '获取原生地址失败'));
  } finally {
    actionBusy.value = '';
  }
}

function togglePlugin(item: DshPlugin): void {
  if (pluginBusy.value !== '') return;
  pluginBusy.value = item.name;
  void (async () => {
    try {
      await api.dsh.setPluginEnabled(hostId, item.name, !item.enabled);
      message.success(item.enabled ? `已停用 ${item.name}` : `已启用 ${item.name}`);
      await loadPlugins();
      await refreshStatus();
    } catch (err) {
      message.error(describe(err, '切换插件状态失败'));
    } finally {
      pluginBusy.value = '';
    }
  })();
}

function updatePlugin(item: DshPlugin): void {
  if (pluginBusy.value !== '') return;
  confirmDialog(
    '更新插件',
    [`将把「${item.name}」更新到 latest，并重启 dsh 让新版本生效。`],
    '更新',
    () => {
      pluginBusy.value = item.name;
      void (async () => {
        try {
          await api.dsh.updatePlugin(hostId, item.name);
          message.success(`${item.name} 已更新`);
          await loadPlugins();
        } catch (err) {
          message.error(describe(err, '更新插件失败'));
        } finally {
          pluginBusy.value = '';
        }
      })();
    },
  );
}

function removePlugin(item: DshPlugin): void {
  if (pluginBusy.value !== '') return;
  confirmDialog(
    '卸载插件',
    [
      `将从 web profile 中卸载「${item.name}」并重启 dsh。`,
      '插件自己的数据目录不会被删除。',
    ],
    '卸载',
    () => {
      pluginBusy.value = item.name;
      void (async () => {
        try {
          await api.dsh.removePlugin(hostId, item.name);
          message.success(`${item.name} 已卸载`);
          await loadPlugins();
        } catch (err) {
          message.error(describe(err, '卸载插件失败'));
        } finally {
          pluginBusy.value = '';
        }
      })();
    },
  );
}

function openInstallPlugin(): void {
  installPluginSpec.value = '';
  installPluginShow.value = true;
}

async function submitInstallPlugin(): Promise<void> {
  const spec = installPluginSpec.value.trim();
  if (spec === '') {
    message.warning('请填写 npm 包名或 spec');
    return;
  }
  installPluginShow.value = false;
  pluginBusy.value = spec;
  try {
    await api.dsh.installPlugin(hostId, spec);
    message.success('插件已安装，dsh 正在重启');
    await loadPlugins();
  } catch (err) {
    message.error(describe(err, '安装插件失败'));
  } finally {
    pluginBusy.value = '';
  }
}

function clearLogs(): void {
  confirmDialog('清空运行日志', ['清空后无法恢复，是否继续？'], '清空', () => {
    void (async () => {
      try {
        await api.dsh.clearLogs(hostId);
        logs.value = [];
        message.success('日志已清空');
      } catch (err) {
        message.error(describe(err, '清空日志失败'));
      }
    })();
  });
}

function downloadLogs(): void {
  const blob = new Blob([logs.value.join('\n')], { type: 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = `dsh-${hostName}.log`;
  document.body.appendChild(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

watch(active, (section) => {
  if (section === 'overview') void refreshOverview();
  if (section === 'workspaces' && !workspacesLoaded.value) void loadWorkspaces();
  if (section === 'plugins' && !pluginsLoaded.value) void loadPlugins();
  if (section === 'logs') void loadLogs(true);
});

// 从 Harness 返回时刷新一次：用户可能在那边装了插件、改了工作区。
watch(harness, (open) => {
  if (!open) void refreshOverview();
});

onMounted(() => {
  void refreshOverview();
  statusTimer = window.setInterval(() => {
    if (!harness.value && active.value === 'overview') void refreshStatus();
  }, 2000);
  logTimer = window.setInterval(() => {
    if (!harness.value && active.value === 'logs') void loadLogs(true);
  }, 2000);
  tickTimer = window.setInterval(() => {
    now.value = Date.now();
  }, 1000);
});

onBeforeUnmount(() => {
  if (statusTimer !== undefined) window.clearInterval(statusTimer);
  if (logTimer !== undefined) window.clearInterval(logTimer);
  if (tickTimer !== undefined) window.clearInterval(tickTimer);
});
</script>

<template>
  <div class="dshm">
    <!-- 进入 Harness：同一个窗口内切到 dsh 本体，左上角留一条回管理器的路。 -->
    <div v-if="harness" class="dshm__harness">
      <div class="dshm__harness-bar">
        <button class="ghost" type="button" @click="harness = false">← 返回管理器</button>
        <span class="dshm__harness-title">{{ plugin.displayName }}</span>
        <span class="dshm__harness-url num">{{ plugin.entry }}</span>
      </div>
      <iframe class="dshm__harness-frame" :src="plugin.entry" :title="plugin.displayName" />
    </div>

    <template v-else>
      <aside class="dshm__side">
        <div class="dshm__brand">
          <span class="dshm__brand-mark" aria-hidden="true">
            <DshLogo />
          </span>
          <span class="dshm__brand-text">
            <strong>DeepSeek</strong>
            <small>Harness 管理器 · {{ hostName }}</small>
          </span>
        </div>

        <nav class="dshm__nav">
          <button
            v-for="item in SECTIONS"
            :key="item.key"
            type="button"
            class="dshm__nav-item"
            :class="{ 'is-active': active === item.key }"
            @click="active = item.key"
          >
            <span class="dshm__nav-glyph" :data-glyph="item.glyph" aria-hidden="true" />
            {{ item.label }}
          </button>
        </nav>

        <div class="dshm__side-foot">
          <span class="dshm__dot" :class="`is-${overview?.state ?? 'disabled'}`" />
          {{ stateText }}
        </div>
      </aside>

      <main class="dshm__main">
        <!-- 概览 -->
        <section v-if="active === 'overview'" class="dshm__page">
          <header class="dshm__page-head">
            <h1>概览</h1>
            <span class="dshm__muted num">
              {{ overview?.version ? `v${overview.version}` : loadingOverview ? '读取中…' : '未安装' }}
            </span>
          </header>

          <div class="dshm__hero">
            <div class="dshm__hero-text">
              <h2>DeepSeek Harness</h2>
              <p class="dshm__muted">
                版本 {{ overview?.version || '—' }} · Node {{ overview?.nodeVersion || '—' }}
              </p>
            </div>
            <button class="primary" type="button" :disabled="!running" @click="openHarness">
              进入 Harness
            </button>
            <button
              class="ghost"
              type="button"
              :disabled="!running || actionBusy !== ''"
              title="绕开面板反代，用本机浏览器直接打开 dsh；用于完成 DeepSeek 账号登录"
              @click="openNative"
            >
              {{ actionBusy === 'native' ? '打开中…' : '原生地址打开' }}
            </button>
          </div>

          <p v-if="running" class="dshm__muted dshm__native-hint">
            原生地址供 dsh 完成 DeepSeek 账号登录，仅在本机浏览器（与面板同机）可用。
          </p>

          <div v-if="overview !== null && !installed" class="dshm__notice">
            <div>
              <strong>目标主机上还没有可用的 dsh</strong>
              <p class="dshm__muted">
                面板会把 Node.js 24 与 dsh 装进当前用户目录，不需要 root。
              </p>
            </div>
            <button class="primary" type="button" :disabled="actionBusy !== ''" @click="installDsh">
              {{ actionBusy === 'provision' ? '安装中…' : '一键安装并启动' }}
            </button>
          </div>

          <div v-else-if="overview !== null && !overview.available" class="dshm__notice">
            <div>
              <strong>dsh 已安装，但还没有启动命令</strong>
              <p class="dshm__muted">探测目标机上 dsh 的绝对路径，回填后立即启动。</p>
            </div>
            <button class="ghost" type="button" :disabled="actionBusy !== ''" @click="detectCommand">
              {{ actionBusy === 'detect' ? '回填中…' : '回填启动命令' }}
            </button>
          </div>

          <div class="dshm__stats">
            <div class="dshm__stat">
              <span class="silk">运行状态</span>
              <strong :class="`state-${overview?.state ?? 'disabled'}`">{{ stateText }}</strong>
            </div>
            <div class="dshm__stat">
              <span class="silk">运行时间</span>
              <strong class="num">{{ uptimeText }}</strong>
            </div>
            <div class="dshm__stat">
              <span class="silk">进程 PID</span>
              <strong class="num">{{ pidText }}</strong>
            </div>
          </div>

          <h3 class="dshm__section-title">运行控制</h3>
          <div class="dshm__actions">
            <button
              class="dshm__action"
              type="button"
              :disabled="actionBusy !== '' || !overview?.available || running"
              @click="startDsh"
            >
              <span class="dshm__action-icon" data-kind="start" aria-hidden="true" />
              <span>{{ actionBusy === 'start' ? '启动中…' : '启动服务' }}</span>
            </button>
            <button
              class="dshm__action"
              type="button"
              :disabled="actionBusy !== '' || !running"
              @click="stopDsh"
            >
              <span class="dshm__action-icon" data-kind="stop" aria-hidden="true" />
              <span>{{ actionBusy === 'stop' ? '停止中…' : '停止服务' }}</span>
            </button>
            <button
              class="dshm__action"
              type="button"
              :disabled="actionBusy !== '' || !overview?.available"
              @click="restartDsh"
            >
              <span class="dshm__action-icon" data-kind="restart" aria-hidden="true" />
              <span>{{ actionBusy === 'restart' ? '重启中…' : '重启服务' }}</span>
            </button>
            <button
              class="dshm__action"
              type="button"
              :disabled="actionBusy !== '' || !installed"
              @click="updateDsh"
            >
              <span class="dshm__action-icon" data-kind="update" aria-hidden="true" />
              <span>{{ actionBusy === 'update' ? '更新中…' : '检查更新' }}</span>
            </button>
            <button
              class="dshm__action"
              type="button"
              :disabled="actionBusy !== ''"
              @click="reinstallDsh"
            >
              <span class="dshm__action-icon" data-kind="rebuild" aria-hidden="true" />
              <span>{{ actionBusy === 'reinstall' ? '重建中…' : '强制重建' }}</span>
            </button>
          </div>

          <p v-if="overview?.message" class="dshm__message">{{ overview.message }}</p>
        </section>

        <!-- 工作区 -->
        <section v-else-if="active === 'workspaces'" class="dshm__page">
          <header class="dshm__page-head">
            <h1>工作区 <span class="dshm__count">共 {{ workspaces.length }} 个</span></h1>
            <div class="dshm__page-actions">
              <button class="ghost" type="button" :disabled="workspacesLoading" @click="loadWorkspaces">
                刷新
              </button>
              <button class="ghost" type="button" @click="copyDataDir">数据目录</button>
            </div>
          </header>

          <p v-if="overview?.dataDir" class="dshm__path num">{{ overview.dataDir }}</p>

          <div v-if="workspaces.length > 0" class="dshm__cards">
            <article v-for="item in workspaces" :key="item.id" class="dshm__card">
              <div class="dshm__card-head">
                <span class="dshm__folder" aria-hidden="true" />
                <div class="dshm__card-title">
                  <strong>{{ item.title || item.path }}</strong>
                  <small class="num">{{ item.path }}</small>
                </div>
                <span class="dshm__badge">{{ item.sessionCount }} 会话</span>
              </div>
              <div class="dshm__card-foot">
                <span>更新于 {{ relativeTime(item.updatedAt) }}</span>
                <span>创建于 {{ formatTime(item.createdAt) }}</span>
              </div>
            </article>
          </div>
          <p v-else-if="!workspacesLoading" class="dshm__empty">
            还没有工作区。在 Harness 里添加一个目录后，这里会显示它的会话与更新时间。
          </p>
        </section>

        <!-- 插件管理 -->
        <section v-else-if="active === 'plugins'" class="dshm__page">
          <header class="dshm__page-head">
            <h1>插件管理 <span class="dshm__count">共 {{ plugins.length }} 个插件</span></h1>
            <div class="dshm__page-actions">
              <button class="ghost" type="button" :disabled="pluginsLoading" @click="loadPlugins">
                刷新
              </button>
              <button class="primary" type="button" @click="openInstallPlugin">安装插件</button>
            </div>
          </header>

          <div class="dshm__search">
            <input v-model="pluginQuery" type="search" placeholder="搜索插件名 / 描述…" spellcheck="false" />
          </div>

          <div v-if="filteredPlugins.length > 0" class="dshm__plugins">
            <article v-for="item in filteredPlugins" :key="item.name" class="dshm__plugin">
              <span class="dshm__plugin-glyph" aria-hidden="true" />
              <div class="dshm__plugin-body">
                <div class="dshm__plugin-head">
                  <strong>{{ item.name }}</strong>
                  <span class="dshm__badge">{{ item.spec }}</span>
                  <span class="dshm__tag" :class="{ 'is-on': item.enabled }">
                    {{ item.enabled ? '运行中' : '已停用' }}
                  </span>
                </div>
                <p class="dshm__plugin-desc">{{ item.description || '没有提供描述' }}</p>
                <div class="dshm__plugin-foot">
                  <span class="num">来源：{{ item.spec }}</span>
                  <a v-if="item.homepage" :href="item.homepage" target="_blank" rel="noreferrer">
                    主页
                  </a>
                </div>
              </div>
              <div class="dshm__plugin-actions">
                <button
                  class="danger"
                  type="button"
                  :disabled="pluginBusy !== ''"
                  @click="removePlugin(item)"
                >
                  卸载
                </button>
                <button
                  class="ghost"
                  type="button"
                  :disabled="pluginBusy !== ''"
                  @click="updatePlugin(item)"
                >
                  更新
                </button>
                <button
                  class="dshm__switch"
                  type="button"
                  :class="{ 'is-on': item.enabled }"
                  :disabled="pluginBusy !== ''"
                  :aria-pressed="item.enabled"
                  :aria-label="item.enabled ? '停用插件' : '启用插件'"
                  @click="togglePlugin(item)"
                >
                  <span />
                </button>
              </div>
            </article>
          </div>
          <p v-else-if="!pluginsLoading" class="dshm__empty">
            这个 profile 还没有安装外部插件。点「安装插件」填入 npm 包名即可。
          </p>
        </section>

        <!-- 运行日志 -->
        <section v-else class="dshm__page dshm__page--logs">
          <header class="dshm__page-head">
            <h1>运行日志</h1>
            <div class="dshm__page-actions">
              <button
                class="ghost dshm__toggle"
                type="button"
                :class="{ 'is-on': autoScroll }"
                @click="autoScroll = !autoScroll"
              >
                自动滚动
              </button>
              <button class="ghost" type="button" @click="downloadLogs">下载</button>
              <button class="danger" type="button" @click="clearLogs">清空</button>
            </div>
          </header>

          <div ref="logBox" class="dshm__logs">
            <p v-if="parsedLogs.length === 0" class="dshm__empty">
              {{ logsLoading ? '读取中…' : '还没有日志。启动一次 dsh 后这里会出现运行输出。' }}
            </p>
            <div v-for="(line, index) in parsedLogs" :key="index" class="dshm__log">
              <span class="dshm__log-time num">{{ line.time }}</span>
              <span class="dshm__log-level" :class="`is-${line.level.toLowerCase()}`">
                [{{ line.level }}]
              </span>
              <span class="dshm__log-text">{{ line.text }}</span>
            </div>
          </div>
        </section>
      </main>
    </template>

    <n-modal
      v-model:show="installPluginShow"
      preset="card"
      title="安装插件"
      style="width: 520px"
      @after-leave="installPluginSpec = ''"
    >
      <p class="dshm__modal-hint">
        填入 npm 包名或 spec（例如 <code>dshmarket</code> 或 <code>dsh-cost-meter@latest</code>）。
        安装后 dsh 会重启，未声明组合包的普通库不会出现在列表里。
      </p>
      <input
        v-model="installPluginSpec"
        class="dshm__input"
        type="text"
        spellcheck="false"
        placeholder="npm 包名或 spec"
        @keyup.enter="submitInstallPlugin"
      />
      <template #footer>
        <div class="dshm__modal-foot">
          <button class="ghost" type="button" @click="installPluginShow = false">取消</button>
          <button class="primary" type="button" @click="submitInstallPlugin">安装</button>
        </div>
      </template>
    </n-modal>
  </div>
</template>

<style scoped>
.dshm {
  display: flex;
  height: 100%;
  min-height: 0;
  background: var(--bg-panel);
  color: var(--ink);
  font-family: var(--font-sans);
  font-size: var(--fs-md);
}

/* ---------- 侧栏 ---------- */
.dshm__side {
  display: flex;
  flex: 0 0 188px;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-4) var(--sp-2);
  border-right: 1px solid var(--line-hair);
  background: var(--bg-module);
}

.dshm__brand {
  display: flex;
  gap: var(--sp-2);
  align-items: center;
  padding: var(--sp-2) var(--sp-2) var(--sp-4);
}

.dshm__brand-mark {
  display: grid;
  width: 34px;
  height: 34px;
  place-items: center;
  border-radius: 50%;
  background: var(--bg-sunken);
  color: var(--sig);
}

.dshm__brand-mark svg {
  width: 22px;
  height: 22px;
}

.dshm__brand-text {
  display: flex;
  min-width: 0;
  flex-direction: column;
  line-height: 1.3;
}

.dshm__brand-text strong {
  color: var(--ink-hi);
  font-size: var(--fs-lg);
}

.dshm__brand-text small {
  overflow: hidden;
  color: var(--ink-lo);
  font-size: var(--fs-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dshm__nav {
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.dshm__nav-item {
  display: flex;
  gap: var(--sp-2);
  align-items: center;
  padding: 9px var(--sp-3);
  border: 0;
  border-radius: var(--r-3);
  background: transparent;
  color: var(--ink);
  cursor: pointer;
  font: inherit;
  text-align: left;
}

.dshm__nav-item:hover {
  background: var(--bg-raise);
  color: var(--ink-hi);
}

.dshm__nav-item.is-active {
  background: var(--sig-wash);
  color: var(--sig-hi);
}

/* 导航图标用纯 CSS 几何，不引入图标字体或 emoji。 */
.dshm__nav-glyph {
  width: 14px;
  height: 14px;
  border: 1.5px solid currentColor;
  border-radius: 3px;
  opacity: 0.9;
}

.dshm__nav-glyph[data-glyph='circle'] {
  border-radius: 50%;
}

.dshm__nav-glyph[data-glyph='folder'] {
  height: 11px;
  border-radius: 2px;
}

.dshm__nav-glyph[data-glyph='lines'] {
  border: 0;
  background: repeating-linear-gradient(
    to bottom,
    currentColor 0 1.5px,
    transparent 1.5px 4px
  );
}

.dshm__nav-glyph[data-glyph='puzzle'] {
  border-radius: 3px 6px 3px 3px;
}

.dshm__side-foot {
  display: flex;
  gap: var(--sp-2);
  align-items: center;
  margin-top: auto;
  padding: var(--sp-3);
  border-top: 1px solid var(--line-hair);
  color: var(--ink-lo);
  font-size: var(--fs-xs);
}

.dshm__dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--ink-ghost);
}

.dshm__dot.is-running {
  background: var(--ok);
  box-shadow: 0 0 0 3px rgba(87, 217, 140, 0.16);
}

.dshm__dot.is-starting {
  background: var(--info);
}

.dshm__dot.is-failed {
  background: var(--danger);
}

/* ---------- 主区 ---------- */
.dshm__main {
  min-width: 0;
  flex: 1;
  overflow: auto;
}

.dshm__page {
  padding: var(--sp-5) var(--sp-6) var(--sp-6);
}

.dshm__page--logs {
  display: flex;
  height: 100%;
  flex-direction: column;
  box-sizing: border-box;
}

.dshm__page-head {
  display: flex;
  gap: var(--sp-3);
  align-items: center;
  margin-bottom: var(--sp-3);
}

.dshm__page-head h1 {
  margin: 0;
  color: var(--ink-hi);
  font-size: var(--fs-xl);
  font-weight: 650;
}

.dshm__count {
  margin-left: var(--sp-2);
  color: var(--ink-lo);
  font-size: var(--fs-sm);
  font-weight: 400;
}

.dshm__page-actions {
  display: flex;
  gap: var(--sp-2);
  margin-left: auto;
}

.dshm__hero {
  display: flex;
  gap: var(--sp-4);
  align-items: center;
  padding: var(--sp-4);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-4);
  background: var(--bg-module);
}

.dshm__hero-text {
  min-width: 0;
  flex: 1;
}

.dshm__hero-text h2 {
  margin: 0 0 4px;
  color: var(--ink-hi);
  font-size: 18px;
  font-weight: 650;
}

.dshm__hero-text p {
  margin: 0;
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
}

.dshm__native-hint {
  margin: var(--sp-2) 0 0;
  font-size: var(--fs-sm);
}

.dshm__muted {
  color: var(--ink-lo);
}

.dshm__notice {
  display: flex;
  gap: var(--sp-4);
  align-items: center;
  margin-top: var(--sp-3);
  padding: var(--sp-4);
  border: 1px solid var(--sig-line);
  border-radius: var(--r-4);
  background: var(--sig-wash);
}

.dshm__notice div {
  flex: 1;
}

.dshm__notice strong {
  color: var(--sig-hi);
}

.dshm__notice p {
  margin: 4px 0 0;
  font-size: var(--fs-sm);
}

.dshm__stats {
  display: grid;
  gap: var(--sp-3);
  margin-top: var(--sp-3);
  grid-template-columns: repeat(3, minmax(0, 1fr));
}

.dshm__stat {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-4);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-4);
  background: var(--bg-module);
}

.dshm__stat strong {
  color: var(--ink-hi);
  font-size: var(--fs-lg);
  font-weight: 600;
}

.dshm__stat .state-running {
  color: var(--ok);
}

.dshm__stat .state-failed,
.dshm__stat .state-disabled {
  color: var(--danger);
}

.dshm__stat .state-starting {
  color: var(--info);
}

.dshm__section-title {
  margin: var(--sp-5) 0 var(--sp-3);
  color: var(--ink-hi);
  font-size: var(--fs-lg);
  font-weight: 600;
}

.dshm__actions {
  display: grid;
  gap: var(--sp-3);
  grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
}

.dshm__action {
  display: flex;
  min-height: 104px;
  flex-direction: column;
  gap: var(--sp-3);
  align-items: center;
  justify-content: center;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-4);
  background: var(--bg-module);
  color: var(--ink);
  cursor: pointer;
  font: inherit;
  font-size: var(--fs-sm);
}

.dshm__action:hover:not(:disabled) {
  border-color: var(--line-strong);
  background: var(--bg-raise);
  color: var(--ink-hi);
}

.dshm__action:disabled {
  cursor: default;
  opacity: 0.45;
}

/* 动作图标：同样用纯 CSS，避免引入图标资源。 */
.dshm__action-icon {
  position: relative;
  display: block;
  width: 26px;
  height: 26px;
  border: 1.5px solid currentColor;
  border-radius: 50%;
  color: var(--sig);
}

.dshm__action-icon[data-kind='start']::after {
  position: absolute;
  top: 7px;
  left: 9px;
  width: 0;
  height: 0;
  border-top: 5px solid transparent;
  border-bottom: 5px solid transparent;
  border-left: 8px solid currentColor;
  content: '';
}

.dshm__action-icon[data-kind='stop']::after {
  position: absolute;
  inset: 8px;
  background: currentColor;
  content: '';
}

.dshm__action-icon[data-kind='restart']::after {
  position: absolute;
  inset: 7px;
  border: 2px solid currentColor;
  border-right-color: transparent;
  border-radius: 50%;
  content: '';
}

.dshm__action-icon[data-kind='update']::after {
  position: absolute;
  top: 5px;
  left: 11px;
  width: 2px;
  height: 10px;
  background: currentColor;
  box-shadow: -3px 6px 0 -1px currentColor, 3px 6px 0 -1px currentColor;
  content: '';
}

.dshm__action-icon[data-kind='rebuild']::after {
  position: absolute;
  inset: 7px;
  border: 2px solid currentColor;
  border-radius: 2px;
  content: '';
}

.dshm__message {
  margin-top: var(--sp-3);
  color: var(--warn);
  font-size: var(--fs-sm);
}

/* ---------- 工作区 ---------- */
.dshm__path {
  margin: 0 0 var(--sp-3);
  color: var(--ink-lo);
  font-size: var(--fs-sm);
  word-break: break-all;
}

.dshm__cards {
  display: grid;
  gap: var(--sp-3);
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
}

.dshm__card {
  padding: var(--sp-4);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-4);
  background: var(--bg-module);
}

.dshm__card-head {
  display: flex;
  gap: var(--sp-3);
  align-items: flex-start;
}

.dshm__folder {
  width: 30px;
  height: 24px;
  flex: 0 0 auto;
  border: 1.5px solid var(--ink-lo);
  border-radius: 3px;
}

.dshm__card-title {
  display: flex;
  min-width: 0;
  flex: 1;
  flex-direction: column;
  gap: 4px;
}

.dshm__card-title strong {
  color: var(--ink-hi);
  font-size: var(--fs-md);
}

.dshm__card-title small {
  overflow: hidden;
  color: var(--ink-lo);
  font-size: var(--fs-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dshm__badge {
  padding: 2px 7px;
  border-radius: 999px;
  background: var(--bg-sunken);
  color: var(--ink);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  white-space: nowrap;
}

.dshm__card-foot {
  display: flex;
  gap: var(--sp-4);
  margin-top: var(--sp-3);
  color: var(--ink-lo);
  font-size: var(--fs-xs);
}

/* ---------- 插件 ---------- */
.dshm__search {
  margin-bottom: var(--sp-3);
}

.dshm__search input,
.dshm__input {
  width: 100%;
  padding: 9px var(--sp-3);
  border: 1px solid var(--line);
  border-radius: var(--r-3);
  background: var(--bg-sunken);
  color: var(--ink-hi);
  font: inherit;
  outline: none;
}

.dshm__search input:focus,
.dshm__input:focus {
  border-color: var(--sig-line);
}

.dshm__plugins {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.dshm__plugin {
  display: flex;
  gap: var(--sp-3);
  align-items: flex-start;
  padding: var(--sp-4);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-4);
  background: var(--bg-module);
}

.dshm__plugin-glyph {
  width: 22px;
  height: 22px;
  flex: 0 0 auto;
  margin-top: 2px;
  border: 1.5px solid var(--ok);
  border-radius: 4px 8px 4px 4px;
}

.dshm__plugin-body {
  min-width: 0;
  flex: 1;
}

.dshm__plugin-head {
  display: flex;
  gap: var(--sp-2);
  align-items: center;
  flex-wrap: wrap;
}

.dshm__plugin-head strong {
  color: var(--ink-hi);
  font-family: var(--font-mono);
  font-size: var(--fs-md);
}

.dshm__tag {
  padding: 1px 8px;
  border-radius: 999px;
  background: rgba(255, 107, 107, 0.12);
  color: var(--danger);
  font-size: var(--fs-xs);
}

.dshm__tag.is-on {
  background: rgba(87, 217, 140, 0.12);
  color: var(--ok);
}

.dshm__plugin-desc {
  margin: var(--sp-2) 0;
  color: var(--ink);
  font-size: var(--fs-sm);
  line-height: 1.6;
}

.dshm__plugin-foot {
  display: flex;
  gap: var(--sp-4);
  color: var(--ink-lo);
  font-size: var(--fs-xs);
}

.dshm__plugin-foot a {
  color: var(--info);
  text-decoration: none;
}

.dshm__plugin-actions {
  display: flex;
  gap: var(--sp-2);
  align-items: center;
}

.dshm__switch {
  position: relative;
  width: 34px;
  height: 19px;
  padding: 0;
  border: 0;
  border-radius: 999px;
  background: var(--bg-raise);
  cursor: pointer;
}

.dshm__switch span {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 15px;
  height: 15px;
  border-radius: 50%;
  background: var(--ink-lo);
  transition: transform var(--dur-1) var(--ease);
}

.dshm__switch.is-on {
  background: rgba(87, 217, 140, 0.28);
}

.dshm__switch.is-on span {
  transform: translateX(15px);
  background: var(--ok);
}

.dshm__switch:disabled {
  cursor: default;
  opacity: 0.5;
}

/* ---------- 日志 ---------- */
.dshm__logs {
  min-height: 0;
  flex: 1;
  overflow: auto;
  padding: var(--sp-3);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-4);
  background: var(--bg-sunken);
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  line-height: 1.65;
}

.dshm__log {
  display: flex;
  gap: var(--sp-2);
  white-space: pre-wrap;
  word-break: break-word;
}

.dshm__log-time {
  flex: 0 0 auto;
  color: var(--ink-ghost);
}

.dshm__log-level {
  flex: 0 0 auto;
  color: var(--info);
}

.dshm__log-level.is-warn {
  color: var(--warn);
}

.dshm__log-level.is-error {
  color: var(--danger);
}

.dshm__log-text {
  color: var(--ink);
}

/* ---------- 通用按钮 ---------- */
.dshm button {
  font-family: var(--font-sans);
}

.dshm .primary,
.dshm .ghost,
.dshm .danger {
  padding: 7px 14px;
  border: 1px solid transparent;
  border-radius: var(--r-3);
  cursor: pointer;
  font-size: var(--fs-sm);
}

.dshm .primary {
  background: var(--sig);
  color: #17120a;
  font-weight: 600;
}

.dshm .primary:hover:not(:disabled) {
  background: var(--sig-hi);
}

.dshm .ghost {
  border-color: var(--line);
  background: transparent;
  color: var(--ink);
}

.dshm .ghost:hover:not(:disabled) {
  border-color: var(--line-strong);
  color: var(--ink-hi);
}

.dshm .danger {
  background: rgba(255, 107, 107, 0.12);
  color: var(--danger);
}

.dshm .danger:hover:not(:disabled) {
  background: rgba(255, 107, 107, 0.2);
}

.dshm button:disabled {
  cursor: default;
  opacity: 0.45;
}

.dshm__toggle.is-on {
  border-color: var(--sig-line);
  color: var(--sig-hi);
}

.dshm__empty {
  padding: var(--sp-6);
  color: var(--ink-lo);
  font-size: var(--fs-sm);
  text-align: center;
}

.dshm__modal-hint {
  margin: 0 0 var(--sp-3);
  color: var(--ink);
  font-size: var(--fs-sm);
  line-height: 1.6;
}

.dshm__modal-hint code {
  padding: 1px 5px;
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  font-family: var(--font-mono);
}

.dshm__modal-foot {
  display: flex;
  gap: var(--sp-2);
  justify-content: flex-end;
}

/* 弹窗被 teleport 到 body，不再位于 .dshm 之内，按钮样式需要独立的祖先。 */
.dshm__modal-foot .primary,
.dshm__modal-foot .ghost {
  padding: 7px 14px;
  border: 1px solid transparent;
  border-radius: var(--r-3);
  cursor: pointer;
  font: inherit;
  font-size: var(--fs-sm);
}

.dshm__modal-foot .primary {
  background: var(--sig);
  color: #17120a;
  font-weight: 600;
}

.dshm__modal-foot .ghost {
  border-color: var(--line);
  background: transparent;
  color: var(--ink);
}

/* ---------- Harness ---------- */
.dshm__harness {
  display: flex;
  width: 100%;
  min-height: 0;
  flex-direction: column;
}

.dshm__harness-bar {
  display: flex;
  gap: var(--sp-3);
  align-items: center;
  padding: var(--sp-2) var(--sp-3);
  border-bottom: 1px solid var(--line-hair);
  background: var(--bg-module);
}

.dshm__harness-title {
  color: var(--ink-hi);
  font-size: var(--fs-sm);
}

.dshm__harness-url {
  overflow: hidden;
  margin-left: auto;
  color: var(--ink-lo);
  font-size: var(--fs-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dshm__harness-frame {
  width: 100%;
  min-height: 0;
  flex: 1;
  border: 0;
  background: var(--bg-sunken);
}
</style>
