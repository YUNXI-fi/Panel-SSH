import { ref } from 'vue';

import { api, type BuiltinPlugin, type CapabilityInfo, type PluginInfo } from '../api/client';

/**
 * 插件列表的共享状态。
 *
 * 桌面图标与应用中心都需要这份数据：在应用中心装/卸/停用插件后，
 * 桌面上的图标要立刻跟着变。放在模块级 ref 里比在两处各自拉取更简单，
 * 也避免了两份状态不同步。
 *
 * 服务端返回的是全部主机上的安装记录，因此这里保留原始列表，
 * 由调用方按 hostId 过滤。不在这里按主机过滤的原因：桌面上的图标要
 * 覆盖"任何一台主机上装了"的插件，与某个窗口选中的主机无关。
 */
const plugins = ref<PluginInfo[]>([]);
const builtins = ref<BuiltinPlugin[]>([]);
const capabilities = ref<CapabilityInfo[]>([]);
const loaded = ref(false);

export function usePlugins() {
  async function refresh(): Promise<void> {
    const data = await api.plugins.list();
    plugins.value = data.plugins;
    builtins.value = data.builtins;
    capabilities.value = data.capabilities;
    loaded.value = true;
  }

  /**
   * 按主机取安装记录。
   *
   * 同名插件在不同主机上各有一条记录，各有自己的版本、授权与启停状态，
   * 因此界面必须按主机切片，否则会看到"三台机器上的同一个插件"挤在一起。
   */
  function onHost(hostId: number): PluginInfo[] {
    return plugins.value.filter((item) => item.hostId === hostId);
  }

  return {
    plugins,
    builtins,
    capabilities,
    loaded,
    refresh,
    onHost,
  };
}
