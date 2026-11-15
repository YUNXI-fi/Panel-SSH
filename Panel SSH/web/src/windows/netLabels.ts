/**
 * 网络接口的展示语汇。
 *
 * 类型名、归属标签、链路状态这三组说法，接口卡片与拓扑图两处都要用 ——
 * 同一个网口在列表里叫"无载波"、在图上叫"未连接"，用户会以为这是两件事。
 * 放在这里共用一份，而不是各写各的。
 */
import type { NetInterface } from '../api/client';

/** 归属标签。物理网卡是常态，不给标签，免得每张卡片都挂一串同样的字。 */
export function ownerLabel(owner: string): string {
  switch (owner) {
    case 'container':
      return '容器';
    case 'virtual':
      return '虚拟';
    case 'kernel':
      return '内核';
    default:
      return '';
  }
}

export const typeNames: Record<string, string> = {
  ethernet: '以太网',
  bridge: '网桥',
  // OVS 网桥的内核设备类型就是 openvswitch，与 Linux 网桥是不同的东西，
  // 标签上必须分开，否则用户会以为能用 nmcli 那套去改它。
  openvswitch: 'OVS 网桥',
  bond: '链路聚合',
  vlan: 'VLAN',
  wireless: '无线',
  veth: '虚拟接口',
  loopback: '回环',
  tun: '隧道',
  dummy: '虚拟占位',
  macvlan: 'MACVLAN',
  wireguard: 'WireGuard',
};

export function typeLabel(type: string): string {
  return typeNames[type] ?? type;
}

/**
 * 接口状态。
 *
 * 管理态与运行态必须分开说：网线没插、对端交换机端口没开时，内核里这个接口
 * 依然是 up 的（管理态），但链路没有载波。只显示一个"已启用"会让用户以为网线是通的。
 */
export function stateLabel(iface: NetInterface): string {
  if (!iface.adminUp) return '已停用';
  return iface.up ? '已连接' : '无载波';
}

export function stateClass(iface: NetInterface): string {
  if (!iface.adminUp) return 'is-off';
  return iface.up ? 'is-up' : 'is-idle';
}

export function ledClass(iface: NetInterface): string {
  // 已停用不给颜色：默认的灰灯表示"没有信号"，比另造一个颜色更符合指示灯语义。
  if (!iface.adminUp) return '';
  return iface.up ? 'led--ok' : 'led--warn';
}