<script setup lang="ts">
/**
 * 系统设置窗口：目标机常用系统参数的预览页。
 *
 * 这一版只做界面与交互骨架，不读写目标机：所有数值都是演示数据，
 * 控件可改但只落在本窗口的草稿里，「应用变更」保持禁用。
 * 这样先把"有哪些设置、分几类、每项写到哪里"定下来，
 * 后续接后端时只需要把 draft 的读写换成真实接口。
 */
import { computed, ref, watch } from 'vue';
import {
  NButton,
  NEmpty,
  NInput,
  NInputNumber,
  NModal,
  NSelect,
  NSwitch,
  useMessage,
  type SelectOption,
} from 'naive-ui';

import HostPicker from '../components/HostPicker.vue';
import { copyText, triggerDownload } from '../api/client';
import { createEd25519Key, demoPrivateKey, type SshKey } from './sshKeys';

type Kind = 'readonly' | 'text' | 'select' | 'switch' | 'number' | 'action';
type SettingValue = string | number | boolean;

interface SettingItem {
  key: string;
  label: string;
  desc?: string;
  kind: Kind;
  value: SettingValue;
  /** number 类型的单位，显示在输入框右侧。 */
  unit?: string;
  options?: SelectOption[];
  /** 这项设置最终落在哪个文件或命令上，让用户知道改的是什么东西。 */
  source?: string;
  /** 修改需要 root 权限。 */
  root?: boolean;
}

interface SettingGroup {
  key: string;
  name: string;
  desc: string;
  items: SettingItem[];
}

/**
 * 常用系统设置项。
 *
 * 分类顺序按"看一眼 → 改身份 → 调网络 → 管远程入口 → 加防护 → 调内核 → 管更新"
 * 排：越靠前越常看，越靠后越少动、改错代价越大。
 */
