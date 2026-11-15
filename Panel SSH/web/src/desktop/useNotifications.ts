import { ref } from 'vue';

import { api, type Notification } from '../api/client';

/**
 * 通知的共享状态。
 *
 * 竖栏铃铛要未读数、抽屉面板要完整列表，两者必须来自同一份数据 ——
 * 各自拉取的话，用户点开面板看到的内容会与刚才角标上的数字对不上。
 * 这与 usePlugins 是同一套理由，因此沿用模块级 ref 的写法。
 */
const items = ref<Notification[]>([]);
const unread = ref(0);
const loaded = ref(false);

export function useNotifications() {
  /**
   * 拉取最新通知与未读数。
   *
   * 失败时静默保留上一次的值：这是后台轮询，网络抖一下不该弹错误提示，
   * 更不该让角标数字突然归零。用户真正的操作（标记已读）失败时会另行提示。
   */
  async function refresh(): Promise<void> {
    try {
      const data = await api.notifications.list();
      items.value = data.items;
      unread.value = data.unread;
      loaded.value = true;
    } catch {
      // 保留旧值，等待下一次轮询。
    }
  }

  /** 把指定通知标为已读。 */
  async function markRead(ids: number[]): Promise<void> {
    const res = await api.notifications.markRead(ids);
    unread.value = res.unread;
    // 本地同步标掉，不再多拉一次列表：标记结果是确定的，回拉只会让界面闪一下。
    for (const item of items.value) {
      if (ids.includes(item.id)) item.read = true;
    }
  }

  /** 全部标为已读。 */
  async function markAllRead(): Promise<void> {
    const res = await api.notifications.markAllRead();
    unread.value = res.unread;
    for (const item of items.value) item.read = true;
  }

  return { items, unread, loaded, refresh, markRead, markAllRead };
}
