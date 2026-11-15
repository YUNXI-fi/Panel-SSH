<script lang="ts">
/**
 * 一个接口最近一段时间的收发速率采样。
 *
 * 速率是两次采样的字节差，单点没有意义，要成串才能看出这台机器此刻在传什么，
 * 因此它与 RateFeed.times 一一对应。
 */
export interface RateTrace {
  rx: number[];
  tx: number[];
}

/**
 * 曲线要用的一整套采样数据。
 *
 * 由网络窗口统一持有，而不是让这个面板自己攒：面板不在当前标签页时会被销毁，
 * 历史放在这里，每切回来都要从零开始画。
 */
export interface RateFeed {
  /** 按接口名索引的采样历史。 */
  history: Record<string, RateTrace>;
  /** 每个采样点的时刻（epoch 毫秒），用于识别时间上的缺口。 */
  times: number[];
  /** 相邻两点的间隔超过它就在图上断开（毫秒）。0 表示不判断缺口。 */
  gapMs: number;
  /** 首次采样还没有基准，此时速率不可用，显示占位语而不是把 0 当成没有流量。 */
  deltaReady: boolean;
}
</script>

<script setup lang="ts">
/**
 * OVS 网桥面板。
 *
 * 只在目标机装了 Open vSwitch 时由父组件挂载 —— 这套能力按"有没有 ovs-vsctl"
 * 探测，不按发行版识别，因此任何装了 OVS 的系统都能用。
 *
 * 这里只负责"看现状 + 选动作"，变更本身交给父组件的下发流程：倒计时、
 * 自动确认、回滚横幅那一整套是两种变更共用的，不该在这里重写一遍。
 *
 * 它也只做两件事：把网口挂上、把网口摘下。新建和删除 OVS 网桥不在此列 ——
 * 这些网桥通常是目标机自己的网络服务建的（飞牛的 enp1s0-ovs 就是），
 * 删掉它会连带毁掉那台机器的网络配置。
 *
 * 展示上除了"谁挂在谁身上"，还给每个网口配上地址、吞吐、链路速率、累计流量
 * 与一段速率曲线：桥上跑着几个口、哪个口在忙，是打开这个页签最常见的两件事。
 * 这些数据全部来自父组件已经采到的那份网络快照，按端口名对回接口即可。
 */
import { computed, h, ref } from 'vue';
import {
  NAlert,
  NButton,
  NDropdown,
  NInput,
  NModal,
  NSelect,
  NSpace,
  useDialog,
  type DropdownOption,
} from 'naive-ui';

import Sparkline, { type SparkSeries } from '../components/Sparkline.vue';
import { formatBytes, formatRate } from '../api/format';
import { ledClass, stateLabel } from './netLabels';
import type { NetChangeRequest, NetInterface, OvsBridge, OvsPort, OvsState } from '../api/client';

const props = defineProps<{
  ovs: OvsState;
  interfaces: NetInterface[];
  /** 最近一段时间的速率采样，用于画曲线。 */
  rates: RateFeed;
  /** 默认路由的出口设备名。用来在动手前点出"这个口动了会断连"。 */
  defaultRouteDevs: string[];
  /** 无法提权时的警告文案，由父组件统一给出。 */
  escalationWarning: string;
  /** 会以 root 执行时的提醒文案。 */
  escalationInfo: string;
  /** 有未决变更时禁用全部操作，避免同时下发两次。 */
  disabled: boolean;
}>();

const emit = defineEmits<{ apply: [request: NetChangeRequest]; unpersist: [] }>();

const dialog = useDialog();

/**
 * 读不到 OVS 状态时禁用全部变更操作。
 *
 * 理由不是"看不见就别动"，而是服务端的变更计划本来就基于这份快照：快照里
 * 没有的网桥会被当成"还没建"、没有的端口会被当成"没被占用"。拿一份残缺的
 * 现状去判断能不能动手，比不许动手危险得多。
 *
 * 「卸掉服务」不受影响：它只删目标机上那几个文件，不读 OVS 状态。
 */
const blocked = computed(() => props.disabled || props.ovs.unreadable);

/** 已经被某个 OVS 网桥占用的端口名。 */
const usedPorts = computed(() => {
  const names = new Set<string>();
  for (const bridge of props.ovs.bridges) {
    for (const port of bridge.ports) names.add(port.name);
  }
  return names;
});

/**
 * 可加入 OVS 网桥的候选网口。
 *
 * 排除四类：回环、OVS 网桥自己、已经挂在某个 OVS 网桥上的、以及已经属于
 * Linux 网桥的。最后一条不是洁癖 —— 一个网口同时属于两套桥接体系时，
 * 地址归谁、由谁拉起都说不清，服务端也会拒绝，不如提前从选项里拿掉。
 */
const attachable = computed(() =>
  props.interfaces.filter(
    (iface) =>
      iface.type !== 'loopback' &&
      iface.type !== 'openvswitch' &&
      iface.master === '' &&
      !usedPorts.value.has(iface.name),
  ),
);

function carriesDefaultRoute(name: string): boolean {
  return props.defaultRouteDevs.includes(name);
}

// ---- 卡片展示数据 ----

/** 曲线配色沿用资源监控与主机卡片的那一组：下行青、上行橙。 */
const RX_COLOR = '#45c7c0';
const TX_COLOR = '#ffab2e';

/**
 * 曲线纵轴上限：按窗口内的峰值自适应。
 *
 * 下限留 1 KB/s 而不是 0：空载时的噪声只有几百字节，不兜住的话整张图会被
 * 这点动静放大成一片起伏，看着像在满跑。
 */
function peakOf(...series: number[][]): number {
  const values = series.flat().filter((value) => Number.isFinite(value));
  return Math.max(1024, ...values) * 1.25;
}

/**
 * 多条曲线逐点相加。
 *
 * 长度不一致时按右端对齐相加 —— 每条曲线的右端都是此刻，与 Sparkline 内部的
 * 对齐规则一致；左对齐会让汇总曲线比实际情况晚一截。
 */
