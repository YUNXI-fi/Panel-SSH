/**
 * 窗口之间的文件拖拽。
 *
 * 面板的多个窗口都在同一个文档里（不是 iframe），所以一次拖拽从开始到落下
 * 始终是同一段 JS。真正麻烦的是 dataTransfer：按规范只有 drop 时才能读
 * getData，dragover 阶段拿到的是一片空字符串 —— 而"这里能不能放"要在
 * dragover 就决定。于是把这次拖拽的内容另外记在模块里（模块只加载一次，
 * 所有窗口共用），dataTransfer 上的自定义类型只作为跨窗口的载体。
 */

export const FILE_DRAG_TYPE = 'application/x-panelssh-files';

export interface FileDragPayload {
  hostId: number;
  hostName: string;
  paths: string[];
}

let active: FileDragPayload | null = null;

/**
 * 拖拽状态的订阅者。
 *
 * 桌面需要一个"现在正在拖文件"的信号，用来浮出主机投放条 ——
 * dragover 阶段读不到 dataTransfer，只能靠模块内这份状态。
 */
type DragListener = (payload: FileDragPayload | null) => void;
const listeners = new Set<DragListener>();

function notify(): void {
  for (const listener of listeners) listener(active);
}

export function onFileDragChange(listener: DragListener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function beginFileDrag(dataTransfer: DataTransfer | null, payload: FileDragPayload): void {
  active = payload;
  notify();
  if (!dataTransfer) return;
  dataTransfer.effectAllowed = 'copyMove';
  try {
    dataTransfer.setData(FILE_DRAG_TYPE, JSON.stringify(payload));
    // 纯文本形态给"拖到面板外"留一条可读的退路（例如拖进编辑器）。
    dataTransfer.setData('text/plain', payload.paths.join('\n'));
  } catch {
    // 个别浏览器会拒绝自定义类型；模块内状态仍然可用，拖拽不受影响。
  }
}

/** 当前是否有面板内部的拖拽在进行。OS 文件拖入时恒为 null。 */
export function internalDragPayload(): FileDragPayload | null {
  return active;
}

/** dragend / drop 之后清空，避免下一次 OS 文件拖入被误认成内部拖拽。 */
export function endFileDrag(): void {
  active = null;
  notify();
}