const groups: SettingGroup[] = [
  {
    key: 'system',
    name: '系统信息',
    desc: '只读概览。接入目标机后由 uname、os-release 与 timedatectl 实时读取。',
    items: [
      { key: 'hostname', label: '主机名', desc: '内核与网络中的本机名称，也会出现在 shell 提示符里。', kind: 'readonly', value: 'web-01' },
      { key: 'os', label: '操作系统', kind: 'readonly', value: 'Debian GNU/Linux 12 (bookworm)', source: '/etc/os-release' },
      { key: 'kernel', label: '内核版本', kind: 'readonly', value: '6.1.0-23-amd64', source: 'uname -r' },
      { key: 'arch', label: '系统架构', kind: 'readonly', value: 'x86_64' },
      { key: 'uptime', label: '已运行', kind: 'readonly', value: '12 天 4 小时' },
      { key: 'clock', label: '当前时间', kind: 'readonly', value: '2026-11-10 18:42:07 CST' },
    ],
  },
  {
    key: 'identity',
    name: '主机与时间',
    desc: '主机名、时区与本地化参数。修改通常需要 root，并影响所有登录会话。',
    items: [
      { key: 'hostname', label: '主机名', kind: 'text', value: 'web-01', source: '/etc/hostname', root: true },
      {
        key: 'timezone',
        label: '时区',
        kind: 'select',
        value: 'Asia/Shanghai',
        source: '/etc/timezone',
        root: true,
        options: [
          { label: 'Asia/Shanghai（CST +08:00）', value: 'Asia/Shanghai' },
          { label: 'Asia/Tokyo（JST +09:00）', value: 'Asia/Tokyo' },
          { label: 'UTC（+00:00）', value: 'UTC' },
          { label: 'Europe/London（GMT +00:00）', value: 'Europe/London' },
          { label: 'America/New_York（EST −05:00）', value: 'America/New_York' },
        ],
      },
      {
        key: 'ntp',
        label: '网络时间同步',
        desc: '通过 systemd-timesyncd 与上游服务器校准系统时钟。',
        kind: 'switch',
        value: true,
        source: 'timedatectl set-ntp',
        root: true,
      },
      { key: 'ntpServer', label: 'NTP 服务器', kind: 'text', value: 'ntp.aliyun.com', source: '/etc/systemd/timesyncd.conf', root: true },
      {
        key: 'locale',
        label: '系统语言',
        kind: 'select',
        value: 'zh_CN.UTF-8',
        source: '/etc/default/locale',
        root: true,
        options: [
          { label: '简体中文（zh_CN.UTF-8）', value: 'zh_CN.UTF-8' },
          { label: 'English（en_US.UTF-8）', value: 'en_US.UTF-8' },
          { label: '最小环境（C.UTF-8）', value: 'C.UTF-8' },
        ],
      },
    ],
  },
  {
    key: 'network',
    name: '网络与 DNS',
    desc: '解析与协议开关。网卡、路由与网桥请到「网络管理」里调整。',
    items: [
      {
        key: 'dns',
        label: 'DNS 服务器',
        desc: '多个地址用空格分隔。写入后会覆盖 DHCP 下发的解析配置。',
        kind: 'text',
        value: '223.5.5.5 119.29.29.29',
        source: '/etc/resolv.conf',
        root: true,
      },
      { key: 'searchDomain', label: '搜索域', kind: 'text', value: 'lan', source: '/etc/resolv.conf' },
      { key: 'ipv6', label: '启用 IPv6', kind: 'switch', value: true, source: '/etc/sysctl.d/99-panel.conf', root: true },
      {
        key: 'proxy',
        label: '系统代理',
        desc: '供包管理与命令行工具使用的 HTTP 代理，留空表示不使用。',
        kind: 'text',
        value: '',
        source: '/etc/environment',
      },
      { key: 'hosts', label: '静态解析条目', kind: 'readonly', value: '12 条', source: '/etc/hosts' },
    ],
  },
  {
    key: 'ssh',
    name: 'SSH 服务',
    desc: 'sshd 的常用选项。修改后需要重载 sshd；改端口前请先确认防火墙已放行。',
    items: [
      { key: 'sshPort', label: '监听端口', kind: 'number', value: 22, unit: '端口', source: '/etc/ssh/sshd_config', root: true },
      {
        key: 'rootLogin',
        label: '允许 root 直接登录',
        desc: '关闭后需先用普通用户登录再提权，建议保持关闭。',
        kind: 'switch',
        value: false,
        source: 'PermitRootLogin',
        root: true,
      },
      {
        key: 'passwordAuth',
        label: '密码登录',
        desc: '关闭前先确认公钥已就位，否则会把自己锁在门外。',
        kind: 'switch',
        value: true,
        source: 'PasswordAuthentication',
        root: true,
      },
      { key: 'pubkeyAuth', label: '公钥登录', kind: 'switch', value: true, source: 'PubkeyAuthentication', root: true },
      { key: 'maxAuthTries', label: '最大认证次数', kind: 'number', value: 6, unit: '次', source: 'MaxAuthTries' },
      { key: 'idleTimeout', label: '空闲断开', kind: 'number', value: 300, unit: '秒', source: 'ClientAliveInterval' },
      {
        key: 'sshPassword',
        label: '登录密码',
        desc: '修改当前 SSH 登录用户的密码。预览页只记录"待修改"，不会写入目标机。',
        kind: 'action',
        value: '',
        source: 'passwd',
        root: true,
      },
      { key: 'sshdStatus', label: '服务状态', kind: 'readonly', value: 'active (running)' },
    ],
  },
  {
    key: 'security',
    name: '安全与防火墙',
    desc: '主机侧的基础防护。这些项目都需要 root，误改可能把自己锁在门外。',
    items: [
      {
        key: 'firewall',
        label: '防火墙',
        desc: '使用 ufw / firewalld 的默认策略拦截未放行的端口。',
        kind: 'switch',
        value: true,
        source: 'ufw status',
        root: true,
      },
      {
        key: 'apparmor',
        label: '访问控制',
        kind: 'select',
        value: 'enforce',
        source: 'aa-status',
        root: true,
        options: [
          { label: '强制（enforce）', value: 'enforce' },
          { label: '仅告警（complain）', value: 'complain' },
          { label: '关闭', value: 'disabled' },
        ],
      },
      {
        key: 'fail2ban',
        label: '登录失败封禁',
        desc: '连续失败达到阈值后临时封禁来源 IP。',
        kind: 'switch',
        value: true,
        source: 'fail2ban',
        root: true,
      },
      { key: 'lockout', label: '失败锁定阈值', kind: 'number', value: 5, unit: '次', root: true },
      { key: 'auditLog', label: '审计日志', kind: 'switch', value: true, source: 'auditd', root: true },
    ],
  },
  {
    key: 'kernel',
    name: '内核与性能',
    desc: 'sysctl 参数，写入 /etc/sysctl.d/99-panel.conf，重启后仍保持。',
    items: [
      {
        key: 'swappiness',
        label: '内存交换倾向',
        desc: '取值 0–100，越大越倾向于把匿名页换出到 swap。',
        kind: 'number',
        value: 60,
        root: true,
        source: 'vm.swappiness',
      },
      { key: 'fileMax', label: '最大文件句柄', kind: 'number', value: 1048576, root: true, source: 'fs.file-max' },
      { key: 'somaxconn', label: '连接队列上限', kind: 'number', value: 4096, root: true, source: 'net.core.somaxconn' },
      { key: 'pidMax', label: '最大进程数', kind: 'number', value: 4194304, root: true, source: 'kernel.pid_max' },
      {
        key: 'thp',
        label: '透明大页',
        kind: 'select',
        value: 'madvise',
        root: true,
        source: '/sys/kernel/mm/transparent_hugepage',
        options: [
          { label: 'always', value: 'always' },
          { label: 'madvise', value: 'madvise' },
          { label: 'never', value: 'never' },
        ],
      },
    ],
  },
  {
    key: 'update',
    name: '启动与更新',
    desc: '默认启动目标与软件包更新策略。批量升级建议先到「环境管理」看预检结果。',
    items: [
      {
        key: 'defaultTarget',
        label: '默认启动目标',
        kind: 'select',
        value: 'multi-user.target',
        source: 'systemctl set-default',
        root: true,
        options: [
          { label: '命令行（multi-user.target）', value: 'multi-user.target' },
          { label: '图形界面（graphical.target）', value: 'graphical.target' },
        ],
      },
      { key: 'autoUpdate', label: '自动安装安全更新', kind: 'switch', value: true, source: 'unattended-upgrades', root: true },
      { key: 'kernelUpdate', label: '自动升级内核', kind: 'switch', value: false, root: true },
      { key: 'autoClean', label: '自动清理旧包', kind: 'switch', value: true, root: true },
      {
        key: 'mirror',
        label: '软件源',
        kind: 'select',
        value: 'tuna',
        source: '/etc/apt/sources.list',
        root: true,
        options: [
          { label: '清华 TUNA', value: 'tuna' },
          { label: '阿里云', value: 'aliyun' },
          { label: '中科大 USTC', value: 'ustc' },
          { label: '发行版官方', value: 'official' },
        ],
      },
    ],
  },
];