function sumTraces(traces: (RateTrace | null)[], key: 'rx' | 'tx'): number[] {
  const length = Math.max(0, ...traces.map((trace) => trace?.[key].length ?? 0));
  const values: number[] = [];
  for (let index = 0; index < length; index += 1) {
    let total = 0;
    let known = false;
    for (const trace of traces) {
      const series = trace?.[key];
      const value = series === undefined ? NaN : series[series.length - length + index];
      if (Number.isFinite(value)) {
        total += value;
        known = true;
      }
    }
    // 全都没有数据就给 NaN：曲线在这里断开，而不是画成 0 拉出一条贴底的直线。
    values.push(known ? total : NaN);
  }
  return values;
}

/** 横轴时刻按序列长度从右端切一段，与曲线的右对齐一致。 */
function timesFor(values: number[]): number[] {
  const times = props.rates.times;
  return times.length > values.length ? times.slice(times.length - values.length) : times;
}

function seriesOf(rx: number[], tx: number[]): SparkSeries[] {
  const times = timesFor(rx);
  return [
    { values: rx, color: RX_COLOR, times },
    { values: tx, color: TX_COLOR, times },
  ];
}

/** 实时吞吐文案。首次采样还没有基准，给占位语而不是把 0 当成真的没有流量。 */
function throughput(value: number): string {
  return props.rates.deltaReady ? formatRate(value) : '采样中…';
}

function addrTexts(iface: NetInterface | null): string[] {
  return (iface?.addrs ?? []).map((addr) => `${addr.addr}/${addr.prefix}`);
}

/**
 * 把地址按族拆开。
 *
 * IPv4 与 IPv6 混在一串里时，想确认"这台机器的 IPv4 是多少"得在一堆冒号里翻，
 * 分成两组之后一眼定位。含冒号即 IPv6 —— 这里拿到的已经是拼好的字符串，
 * 比回头去翻 NetAddress 的字段省事。
 */
function splitAddrs(iface: NetInterface | null): { v4: string[]; v6: string[] } {
  const texts = addrTexts(iface);
  return {
    v4: texts.filter((text) => !text.includes(':')),
    v6: texts.filter((text) => text.includes(':')),
  };
}

/** 一个端口行。 */
interface PortView {
  port: OvsPort;
  iface: NetInterface | null;
  addrs: string[];
  series: SparkSeries[];
  max: number;
  /**
   * 链路状态与它的指示灯配色。
   *
   * 与网络列表、拓扑图共用 netLabels 里那一套说法：同一个网口在这里显示"无载波"、
   * 在别处显示"未连接"，用户会以为是两件事。读不到接口时给空串，整块状态不出现。
   */
  status: string;
  led: string;
}

/** 一张网桥卡片。 */
interface CardView {
  bridge: OvsBridge;
  /** 网桥自己的内核接口 —— 桥接之后地址落在它身上，这张卡片的地址看这里。 */
  addrsV4: string[];
  addrsV6: string[];
  /** 网桥接口的链路状态与灯色，跟着它的内核接口走；读不到接口时为空串。 */
  status: string;
  led: string;
  /**
   * 网桥自身接口的实时吞吐，读不到接口时为 null。
   *
   * 它与"物理口合计"不是一回事：OVS 的数据转发走端口网卡本身，网桥内部口只统计
   * 送进本机协议栈的那部分。两个数字并排是为了区分"过境的流量"与"真正落地的流量"。
   */
  selfRates: { rx: number; tx: number } | null;
  /**
   * 收起后表头要保留的读数。
   *
   * 收起是为了网桥多的时候一屏能扫完，但"这座桥现在跑多少"恰恰是扫视时最要看的一件事 ——
   * 表头不带读数，收起就等于把信息清零，用户还得展开看一眼再收回去。取的就是曲线上那条合计：
   * 有物理口时是物理口合计，否则退回网桥自身。
   */
  headRate: { rx: number; tx: number } | null;
  ports: PortView[];
  /** 有物理口才有合计值：没有物理口的网桥（br-int 这类）合计恒为 0，写出来是误导。 */
  hasPhysical: boolean;
  series: SparkSeries[];
  max: number;
  rxRate: number;
  txRate: number;
  rxBytes: number;
  txBytes: number;
}

/** 接口名 -> 接口，免得模板里每渲染一行都去遍历整张接口表。 */
const byName = computed(() => {
  const map = new Map<string, NetInterface>();
  for (const iface of props.interfaces) map.set(iface.name, iface);
  return map;
});

/**
 * 卡片与端口行的展示数据。
 *
 * 合计只算**物理端口**：OVS 的数据转发走端口网卡本身，网桥内部口（br0）的计数
 * 只包含送进本机协议栈的那部分 —— 把它一并加上去，宿主自己收发的包就被算了两遍。
 */
const cards = computed<CardView[]>(() =>
  props.ovs.bridges.map((bridge) => {
    const ports: PortView[] = bridge.ports.map((port) => {
      const iface = byName.value.get(port.name) ?? null;
      const trace = props.rates.history[port.name] ?? null;
      const rx = trace?.rx ?? [];
      const tx = trace?.tx ?? [];
      return {
        port,
        iface,
        addrs: addrTexts(iface),
        series: seriesOf(rx, tx),
        max: peakOf(rx, tx),
        status: iface === null ? '' : stateLabel(iface),
        led: iface === null ? '' : ledClass(iface),
      };
    });

    const physical = ports.filter((view) => view.port.physical && view.iface !== null);
    const aggRx = sumTraces(physical.map((view) => props.rates.history[view.port.name] ?? null), 'rx');
    const aggTx = sumTraces(physical.map((view) => props.rates.history[view.port.name] ?? null), 'tx');
    const sumOf = (pick: (iface: NetInterface) => number): number =>
      physical.reduce((total, view) => total + (view.iface === null ? 0 : pick(view.iface)), 0);
    const self = byName.value.get(bridge.name) ?? null;
    const selfAddrs = splitAddrs(self);
    const hasPhysical = physical.length > 0;
    const rxRate = sumOf((iface) => iface.rxRate);
    const txRate = sumOf((iface) => iface.txRate);
    const selfRate = self === null ? null : { rx: self.rxRate, tx: self.txRate };

    return {
      bridge,
      addrsV4: selfAddrs.v4,
      addrsV6: selfAddrs.v6,
      status: self === null ? '' : stateLabel(self),
      led: self === null ? '' : ledClass(self),
      selfRates: selfRate,
      // 曲线画的就是物理口合计，所以表头收起后保留的读数与它取同一个值；
      // 没有物理口时退回网桥自身 —— 那种情况下那是这条线上唯一有意义的数。
      headRate: hasPhysical ? { rx: rxRate, tx: txRate } : selfRate,
      ports,
      hasPhysical,
      series: seriesOf(aggRx, aggTx),
      max: peakOf(aggRx, aggTx),
      rxRate,
      txRate,
      rxBytes: sumOf((iface) => iface.rxBytes),
      txBytes: sumOf((iface) => iface.txBytes),
    };
  }),
);

