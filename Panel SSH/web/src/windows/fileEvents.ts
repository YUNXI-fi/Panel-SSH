/**
 * 文件列表变更的广播。
 *
 * 跨主机搬运的源窗口与目标窗口是两个独立实例，一方搬完另一方不会自己知道：
 * 移动之后源窗口还列着已经不存在的条目，再点一次就是一个必然失败的请求。
 * 用 window 上的一个自定义事件把"哪几台机器变了"传出去，各窗口自己决定要不要刷新。
 */

const FILE_CHANGED_EVENT = 'panelssh:files-changed';

export function notifyFilesChanged(hostIds: number[]): void {
  window.dispatchEvent(new CustomEvent(FILE_CHANGED_EVENT, { detail: { hostIds } }));
}

/** 订阅变更，返回取消订阅的函数（组件卸载时必须调用）。 */
export function onFilesChanged(handler: (hostIds: number[]) => void): () => void {
  const listener = (event: Event): void => {
    const ids = (event as CustomEvent<{ hostIds?: number[] }>).detail?.hostIds ?? [];
    handler(ids);
  };
  window.addEventListener(FILE_CHANGED_EVENT, listener);
  return () => window.removeEventListener(FILE_CHANGED_EVENT, listener);
}
