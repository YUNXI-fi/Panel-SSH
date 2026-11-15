<script setup lang="ts">
/**
 * Docker 管理窗口。
 *
 * 五块内容：容器、Compose 项目、本地镜像、镜像仓库（搜索与拉取）、网络。
 *
 * 所有操作都通过目标机上的 docker CLI 完成，而不是连 Docker 的 HTTP API：
 * 目标机上可能只开了 unix socket，而面板已经有一条能执行命令的通道。
 * 状态由一条 summary 命令一次取齐 —— 轮询时每多一条 SSH 会话，
 * 就多一次目标机上的进程创建。
 *
 * 容器与镜像的删除都走确认弹窗：它们是不可撤销的（容器里的临时数据会一起没）。
 */
import { computed, h, ref, watch } from 'vue';
import {
  NAlert,
  NButton,
  NCheckbox,
  NDataTable,
  NInput,
  NModal,
  NPopconfirm,
  NSelect,
  NSpace,
  NSpin,
  NTabPane,
  NTabs,
  NTag,
  useMessage,
  type DataTableColumns,
} from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import DirPicker from '../components/DirPicker.vue';
import EnvPlanDialog from './EnvPlanDialog.vue';
import EnvRunDialog from './EnvRunDialog.vue';
import { useVisiblePoll } from '../desktop/useVisiblePoll';
import { formatBytes } from '../api/format';
import {
  ApiError,
  api,
  type DockerComposeAction,
  type DockerContainer,
  type DockerContainerAction,
  type DockerContainerFile,
  type DockerContainerFiles,
  type DockerContainerText,
  type DockerImage,
  type DockerNetwork,
  type DockerNetworkDetail,
  type DockerProject,
  type DockerSearchHit,
  type DockerSummary,
  type EnvPlan,
  type EnvRunRequest,
} from '../api/client';

const props = defineProps<{ hostId?: number; hostName?: string }>();

const message = useMessage();

// 容器状态变化不频繁，10 秒一轮足够；也让目标机少跑几遍 docker。
const DOCKER_POLL_MS = 10000;

const root = ref<HTMLElement | null>(null);
const target = ref<number | null>(props.hostId ?? null);
const pickedName = ref(props.hostName ?? '');
const activeTab = ref('overview');

watch(
  () => props.hostId,
  (id) => {
    if (id !== undefined) target.value = id;
  },
);

const summary = ref<DockerSummary | null>(null);
const fetching = ref(false);
const lastError = ref('');

function describe(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (err.status === 504) return `${err.message}。请检查目标机负载与网络`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return fallback;
}

async function refresh(options: { silent?: boolean } = {}): Promise<void> {
  const id = target.value;
  if (id === null) return;

  // 已有数据时的轮询不点亮按钮：每 10 秒闪一次转圈只是噪音。
  const showLoading = !options.silent || summary.value === null;
  if (showLoading) fetching.value = true;
  try {
    summary.value = await api.docker.summary(id);
    lastError.value = '';
  } catch (err) {
    lastError.value = describe(err, '读取 Docker 状态失败');
  } finally {
    if (showLoading) fetching.value = false;
  }
}

function onRefreshClick(): void {
  if (fetching.value) return;
  void refresh();
}

useVisiblePoll({
  root,
  intervalMs: DOCKER_POLL_MS,
  task: () => refresh({ silent: true }),
  enabled: () => target.value !== null,
});

watch(target, (id) => {
  // 换主机先清空：否则新数据到达前，窗口里还挂着上一台的容器，
  // 这时候点删除会打到错误的机器上。
  summary.value = null;
  lastError.value = '';
  searchHits.value = [];
  if (id !== null) void refresh();
});

const probe = computed(() => summary.value?.probe ?? null);
const usable = computed(() => !!summary.value && summary.value.probe.available && !summary.value.probe.error);
const hasCompose = computed(() => (summary.value?.probe.composeVersion ?? '') !== '');

// ---- 一键安装 ----
//
// 不在面板里另写一套安装脚本，而是复用环境管理的 Docker 配方：
// 官方源、compose 插件、安装前预检、变更快照与失败回滚都已经在那边。
// 因此这里只做两件事：拉一次预检、把执行交给同一个 WebSocket 执行器。

const installPlanOpen = ref(false);
const installPlanLoading = ref(false);
const installPlan = ref<EnvPlan | null>(null);
const installRunOpen = ref(false);
const installRequest = ref<EnvRunRequest | null>(null);

async function openInstall(): Promise<void> {
  const id = target.value;
  if (id === null || installPlanLoading.value) return;

  installPlan.value = null;
  installPlanOpen.value = true;
  installPlanLoading.value = true;
  try {
    installPlan.value = await api.env.plan(id, { action: 'install', recipeSlug: 'docker' });
  } catch (err) {
    installPlanOpen.value = false;
    message.error(describe(err, '生成 Docker 安装计划失败'));
  } finally {
    installPlanLoading.value = false;
  }
}

function startInstall(): void {
  if (!installPlan.value) return;
  installPlanOpen.value = false;
  installRequest.value = { action: 'install', recipeSlug: 'docker' };
  installRunOpen.value = true;
}

function onInstallDone(): void {
  installRunOpen.value = false;
  // 安装器完成后的复核已经由环境管理做过；这里重新取一次 Docker 状态，
  // 让窗口从"没有 Docker"直接切到容器/镜像/网络页。
  void refresh();
}

const containers = computed(() => summary.value?.containers ?? []);
const images = computed(() => summary.value?.images ?? []);
const networks = computed(() => summary.value?.networks ?? []);
const projects = computed(() => summary.value?.projects ?? []);

const runningCount = computed(() => containers.value.filter((item) => item.state === 'running').length);

const blockErrors = computed(() => {
  const errors = summary.value?.errors ?? {};
  return [
    errors.containers ? `容器列表：${errors.containers}` : '',
    errors.images ? `镜像列表：${errors.images}` : '',
    errors.networks ? `网络列表：${errors.networks}` : '',
  ].filter((item) => item !== '');
});

const networkOptions = computed(() => [
  { label: '默认（bridge）', value: '' },
  ...networks.value.map((item) => ({ label: `${item.name} · ${item.driver}`, value: item.name })),
]);

// ---- 仪表盘 ----
//
// 只聚合 docker summary 已经拿到的数据，不额外开一条 SSH 会话：
// 概览页回答的是"这台机器现在什么状况"，而不是新的采集维度。

/**
 * docker images 的 Size 列是 "1.23GB" 这类人类可读串，换算成字节才能求和。
 * docker 内部按 1024 进制计算，只是标签写成了 kB/MB/GB。
 */
function parseImageSize(text: string): number {
  const hit = /^([\d.]+)\s*([kmgt]?)i?b$/i.exec(text.trim());
  if (!hit) return 0;
  const value = Number.parseFloat(hit[1]);
  if (!Number.isFinite(value)) return 0;
  const unit = hit[2].toLowerCase();
  const scale: Record<string, number> = { '': 1, k: 1024, m: 1024 ** 2, g: 1024 ** 3, t: 1024 ** 4 };
  return value * (scale[unit] ?? 1);
}

/** exited 容器的退出码；不是 exited 或读不出时返回 null。 */
function exitCode(item: DockerContainer): number | null {
  const hit = /Exited \((\d+)\)/.exec(item.status);
  return hit ? Number(hit[1]) : null;
}

/** 容器状态灯：非 0 退出才算异常，正常退出用警告色提示"它停了"。 */
function containerLed(item: DockerContainer): string {
  if (item.state === 'running') return 'led--ok';
  if (item.state === 'dead' || (item.state === 'exited' && exitCode(item) !== 0)) return 'led--danger';
  if (item.state === 'paused' || item.state === 'restarting' || item.state === 'exited') return 'led--warn';
  return '';
}

const imageBytes = computed(() => images.value.reduce((sum, item) => sum + parseImageSize(item.size), 0));
const largestImageBytes = computed(() =>
  images.value.reduce((max, item) => Math.max(max, parseImageSize(item.size)), 0),
);
const danglingImages = computed(() => images.value.filter((item) => item.dangling));
const danglingBytes = computed(() => danglingImages.value.reduce((sum, item) => sum + parseImageSize(item.size), 0));
/** 悬空镜像占镜像总占用的比例。没有镜像时按 0 算，不显示"NaN%"。 */
const danglingShare = computed(() =>
  imageBytes.value > 0 ? Math.min(100, (danglingBytes.value / imageBytes.value) * 100) : 0,
);

const pausedCount = computed(() => containers.value.filter((item) => item.state === 'paused').length);

/**
 * 未运行的容器，问题优先：死了的、反复重启的、非 0 退出的排在最前。
 * 仪表盘只列前几条，排不好序就等于没给线索。
 */
const stoppedContainers = computed(() => {
  const rank = (item: DockerContainer): number => {
    if (item.state === 'dead') return 0;
    if (item.state === 'restarting') return 1;
    if (item.state === 'exited' && exitCode(item) !== 0) return 2;
    if (item.state === 'paused') return 3;
    return 4;
  };
  return containers.value
    .filter((item) => item.state !== 'running')
    .slice()
    .sort((a, b) => rank(a) - rank(b));
});

