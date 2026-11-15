import { ref } from 'vue';

import { api, type LaunchApp } from '../api/client';

/**
 * 已安装且可在面板里打开的应用。
 *
 * 应用中心与桌面共用这份模块级状态：在应用中心装完一个安全工具后，
 * 桌面图标要立刻出现，不能等用户刷新页面。
 */
const launchApps = ref<LaunchApp[]>([]);
const loaded = ref(false);

export function useLaunchApps() {
  async function refresh(): Promise<void> {
    const data = await api.env.launchApps();
    launchApps.value = data.apps;
    loaded.value = true;
  }

  return { launchApps, loaded, refresh };
}