const message = useMessage();

type SshAuth = 'password' | 'key';

interface SshUser {
  name: string;
  role: string;
  /** 最近一次登录时间；从未登录时直接给说明文字。 */
  lastLogin: string;
  auth: SshAuth;
  key: SshKey | null;
}

/** deploy 已有一把密钥，另外两个用户为空：让"创建密钥"两种状态都能看到。 */
const deployKey: SshKey = {
  publicKey: 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIL9m4kQ2vXb3n8pQ1sT7yR5wK6cJ0dF2gH4jL8mN0pQ deploy@web-01',
  fingerprint: 'SHA256:5mJ8kQ2vXb3n8pQ1sT7yR5wK6cJ0dF2gH4jL8mN0pQ',
  note: 'deploy@web-01',
  createdAt: '2026-11-13 22:38:10',
};

/** 访问管理的演示数据：允许登录的用户与各自认证方式。 */
const sshUserDefaults: SshUser[] = [
  { name: 'root', role: '超级用户', lastLogin: '2026-11-14 09:12', auth: 'password', key: null },
  { name: 'deploy', role: '部署账户', lastLogin: '2026-11-13 22:40', auth: 'key', key: deployKey },
  { name: 'www-data', role: '服务账户', lastLogin: '从未登录', auth: 'password', key: null },
];

const sshUsers = ref<SshUser[]>(sshUserDefaults.map((user) => ({ ...user })));

const authOptions: SelectOption[] = [
  { label: '密码认证', value: 'password' },
  { label: '密钥认证', value: 'key' },
];

/** 密码修改只记一个"改过了"的标记，明文不进草稿，也不留在组件里。 */
const passwordDialog = ref(false);
const newPassword = ref('');
const confirmPassword = ref('');
const passwordChanged = ref(false);

/** 创建密钥弹窗：一次只针对一个用户。 */
const keyDialog = ref(false);
const keyUser = ref<SshUser | null>(null);
const keyNote = ref('');
const generatedKey = ref<SshKey | null>(null);
const privateKeyDownloaded = ref(false);

const sshUserDirty = computed(
  () =>
    sshUsers.value.filter((user, index) => {
      const base = sshUserDefaults[index];
      return user.auth !== base.auth || (user.key?.fingerprint ?? '') !== (base.key?.fingerprint ?? '');
    }).length,
);

const hostId = ref<number | null>(null);
const hostName = ref('');
const query = ref('');
const activeKey = ref(groups[0].key);

/** 每项设置的初始预览值，用于判断"改过没有"与恢复默认。 */
const defaults = new Map<string, SettingValue>();
for (const group of groups) {
  for (const item of group.items) defaults.set(item.key, item.value);
}

/**
 * 草稿值。
 *
 * 键是设置项的 key：同一项在「系统信息」里是只读展示、在「主机与时间」里可编辑，
 * 两处共用同一个键，改完之后概览页也能跟着变 —— 这是预览页最值得演示的部分。
 */