const healthyProjects = computed(
  () => projects.value.filter((item) => item.total > 0 && item.running === item.total).length,
);

const bridgeCount = computed(() => networks.value.filter((item) => item.driver === 'bridge').length);

const overviewCards = computed(() => [
  {
    key: 'containers',
    label: '容器',
    value: String(runningCount.value),
    unit: ` / ${containers.value.length}`,
    foot: `${stoppedContainers.value.length} 个未运行${pausedCount.value > 0 ? ` · ${pausedCount.value} 个暂停` : ''}`,
  },
  {
    key: 'images',
    label: '镜像',
    value: String(images.value.length),
    unit: ' 个',
    foot: `占用 ${formatBytes(imageBytes.value)}`,
  },
  {
    key: 'networks',
    label: '网络',
    value: String(networks.value.length),
    unit: ' 个',
    foot: `${bridgeCount.value} 个 bridge 驱动`,
  },
  {
    key: 'compose',
    label: 'COMPOSE',
    value: String(projects.value.length),
    unit: ' 个',
    foot: projects.value.length > 0 ? `${healthyProjects.value} 个全部运行` : '暂无 compose 项目',
  },
]);

function stateType(state: string): 'default' | 'success' | 'warning' | 'error' | 'info' {
  switch (state) {
    case 'running':
      return 'success';
    case 'paused':
    case 'restarting':
      return 'warning';
    case 'dead':
      return 'error';
    case 'exited':
    case 'created':
      return 'default';
    default:
      return 'info';
  }
}

function shortID(id: string): string {
  return id.startsWith('sha256:') ? id.slice(7, 19) : id.slice(0, 12);
}

// ---- 通用动作与确认 ----

const acting = ref(false);

interface PendingConfirm {
  title: string;
  summary: string;
  detail?: string;
  label: string;
  run: () => Promise<unknown>;
}

const pending = ref<PendingConfirm | null>(null);

const confirmOpen = computed({
  get: () => pending.value !== null,
  set: (value: boolean) => {
    if (!value) pending.value = null;
  },
});

async function runTask(label: string, task: () => Promise<unknown>): Promise<boolean> {
  acting.value = true;
  try {
    await task();
    message.success(label);
    void refresh({ silent: true });
    return true;
  } catch (err) {
    message.error(describe(err, `${label} 失败`));
    return false;
  } finally {
    acting.value = false;
  }
}

async function confirmPending(): Promise<void> {
  const item = pending.value;
  if (!item) return;
  const done = await runTask(item.label, item.run);
  if (done) pending.value = null;
}

function askRemove(label: string, title: string, text: string, run: () => Promise<unknown>, detail?: string): void {
  pending.value = { title, summary: text, detail, label, run };
}

// ---- 容器 ----

const containerLabels: Record<DockerContainerAction, string> = {
  start: '启动',
  stop: '停止',
  restart: '重启',
  pause: '暂停',
  unpause: '恢复',
  remove: '删除',
};

function containerAct(container: DockerContainer, action: DockerContainerAction): void {
  const id = target.value;
  if (id === null) return;

  const label = `${containerLabels[action]}容器 ${container.name}`;
  if (action === 'remove') {
    askRemove(
      label,
      '删除容器',
      `容器 ${container.name} 将被删除，容器内未落在卷里的数据会一起消失。`,
      () => api.docker.containerAction(id, container.id, action, true),
    );
    return;
  }
  void runTask(label, () => api.docker.containerAction(id, container.id, action));
}

function actionButton(label: string, onClick: () => void) {
  return h(NButton, { size: 'tiny', quaternary: true, disabled: acting.value, onClick }, { default: () => label });
}

const containerColumns = computed<DataTableColumns<DockerContainer>>(() => [
  {
    title: '名称',
    key: 'name',
    width: 200,
    render: (row) =>
      h('div', { class: 'dwin__cell-name' }, [
        h('span', { class: 'num' }, row.name),
        row.project
          ? h(NTag, { size: 'tiny', bordered: false, type: 'info' }, { default: () => row.project })
          : null,
      ]),
  },
  {
    title: '镜像',
    key: 'image',
    minWidth: 160,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, row.image),
  },
  {
    title: '状态',
    key: 'state',
    width: 150,
    render: (row) =>
      h(
        NTag,
        { size: 'tiny', bordered: false, type: stateType(row.state) },
        { default: () => row.status || row.state },
      ),
  },
  {
    title: '端口',
    key: 'ports',
    minWidth: 170,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, row.ports || '—'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 290,
    align: 'right',
    render: (row) => {
      const buttons =
        row.state === 'running'
          ? [
              actionButton('停止', () => containerAct(row, 'stop')),
              actionButton('重启', () => containerAct(row, 'restart')),
              actionButton('文件', () => openContainerFiles(row)),
            ]
          : row.state === 'paused'
            ? [actionButton('恢复', () => containerAct(row, 'unpause'))]
            : [actionButton('启动', () => containerAct(row, 'start'))];
      buttons.push(actionButton('日志', () => void openContainerLogs(row)));
      buttons.push(actionButton('删除', () => containerAct(row, 'remove')));
      return h(NSpace, { size: 2, justify: 'end' }, { default: () => buttons });
    },
  },
]);

// ---- 新建容器 ----

const createOpen = ref(false);
const creating = ref(false);
const createForm = ref({
  image: '',
  name: '',
  command: '',
  ports: '',
  env: '',
  volumes: '',
  restart: 'unless-stopped',
  network: '',
});

const restartOptions = [
  { label: 'unless-stopped（推荐）', value: 'unless-stopped' },
  { label: 'always', value: 'always' },
  { label: 'no', value: 'no' },
  { label: 'on-failure', value: 'on-failure' },
];

function openCreate(): void {
  createForm.value = {
    image: '',
    name: '',
    command: '',
    ports: '',
    env: '',
    volumes: '',
    restart: 'unless-stopped',
    network: '',
  };
  createOpen.value = true;
}

function splitLines(raw: string): string[] {
  return raw
    .split('\n')
    .map((item) => item.trim())
    .filter((item) => item !== '');
}

async function submitCreate(): Promise<void> {
  const id = target.value;
  if (id === null) return;
  if (!createForm.value.image.trim()) {
    message.warning('请填写镜像名，例如 nginx:latest');
    return;
  }

  creating.value = true;
  try {
    await api.docker.createContainer(id, {
      image: createForm.value.image.trim(),
      name: createForm.value.name.trim(),
      command: createForm.value.command.trim(),
      ports: splitLines(createForm.value.ports),
      env: splitLines(createForm.value.env),
      volumes: splitLines(createForm.value.volumes),
      restart: createForm.value.restart,
      network: createForm.value.network,
    });
    createOpen.value = false;
    message.success('容器已创建并启动');
    void refresh({ silent: true });
  } catch (err) {
    message.error(describe(err, '创建容器失败'));
  } finally {
    creating.value = false;
  }
}

// ---- 日志 ----

const logsOpen = ref(false);
const logsTitle = ref('');
const logsText = ref('');
const logsLoading = ref(false);
const logsReload = ref<(() => Promise<void>) | null>(null);

async function showLogs(title: string, load: () => Promise<string>): Promise<void> {
  logsTitle.value = title;
  logsOpen.value = true;
  logsLoading.value = true;
  logsReload.value = async () => {
    logsLoading.value = true;
    try {
      logsText.value = await load();
    } catch (err) {
      logsText.value = describe(err, '读取日志失败');
    } finally {
      logsLoading.value = false;
    }
  };
  await logsReload.value();
}

async function openContainerLogs(container: DockerContainer): Promise<void> {
  const id = target.value;
  if (id === null) return;
  await showLogs(`容器日志 · ${container.name}`, async () => {
    const res = await api.docker.containerLogs(id, container.id, 300);
    return res.output || '（没有输出）';
  });
}

async function openComposeLogs(project: DockerProject): Promise<void> {
  const id = target.value;
  if (id === null) return;
  if (project.configFiles.length === 0) {
    message.warning('这个项目没有记录 compose 文件路径，读不到日志');
    return;
  }
  await showLogs(`项目日志 · ${project.name}`, async () => {
    const res = await api.docker.composeAction(id, project.name, project.configFiles, 'logs');
    return res.output || '（没有输出）';
  });
}

// ---- 容器内文件（只读） ----
//
// 走容器自己的 sh，而不是宿主机上的路径映射：容器可能用了 overlayfs、tmpfs、
// 或者根本没有对应的宿主机目录。只读是刻意的 —— 这是"看一眼"的工具。

const filesOpen = ref(false);
const filesContainer = ref<DockerContainer | null>(null);
const filesListing = ref<DockerContainerFiles | null>(null);
const filesPath = ref('/');
const filesLoading = ref(false);
const filesError = ref('');
const fileView = ref<DockerContainerText | null>(null);

const fileEntries = computed(() => filesListing.value?.entries ?? []);
const filesParent = computed(() => filesListing.value?.parent ?? '');

