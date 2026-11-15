import { onBeforeUnmount, onMounted, watch, type Ref } from 'vue';

interface VisiblePollOptions {
  /** 窗口根元素，用于判断它是否被最小化。 */
  root: Ref<HTMLElement | null>;
  /**
   * 轮询间隔（毫秒）。
   *
   * 传函数表示间隔可变（例如监控窗口的采样频率选择器），值变化时会立即按新间隔重建计时器。
   */
  intervalMs: number | (() => number);
  task: () => void | Promise<void>;
  /** 窗口虽然可见、但当前还无可采集对象时返回 false（例如尚未选择目标主机）。 */
  enabled?: () => boolean;
}

/**
 * 只在窗口真正可见时轮询。
 *
 * 桌面式面板上常常同时开着好几个窗口，其中大部分是背景窗口 ——
 * 被最小化的窗口（v-show 置成 display:none，offsetParent 为 null）
 * 以及切到后台的标签页都不会被用户看到，继续按秒去目标机上跑命令
 * 是纯粹的浪费，而且会在目标机上叠加负载。
 *
 * 资源监控与进程管理两个窗口用的是同一套判断，抽在这里而不是各写一份。
 */
export function useVisiblePoll(options: VisiblePollOptions): () => boolean {
  const { root, intervalMs, task, enabled } = options;

  function shouldPoll(): boolean {
    if (typeof document !== 'undefined' && document.hidden) return false;
    if (enabled && !enabled()) return false;
    const el = root.value;
    return el ? el.offsetParent !== null : false;
  }

  function onVisibilityChange(): void {
    // 回到可见时立刻取一次，避免界面上挂着几分钟前的旧数据。
    if (!document.hidden && shouldPoll()) void task();
  }

  let timer: number | null = null;
  let stopWatch: (() => void) | null = null;

  function currentInterval(): number {
    const value = typeof intervalMs === 'function' ? intervalMs() : intervalMs;
    // 兜底：0 或负数会让 setInterval 退化成 4ms 的忙轮询。
    return Math.max(50, value);
  }

  function restart(): void {
    if (timer !== null) window.clearInterval(timer);
    timer = window.setInterval(() => {
      if (shouldPoll()) void task();
    }, currentInterval());
  }

  onMounted(() => {
    if (shouldPoll()) void task();
    restart();
    // 间隔可变时盯着它：改完立刻按新节奏走，而不是等旧计时器走完一轮 ——
    // 从 5 秒切到 200ms 时，等旧的一轮会让人以为没生效。
    if (typeof intervalMs === 'function') stopWatch = watch(intervalMs, restart);
    document.addEventListener('visibilitychange', onVisibilityChange);
  });

  onBeforeUnmount(() => {
    if (timer !== null) window.clearInterval(timer);
    if (stopWatch) stopWatch();
    document.removeEventListener('visibilitychange', onVisibilityChange);
  });

  return shouldPoll;
}
