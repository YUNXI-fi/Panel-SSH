import { ref, watch } from 'vue';

/**
 * 当前正在被观察的监控目标。
 *
 * 为什么用模块级单例而不是 provide/inject：
 * 右上角状态胶囊需要知道"现在该显示哪台机器的数据"，而资源监控窗口的目标主机
 * 是窗口内部状态（桌面图标打开时还要等用户在窗口里选）。两者之间隔着窗口系统，
 * 顺着 props 传下去要先把「窗口自持状态」改成「父级托管状态」，代价远大于收益。
 * 「此刻在监控哪台机器」本身就是个全局唯一的语义，用单例最贴合。
 *
 * 多开监控窗口时后选中的胜出 —— 同一时刻只有一个右上角胶囊，这个取舍是明确的。
 */

/**
 * 「宿主机」——面板自己所在的那台机器的保留 ID，需与后端 api.LocalHostID 保持一致。
 *
 * 它不是一个 SSH 目标（没有地址与凭据），而是本地面板自身的采集通道：
 * 后端对 0 走本地直采，对其他 ID 才建立 SSH 连接。
 */
export const LOCAL_HOST_ID = 0;

export const LOCAL_HOST_NAME = '宿主机';

export interface WatchedHost {
  id: number;
  name: string;
}

export const LOCAL_HOST: WatchedHost = { id: LOCAL_HOST_ID, name: LOCAL_HOST_NAME };

/** 记住上次选择用的键。 */
const STORAGE_KEY = 'panelssh.watched-host';

/**
 * 恢复上次的选择，没有记录时落在宿主机上。
 *
 * 默认宿主机而不是"未选择"：面板自己那台机器的负载是无人值守时最该被看到的一组数字，
 * 让胶囊一进来就为空等于把最常用的信息藏在一层点击之后。
 */
function restore(): WatchedHost {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw) {
      const saved = JSON.parse(raw) as Partial<WatchedHost>;
      if (typeof saved.id === 'number' && typeof saved.name === 'string') {
        return { id: saved.id, name: saved.name };
      }
    }
  } catch {
    // 存储不可用（隐私模式）或内容损坏都退回宿主机，监控本身不该因此不可用。
  }
  return LOCAL_HOST;
}

export const watchedHost = ref<WatchedHost>(restore());

// 记住上次选择：刷新页面后胶囊仍然盯着同一台机器，不必每次重新点一遍。
// 赋值处一律写入新对象，所以浅层 watch 足够。
watch(watchedHost, (host) => {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(host));
  } catch {
    // 写不进去只是失去记忆，不该影响正在进行的监控。
  }
});