async function loadContainerFiles(dir: string): Promise<void> {
  const id = target.value;
  const container = filesContainer.value;
  if (id === null || !container) return;

  filesLoading.value = true;
  filesError.value = '';
  fileView.value = null;
  try {
    const res = await api.docker.containerFiles(id, container.id, dir);
    filesListing.value = res;
    filesPath.value = res.path;
  } catch (err) {
    filesError.value = describe(err, '读取容器目录失败');
  } finally {
    filesLoading.value = false;
  }
}

function openContainerFiles(container: DockerContainer): void {
  filesContainer.value = container;
  filesListing.value = null;
  filesPath.value = '/';
  filesError.value = '';
  fileView.value = null;
  filesOpen.value = true;
  void loadContainerFiles('/');
}

function openContainerFile(entry: DockerContainerFile): void {
  if (entry.dir) {
    void loadContainerFiles(entry.path);
    return;
  }
  void readContainerFile(entry);
}

async function readContainerFile(entry: DockerContainerFile): Promise<void> {
  const id = target.value;
  const container = filesContainer.value;
  if (id === null || !container) return;

  filesLoading.value = true;
  filesError.value = '';
  try {
    fileView.value = await api.docker.readContainerFile(id, container.id, entry.path);
  } catch (err) {
    filesError.value = describe(err, '读取容器文件失败');
  } finally {
    filesLoading.value = false;
  }
}

function fileSizeText(entry: DockerContainerFile): string {
  if (entry.dir) return '—';
  if (entry.link) return '链接';
  return formatBytes(entry.size);
}

// ---- Compose ----

const composeActing = ref(false);

function composeAct(project: DockerProject, action: DockerComposeAction): void {
  const id = target.value;
  if (id === null) return;
  if (project.configFiles.length === 0) {
    message.warning('这个项目没有记录 compose 文件路径，无法执行该操作');
    return;
  }

  const labels: Record<DockerComposeAction, string> = {
    up: '启动',
    down: '移除',
    stop: '停止',
    start: '启动',
    restart: '重启',
    pull: '拉取镜像',
    logs: '查看日志',
  };
  const label = `${labels[action]}项目 ${project.name}`;
  if (action === 'down') {
    askRemove(
      label,
      '移除 Compose 项目',
      `项目 ${project.name} 的容器与网络会被移除（命名卷默认保留）。`,
      () => api.docker.composeAction(id, project.name, project.configFiles, action),
      project.configFiles.join('、'),
    );
    return;
  }
  composeActing.value = true;
  void runTask(label, () => api.docker.composeAction(id, project.name, project.configFiles, action)).finally(
    () => {
      composeActing.value = false;
    },
  );
}

// ---- compose 在线编辑 ----

const editorOpen = ref(false);
/** 目录选择器与编辑器分离：它只负责把绝对路径交回来，不碰编辑器里的其他字段。 */
const dirPickerOpen = ref(false);
const editor = ref({
  dir: '',
  fileName: 'compose.yaml',
  project: '',
  content: '',
  loading: false,
});

function applyProjectDir(path: string): void {
  editor.value.dir = path;
}

const defaultCompose = `services:
  app:
    image: nginx:latest
    container_name: app
    restart: unless-stopped
    ports:
      - "8080:80"
`;

function dirname(path: string): string {
  const index = path.lastIndexOf('/');
  return index <= 0 ? '/' : path.slice(0, index);
}

function basename(path: string): string {
  const index = path.lastIndexOf('/');
  return index < 0 ? path : path.slice(index + 1);
}

function composePath(): string {
  const dir = editor.value.dir.trim().replace(/\/+$/, '');
  const name = editor.value.fileName.trim();
  if (dir === '' || name === '') return '';
  return `${dir}/${name}`;
}

async function openComposeEditor(project?: DockerProject): Promise<void> {
  const id = target.value;
  if (id === null) return;

  if (!project) {
    editor.value = {
      dir: '',
      fileName: 'compose.yaml',
      project: '',
      content: defaultCompose,
      loading: false,
    };
    editorOpen.value = true;
    return;
  }

  const file = project.configFiles[0] ?? '';
  editor.value = {
    dir: file ? dirname(file) : project.workingDir,
    fileName: file ? basename(file) : 'compose.yaml',
    project: project.name,
    content: '',
    loading: file !== '',
  };
  editorOpen.value = true;

  if (file === '') {
    editor.value.loading = false;
    message.warning('这个项目没有记录 compose 文件路径，只能按目录新建或修改一个文件');
    return;
  }
  try {
    const res = await api.docker.readCompose(id, file);
    editor.value.content = res.content;
  } catch (err) {
    message.error(describe(err, '读取 compose 文件失败'));
  } finally {
    editor.value.loading = false;
  }
}

async function saveCompose(deploy: boolean): Promise<void> {
  const id = target.value;
  if (id === null) return;

  const path = composePath();
  if (path === '') {
    message.warning('请填写绝对路径的目录与文件名');
    return;
  }
  if (!path.startsWith('/')) {
    message.warning('目录必须是绝对路径，例如 /opt/app');
    return;
  }
  if (!editor.value.content.trim()) {
    message.warning('compose 内容不能为空');
    return;
  }

  editor.value.loading = true;
  try {
    const res = await api.docker.saveCompose(id, {
      path,
      content: editor.value.content,
      project: editor.value.project.trim(),
      deploy,
    });
    editorOpen.value = false;
    message.success(deploy ? '已保存并部署' : `已保存 ${res.path}`);
    void refresh({ silent: true });
  } catch (err) {
    message.error(describe(err, deploy ? '保存并部署失败' : '保存失败'));
  } finally {
    editor.value.loading = false;
  }
}

// ---- 镜像 ----

const imageColumns = computed<DataTableColumns<DockerImage>>(() => [
  {
    title: '镜像',
    key: 'ref',
    minWidth: 220,
    render: (row) =>
      h('div', { class: 'dwin__cell-name' }, [
        h('span', { class: 'num' }, row.ref),
        row.dangling ? h(NTag, { size: 'tiny', bordered: false }, { default: () => '悬空' }) : null,
      ]),
  },
  {
    title: '镜像 ID',
    key: 'id',
    width: 140,
    render: (row) => h('span', { class: 'num' }, shortID(row.id)),
  },
  { title: '大小', key: 'size', width: 100, render: (row) => h('span', { class: 'num' }, row.size || '—') },
  {
    title: '创建',
    key: 'created',
    minWidth: 120,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { class: 'num' }, row.created || '—'),
  },
  {
    title: '操作',
    key: 'actions',
    width: 100,
    align: 'right',
    render: (row) =>
      h(
        NSpace,
        { size: 2, justify: 'end' },
        { default: () => [actionButton('删除', () => removeImage(row))] },
      ),
  },
]);

function removeImage(image: DockerImage): void {
  const id = target.value;
  if (id === null) return;
  askRemove(
    `删除镜像 ${image.ref}`,
    '删除镜像',
    `镜像 ${image.ref}（${image.size || '大小未知'}）将从目标机上删除。`,
    () => api.docker.removeImages(id, [image.ref], false),
    '如果它还被容器引用，docker 会拒绝删除 —— 先删掉那个容器再来。',
  );
}

// ---- 镜像仓库（只拉取） ----

const searchTerm = ref('');
const searching = ref(false);
const searchHits = ref<DockerSearchHit[]>([]);
const searchNote = ref('');

async function doSearch(): Promise<void> {
  const id = target.value;
  if (id === null || searching.value) return;
  const term = searchTerm.value.trim();
  if (!term) {
    message.warning('请输入要搜索的镜像名');
    return;
  }

  searching.value = true;
  try {
    const res = await api.docker.searchImages(id, term);
    searchHits.value = res.hits ?? [];
    searchNote.value = res.note ?? '';
  } catch (err) {
    searchHits.value = [];
    searchNote.value = describe(err, '搜索镜像失败');
  } finally {
    searching.value = false;
  }
}

const pullOpen = ref(false);
const pullRef = ref('');
const pulling = ref(false);
const pullOutput = ref('');

function openPull(reference = ''): void {
  pullRef.value = reference;
  pullOutput.value = '';
  pullOpen.value = true;
}

async function doPull(): Promise<void> {
  const id = target.value;
  if (id === null || pulling.value) return;
  const reference = pullRef.value.trim();
  if (!reference) {
    message.warning('请填写镜像名，例如 nginx:latest');
    return;
  }

  pulling.value = true;
  pullOutput.value = '';
  try {
    const res = await api.docker.pullImage(id, reference);
    pullOutput.value = res.output || '拉取完成';
    message.success(`已拉取 ${reference}`);
    void refresh({ silent: true });
  } catch (err) {
    pullOutput.value = describe(err, '拉取镜像失败');
    message.error(describe(err, '拉取镜像失败'));
  } finally {
    pulling.value = false;
  }
}

// ---- 镜像加速与代理 ----
//
// 这两项必须落在目标机的 daemon.json 上：docker pull 由 dockerd 发起，
// 面板进程里的代理环境变量传不过去。保存会重启 Docker，因此界面上要二次确认。