const draft = ref<Record<string, SettingValue>>({});

/** 一次性还原所有草稿：普通项、SSH 用户认证方式、密码修改标记。 */
function resetChanges(): void {
  draft.value = Object.fromEntries(defaults) as Record<string, SettingValue>;
  sshUsers.value = sshUserDefaults.map((user) => ({ ...user }));
  passwordChanged.value = false;
}

resetChanges();

// 换主机等于换一台机器，上一台的草稿不能带过去。
watch(hostId, resetChanges);

const activeGroup = computed(() => groups.find((group) => group.key === activeKey.value) ?? groups[0]);

/** 有可编辑版本的同名项：搜索时只保留那一份，避免"主机名"这类项出现两条。 */
const editableKeys = new Set(
  groups.flatMap((group) =>
    group.items.filter((item) => item.kind !== 'readonly').map((item) => item.key),
  ),
);

interface PaneRow {
  item: SettingItem;
  /** 搜索结果里要标出这一项属于哪一类；分类视图下不显示。 */
  group: SettingGroup;
}

const searching = computed(() => query.value.trim().length > 0);

/**
 * 全局搜索。
 *
 * 只搜当前分类会让用户以为搜索坏了 —— 输入 DNS 时必须先手动切到「网络与 DNS」。
 * 这里跨全部分类检索，并在结果行上标出分类名。
 */
const searchHits = computed<PaneRow[]>(() => {
  const q = query.value.trim().toLowerCase();
  if (!q) return [];
  return groups.flatMap((group) =>
    group.items
      .filter((item) => !(item.kind === 'readonly' && editableKeys.has(item.key)))
      .filter((item) => `${item.label} ${item.desc ?? ''} ${item.source ?? ''}`.toLowerCase().includes(q))
      .map((item) => ({ item, group })),
  );
});

const rows = computed<PaneRow[]>(() =>
  searching.value
    ? searchHits.value
    : activeGroup.value.items.map((item) => ({ item, group: activeGroup.value })),
);

/** 搜索结果覆盖了几个分类。头部副标题要用它。 */
const hitGroupCount = computed(() => new Set(searchHits.value.map((row) => row.group.key)).size);

/** 分类导航上的计数：搜索时显示命中数，让用户知道哪一类还有结果。 */
function navCount(group: SettingGroup): number {
  if (!searching.value) return group.items.length;
  return searchHits.value.filter((row) => row.group.key === group.key).length;
}

function selectGroup(key: string): void {
  activeKey.value = key;
  // 带着关键字切分类，右侧仍是搜索结果，看起来像"点了没反应"。
  query.value = '';
}

/** 系统信息这一类是纯只读概览，用双列卡片呈现比一行一条更紧凑。 */
const isOverview = computed(() => !searching.value && activeGroup.value.key === 'system');

const totalItems = computed(() => groups.reduce((sum, group) => sum + group.items.length, 0));

/** 有改动的项数。预览页不会写入目标机，这个数字只用来提示"还有没落地的调整"。 */
const modifiedCount = computed(
  () =>
    Object.keys(draft.value).filter((key) => draft.value[key] !== defaults.get(key)).length +
    sshUserDirty.value +
    (passwordChanged.value ? 1 : 0),
);

/** 每个分类里未应用的改动数，用来在导航上点灯。 */
function dirtyCount(group: SettingGroup): number {
  let count = group.items.filter(
    (item) => item.kind !== 'readonly' && item.kind !== 'action' && draft.value[item.key] !== defaults.get(item.key),
  ).length;
  // SSH 分类还有访问管理（用户认证方式）与密码修改，它们不在 items 里。
  if (group.key === 'ssh') count += sshUserDirty.value + (passwordChanged.value ? 1 : 0);
  return count;
}

const footHint = computed(() =>
  modifiedCount.value > 0
    ? `有 ${modifiedCount.value} 项修改未应用（预览页不会写入目标机）`
    : '预览模式：变更不会写入目标机',
);

function asText(key: string): string {
  const value = draft.value[key];
  return value === undefined ? '' : String(value);
}

function asBool(key: string): boolean {
  return draft.value[key] === true;
}

function asNumber(key: string): number {
  const value = draft.value[key];
  return typeof value === 'number' ? value : Number(value) || 0;
}

function setText(key: string, value: string): void {
  draft.value[key] = value;
}

function setBool(key: string, value: boolean): void {
  draft.value[key] = value;
}

function setNumber(key: string, value: number | null): void {
  draft.value[key] = value ?? 0;
}

function isModified(item: SettingItem): boolean {
  return draft.value[item.key] !== defaults.get(item.key);
}