/**
 * 收起状态按网桥名记，而不是按下标。
 *
 * 卡片是列表渲染的，目标机上增删一座桥就会让下标整体错位 —— 收到名字上，
 * "我收起的那座桥"才不会在下一轮采集后变成另一座。
 */
const collapsedNames = ref<Set<string>>(new Set());

function isCollapsed(name: string): boolean {
  return collapsedNames.value.has(name);
}

/** 默认全部展开：打开这个页签的第一件事是看现状，不是看一排折叠起来的标题。 */
function toggleCollapse(name: string): void {
  const next = new Set(collapsedNames.value);
  if (!next.delete(name)) next.add(name);
  collapsedNames.value = next;
}

/**
 * 网桥卡片右上角菜单的选项。
 *
 * 把动作收进菜单是为了把标题行还给网桥名与标签：按钮并排摆开时，
 * 最先被挤掉的总是名字。
 */
function cardMenuOptions(card: CardView): DropdownOption[] {
  const options: DropdownOption[] = [
    {
      label: '加入网口…',
      key: 'attach',
      // 没有可加入的网口时只禁用这一项，而不是禁用整个菜单：菜单里还有删除入口，
      // 整块禁掉会让那个入口也跟着打不开。
      disabled: attachable.value.length === 0,
    },
  ];
  // 只有面板建的桥才给删除入口：目标机自己的网络服务建的桥删掉会连带毁掉
  // 那台机器的网络配置，服务端也会拒绝。
  if (card.bridge.created) {
    options.push({
      label: '删除网桥…',
      key: 'delete',
      // 破坏性动作在菜单里也标红：菜单刚展开的那一刻就该看出哪个不能乱点。
      props: { style: 'color: var(--danger)' },
    });
  }
  return options;
}

function onCardMenu(key: string | number, card: CardView): void {
  if (key === 'attach') {
    openAttach(card.bridge);
    return;
  }
  askDeleteBridge(card.bridge);
}

const attachOptions = computed(() =>
  attachable.value.map((iface) => ({
    label: carriesDefaultRoute(iface.name) ? `${iface.name}（当前默认路由出口）` : iface.name,
    value: iface.name,
  })),
);

// ---- 加入网口 ----

const attachShow = ref(false);
const attachBridge = ref<OvsBridge | null>(null);
const attachMembers = ref<string[]>([]);

function openAttach(bridge: OvsBridge): void {
  attachBridge.value = bridge;
  attachMembers.value = [];
  attachShow.value = true;
}

const attachTitle = computed(() => `把网口加入 OVS 网桥 ${attachBridge.value?.name ?? ''}`);

/**
 * 把当前默认路由出口挂进网桥的风险提示。
 *
 * 这是这个面板里最容易把用户关在门外的一步：网口进了网桥之后地址会移到
 * 网桥上，面板自己那条 SSH 就断了。不能拦住他（有人恰恰就是要这么改），
 * 但必须在按下按钮之前说清楚，并且让他知道断了也会在窗口结束后自动恢复。
 */
function defaultRouteWarning(members: string[]): string {
  const risky = members.filter(carriesDefaultRoute);
  if (risky.length === 0) return '';
  return `${risky.join('、')} 是当前的默认路由出口，这一步会让地址转移到网桥上，面板与它的连接可能中断。若一直连不回来，确认窗口结束后目标机会自动恢复到变更前的状态。`;
}

const attachRisk = computed(() => defaultRouteWarning(attachMembers.value));

function submitAttach(): void {
  const bridge = attachBridge.value;
  if (bridge === null || attachMembers.value.length === 0) return;
  emit('apply', {
    action: 'ovs-attach',
    bridge: bridge.name,
    members: [...attachMembers.value],
  });
  attachShow.value = false;
}

// ---- 新建网桥 ----

const createShow = ref(false);
const createName = ref('');
const createMembers = ref<string[]>([]);

function openCreate(): void {
  createName.value = '';
  createMembers.value = [];
  createShow.value = true;
}

const createRisk = computed(() => defaultRouteWarning(createMembers.value));

function submitCreate(): void {
  const name = createName.value.trim();
  if (name === '') return;
  emit('apply', {
    action: 'ovs-bridge-create',
    bridge: name,
    members: [...createMembers.value],
  });
  createShow.value = false;
}

/**
 * 删除网桥要先确认。
 *
 * del-br 会连同端口一起释放，被桥接的网口会变回独立网卡 —— 如果那正是面板
 * 自己走的链路，点下去当场断连。
 */
function askDeleteBridge(bridge: OvsBridge): void {
  const lines = [`将删除 OVS 网桥 ${bridge.name}，它上面挂着的网口会变回独立网卡。`];
  if (bridge.carriesDefaultRoute) {
    lines.push('注意：这台机器的默认路由出口就是这个网桥，删除它会立刻切断面板与目标机的连接。');
  }
  lines.push('若一直连不回来，确认窗口结束后目标机会自动恢复到变更前的状态。');
  const escalationText = props.escalationWarning || props.escalationInfo;
  if (escalationText) lines.push(escalationText);

  dialog.warning({
    title: '删除 OVS 网桥',
    content: () => h('div', { style: 'white-space: pre-wrap' }, lines.join('\n')),
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: () => emit('apply', { action: 'ovs-bridge-delete', bridge: bridge.name }),
  });
}

/** 卸掉开机自恢复要先确认：它删的是目标机上的服务与状态文件，且重启后不再自动补口。 */
function askUnpersist(): void {
  dialog.warning({
    title: '卸掉开机自恢复服务',
    content: () =>
      h(
        'div',
        { style: 'white-space: pre-wrap' },
        [
          '将从目标机上删除 panelssh-ovs.service、它的执行脚本，以及保存的期望状态。',
          '已经挂上去的网口不受影响，但目标机重启后不会再自动补回来。',
          '之后只要再做一次 OVS 桥接变更并确认，服务会被重新装上。',
        ].join('\n'),
      ),
    positiveText: '卸掉',
    negativeText: '取消',
    onPositiveClick: () => emit('unpersist'),
  });
}