const settingsOpen = ref(false);
const settingsLoading = ref(false);
const settingsSaving = ref(false);
const settingsError = ref('');
const settingsMeta = ref<{ path: string; hasFile: boolean } | null>(null);
const settingsForm = ref({ mirrors: '', proxy: '', noProxy: '' });

async function openSettings(): Promise<void> {
  const id = target.value;
  if (id === null) return;

  settingsOpen.value = true;
  settingsLoading.value = true;
  settingsError.value = '';
  settingsMeta.value = null;
  try {
    const cfg = await api.docker.getConfig(id);
    settingsForm.value = {
      mirrors: cfg.mirrors.join('\n'),
      proxy: cfg.proxy,
      noProxy: cfg.noProxy,
    };
    settingsMeta.value = { path: cfg.path, hasFile: cfg.hasFile };
  } catch (err) {
    settingsError.value = describe(err, '读取 Docker 配置失败');
  } finally {
    settingsLoading.value = false;
  }
}

function clearSettings(): void {
  settingsForm.value = { mirrors: '', proxy: '', noProxy: '' };
}

async function saveSettings(): Promise<void> {
  const id = target.value;
  if (id === null || settingsSaving.value) return;

  const mirrors = settingsForm.value.mirrors
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
  const proxy = settingsForm.value.proxy.trim();

  settingsSaving.value = true;
  settingsError.value = '';
  try {
    await api.docker.setConfig(id, {
      mirrors,
      proxy,
      // 没有代理时 NO_PROXY 会被后端一并清掉，这里提前归零，避免界面看起来"保存了"。
      noProxy: proxy ? settingsForm.value.noProxy.trim() : '',
    });
    message.success('已写入 Docker 配置并重启服务');
    settingsOpen.value = false;
    void refresh({ silent: true });
  } catch (err) {
    settingsError.value = describe(err, '保存 Docker 配置失败');
  } finally {
    settingsSaving.value = false;
  }
}

// ---- 网络 ----

const networkColumns = computed<DataTableColumns<DockerNetwork>>(() => [
  {
    title: '名称',
    key: 'name',
    minWidth: 180,
    render: (row) =>
      h('div', { class: 'dwin__cell-name' }, [
        h('span', { class: 'num' }, row.name),
        row.internal ? h(NTag, { size: 'tiny', bordered: false }, { default: () => 'internal' }) : null,
        row.ipv6 ? h(NTag, { size: 'tiny', bordered: false }, { default: () => 'IPv6' }) : null,
      ]),
  },
  { title: '驱动', key: 'driver', width: 100, render: (row) => h('span', { class: 'num' }, row.driver) },
  { title: '范围', key: 'scope', width: 90, render: (row) => h('span', { class: 'num' }, row.scope) },
  {
    title: '网络 ID',
    key: 'id',
    width: 130,
    render: (row) => h('span', { class: 'num' }, shortID(row.id)),
  },
  {
    title: '操作',
    key: 'actions',
    width: 150,
    align: 'right',
    render: (row) =>
      h(NSpace, { size: 2, justify: 'end' }, {
        default: () => [
          actionButton('详情', () => void openNetworkDetail(row)),
          actionButton('删除', () => removeNetwork(row)),
        ],
      }),
  },
]);

function removeNetwork(network: DockerNetwork): void {
  const id = target.value;
  if (id === null) return;
  askRemove(
    `删除网络 ${network.name}`,
    '删除 Docker 网络',
    `网络 ${network.name} 将被删除。仍有容器接入时 docker 会拒绝删除。`,
    () => api.docker.removeNetwork(id, network.name),
    network.driver === 'bridge' ? 'bridge 驱动是默认网络，通常不该删；请确认这是你自己建的那一个。' : undefined,
  );
}

const networkOpen = ref(false);
const networkForm = ref({
  name: '',
  driver: 'bridge',
  subnet: '',
  gateway: '',
  internal: false,
});
const networkCreating = ref(false);

const driverOptions = [
  { label: 'bridge', value: 'bridge' },
  { label: 'overlay', value: 'overlay' },
  { label: 'macvlan', value: 'macvlan' },
  { label: 'ipvlan', value: 'ipvlan' },
];

function openNetworkCreate(): void {
  networkForm.value = { name: '', driver: 'bridge', subnet: '', gateway: '', internal: false };
  networkOpen.value = true;
}

async function submitNetwork(): Promise<void> {
  const id = target.value;
  if (id === null) return;
  if (!networkForm.value.name.trim()) {
    message.warning('请填写网络名');
    return;
  }

  networkCreating.value = true;
  try {
    await api.docker.createNetwork(id, {
      name: networkForm.value.name.trim(),
      driver: networkForm.value.driver,
      subnet: networkForm.value.subnet.trim(),
      gateway: networkForm.value.gateway.trim(),
      internal: networkForm.value.internal,
    });
    networkOpen.value = false;
    message.success('网络已创建');
    void refresh({ silent: true });
  } catch (err) {
    message.error(describe(err, '创建网络失败'));
  } finally {
    networkCreating.value = false;
  }
}

const networkDetailOpen = ref(false);
const networkDetail = ref<DockerNetworkDetail | null>(null);
const networkDetailLoading = ref(false);
const connectTarget = ref<string | null>(null);

async function openNetworkDetail(network: DockerNetwork): Promise<void> {
  const id = target.value;
  if (id === null) return;
  networkDetailOpen.value = true;
  networkDetail.value = null;
  networkDetailLoading.value = true;
  connectTarget.value = null;
  try {
    networkDetail.value = await api.docker.networkDetail(id, network.name);
    const attached = new Set(networkDetail.value.containers.map((item) => item.name));
    const candidate = containers.value.find((item) => !attached.has(item.name));
    connectTarget.value = candidate?.id ?? null;
  } catch (err) {
    message.error(describe(err, '读取网络详情失败'));
    networkDetailOpen.value = false;
  } finally {
    networkDetailLoading.value = false;
  }
}

async function reloadNetworkDetail(): Promise<void> {
  const detail = networkDetail.value;
  if (!detail) return;
  const id = target.value;
  if (id === null) return;
  try {
    networkDetail.value = await api.docker.networkDetail(id, detail.name);
  } catch (err) {
    message.error(describe(err, '读取网络详情失败'));
  }
}

async function connectContainer(): Promise<void> {
  const id = target.value;
  const detail = networkDetail.value;
  const container = connectTarget.value;
  if (id === null || !detail || !container) {
    message.warning('请选择要接入的容器');
    return;
  }
  const done = await runTask(`把 ${container} 接入 ${detail.name}`, () =>
    api.docker.connectNetwork(id, detail.name, container),
  );
  if (done) {
    await reloadNetworkDetail();
    connectTarget.value = null;
  }
}

async function disconnectContainer(container: string): Promise<void> {
  const id = target.value;
  const detail = networkDetail.value;
  if (id === null || !detail) return;
  const done = await runTask(`断开 ${container} 与 ${detail.name}`, () =>
    api.docker.connectNetwork(id, detail.name, container, true),
  );
  if (done) await reloadNetworkDetail();
}

const containerOptions = computed(() =>
  containers.value.map((item) => ({
    label: `${item.name}（${item.state}）`,
    value: item.id,
  })),
);

const hostLabel = computed(() => props.hostName || pickedName.value || '未选择主机');
</script>