/** 只读值直接复制走：排错时经常要贴内核版本、当前时间，不该靠手动框选。 */
async function copyValue(item: SettingItem): Promise<void> {
  const ok = await copyText(asText(item.key));
  if (ok) message.success(`已复制${item.label}`);
  else message.error('复制失败，请手动选择文本');
}

function setUserAuth(name: string, value: string | number | null): void {
  if (value !== 'password' && value !== 'key') return;
  const user = sshUsers.value.find((item) => item.name === name);
  if (user) user.auth = value;
}

/** 这个用户的认证方式或密钥是否被改过；用于行高亮与下拉框描边。 */
function isUserDirty(user: SshUser): boolean {
  const base = sshUserDefaults.find((item) => item.name === user.name);
  if (!base) return false;
  return base.auth !== user.auth || (base.key?.fingerprint ?? '') !== (user.key?.fingerprint ?? '');
}

/** 密钥认证不依赖口令，批量切换是访问管理里最常做的一步。 */
function switchAllToKey(): void {
  const pending = sshUsers.value.filter((user) => user.auth !== 'key').length;
  for (const user of sshUsers.value) user.auth = 'key';
  message.success(pending > 0 ? `已将 ${pending} 个用户切换为密钥认证` : '所有用户已是密钥认证');
}

function openPasswordDialog(): void {
  newPassword.value = '';
  confirmPassword.value = '';
  passwordDialog.value = true;
}

function submitPassword(): void {
  if (newPassword.value.length < 8) {
    message.warning('密码至少 8 位');
    return;
  }
  if (newPassword.value !== confirmPassword.value) {
    message.warning('两次输入的密码不一致');
    return;
  }
  // 明文用完即弃：预览页只记"改过了"，不把口令留在任何状态里。
  passwordChanged.value = true;
  passwordDialog.value = false;
  newPassword.value = '';
  confirmPassword.value = '';
  message.success('新密码已记录，将在接入目标机后写入');
}

function openKeyDialog(user: SshUser): void {
  keyUser.value = user;
  keyNote.value = user.key?.note ?? `panel@${hostName.value || 'host'}`;
  generatedKey.value = user.key ? { ...user.key } : null;
  privateKeyDownloaded.value = false;
  keyDialog.value = true;
}

/** 预览页生成的是演示密钥：公钥结构合法，但不能用于真实登录。 */
async function generateKeyPair(): Promise<void> {
  if (!keyUser.value) return;
  const user = sshUsers.value.find((item) => item.name === keyUser.value?.name);
  if (!user) return;
  const note = keyNote.value.trim() || `panel@${hostName.value || 'host'}`;
  const key = await createEd25519Key(note);
  generatedKey.value = key;
  user.key = key;
  // 创建密钥的目的就是走密钥登录，顺手把该用户切到密钥认证。
  const switched = user.auth !== 'key';
  user.auth = 'key';
  privateKeyDownloaded.value = false;
  message.success(switched ? `已为 ${user.name} 生成密钥并切换为密钥认证` : `已为 ${user.name} 重新生成密钥`);
}

async function copyPublicKey(): Promise<void> {
  if (!generatedKey.value) return;
  const ok = await copyText(generatedKey.value.publicKey);
  if (ok) message.success('公钥已复制');
  else message.error('复制失败，请手动选择');
}

/** 私钥只允许下载一次；文件头明确标注演示占位，避免被当成可用凭据。 */
function downloadPrivateKey(): void {
  const key = generatedKey.value;
  const user = keyUser.value;
  if (!key || !user) return;
  const text = demoPrivateKey(key.publicKey);
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }));
  triggerDownload(url, `id_ed25519_demo_${user.name}`);
  setTimeout(() => URL.revokeObjectURL(url), 1000);
  privateKeyDownloaded.value = true;
  message.success('私钥已下载，预览页不保留私钥');
}
</script>