// ---- 移出网口 ----

/**
 * 移出网口要先确认。
 *
 * 摘掉承载默认路由的那个口，等于当场把自己的连接掐断；这一步虽然也会在
 * 确认窗口结束后自动恢复，但恢复要等整整一个窗口，值得多问一句。
 */
function askDetach(bridge: OvsBridge, port: OvsPort): void {
  const lines = [`将把 ${port.name} 从 OVS 网桥 ${bridge.name} 上摘下。`];
  if (port.carriesDefaultRoute || bridge.carriesDefaultRoute) {
    lines.push(
      `注意：${bridge.name} 承载着这台机器的默认路由，摘掉它的端口很可能切断面板与目标机的连接。` +
        '若一直连不回来，确认窗口结束后目标机会自动恢复到变更前的状态。',
    );
  }
  const escalationText = props.escalationWarning || props.escalationInfo;
  if (escalationText) lines.push(escalationText);

  dialog.warning({
    title: '从 OVS 网桥移出网口',
    // content 只接受字符串或返回 VNode 的函数；靠 pre-wrap 保留多段之间的换行。
    content: () => h('div', { style: 'white-space: pre-wrap' }, lines.join('\n')),
    positiveText: '移出',
    negativeText: '取消',
    onPositiveClick: () =>
      emit('apply', { action: 'ovs-detach', interface: port.name, bridge: bridge.name }),
  });
}
</script>

