/**
 * 已打开的文件管理窗口登记表。
 *
 * 把文件拖到某台主机上时，需要知道"这台机器现在停在哪个目录"，
 * 才能把东西直接放进去。文件管理窗口在挂载时把自己登记进来，卸载时注销；
 * 没有登记就说明它没开着，调用方只能新开一个窗口。
 */
import type { FileDragPayload } from './fileDrag';

export interface FileTarget {
  hostId: number;
  /** 该窗口当前所在目录。 */
  path: () => string;
  /** 把拖来的条目复制/移动到当前目录。 */
  accept: (payload: FileDragPayload, mode: 'copy' | 'move') => void;
}

const targets = new Map<number, FileTarget>();

export function registerFileTarget(target: FileTarget): () => void {
  targets.set(target.hostId, target);
  return () => {
    // 只删自己那一份：同一台主机可能先关了旧窗口、又开了新窗口，
    // 旧窗口卸载时不能把新窗口的登记一起删掉。
    if (targets.get(target.hostId) === target) targets.delete(target.hostId);
  };
}

export function findFileTarget(hostId: number): FileTarget | undefined {
  return targets.get(hostId);
}