<template>
  <div class="settings">
    <div class="settings__bar page-bar">
      <div class="page-head">
        <p class="page-eyebrow">SETTINGS</p>
        <span class="page-title">系统设置</span>
      </div>
      <div class="settings__tools">
        <n-input
          v-model:value="query"
          class="settings__search"
          size="small"
          placeholder="搜索全部设置项"
          clearable
          aria-label="搜索设置项"
        />
        <!-- 主机胶囊紧跟搜索框：两者都是"当前在看哪台机器"的上下文，放在一起。 -->
        <HostPicker v-model:host-id="hostId" v-model:host-name="hostName" include-local />
        <span class="settings__mode" title="预览页尚未接入目标机">预览</span>
      </div>
    </div>

    <p class="settings__notice">
      <svg class="settings__notice-icon" viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="8" cy="8" r="6.2" fill="none" stroke="currentColor" stroke-width="1.3" />
        <path d="M8 7.2v3.6M8 5.2v.1" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
      </svg>
      <span><b>预览模式</b>：下列数值为演示数据，可试改，但不会写入目标机。</span>
    </p>

    <div class="settings__body">
      <nav class="settings__nav" aria-label="设置分类">
        <button
          v-for="group in groups"
          :key="group.key"
          type="button"
          class="settings__nav-item"
          :class="{
            'is-active': group.key === activeKey,
            'is-zero': searching && navCount(group) === 0,
          }"
          :aria-current="group.key === activeKey ? 'page' : undefined"
          @click="selectGroup(group.key)"
        >
          <svg class="settings__nav-icon" viewBox="0 0 16 16" aria-hidden="true">
            <g
              v-if="group.key === 'system'"
              fill="none"
              stroke="currentColor"
              stroke-width="1.3"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <rect x="2" y="2.6" width="12" height="8.4" rx="1.2" />
              <path d="M6 13.4h4M8 11v2.4" />
            </g>
            <g
              v-else-if="group.key === 'identity'"
              fill="none"
              stroke="currentColor"
              stroke-width="1.3"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <circle cx="8" cy="8" r="5.6" />
              <path d="M8 4.8V8l2.2 1.5" />
            </g>
            <g
              v-else-if="group.key === 'network'"
              fill="none"
              stroke="currentColor"
              stroke-width="1.3"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <circle cx="8" cy="8" r="5.6" />
              <path d="M2.4 8h11.2" />
              <path d="M8 2.4c1.5 1.6 2.3 3.5 2.3 5.6S9.5 12 8 13.6C6.5 12 5.7 10.1 5.7 8S6.5 4 8 2.4z" />
            </g>
            <g
              v-else-if="group.key === 'ssh'"
              fill="none"
              stroke="currentColor"
              stroke-width="1.3"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <rect x="2" y="3" width="12" height="10" rx="1.4" />
              <path d="M5.2 6.4l1.9 1.6-1.9 1.6M8.6 9.8h2.4" />
            </g>
            <g
              v-else-if="group.key === 'security'"
              fill="none"
              stroke="currentColor"
              stroke-width="1.3"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path d="M8 2.2l4.4 1.7v3.8c0 2.7-1.7 4.8-4.4 5.9-2.7-1.1-4.4-3.2-4.4-5.9V3.9L8 2.2z" />
              <path d="M6.1 7.9l1.4 1.4L10 6.5" />
            </g>
            <g
              v-else-if="group.key === 'kernel'"
              fill="none"
              stroke="currentColor"
              stroke-width="1.3"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <rect x="4.2" y="4.2" width="7.6" height="7.6" rx="1" />
              <path d="M6.2 1.8v2.4M9.8 1.8v2.4M6.2 11.8v2.4M9.8 11.8v2.4M1.8 6.2h2.4M1.8 9.8h2.4M11.8 6.2h2.4M11.8 9.8h2.4" />
            </g>
            <g
              v-else
              fill="none"
              stroke="currentColor"
              stroke-width="1.3"
              stroke-linecap="round"
              stroke-linejoin="round"
            >
              <path d="M8 2.4v6.8M5.2 6.5L8 9.4l2.8-2.9" />
              <path d="M2.6 11.2v1.2a1.2 1.2 0 001.2 1.2h8.4a1.2 1.2 0 001.2-1.2v-1.2" />
            </g>
          </svg>
          <span class="settings__nav-name">{{ group.name }}</span>
          <span
            v-if="dirtyCount(group) > 0"
            class="settings__nav-dirty"
            :title="`${dirtyCount(group)} 项改动未应用`"
          />
          <span class="settings__nav-count num">{{ navCount(group) }}</span>
        </button>
        <div class="settings__nav-foot">
          <span class="num">{{ groups.length }}</span> 类 / <span class="num">{{ totalItems }}</span> 项
        </div>
      </nav>

      <section class="settings__pane">
        <header class="settings__head">
          <h2 class="settings__title">{{ searching ? '搜索结果' : activeGroup.name }}</h2>
          <p class="settings__desc">
            {{ searching ? `在 ${hitGroupCount} 个分类中找到 ${searchHits.length} 项` : activeGroup.desc }}
          </p>
        </header>

        <div class="settings__list" :class="{ 'settings__list--grid': isOverview }">
          <!-- 只读概览：双列卡片，字段名在上、值在下，比一行一条更省纵向空间 -->
          <template v-if="isOverview">
            <article v-for="row in rows" :key="row.item.key" class="ov">
              <span class="ov__label silk">{{ row.item.label }}</span>
              <span class="ov__value num">{{ asText(row.item.key) }}</span>
              <span v-if="row.item.desc" class="ov__desc">{{ row.item.desc }}</span>
              <span v-if="row.item.source" class="ov__source">{{ row.item.source }}</span>
              <button
                type="button"
                class="settings__copy"
                :title="`复制${row.item.label}`"
                :aria-label="`复制${row.item.label}`"
                @click="copyValue(row.item)"
              >
                <svg viewBox="0 0 16 16" aria-hidden="true">
                  <rect
                    x="5.2"
                    y="5.2"
                    width="8"
                    height="8"
                    rx="1.2"
                    fill="none"
                    stroke="currentColor"
                    stroke-width="1.3"
                  />
                  <path
                    d="M10.8 5.2V4a1.2 1.2 0 00-1.2-1.2H4a1.2 1.2 0 00-1.2 1.2v5.6A1.2 1.2 0 004 10.8h1.2"
                    fill="none"
                    stroke="currentColor"
                    stroke-width="1.3"
                    stroke-linecap="round"
                  />
                </svg>
              </button>
            </article>
          </template>

          <template v-else>
            <div
              v-for="row in rows"
              :key="`${row.group.key}:${row.item.key}`"
              class="setting"
              :class="{ 'is-modified': isModified(row.item) || (row.item.kind === 'action' && passwordChanged) }"
            >
              <div class="setting__main">
                <div class="setting__label">
                  <span v-if="searching" class="setting__group">{{ row.group.name }}</span>
                  <span>{{ row.item.label }}</span>
                  <span v-if="row.item.root" class="setting__flag">需 root</span>
                  <span
                    v-if="isModified(row.item) || (row.item.kind === 'action' && passwordChanged)"
                    class="setting__modified"
                  >已修改</span>
                </div>
                <div v-if="row.item.desc" class="setting__desc">{{ row.item.desc }}</div>
                <div v-if="row.item.source" class="setting__source">{{ row.item.source }}</div>
              </div>

              <div class="setting__control">
                <n-button
                  v-if="row.item.kind === 'action'"
                  class="setting__action"
                  size="small"
                  @click="openPasswordDialog"
                >
                  {{ passwordChanged ? '重新设置密码' : '修改密码' }}
                </n-button>

                <span v-else-if="row.item.kind === 'readonly'" class="setting__readonly">
                  <span class="setting__value num">{{ asText(row.item.key) }}</span>
                  <button
                    type="button"
                    class="settings__copy"
                    :title="`复制${row.item.label}`"
                    :aria-label="`复制${row.item.label}`"
                    @click="copyValue(row.item)"
                  >
                    <svg viewBox="0 0 16 16" aria-hidden="true">
                      <rect
                        x="5.2"
                        y="5.2"
                        width="8"
                        height="8"
                        rx="1.2"
                        fill="none"
                        stroke="currentColor"
                        stroke-width="1.3"
                      />
                      <path
                        d="M10.8 5.2V4a1.2 1.2 0 00-1.2-1.2H4a1.2 1.2 0 00-1.2 1.2v5.6A1.2 1.2 0 004 10.8h1.2"
                        fill="none"
                        stroke="currentColor"
                        stroke-width="1.3"
                        stroke-linecap="round"
                      />
                    </svg>
                  </button>
                </span>

                <n-select
                  v-else-if="row.item.kind === 'select'"
                  class="setting__select"
                  size="small"
                  :value="asText(row.item.key)"
                  :options="row.item.options"
                  @update:value="(value: string | number | null) => setText(row.item.key, String(value ?? ''))"
                />

                <n-switch
                  v-else-if="row.item.kind === 'switch'"
                  size="small"
                  :value="asBool(row.item.key)"
                  @update:value="(value: boolean) => setBool(row.item.key, value)"
                />

                <template v-else-if="row.item.kind === 'number'">
                  <n-input-number
                    class="setting__number"
                    size="small"
                    :value="asNumber(row.item.key)"
                    :min="0"
                    :show-button="false"
                    @update:value="(value: number | null) => setNumber(row.item.key, value)"
                  />
                  <span v-if="row.item.unit" class="setting__unit">{{ row.item.unit }}</span>
                </template>

                <n-input
                  v-else
                  class="setting__input"
                  size="small"
                  :value="asText(row.item.key)"
                  :placeholder="row.item.key === 'proxy' ? '留空表示不使用' : ''"
                  @update:value="(value: string) => setText(row.item.key, value)"
                />
              </div>
            </div>
          </template>

          <!-- 访问管理只在 SSH 分类下出现：允许登录的用户与各自认证方式 -->
          <section v-if="!searching && activeGroup.key === 'ssh'" class="access">
            <header class="access__head">
              <div class="access__intro">
                <h3 class="access__title">访问管理</h3>
                <p class="access__desc">
                  控制允许通过 SSH 登录的用户。密钥认证不依赖口令，建议全部切换。
                </p>
              </div>
              <n-button size="tiny" @click="switchAllToKey">全部改为密钥</n-button>
            </header>

            <div
              v-for="user in sshUsers"
              :key="user.name"
              class="access__row"
              :class="{ 'is-dirty': isUserDirty(user) }"
            >
              <span class="access__name num">{{ user.name }}</span>
              <span class="access__role">{{ user.role }}</span>
              <span class="access__last num">{{ user.lastLogin }}</span>
              <span class="access__filler" />
              <n-select
                class="access__auth"
                :class="{ 'is-dirty': isUserDirty(user) }"
                size="small"
                :value="user.auth"
                :options="authOptions"
                @update:value="(value: string | number | null) => setUserAuth(user.name, value)"
              />
              <n-button size="tiny" class="access__key" @click="openKeyDialog(user)">
                {{ user.key ? '查看密钥' : '创建密钥' }}
              </n-button>
            </div>
          </section>

          <n-empty
            v-if="rows.length === 0"
            class="settings__empty"
            :description="searching ? `没有匹配「${query.trim()}」的设置项` : '没有匹配的设置项'"
          />
        </div>

        <footer class="settings__foot">
          <span class="settings__foot-hint">{{ footHint }}</span>
          <span class="settings__spacer" />
          <n-button size="small" :disabled="modifiedCount === 0" @click="resetChanges">还原改动</n-button>
          <n-button size="small" type="primary" disabled title="预览页尚未接入目标机">应用变更</n-button>
        </footer>
      </section>
    </div>

    <n-modal
      v-model:show="passwordDialog"
      preset="card"
      title="修改 SSH 登录密码"
      class="settings__modal"
      style="width: 420px"
    >
      <div class="pw">
        <n-input
          v-model:value="newPassword"
          type="password"
          show-password-on="click"
          placeholder="新密码（至少 8 位）"
        />
        <n-input
          v-model:value="confirmPassword"
          type="password"
          show-password-on="click"
          placeholder="再次输入新密码"
          @keyup.enter="submitPassword"
        />
        <p class="pw__hint">预览模式：密码不会离开本窗口，也不会写入目标机。</p>
      </div>
      <template #footer>
        <div class="pw__foot">
          <n-button size="small" @click="passwordDialog = false">取消</n-button>
          <n-button size="small" type="primary" @click="submitPassword">确认修改</n-button>
        </div>
      </template>
    </n-modal>
    <n-modal
      v-model:show="keyDialog"
      preset="card"
      :title="`SSH 密钥 · ${keyUser?.name ?? ''}`"
      style="width: 520px"
    >
      <div class="keydlg">
        <div class="keydlg__field">
          <span class="keydlg__label silk">密钥类型</span>
          <span class="keydlg__type num">ed25519</span>
        </div>
        <div class="keydlg__field">
          <span class="keydlg__label silk">备注</span>
          <n-input v-model:value="keyNote" size="small" placeholder="例如 panel@web-01" />
        </div>

        <template v-if="generatedKey">
          <div class="keydlg__field">
            <span class="keydlg__label silk">指纹</span>
            <span class="keydlg__fingerprint num">{{ generatedKey.fingerprint }}</span>
          </div>
          <div class="keydlg__field keydlg__field--column">
            <span class="keydlg__label silk">公钥</span>
            <div class="keydlg__public num">{{ generatedKey.publicKey }}</div>
          </div>
          <div class="keydlg__actions">
            <n-button size="tiny" @click="copyPublicKey">复制公钥</n-button>
            <n-button size="tiny" :disabled="privateKeyDownloaded" @click="downloadPrivateKey">
              {{ privateKeyDownloaded ? '私钥已下载' : '下载私钥（演示）' }}
            </n-button>
          </div>
          <p class="keydlg__hint">
            创建于 {{ generatedKey.createdAt }} · 预览页不会写入目标机的 authorized_keys。
          </p>
        </template>
        <p v-else class="keydlg__hint">生成后公钥会展示在这里，私钥只允许下载一次。</p>
      </div>
      <template #footer>
        <div class="pw__foot">
          <n-button size="small" @click="keyDialog = false">关闭</n-button>
          <n-button size="small" type="primary" @click="generateKeyPair">
            {{ generatedKey ? '重新生成' : '生成密钥' }}
          </n-button>
        </div>
      </template>
    </n-modal>
  </div>
</template>