<template>
  <div class="ovspanel">
    <!--
      面板会在目标机上装一个 systemd 服务，这件事必须写在明面上。
      用户不该靠翻 systemd 才发现自己的机器上多了个常驻单元。
    -->
    <div class="ovshead">
      <span class="ovshead__k">开机自恢复</span>
      <span class="ovshead__v" :class="{ 'is-on': props.ovs.persistent }">
        {{ props.ovs.persistent ? '已启用' : '未启用' }}
      </span>
      <span class="ovshead__spacer" />
      <n-button
        v-if="props.ovs.persistent"
        size="tiny"
        :disabled="props.disabled"
        @click="askUnpersist"
      >
        卸掉服务
      </n-button>
      <n-button size="small" :disabled="blocked" @click="openCreate">
        新建 OVS 网桥…
      </n-button>
    </div>
    <p class="ovshead__hint">
      {{
        props.ovs.persistent
          ? '目标机上装有 panelssh-ovs.service，每次开机由它把面板管过的物理网口补回网桥。'
          : '提交一次桥接变更后，面板会在目标机上安装开机服务，重启后自动把网口补回网桥。'
      }}
    </p>

    <!-- 读不到和没有是两回事：前者面板根本没看见现状，后者是真的还没建。
         混成同一句话报出去，用户就会对着自己明明建好的桥琢磨是哪里没打开。
         失败原因由后端一并带回来并显示在这里：同一句"读不到"背后的修法完全
        不同（提权口令不对要去核对主机配置、套接字权限不足要加 openvswitch 组、服务没起
        要去看 systemd），让用户自己去目标机上复现一遍正是这个功能该省掉的事。 -->
    <n-alert v-if="props.ovs.unreadable" type="warning">
      面板已经用 root 权限（自动提权）试过读取，但没能读成。<br />
      <template v-if="props.ovs.unreadableReason">原因：{{ props.ovs.unreadableReason }}<br /></template>
      OVS 的状态不在内核里，而在它自己的数据库中，那个套接字默认只允许 root 与 openvswitch 组访问。
      把登录账号加入 openvswitch 组、改用 root 账号连接，或确认 OVS 服务已启动，这里就会正常显示网桥。
    </n-alert>

    <n-alert v-else-if="props.ovs.bridges.length === 0" type="info">
      这台机器装了 Open vSwitch，但还没有任何网桥 —— 装上 OVS 不等于就有桥，桥要单独建。
      可以在下面点「新建 OVS 网桥」建一座；如果已经用目标机自己的网络管理界面建好了，
      回来看这里就行。两者的区别是：面板建的桥由面板管理、随时能删，系统建的桥归系统管，
      面板只往里加网口，不会删它。
    </n-alert>

    <div v-else class="ovscards">
      <p class="ovscards__hint">
        曲线是最近约两分钟的收发速率：青线为下行、橙线为上行。网桥那条是成员口的合计，
        端口行右侧那条小的是该口自己的走势。
        网口加入网桥后，它自己的地址会转移到网桥上（见每张卡片顶部的地址），
        因此端口行只在它自己还有地址时才显示地址。
      </p>

      <!-- 承载默认路由的那座桥是"一动就可能断连"的那一座，左侧压一道竖线标出来。
           它常常不是列表里的第一座，而标签藏在标题行里，扫视几座桥时容易被跳过。 -->
      <div
        v-for="card in cards"
        :key="card.bridge.name"
        class="ovscard"
        :class="{ 'is-critical': card.bridge.carriesDefaultRoute }"
      >
        <div class="ovscard__head">
          <!-- 折叠箭头沿用环境面板那两个对话框里步骤收起的同一套字符（▾/▸），
               不为这一处另造一个图标。 -->
          <n-button
            class="ovscard__caret"
            size="small"
            quaternary
            :aria-expanded="!isCollapsed(card.bridge.name)"
            :aria-label="`${card.bridge.name} 的展开与收起`"
            :title="`${card.bridge.name} 的展开与收起`"
            @click="toggleCollapse(card.bridge.name)"
          >
            <span class="ovscard__caretmark" aria-hidden="true">
              {{ isCollapsed(card.bridge.name) ? '▸' : '▾' }}
            </span>
          </n-button>
          <span class="ovscard__badge">OVS</span>
          <!-- 链路状态与网络列表、拓扑图共用一套说法（见 netLabels）：网线没插时是
               "无载波"而不是"已启用"—— 后者会让人以为线是通的。
               排在网桥名之前：先看这座桥通不通，再看它是哪一座。 -->
          <span v-if="card.status" class="ovscard__state">
            <span class="led" :class="card.led" />
            {{ card.status }}
          </span>
          <span class="ovscard__name">{{ card.bridge.name }}</span>
          <span v-if="card.bridge.carriesDefaultRoute" class="ovscard__tag is-warn">承载默认路由</span>
          <span class="ovscard__count">{{ card.bridge.ports.length }} 个网口</span>
          <span class="ovscard__spacer" />
          <!-- 收起后留在表头的读数：少了它，"收起"就等于让这座桥的吞吐归零。 -->
          <span v-if="isCollapsed(card.bridge.name) && card.headRate" class="ovscard__sumrate">
            <span class="ovsmetric__v is-rx">↓ {{ throughput(card.headRate.rx) }}</span>
            <span class="ovsmetric__v is-tx">↑ {{ throughput(card.headRate.tx) }}</span>
          </span>
          <!-- 动作收进菜单（加入网口、删除网桥）：两个按钮并排摆着，网桥名是第一个
               被挤掉的；换成三点按钮之后，标题行能一直把名字和标签摆得下。
               菜单里各项的可用条件与告警色见 cardMenuOptions。 -->
          <n-dropdown
            trigger="click"
            placement="bottom-end"
            :options="cardMenuOptions(card)"
            @select="(key) => onCardMenu(key, card)"
          >
            <n-button
              class="ovscard__menu"
              size="small"
              quaternary
              :disabled="blocked"
              :aria-label="`${card.bridge.name} 的网桥操作`"
              :title="`${card.bridge.name} 的网桥操作`"
            >
              <svg class="ovscard__icon" aria-hidden="true">
                <use href="#glyph-more" />
              </svg>
            </n-button>
          </n-dropdown>
        </div>

        <!-- 展开态。收起时整块不渲染：曲线与端口明细没有留着的价值，
             而且网桥多的机器上少渲染几块是实打实的开销差别。 -->
        <template v-if="!isCollapsed(card.bridge.name)">
          <!-- 汇总块：网口进了网桥之后地址会转移到网桥上，所以这里才是"这座桥的地址"，
               端口行里的地址多半是空的；合计只算物理口，理由见 cards 的注释。
               左边放静态量与"本机自身"的读数，右边那块连同读数一起交给曲线 ——
               曲线画的就是物理口合计，把它的数字贴在图上比另起一行更省地方。 -->
          <div class="ovscard__sum">
            <div class="ovscard__facts">
              <!-- 地址按族分两行：IPv4 一行、IPv6 一行，各自成组。混在一串里时，
                   "这台机器的 IPv4 是多少"要在一堆冒号里翻。 -->
              <span class="ovscard__addrs">
                <template v-if="card.addrsV4.length > 0 || card.addrsV6.length > 0">
                  <span v-if="card.addrsV4.length > 0" class="ovscard__addrgroup">
                    <span class="ovscard__addrfam">IPv4</span>
                    <span v-for="addr in card.addrsV4" :key="addr" class="ovscard__addr num">{{ addr }}</span>
                  </span>
                  <span v-if="card.addrsV6.length > 0" class="ovscard__addrgroup">
                    <span class="ovscard__addrfam">IPv6</span>
                    <span v-for="addr in card.addrsV6" :key="addr" class="ovscard__addr num">{{ addr }}</span>
                  </span>
                </template>
                <span v-else class="ovscard__addr is-empty">未配置地址</span>
              </span>
              <span class="ovscard__metrics">
                <!-- 网桥自身与物理口合计是两个不同的数：前者是本机协议栈真正收发的，
                     后者是所有成员口线上的总量，中间的差额就是过境的转发流量。
                     物理口合计这一组不在这儿 —— 它跟着曲线走，见下面的读数行。 -->
                <span v-if="card.selfRates" class="ovsmetric">
                  <span class="ovsmetric__k">网桥自身</span>
                  <span class="ovsmetric__v is-rx">↓ {{ throughput(card.selfRates.rx) }}</span>
                  <span class="ovsmetric__v is-tx">↑ {{ throughput(card.selfRates.tx) }}</span>
                </span>
                <span class="ovsmetric">
                  <span class="ovsmetric__k">累计</span>
                  <span class="ovsmetric__v is-rx">↓ {{ formatBytes(card.rxBytes) }}</span>
                  <span class="ovsmetric__v is-tx">↑ {{ formatBytes(card.txBytes) }}</span>
                </span>
              </span>
            </div>
            <!-- 读数与曲线同框：读数占框顶那一行，图在下面。汇总块总高因此仍是 60px，
                 读数没有另占一行；而"青线下行、橙线上行"的解释也随之不必再写一遍。 -->
            <div v-if="card.hasPhysical" class="ovscard__rate">
              <div class="ovscard__rateline">
                <span class="ovscard__ratelabel">实时速率</span>
                <span class="ovsmetric__v is-rx">↓ {{ throughput(card.rxRate) }}</span>
                <span class="ovsmetric__v is-tx">↑ {{ throughput(card.txRate) }}</span>
              </div>
              <Sparkline
                class="ovscard__chart"
                :series="card.series"
                :max="card.max"
                :gap-ms="props.rates.gapMs"
              />
            </div>
          </div>

          <div v-if="card.ports.length === 0" class="ovscard__empty">这个网桥上还没有端口</div>
          <div v-else class="ovsports">
            <div v-for="view in card.ports" :key="view.port.name" class="ovsport">
              <div class="ovsport__head">
                <!-- 这个口自己那条链路的载波状态，与网络列表共用一套说法（见 netLabels）。
                     排在端口名之前：先看这个口通不通，再看它是哪个口。 -->
                <span v-if="view.status" class="ovsport__state">
                  <span class="led" :class="view.led" />
                  {{ view.status }}
                </span>
                <span class="ovsport__name">{{ view.port.name }}</span>
                <!-- MAC 跟着端口名走：对着交换机查 fdb、确认网线插的是哪个物理口，
                     靠的都是它。读不到接口或没读到 MAC 时整块不出现，不留空占位。 -->
                <span v-if="view.iface?.mac" class="ovsport__mac num">{{ view.iface.mac }}</span>
                <!-- 虚拟口（internal / patch 这些）背后没有网线，摘掉它不会断网。
                     这个区别值得写出来，但不该比端口名更抢眼。 -->
                <span v-if="!view.port.physical" class="ovsport__kind">虚拟口</span>
                <span v-if="view.port.carriesDefaultRoute" class="ovscard__tag is-warn">默认路由出口</span>
                <span class="ovscard__spacer" />
                <!-- 移出是破坏性操作，而且每个端口行都有一个。它常驻（触屏没有 hover，
                     藏起来就等于没有），但压到最轻 —— 一排同样醒目的按钮会把端口名
                     挤成配角。改用三点图标是为了把这一行的横向空间还给端口名、MAC 与
                     标签；危险感交给 hover 变红和确认对话框去表达，动作本身写在
                     aria-label 与 title 里，读屏和悬停都还看得见。 -->
                <n-button
                  class="ovsport__out"
                  size="tiny"
                  quaternary
                  :disabled="blocked"
                  :aria-label="`把 ${view.port.name} 移出 ${card.bridge.name}`"
                  :title="`把 ${view.port.name} 移出 ${card.bridge.name}`"
                  @click="askDetach(card.bridge, view.port)"
                >
                  <svg class="ovsport__icon" aria-hidden="true">
                    <use href="#glyph-more" />
                  </svg>
                </n-button>
              </div>

              <div v-if="view.iface" class="ovsport__body">
                <div class="ovsport__facts">
                  <div class="ovsport__metrics">
                    <!-- 只在端口自己有地址时才显示这一组。被桥接的物理口地址都转移到了
                         网桥上，若照直写出来，一座四口的桥就会把同一句"由网桥提供"印四遍，
                         而它要说明的事在卡片顶部的汇总行里已经写着（那句解释见 ovscards__hint）。 -->
                    <span v-if="view.addrs.length > 0" class="ovsmetric">
                      <span class="ovsmetric__k">地址</span>
                      <span class="ovsmetric__v">{{ view.addrs.join(' ') }}</span>
                    </span>
                    <!-- 吞吐与链路速率并排放：前者回答"现在跑多少"，后者是它的天花板，
                         两个一起看才知道是不是快满了。 -->
                    <span class="ovsmetric">
                      <span class="ovsmetric__k">吞吐</span>
                      <span class="ovsmetric__v is-rx">↓ {{ throughput(view.iface.rxRate) }}</span>
                      <span class="ovsmetric__v is-tx">↑ {{ throughput(view.iface.txRate) }}</span>
                    </span>
                    <span class="ovsmetric">
                      <span class="ovsmetric__k">链路速率</span>
                      <span class="ovsmetric__v">
                        {{ view.iface.speedMbps > 0 ? `${view.iface.speedMbps} Mb/s` : '未知' }}
                      </span>
                    </span>
                    <span class="ovsmetric">
                      <span class="ovsmetric__k">累计</span>
                      <span class="ovsmetric__v is-rx">↓ {{ formatBytes(view.iface.rxBytes) }}</span>
                      <span class="ovsmetric__v is-tx">↑ {{ formatBytes(view.iface.txBytes) }}</span>
                    </span>
                  </div>
                </div>
                <Sparkline
                  class="ovsport__chart"
                  :series="view.series"
                  :max="view.max"
                  :gap-ms="props.rates.gapMs"
                />
              </div>
              <div v-else class="ovsport__absent">
                没有读到这个端口对应的内核接口，速率与流量暂不可见。
              </div>
            </div>
          </div>
        </template>
      </div>
    </div>

    <n-modal v-model:show="attachShow" preset="card" :title="attachTitle" style="width: 520px">
      <div class="ovsform">
        <!-- 提权提醒放在表单最前面：用户就是在这里按下"下发变更"的，
             得在按之前知道这次会不会以 root 执行、以及是不是注定失败。 -->
        <n-alert v-if="props.escalationWarning" type="error">{{ props.escalationWarning }}</n-alert>
        <n-alert v-else-if="props.escalationInfo" type="info">{{ props.escalationInfo }}</n-alert>
        <n-alert v-if="attachRisk" type="warning">{{ attachRisk }}</n-alert>

        <div class="ovsform__field">
          <span class="ovsform__label">要加入的网口</span>
          <n-select
            v-model:value="attachMembers"
            multiple
            :options="attachOptions"
            placeholder="可多选"
          />
        </div>
        <div class="ovsform__hint">
          网口加入网桥后，它自己的地址会转移到网桥上；这一步不会改动 OVS 网桥本身的配置。
        </div>
        <div class="ovsform__hint">
          变更确认通过后，面板会把这台机器上出现过的物理网口固化下来，并安装开机服务，
          让目标机重启后自动补回去。不想要这个服务时，可以在页面上直接卸掉。
        </div>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button @click="attachShow = false">取消</n-button>
          <n-button
            type="primary"
            :disabled="attachMembers.length === 0"
            @click="submitAttach"
          >
            下发变更
          </n-button>
        </n-space>
      </template>
    </n-modal>

    <n-modal v-model:show="createShow" preset="card" title="新建 OVS 网桥" style="width: 520px">
      <div class="ovsform">
        <n-alert v-if="props.escalationWarning" type="error">{{ props.escalationWarning }}</n-alert>
        <n-alert v-else-if="props.escalationInfo" type="info">{{ props.escalationInfo }}</n-alert>
        <n-alert v-if="createRisk" type="warning">{{ createRisk }}</n-alert>

        <div class="ovsform__field">
          <span class="ovsform__label">网桥名</span>
          <n-input v-model:value="createName" placeholder="例如 br-ovs（字母数字与 . _ : @ - ，不超过 15 个字符）" />
        </div>
        <div class="ovsform__field">
          <span class="ovsform__label">成员网口（可留空，之后再挂）</span>
          <n-select
            v-model:value="createMembers"
            multiple
            :options="attachOptions"
            placeholder="可多选"
          />
        </div>
        <div class="ovsform__hint">
          新建的网桥会带上面板的标记，之后可以从这里删掉。目标机自己的网络服务建的网桥没有
          这个标记，面板不会去动它们。
        </div>
      </div>

      <template #footer>
        <n-space justify="end">
          <n-button @click="createShow = false">取消</n-button>
          <n-button type="primary" :disabled="createName.trim() === ''" @click="submitCreate">
            下发变更
          </n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>