<template>
  <div ref="root" class="dwin">
    <div class="dwin__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">CONTAINERS</p>
        <span class="dwin__title page-title">
          <span class="led" :class="probe && !probe.error ? 'led--ok' : 'led--danger'" aria-hidden="true" />
          Docker 管理
        </span>
        <span class="dwin__note page-meta">
          <template v-if="summary">
            {{ hostLabel }} · {{ containers.length }} 个容器（{{ runningCount }} 运行）·
            {{ images.length }} 个镜像 · {{ networks.length }} 个网络
            <template v-if="probe?.version">· Docker {{ probe.version }}</template>
          </template>
          <template v-else>{{ hostLabel }}</template>
        </span>
      </div>
      <n-button v-if="usable" size="small" @click="openPull()">拉取镜像</n-button>
      <n-button v-if="usable" size="small" type="primary" :disabled="acting" @click="openCreate">
        新建容器
      </n-button>
      <n-button size="small" class="dwin__refresh" @click="onRefreshClick">
        <template #icon v-if="fetching">
          <n-spin :size="16" />
        </template>
        <template v-if="!fetching">刷新</template>
      </n-button>

      <!-- 主机胶囊放页头右端：它和标题回答的是同一个问题（现在看的是哪台机器）。 -->
      <host-picker
        v-if="props.hostId === undefined"
        v-model:host-id="target"
        v-model:host-name="pickedName"
        include-local
      />
    </div>

    <div v-if="lastError" class="dwin__error">
      <span class="led led--danger" aria-hidden="true" />
      {{ lastError }}
    </div>

    <n-alert
      v-if="summary && probe?.error"
      :type="probe.available ? 'warning' : 'info'"
      :title="probe.available ? 'Docker 守护进程不可用' : '目标机上没有 Docker'"
    >
      {{ probe.error }}
      <template v-if="probe.available">
        请确认 docker 服务已启动、当前账号在 docker 组里（或用 root 登录），面板会尝试自动提权。
      </template>
      <template v-else>
        可以直接点下面的「一键安装 Docker」：面板会用目标机的包管理器安装 Engine 与 compose 插件。
      </template>
    </n-alert>

    <div v-if="summary && !usable" class="dwin__install">
      <n-button type="primary" :loading="installPlanLoading" @click="openInstall">
        {{ probe?.available ? '尝试修复 Docker 服务' : '一键安装 Docker' }}
      </n-button>
      <span class="dwin__install-note">
        使用 Docker 官方源安装 engine、buildx 与 compose 插件；执行前会先给出变更预检，需要 root 时自动提权，
        安装前的系统状态会留下快照。
      </span>
    </div>

    <div v-for="item in blockErrors" :key="item" class="dwin__warn">
      <span class="led led--warn" aria-hidden="true" />
      {{ item }}
    </div>

    <n-tabs v-if="usable" v-model:value="activeTab" class="dwin__tabs" type="line" animated>
      <n-tab-pane name="overview" tab="仪表盘">
        <div class="dwin__dash">
          <div class="ui-stats">
            <article v-for="card in overviewCards" :key="card.key" class="ui-card ui-stat">
              <span class="ui-stat__label">{{ card.label }}</span>
              <b class="ui-stat__value">{{ card.value }}<span>{{ card.unit }}</span></b>
              <span class="ui-stat__foot">{{ card.foot }}</span>
            </article>
          </div>

          <div class="dwin__dash-grid">
            <section class="ui-card">
              <div class="ui-card__head">
                <div class="ui-card__ident">
                  <p class="ui-card__eyebrow">RUNTIME</p>
                  <h2 class="ui-card__title">健康与版本</h2>
                </div>
                <span class="ui-card__meta">{{ usable ? '可用' : '不可用' }}</span>
              </div>
              <div class="ui-kv">
                <div>
                  <span>Docker 引擎</span>
                  <b>{{ probe?.version ? `v${probe.version}` : '—' }}</b>
                </div>
                <div>
                  <span>Compose</span>
                  <b>{{ probe?.composeVersion ? `v${probe.composeVersion}` : '未安装' }}</b>
                </div>
                <div>
                  <span>容器运行时</span>
                  <b>{{ runningCount }} / {{ containers.length }} 运行</b>
                </div>
                <div>
                  <span>采集状态</span>
                  <b>{{ blockErrors.length > 0 ? `${blockErrors.length} 块失败` : '正常' }}</b>
                </div>
              </div>
              <div v-if="blockErrors.length > 0" class="dwin__warn dwin__dash-warn">
                {{ blockErrors.join('；') }}
              </div>
            </section>

            <section class="ui-card">
              <div class="ui-card__head">
                <div class="ui-card__ident">
                  <p class="ui-card__eyebrow">CAPACITY</p>
                  <h2 class="ui-card__title">容量与冗余</h2>
                </div>
                <span class="ui-card__meta">{{ images.length }} 个镜像</span>
              </div>
              <div class="ui-kv">
                <div>
                  <span>镜像总占用</span>
                  <b>{{ formatBytes(imageBytes) }}</b>
                </div>
                <div>
                  <span>悬空镜像</span>
                  <b>{{ danglingImages.length }} 个 · {{ formatBytes(danglingBytes) }}</b>
                </div>
                <div>
                  <span>最大单镜像</span>
                  <b>{{ formatBytes(largestImageBytes) }}</b>
                </div>
              </div>
              <div class="ui-meter dwin__dash-meter">
                <span>冗余</span>
                <div class="ui-bar">
                  <i :class="{ 'is-warn': danglingShare >= 20 }" :style="{ width: `${danglingShare}%` }" />
                </div>
                <b>{{ danglingShare.toFixed(0) }}%</b>
              </div>
              <p class="ui-card__foot">
                {{
                  danglingImages.length > 0
                    ? `悬空镜像不被任何容器引用，删除后腾出 ${formatBytes(danglingBytes)}。`
                    : '没有悬空镜像，镜像层没有可回收的冗余。'
                }}
              </p>
            </section>

            <section class="ui-card">
              <div class="ui-card__head">
                <div class="ui-card__ident">
                  <p class="ui-card__eyebrow">CONTAINERS</p>
                  <h2 class="ui-card__title">容器速览</h2>
                </div>
                <span class="ui-card__meta">{{ runningCount }} / {{ containers.length }} 运行</span>
              </div>
              <div v-if="stoppedContainers.length > 0" class="dwin__dash-list">
                <div
                  v-for="item in stoppedContainers.slice(0, 6)"
                  :key="item.id || item.name"
                  class="dwin__dash-row"
                >
                  <span class="led" :class="containerLed(item)" aria-hidden="true" />
                  <span class="dwin__dash-name num">{{ item.name }}</span>
                  <span class="dwin__dash-status">{{ item.status || item.state }}</span>
                  <span class="dwin__dash-image num">{{ item.image }}</span>
                </div>
                <p v-if="stoppedContainers.length > 6" class="dwin__dash-more">
                  还有 {{ stoppedContainers.length - 6 }} 个未运行容器，见「容器」页。
                </p>
              </div>
              <p v-else class="dwin__empty">所有容器都在运行</p>
            </section>

            <section class="ui-card">
              <div class="ui-card__head">
                <div class="ui-card__ident">
                  <p class="ui-card__eyebrow">COMPOSE</p>
                  <h2 class="ui-card__title">项目状态</h2>
                </div>
                <span class="ui-card__meta">{{ healthyProjects }} / {{ projects.length }} 全部运行</span>
              </div>
              <div v-if="projects.length > 0" class="dwin__dash-list">
                <div v-for="project in projects.slice(0, 6)" :key="project.name" class="dwin__dash-row">
                  <span
                    class="led"
                    :class="project.running === project.total ? 'led--ok' : 'led--warn'"
                    aria-hidden="true"
                  />
                  <span class="dwin__dash-name num">{{ project.name }}</span>
                  <span class="dwin__dash-status">{{ project.running }} / {{ project.total }} 运行</span>
                </div>
                <p v-if="projects.length > 6" class="dwin__dash-more">
                  还有 {{ projects.length - 6 }} 个项目，见「Compose」页。
                </p>
              </div>
              <p v-else class="dwin__empty">还没有由 compose 启动的项目</p>
            </section>
          </div>
        </div>
      </n-tab-pane>

      <n-tab-pane name="containers" :tab="`容器 (${containers.length})`">
        <n-data-table
          class="dwin__table"
          :columns="containerColumns"
          :data="containers"
          :bordered="false"
          :row-key="(row: DockerContainer) => row.id || row.name"
          size="small"
          flex-height
        />
      </n-tab-pane>

      <n-tab-pane name="compose" :tab="`Compose (${projects.length})`">
        <div class="dwin__compose-bar">
          <span class="dwin__compose-note">
            <template v-if="hasCompose">compose {{ probe?.composeVersion }}</template>
            <template v-else>目标机上没有 docker compose，项目管理不可用</template>
          </span>
          <n-button size="small" :disabled="!hasCompose" @click="openComposeEditor()">新建 / 编辑项目</n-button>
        </div>

        <div class="dwin__projects">
          <article v-for="project in projects" :key="project.name" class="dwin__project">
            <header class="dwin__project-head">
              <span class="dwin__project-name num">{{ project.name }}</span>
              <n-tag
                size="tiny"
                :bordered="false"
                :type="project.running === project.total && project.total > 0 ? 'success' : 'warning'"
              >
                {{ project.running }} / {{ project.total }} 运行
              </n-tag>
              <span class="dwin__spacer" />
              <n-button size="tiny" quaternary :disabled="!hasCompose || composeActing" @click="composeAct(project, 'up')">
                启动
              </n-button>
              <n-button size="tiny" quaternary :disabled="!hasCompose || composeActing" @click="composeAct(project, 'stop')">
                停止
              </n-button>
              <n-button size="tiny" quaternary :disabled="!hasCompose || composeActing" @click="composeAct(project, 'restart')">
                重启
              </n-button>
              <n-button size="tiny" quaternary :disabled="!hasCompose" @click="openComposeLogs(project)">日志</n-button>
              <n-button size="tiny" quaternary :disabled="!hasCompose" @click="openComposeEditor(project)">编辑</n-button>
              <n-button size="tiny" quaternary :disabled="!hasCompose || composeActing" @click="composeAct(project, 'down')">
                移除
              </n-button>
            </header>
            <div class="dwin__project-files num">
              {{ project.configFiles.length > 0 ? project.configFiles.join('、') : '未记录 compose 文件路径' }}
            </div>
            <div class="dwin__project-containers num">{{ project.containers.join('、') }}</div>
          </article>
          <p v-if="projects.length === 0" class="dwin__empty">
            目标机上还没有由 compose 启动的项目。可以用「新建 / 编辑项目」写一份 compose.yaml 并部署。
          </p>
        </div>
      </n-tab-pane>

      <n-tab-pane name="images" :tab="`镜像 (${images.length})`">
        <div class="dwin__compose-bar">
          <span class="dwin__compose-note">本地镜像。删除仍被容器引用的镜像会被 docker 拒绝。</span>
          <n-button size="small" @click="openPull()">拉取镜像</n-button>
        </div>
        <n-data-table
          class="dwin__table"
          :columns="imageColumns"
          :data="images"
          :bordered="false"
          :row-key="(row: DockerImage) => row.id + row.ref"
          size="small"
          flex-height
        />
      </n-tab-pane>

      <n-tab-pane name="registry" tab="镜像仓库">
        <div class="dwin__search">
          <n-input
            v-model:value="searchTerm"
            placeholder="搜索镜像，例如 nginx、redis、bitnami/postgresql"
            :disabled="searching"
            @keyup.enter="doSearch"
          />
          <n-button type="primary" :loading="searching" @click="doSearch">搜索</n-button>
          <n-button :disabled="target === null" @click="openSettings">设置</n-button>
        </div>
        <p class="dwin__registry-note">
          搜索与拉取都从目标机发起：目标机需要能访问外网或配置了镜像加速。面板不保存仓库凭据。
        </p>
        <div v-if="searchNote" class="dwin__warn">
          <span class="led led--warn" aria-hidden="true" />
          {{ searchNote }}
        </div>
        <ul class="dwin__hits">
          <li v-for="hit in searchHits" :key="hit.name" class="dwin__hit">
            <div class="dwin__hit-head">
              <span class="dwin__hit-name num">{{ hit.name }}</span>
              <n-tag v-if="hit.official" size="tiny" :bordered="false" type="success">官方</n-tag>
              <span class="dwin__hit-stars num">{{ hit.stars }} 星</span>
              <span class="dwin__spacer" />
              <n-button size="tiny" @click="openPull(hit.name)">拉取</n-button>
            </div>
            <p class="dwin__hit-desc">{{ hit.description || '（没有描述）' }}</p>
          </li>
        </ul>
        <p v-if="searchHits.length === 0" class="dwin__empty">输入镜像名后搜索</p>
      </n-tab-pane>

      <n-tab-pane name="networks" :tab="`网络 (${networks.length})`">
        <div class="dwin__compose-bar">
          <span class="dwin__compose-note">Docker 网络：驱动、范围与接入的容器。</span>
          <n-button size="small" @click="openNetworkCreate">新建网络</n-button>
        </div>
        <n-data-table
          class="dwin__table"
          :columns="networkColumns"
          :data="networks"
          :bordered="false"
          :row-key="(row: DockerNetwork) => row.id"
          size="small"
          flex-height
        />
      </n-tab-pane>
    </n-tabs>

    <div v-else-if="!lastError && !(summary && probe?.error)" class="dwin__placeholder">
      {{ target === null ? '请先选择一台主机' : fetching ? '正在读取 Docker 状态…' : '暂无数据' }}
    </div>

    <!-- 新建容器 -->
    <n-modal
      v-model:show="createOpen"
      preset="card"
      title="新建容器"
      style="width: 640px"
      :mask-closable="!creating"
    >
      <div class="dwform">
        <div class="dwform__field">
          <span class="dwform__label">镜像</span>
          <n-input v-model:value="createForm.image" placeholder="nginx:latest" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">容器名</span>
          <n-input v-model:value="createForm.name" placeholder="留空由 docker 随机生成" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">端口映射</span>
          <n-input
            v-model:value="createForm.ports"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 5 }"
            placeholder="一行一条，例如：&#10;8080:80&#10;127.0.0.1:5432:5432"
          />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">环境变量</span>
          <n-input
            v-model:value="createForm.env"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 5 }"
            placeholder="一行一条，例如：&#10;TZ=Asia/Shanghai&#10;MYSQL_ROOT_PASSWORD=secret"
          />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">卷挂载</span>
          <n-input
            v-model:value="createForm.volumes"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 5 }"
            placeholder="一行一条，例如：&#10;/opt/data:/data&#10;/etc/localtime:/etc/localtime:ro"
          />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">启动命令</span>
          <n-input v-model:value="createForm.command" placeholder="留空使用镜像默认 CMD" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">重启策略</span>
          <n-select v-model:value="createForm.restart" :options="restartOptions" size="small" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">网络</span>
          <n-select v-model:value="createForm.network" :options="networkOptions" size="small" />
        </div>
        <p class="dwform__tip">镜像不存在时 docker 会自动先拉取，可能需要几分钟。</p>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button :disabled="creating" @click="createOpen = false">取消</n-button>
          <n-button type="primary" :loading="creating" @click="submitCreate">创建并启动</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 日志 -->
    <n-modal v-model:show="logsOpen" preset="card" :title="logsTitle" style="width: 900px">
      <div class="dwin__logs-wrap">
        <n-spin v-if="logsLoading" :size="20" />
        <pre v-else class="dwin__logs">{{ logsText }}</pre>
      </div>
      <template #footer>
        <n-space justify="end">
          <n-button :loading="logsLoading" @click="logsReload?.()">重新读取</n-button>
          <n-button @click="logsOpen = false">关闭</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 容器内文件（只读） -->
    <n-modal
      v-model:show="filesOpen"
      preset="card"
      :title="`容器文件 · ${filesContainer?.name ?? ''}`"
      style="width: 900px"
    >
      <div class="dwfiles">
        <div class="dwfiles__bar">
          <n-button size="small" :disabled="!filesParent || filesLoading" @click="loadContainerFiles(filesParent)">
            上级
          </n-button>
          <span class="dwfiles__path num" :title="filesPath">{{ filesPath }}</span>
          <span class="dwin__spacer" />
          <n-button v-if="fileView" size="small" :disabled="filesLoading" @click="fileView = null">返回目录</n-button>
          <n-button size="small" :loading="filesLoading" @click="loadContainerFiles(filesPath)">刷新</n-button>
        </div>

        <div v-if="filesError" class="dwin__error">
          <span class="led led--danger" aria-hidden="true" />
          {{ filesError }}
        </div>

        <div v-if="filesLoading" class="dwfiles__pending">
          <n-spin :size="20" />
        </div>

        <template v-else-if="fileView">
          <div class="dwfiles__file-head">
            <span class="num">{{ fileView.path }}</span>
            <n-tag v-if="fileView.truncated" size="tiny" :bordered="false" type="warning">仅显示前 256 KB</n-tag>
          </div>
          <p v-if="fileView.binary" class="dwfiles__empty">这是二进制文件，无法以文本查看。</p>
          <p v-else-if="!fileView.content" class="dwfiles__empty">文件为空。</p>
          <pre v-else class="dwfiles__content">{{ fileView.content }}</pre>
        </template>

        <template v-else>
          <ul class="dwfiles__list">
            <li
              v-for="entry in fileEntries"
              :key="entry.path"
              class="dwfiles__row"
              @click="openContainerFile(entry)"
            >
              <svg class="dwfiles__icon" aria-hidden="true">
                <use :href="entry.dir ? '#glyph-folder' : '#glyph-file'" />
              </svg>
              <span class="dwfiles__name num" :title="entry.name">{{ entry.name }}</span>
              <n-tag v-if="entry.link" size="tiny" :bordered="false">链接</n-tag>
              <span class="dwfiles__size num">{{ fileSizeText(entry) }}</span>
            </li>
          </ul>
          <p v-if="fileEntries.length === 0" class="dwfiles__empty">这个目录是空的</p>
          <p v-if="filesListing?.truncated" class="dwfiles__hint">条目过多，只列出了前 800 项。</p>
        </template>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button @click="filesOpen = false">关闭</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- compose 编辑器 -->
    <n-modal
      v-model:show="editorOpen"
      preset="card"
      title="Compose 文件"
      style="width: 860px"
      :mask-closable="!editor.loading"
    >
      <div class="dwform">
        <div class="dwform__field">
          <span class="dwform__label">项目目录</span>
          <div class="dwform__path">
            <n-input
              v-model:value="editor.dir"
              class="dwform__path-input"
              placeholder="/opt/app（绝对路径）"
            />
            <n-button size="small" :disabled="target === null" @click="dirPickerOpen = true">
              选择
            </n-button>
          </div>
        </div>
        <div class="dwform__field">
          <span class="dwform__label">文件名</span>
          <n-input v-model:value="editor.fileName" placeholder="compose.yaml" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">项目名</span>
          <n-input v-model:value="editor.project" placeholder="留空则由目录名推导" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">内容</span>
          <n-input
            v-model:value="editor.content"
            type="textarea"
            :autosize="{ minRows: 14, maxRows: 24 }"
            :input-props="{ class: 'dwform__yaml', spellcheck: false }"
            placeholder="services: ..."
          />
        </div>
        <p class="dwform__tip">
          {{ composePath() || '（尚未填写完整路径）' }} ·
          保存并部署会在目标机上执行 compose up -d；相对路径以该目录为基准。
        </p>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button :disabled="editor.loading" @click="editorOpen = false">取消</n-button>
          <n-button :loading="editor.loading" @click="saveCompose(false)">仅保存</n-button>
          <n-button type="primary" :loading="editor.loading" @click="saveCompose(true)">保存并部署</n-button>
        </n-space>
      </template>
    </n-modal>

    <DirPicker
      v-model:show="dirPickerOpen"
      :host-id="target"
      :initial-path="editor.dir"
      @select="applyProjectDir"
    />

    <!-- 拉取镜像 -->
    <n-modal
      v-model:show="pullOpen"
      preset="card"
      title="拉取镜像"
      style="width: 640px"
      :mask-closable="!pulling"
    >
      <div class="dwform">
        <div class="dwform__field">
          <span class="dwform__label">镜像</span>
          <n-input
            v-model:value="pullRef"
            placeholder="nginx:latest 或 registry.example.com/ns/app:1.0"
            :disabled="pulling"
            @keyup.enter="doPull"
          />
        </div>
        <p class="dwform__tip">拉取由目标机发起，可能需要几分钟；输出会显示在下面。</p>
        <pre v-if="pullOutput" class="dwin__logs">{{ pullOutput }}</pre>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button :disabled="pulling" @click="pullOpen = false">关闭</n-button>
          <n-button type="primary" :loading="pulling" @click="doPull">拉取</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 镜像加速与代理 -->
    <n-modal
      v-model:show="settingsOpen"
      preset="card"
      title="镜像加速与代理"
      style="width: 680px"
      :mask-closable="!settingsSaving"
    >
      <div class="dwform">
        <div class="dwform__field">
          <span class="dwform__label">加速源</span>
          <div class="dwform__stack">
            <n-input
              v-model:value="settingsForm.mirrors"
              type="textarea"
              :autosize="{ minRows: 3, maxRows: 6 }"
              placeholder="每行一个，例如 https://docker.mirrors.ustc.edu.cn"
              :disabled="settingsLoading || settingsSaving"
            />
            <p class="dwform__tip">
              写入 daemon.json 的 registry-mirrors；多个按顺序尝试，留空表示不启用。
            </p>
          </div>
        </div>

        <div class="dwform__field">
          <span class="dwform__label">代理</span>
          <div class="dwform__stack">
            <n-input
              v-model:value="settingsForm.proxy"
              placeholder="http://127.0.0.1:7890（留空表示不使用代理）"
              :disabled="settingsLoading || settingsSaving"
            />
            <p class="dwform__tip">
              同时用于 HTTP 与 HTTPS，需要目标机 Docker 23.0 及以上。
            </p>
          </div>
        </div>

        <div class="dwform__field">
          <span class="dwform__label">NO_PROXY</span>
          <div class="dwform__stack">
            <n-input
              v-model:value="settingsForm.noProxy"
              placeholder="localhost,127.0.0.1,.internal（逗号分隔）"
              :disabled="settingsLoading || settingsSaving"
            />
            <p class="dwform__tip">只在填写代理时生效。</p>
          </div>
        </div>

        <p v-if="settingsLoading" class="dwform__tip">正在读取目标机配置…</p>
        <p v-else-if="settingsMeta" class="dwform__tip num">
          {{ settingsMeta.path }} ·
          {{ settingsMeta.hasFile ? '已有配置，保存会保留其他字段' : '文件不存在，保存会新建' }}
        </p>

        <div v-if="settingsError" class="dwin__warn">
          <span class="led led--warn" aria-hidden="true" />
          {{ settingsError }}
        </div>

        <div class="dwin__warn">
          <span class="led led--warn" aria-hidden="true" />
          保存会写入目标机 /etc/docker/daemon.json 并重启 Docker，所有容器会短暂中断；
          重启失败会自动恢复原配置。
        </div>
      </div>

      <template #footer>
        <n-space justify="space-between">
          <n-button :disabled="settingsLoading || settingsSaving" @click="clearSettings">
            清空
          </n-button>
          <n-space>
            <n-button :disabled="settingsSaving" @click="settingsOpen = false">取消</n-button>
            <n-popconfirm @positive-click="saveSettings">
              <template #trigger>
                <n-button type="primary" :loading="settingsSaving" :disabled="settingsLoading">
                  保存并重启 Docker
                </n-button>
              </template>
              重启 Docker 会中断这台主机上的所有容器（带 restart 策略的会自动拉起）。确定继续？
            </n-popconfirm>
          </n-space>
        </n-space>
      </template>
    </n-modal>

    <!-- 新建网络 -->
    <n-modal
      v-model:show="networkOpen"
      preset="card"
      title="新建 Docker 网络"
      style="width: 560px"
      :mask-closable="!networkCreating"
    >
      <div class="dwform">
        <div class="dwform__field">
          <span class="dwform__label">名称</span>
          <n-input v-model:value="networkForm.name" placeholder="app-net" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">驱动</span>
          <n-select v-model:value="networkForm.driver" :options="driverOptions" size="small" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">子网</span>
          <n-input v-model:value="networkForm.subnet" placeholder="留空自动分配，例如 172.20.0.0/16" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label">网关</span>
          <n-input v-model:value="networkForm.gateway" placeholder="留空自动分配，例如 172.20.0.1" />
        </div>
        <div class="dwform__field">
          <span class="dwform__label" />
          <n-checkbox v-model:checked="networkForm.internal">内部网络（不接外网）</n-checkbox>
        </div>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button :disabled="networkCreating" @click="networkOpen = false">取消</n-button>
          <n-button type="primary" :loading="networkCreating" @click="submitNetwork">创建</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 网络详情 -->
    <n-modal
      v-model:show="networkDetailOpen"
      preset="card"
      :title="networkDetail ? `网络 · ${networkDetail.name}` : '网络详情'"
      style="width: 680px"
    >
      <n-spin v-if="networkDetailLoading" :size="20" />
      <div v-else-if="networkDetail" class="dwnet">
        <div class="dwnet__facts">
          <div><span class="dwnet__k">驱动</span><span class="num">{{ networkDetail.driver }}</span></div>
          <div><span class="dwnet__k">范围</span><span class="num">{{ networkDetail.scope }}</span></div>
          <div>
            <span class="dwnet__k">地址池</span>
            <span class="num">
              <template v-if="networkDetail.subnets.length === 0">—</template>
              <template v-for="(subnet, index) in networkDetail.subnets" :key="subnet.subnet">
                {{ index > 0 ? '、' : '' }}{{ subnet.subnet
                }}<template v-if="subnet.gateway">（网关 {{ subnet.gateway }}）</template>
              </template>
            </span>
          </div>
        </div>

        <div class="dwnet__connect">
          <n-select
            v-model:value="connectTarget"
            size="small"
            :options="containerOptions"
            placeholder="选择要接入的容器"
            filterable
          />
          <n-button size="small" type="primary" :disabled="!connectTarget" @click="connectContainer">
            接入
          </n-button>
        </div>

        <ul class="dwnet__list">
          <li v-for="item in networkDetail.containers" :key="item.containerId" class="dwnet__row">
            <span class="dwnet__name num">{{ item.name }}</span>
            <span class="dwnet__ip num">{{ item.ipv4 || '—' }}</span>
            <span class="dwnet__spacer" />
            <n-button size="tiny" quaternary @click="disconnectContainer(item.name)">断开</n-button>
          </li>
          <li v-if="networkDetail.containers.length === 0" class="dwnet__empty">还没有容器接入这个网络</li>
        </ul>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button @click="networkDetailOpen = false">关闭</n-button>
        </n-space>
      </template>
    </n-modal>

    <!-- 一键安装：预检与执行都复用环境管理的 Docker 配方 -->
    <env-plan-dialog
      v-model:show="installPlanOpen"
      :plan="installPlan"
      :loading="installPlanLoading"
      @confirm="startInstall"
    />
    <env-run-dialog
      v-model:show="installRunOpen"
      :host-id="target ?? 0"
      :host-name="hostLabel"
      :request="installRequest"
      @done="onInstallDone"
    />

    <!-- 通用确认 -->
    <n-modal v-model:show="confirmOpen" preset="card" :title="pending?.title" style="width: 480px">
      <p class="dwin__confirm">{{ pending?.summary }}</p>
      <p v-if="pending?.detail" class="dwin__confirm-detail">{{ pending.detail }}</p>

      <template #footer>
        <n-space justify="end">
          <n-button :disabled="acting" @click="pending = null">取消</n-button>
          <n-button type="error" :loading="acting" @click="confirmPending">确认执行</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>