<style scoped>
.ovspanel {
  display: flex;
  flex: 1;
  flex-direction: column;
  min-height: 0;
  overflow-y: auto;
  padding-right: var(--sp-1);
}

.ovshead {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--sp-2);
  padding-bottom: var(--sp-2);
}

.ovshead__k {
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

.ovshead__v {
  font-size: var(--fs-sm);
  color: var(--ink-lo);
}

/* 已启用是要让用户一眼看到的状态：它在目标机上留了一个常驻服务。 */
.ovshead__v.is-on {
  color: var(--ok);
}

.ovshead__spacer {
  flex: 1;
}

/* 说明文字另起一行：它跟着状态变，和按钮挤在同一行会在窄窗口里被压成几段。 */
.ovshead__hint {
  margin: 0 0 var(--sp-3);
  padding-bottom: var(--sp-3);
  border-bottom: 1px solid var(--line-hair);
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--ink-lo);
}

.ovscards {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  margin-top: var(--sp-3);
}

/* 曲线与配色的说明只写一次，不跟着每张卡片重复。 */
.ovscards__hint {
  margin: 0;
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--ink-lo);
}

/* 竖向只留一档间距：卡片本身是"一屏里叠好几张"的形态，上下各 12px 的内边距
   乘以卡片数就是白白吃掉的屏高，而头行与汇总块本身已有自己的行距。 */