<style scoped>
.dwin {
  display: flex;
  flex-direction: column;
  /* 和其他窗口一致：断点按窗口宽度生效，窄窗口里统计卡与卡片网格才会折行。 */
  container-type: inline-size;
  gap: var(--sp-3);
  height: 100%;
  padding: 12px 14px 14px;
  overflow: hidden;
}

.dwin__bar {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}

.dwin__title {
  display: flex;
  flex: 0 0 auto;
  align-items: center;
  gap: var(--sp-2);
  font-family: var(--font-mono);
  font-size: var(--fs-md);
  letter-spacing: 0.04em;
  color: var(--ink-hi);
}

.dwin__note {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-align: right;
  text-overflow: ellipsis;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
  white-space: nowrap;
}

.dwin__refresh {
  min-width: 62px;
}

.dwin__error,
.dwin__warn {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  border-radius: var(--r-2);
  font-size: var(--fs-sm);
}

.dwin__error {
  border: 1px solid rgba(255, 107, 107, 0.4);
  background: rgba(255, 107, 107, 0.1);
  color: #ffb3b3;
}

.dwin__warn {
  border: 1px solid rgba(255, 176, 46, 0.35);
  background: rgba(255, 176, 46, 0.08);
  color: #ffd28a;
  white-space: pre-wrap;
  word-break: break-all;
}

.dwin__placeholder {
  display: flex;
  flex: 1;
  align-items: center;
  justify-content: center;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
}

.dwin__install {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-module);
}

.dwin__install-note {
  flex: 1;
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
  line-height: 1.6;
}

.dwin__tabs {
  display: flex;
  flex: 1;
  flex-direction: column;
  min-height: 0;
}

.dwin__tabs :deep(.n-tabs-pane-wrapper) {
  flex: 1;
  min-height: 0;
}

.dwin__tabs :deep(.n-tab-pane) {
  display: flex;
  flex-direction: column;
  height: 100%;
  padding-top: var(--sp-3);
  box-sizing: border-box;
}

/* 仪表盘自己滚：卡片高度超过窗口时，滚动条出现在内容区而不是把页头推走。 */
.dwin__dash {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: var(--sp-3);
  min-height: 0;
  overflow: auto;
}

/* 固定两列：四张卡正好 2×2，不随窗口变宽排出 3+1 的孤行。 */
.dwin__dash-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: var(--sp-3);
  align-content: start;
}

/* 与全局 860px 断点同口径：统计卡降到两列时，卡片也折成一列。 */
@container (max-width: 860px) {
  .dwin__dash-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}

/* 同一行两张卡不比高度：内容少的卡不必被拉出大片空白。 */
.dwin__dash-grid > .ui-card {
  align-self: start;
}