.ovscard {
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--line-hair);
  border-radius: var(--r-2);
  background: var(--bg-module);
}

/* 承载默认路由的那座桥：动它的端口就是动面板自己走的这条链路。
   左侧压一道竖线而不是再加一个标签 —— 竖线在扫视时才起作用，而网桥一多，
   没人会逐行去读标题里的标签。
   用 inset 阴影而不是 border-left：后者会把这一张卡片的内容整体推右 1px，
   几张卡片并排时看得出来。 */
.ovscard.is-critical {
  box-shadow: inset 2px 0 0 var(--warn);
}

.ovscard__head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.ovscard__badge {
  flex: none;
  padding: 1px 6px;
  border: 1px solid var(--line);
  border-radius: var(--r-1);
  font-family: var(--font-mono);
  font-size: var(--fs-label);
  letter-spacing: 0.08em;
  color: var(--ink-lo);
}

/* 网桥名是这一行的主体，但它必须能让位：这一行还挂着状态点与（收起时的）读数，
   不让它收缩的话，窄窗口下先被顶出去的是菜单和状态 —— 那才是更该留住的信息。 */
.ovscard__name {
  min-width: 0;
  overflow: hidden;
  font-size: var(--fs-md);
  color: var(--ink-hi);
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 网口数量回答的是"这座桥有多大"，属于补充信息，不与网桥名争注意力。 */
.ovscard__count {
  flex: none;
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

/*
 * 折叠箭头。用 naive-ui 的按钮而不是裸 <button>：焦点框、悬停底色、波纹都由它给，
 * 不必在这里重造一套；配色与右边的菜单按钮保持一致（静息压暗、悬停提亮）。
 */
.ovscard__caret {
  flex: none;
  color: var(--ink-lo);
}

.ovscard__caret:hover:not(:disabled),
.ovscard__caret:focus-visible:not(:disabled) {
  color: var(--ink-hi);
}

/* 箭头字符本身比按钮里的图标细，给一整行高才不至于在 28px 的按钮里偏上。 */
.ovscard__caretmark {
  display: block;
  line-height: 1;
}

/* 链路状态：绿点加一句"已连接 / 无载波 / 已停用"，说法与网络列表、拓扑图共用一份
   （见 netLabels）。它排在名字之前，但压暗一级 —— 先读到它，注意力仍归名字。 */
.ovscard__state,
.ovsport__state {
  flex: none;
  display: flex;
  align-items: center;
  gap: var(--sp-1);
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

/* 收起后留在表头的整桥吞吐。与读数用的是同一套等宽数字与配色。 */
.ovscard__sumrate {
  flex: none;
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
}

/* 卡片头的三点按钮。它只是把菜单叫出来，本身不改变任何东西，所以悬停只提亮、
   不变红 —— 变红留给端口行那个真的会摘网口的按钮。直接写 color 的理由与
   .ovsport__out 相同：naive-ui 把主题变量挂在 inline style 上，类里的 color 才压得住。 */
.ovscard__menu {
  flex: none;
  color: var(--ink-lo);
}

.ovscard__menu:hover:not(:disabled),
.ovscard__menu:focus-visible:not(:disabled) {
  color: var(--ink-hi);
}

.ovscard__tag {
  padding: 1px 6px;
  border-radius: var(--r-1);
  background: var(--bg-sunken);
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

/* 承载默认路由是"点了会断连"的信号，用警告色而不是普通标签。 */
.ovscard__tag.is-warn {
  color: var(--warn);
}

.ovscard__spacer {
  flex: 1;
}

.ovscard__empty {
  margin-top: var(--sp-2);
  font-size: var(--fs-xs);
  color: var(--ink-ghost);
}

/* 汇总块：地址与合计读数竖排在左，曲线贴在整块最右侧。 */
.ovscard__sum {
  display: flex;
  align-items: stretch;
  gap: var(--sp-2);
  margin-top: var(--sp-1);
}

.ovscard__facts {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
  flex: 1;
  min-width: 0;
}

/* 一组指标排一行：指标名在前、读数在后。一行里挤着四组时，读数才是要扫的东西。
   汇总块与端口块共用这条规则，两边的间距因此必然一致。 */
.ovscard__metrics,
.ovsport__metrics {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  /* 组间距比字间大一档即可：一行里能挤下的指标组越多，窄窗口下越不容易折成两行，
     而折行正是这些卡片变高的主要原因。 */
  gap: var(--sp-1) var(--sp-2);
}

/* 地址分组竖排：IPv4 一行、IPv6 一行。 */
.ovscard__addrs {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

/* 同一族里可能有多个地址（IPv6 尤其常见），族内横向排，排不下再换行。 */
.ovscard__addrgroup {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: var(--sp-2);
  min-width: 0;
}

/* 族标签只是路标，比地址弱一档，不去跟地址本身争注意力。 */
.ovscard__addrfam {
  flex: none;
  font-size: var(--fs-xs);
  color: var(--ink-ghost);
}

/* 网桥自己的地址是这一行的主体：桥接之后它才是这台机器真正的地址。 */
.ovscard__addr {
  font-size: var(--fs-sm);
  color: var(--ink-hi);
  /* IPv6 是一整串十六进制，没有可断的空隙；不强制断开就会顶穿卡片。 */
  overflow-wrap: anywhere;
}

.ovscard__addr.is-empty {
  color: var(--ink-ghost);
}

/*
 * 读数框：读数行在上、曲线在下，两者同框。
 *
 * 把读数放进这个框而不是另起一行，是为了让汇总块的高度仍只有 60px —— 参考图那种
 * "图表配一整列键值"的摆法会把这一块顶到 90px 以上，与"更紧凑"正好相反。
 * 宽度给 200px：要同时容下"实时速率"这四个字与右侧的 ↑↓ 读数。
 */
.ovscard__rate {
  flex: none;
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
  width: 200px;
}

/* 读数行：标签贴左、两个读数贴右。 */
.ovscard__rateline {
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
}

/* 标签可以压缩、读数不换行：窄窗口下先该省略的是"实时速率"那四个字，而不是数字。 */
.ovscard__ratelabel {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  font-size: var(--fs-label);
  color: var(--ink-lo);
  text-overflow: ellipsis;
  white-space: nowrap;
}

/*
 * 两条曲线的高度写死：曲线是看走势的，让它跟着容器高度伸缩，几座桥之间就没法比了。
 *
 * .sparkbox 那条全局规则（flex: 1 1 40px; min-height: 40px）带主轴方向的弹性，
 * 必须用 flex: none 压掉，否则它会去抢左边的宽度；它的 min-height: 40px 也要在这里
 * 重新声明覆盖 —— 端口曲线只有 18px 高，不压掉的话会把每个端口行重新撑回 40px，
 * 这一轮压缩省下的高度就被它全吃回去了。
 */
.ovscard__chart,
.ovsport__chart {
  display: block;
  flex: none;
}

/* 汇总曲线宽度吃满读数框（.sparkbox 的 width: 100%），高度是 60px 的框减去顶部那行读数。 */
.ovscard__chart {
  height: 44px;
}

/* 端口曲线降级为行内微缩走势：一眼判断"这个口此刻在不在传"，不再独占一块。
   56×18 是还能同时画下上下行两条线的最小尺寸。 */
.ovsport__chart {
  width: 56px;
  height: 18px;
  min-height: 18px;
}

/* 一组指标：名字在前、读数在后；一行里挤着四组时，读数才是要扫的东西。 */
.ovsmetric {
  display: flex;
  align-items: baseline;
  gap: var(--sp-1);
}

.ovsmetric__k {
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

.ovsmetric__v {
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  color: var(--ink-hi);
}

/* 收发沿用资源监控与主机卡片的那一组配色：下行青、上行橙。 */
.ovsmetric__v.is-rx {
  color: var(--data-2);
}

.ovsmetric__v.is-tx {
  color: var(--data-1);
}

.ovsports {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
  margin-top: var(--sp-2);
  padding-top: var(--sp-2);
  border-top: 1px solid var(--line-hair);
}

/* 一个端口现在是一小块而不是一行：名字与操作在头一行，指标与曲线在下面两行。 */
.ovsport {
  /* 负边距把 hover 底色铺到卡片内边距之外，让整块都算"这一块"，
     而不是悬在名字和按钮之间的一条窄带。 */
  margin: 0 calc(var(--sp-2) * -1);
  padding: var(--sp-1) var(--sp-2);
  border-radius: var(--r-1);
  transition: background-color 120ms ease;
}

/* 块本身不是操作目标，指针划过时给一层浅底：端口一多，
   光靠间距已经分不清哪个按钮属于哪个名字。 */
.ovsport:hover {
  background: var(--bg-sunken);
}

.ovsport__head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  /* 与卡片头行同高：两端对齐之后，几个端口块的文字起跑线才落在同一列上。 */
  min-height: 24px;
}

/*
 * 端口块的下半部分：指标在左，微缩曲线贴右。
 *
 * align-items 用 center 而不是 stretch —— 曲线只有 18px 高，而指标在窄窗口下会折成
 * 两行；用 stretch 会把曲线跟着拉长，那一行的走势就被拉扁成了噪声。
 */
.ovsport__body {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.ovsport__facts {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
  flex: 1;
  min-width: 0;
}

/* 端口在 OVS 里有、内核里没有对应接口时（bond、patch 这类）的说明。 */
.ovsport__absent {
  font-size: var(--fs-xs);
  color: var(--ink-ghost);
}

/* 端口名是这一行的主体 —— 桥下挂着什么，全靠这一列回答。 */
.ovsport__name {
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  color: var(--ink-hi);
}

/* MAC 是拿去做对照的数据（交换机 fdb、物理口贴纸），不是扫视目标，
   所以与端口名同字体但压一档灰度。 */
.ovsport__mac {
  flex: none;
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

/* 虚拟口（internal / patch 这些）背后没有网线，摘掉它不会断网。
   这个区别值得写出来，但它只是注脚，用最小字号与最弱的合规灰度。 */
.ovsport__kind {
  flex: none;
  font-size: var(--fs-label);
  color: var(--ink-lo);
}

/* 移出：常驻（触屏没有 hover，藏起来等于没有），但静息时与注脚同级 ——
   一列同样醒目的按钮会把端口名挤成配角。指到它的那一刻才变危险色。
   键盘用户走 focus-visible，不该只有鼠标能看见这个提示。
   直接写 color 而不是覆盖 --n-text-color：naive-ui 把主题变量以 inline style
   挂在按钮上，类里的变量赋值压不过它（inline 优先），而类选择器的 color 能
   压过 .n-button 自己那条 —— 省掉一层 !important。 */
.ovsport__out {
  color: var(--ink-lo);
}

/* 竖排三点本身画得很细，宿主给到 16px 才不至于在 tiny 按钮里缩成一个点。
   颜色走 currentColor，随按钮的静息 / 悬停变红一起变。
   两处的三点按钮（端口行的移出、卡片头的网桥菜单）共用这一条尺寸约定。 */
.ovscard__icon,
.ovsport__icon {
  display: block;
  width: 16px;
  height: 16px;
}

.ovsport__out:hover:not(:disabled),
.ovsport__out:focus-visible:not(:disabled) {
  color: var(--danger);
}

/* 禁用态要比静息态更弱，否则"移出"看起来仍然可以点。 */
.ovsport__out:disabled {
  color: var(--ink-ghost);
}

.ovsform {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}

.ovsform__field {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
}

.ovsform__label {
  font-size: var(--fs-xs);
  color: var(--ink-lo);
}

.ovsform__hint {
  font-size: var(--fs-xs);
  line-height: 1.6;
  color: var(--ink-lo);
}
</style>