.dwin__dash-meter {
  margin-top: 14px;
}

.dwin__dash-warn {
  margin-top: 12px;
  font-size: 10px;
}

.dwin__dash-row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 7px 0;
  border-bottom: 1px solid var(--line-hair);
}

.dwin__dash-row:last-of-type {
  border-bottom: 0;
}

.dwin__dash-name {
  min-width: 0;
  overflow: hidden;
  color: var(--ink);
  font-size: 11px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dwin__dash-status {
  flex: none;
  color: var(--ink-lo);
  font-family: var(--font-mono);
  font-size: 10px;
}

.dwin__dash-image {
  min-width: 0;
  margin-left: auto;
  overflow: hidden;
  color: var(--ink-lo);
  font-size: 10px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.dwin__dash-more {
  margin: 8px 0 0;
  color: var(--ink-lo);
  font-size: 10px;
  line-height: 1.6;
}

.dwin__table {
  flex: 1;
  min-height: 0;
}

.dwin__empty {
  margin: var(--sp-3) 0 0;
  text-align: center;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
}

.dwin__cell-name {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
  overflow: hidden;
}

.dwin__cell-name > .num {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* ---- Compose ---- */

.dwin__compose-bar {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  margin-bottom: var(--sp-2);
}

.dwin__compose-note {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
  white-space: nowrap;
}

.dwin__projects {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: var(--sp-2);
  min-height: 0;
  overflow-y: auto;
  padding-right: var(--sp-1);
}

.dwin__project {
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-module);
}

.dwin__project-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.dwin__project-name {
  font-size: var(--fs-md);
  color: var(--ink-hi);
}

.dwin__spacer {
  flex: 1;
}

.dwin__project-files,
.dwin__project-containers {
  overflow: hidden;
  text-overflow: ellipsis;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
  white-space: nowrap;
}

.dwin__project-files {
  margin-top: var(--sp-1);
}

.dwin__project-containers {
  margin-top: 2px;
  color: var(--ink-ghost);
}

/* ---- 镜像仓库 ---- */

.dwin__search {
  display: flex;
  gap: var(--sp-2);
}

.dwin__registry-note {
  margin: var(--sp-2) 0;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

.dwin__hits {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: var(--sp-2);
  min-height: 0;
  margin: 0;
  padding: 0 var(--sp-1) 0 0;
  overflow-y: auto;
  list-style: none;
}

.dwin__hit {
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-module);
}

.dwin__hit-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.dwin__hit-name {
  font-size: var(--fs-md);
  color: var(--ink-hi);
}

.dwin__hit-stars {
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

.dwin__hit-desc {
  margin: var(--sp-1) 0 0;
  font-size: var(--fs-xs);
  color: var(--ink);
  line-height: 1.5;
}

/* ---- 弹窗表单 ---- */

.dwform {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

.dwform__field {
  display: grid;
  grid-template-columns: 82px 1fr;
  align-items: start;
  gap: var(--sp-2);
}

/* 路径输入与"选择"按钮同排：按钮跟着输入框走，不要把长路径挤成两行。 */
.dwform__path {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  min-width: 0;
}

.dwform__path-input {
  flex: 1;
  min-width: 0;
}

/* 多行说明跟着输入框排：textarea 与提示文字属于同一个字段。 */
.dwform__stack {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
  min-width: 0;
}

.dwform__label {
  padding-top: 5px;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--ink-lo);
}

.dwform__tip {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
  line-height: 1.6;
}

.dwform :deep(.dwform__yaml) {
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  line-height: 1.6;
}

/* ---- 容器内文件 ---- */

.dwfiles {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  min-height: 340px;
  max-height: 62vh;
}

.dwfiles__bar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.dwfiles__path {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  font-size: var(--fs-sm);
  color: var(--ink);
  white-space: nowrap;
}

.dwfiles__pending {
  display: flex;
  flex: 1;
  align-items: center;
  justify-content: center;
  padding: 40px;
}

.dwfiles__list {
  display: flex;
  flex: 1;
  flex-direction: column;
  gap: 1px;
  min-height: 0;
  margin: 0;
  padding: 0;
  overflow-y: auto;
  list-style: none;
}

.dwfiles__row {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: 5px var(--sp-2);
  border-radius: var(--r-1);
  font-size: var(--fs-sm);
  cursor: pointer;
}

.dwfiles__row:hover {
  background: rgba(255, 255, 255, 0.05);
}

.dwfiles__icon {
  flex: 0 0 auto;
  width: 14px;
  height: 14px;
  color: var(--ink-lo);
}

.dwfiles__name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  color: var(--ink);
  white-space: nowrap;
}

.dwfiles__size {
  flex: 0 0 auto;
  padding-left: var(--sp-2);
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

.dwfiles__file-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-sm);
  color: var(--ink);
  word-break: break-all;
}

.dwfiles__content {
  flex: 1;
  min-height: 0;
  max-height: 55vh;
  margin: 0;
  padding: var(--sp-3);
  overflow: auto;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  line-height: 1.55;
  color: var(--ink);
  white-space: pre-wrap;
  word-break: break-all;
}

.dwfiles__empty {
  padding: 32px;
  text-align: center;
  font-size: var(--fs-sm);
  color: var(--ink-lo);
}

.dwfiles__hint {
  margin: 0;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

/* ---- 日志 ---- */

.dwin__logs-wrap {
  display: flex;
  justify-content: center;
  max-height: 60vh;
  overflow: hidden;
}

.dwin__logs {
  width: 100%;
  max-height: 60vh;
  margin: 0;
  padding: var(--sp-3);
  overflow: auto;
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-sunken);
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  line-height: 1.55;
  color: var(--ink);
  white-space: pre-wrap;
  word-break: break-all;
}

/* ---- 网络详情 ---- */

.dwnet {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}

.dwnet__facts {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
  font-size: var(--fs-sm);
  color: var(--ink);
}

.dwnet__facts > div {
  display: flex;
  gap: var(--sp-3);
}

.dwnet__k {
  flex: 0 0 60px;
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--ink-lo);
}

.dwnet__connect {
  display: flex;
  gap: var(--sp-2);
}

.dwnet__list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 280px;
  margin: 0;
  padding: 0;
  overflow-y: auto;
  list-style: none;
}

.dwnet__row {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  padding: var(--sp-1) var(--sp-2);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-1);
  background: var(--bg-module);
  font-size: var(--fs-sm);
}

.dwnet__name {
  color: var(--ink-hi);
}

.dwnet__ip {
  color: var(--ink-lo);
}

.dwnet__spacer {
  flex: 1;
}

.dwnet__empty {
  padding: var(--sp-2);
  color: var(--ink-lo);
  font-size: var(--fs-sm);
}

/* ---- 确认 ---- */

.dwin__confirm {
  margin: 0;
  font-size: var(--fs-md);
  color: var(--ink-hi);
  line-height: 1.6;
}

.dwin__confirm-detail {
  margin: var(--sp-2) 0 0;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
  line-height: 1.6;
  word-break: break-all;
}
</style>
