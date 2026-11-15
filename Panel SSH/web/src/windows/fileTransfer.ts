/**
 * 跨主机搬运（复制 / 移动）的一次执行。
 *
 * 抽出来给「文件管理」和终端左侧文件栏共用：两边都只是把进度画在自己的
 * 进度条上，协议解析与"这一趟到底成没成"的判定完全一样，抄第二份迟早会走散。
 */

import { ApiError, api } from '../api/client';

export interface CrossHostProgress {
  /** direct = 源机直连目标机（只有根条目数），relay = 经面板中转（有字节数）。 */
  mode: 'direct' | 'relay' | '';
  done: number;
  total: number;
  bytes: number;
  bytesTotal: number;
  /** 正在处理的条目路径（中转时是文件，直连时是根条目）。 */
  path: string;
  /** 给进度条旁边那行字用的说明。 */
  note: string;
}

export interface CrossHostResult {
  /** interrupted 表示流结束了却始终没收到结论 —— 网络断开时就是这样。 */
  outcome: 'done' | 'error' | 'interrupted';
  /** 失败原因，或"有 N 个文件未能传输"这类收尾说明；成功且干净时为空串。 */
  message: string;
  /** 没能搬过去的文件数（整体仍然算完成）。 */
  failed: number;
}

export interface CrossHostRequest {
  fromHostId: number;
  toHostId: number;
  paths: string[];
  target: string;
  move: boolean;
}

export interface CrossHostHandlers {
  onProgress: (progress: CrossHostProgress) => void;
  /**
   * 需要让用户知道、但不影响进度条的一句话（目前只有"直连失败，已改走中转"）。
   *
   * 不塞进 note 里：progress 帧一到 note 就会被"正在复制 xxx"覆盖，
   * 用户根本来不及看清为什么换了通道。
   */
  onNotice?: (text: string) => void;
}

function describe(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 504) return `${err.message}。请检查目标机是否可达`;
    return err.status === 401 ? '会话已过期，请重新登录' : err.message;
  }
  return '跨主机传输失败';
}

export async function crossHostTransfer(
  request: CrossHostRequest,
  handlers: CrossHostHandlers,
  signal?: AbortSignal,
): Promise<CrossHostResult> {
  let outcome: 'done' | 'error' | null = null;
  let failed = 0;
  let message = '';
  let current: CrossHostProgress = {
    mode: '',
    done: 0,
    total: 0,
    bytes: 0,
    bytesTotal: 0,
    path: '',
    note: '正在探测直连…',
  };
  handlers.onProgress(current);

  try {
    await api.files.transfer(
      request.fromHostId,
      request.toHostId,
      request.paths,
      request.target,
      request.move,
      (event) => {
        switch (event.type) {
          case 'prepare':
            current = { ...current, note: '正在统计文件…' };
            break;
          case 'mode':
            // 直连失败会自动改走中转，所以这个帧可能来两次 —— 每次都如实改文案。
            current = {
              mode: event.mode,
              done: 0,
              total: 0,
              bytes: 0,
              bytesTotal: 0,
              path: '',
              note: event.mode === 'direct' ? '直连传输中（不提供字节进度）' : '经面板中转',
            };
            if (event.message) handlers.onNotice?.(event.message);
            break;
          case 'progress':
            current = {
              ...current,
              done: event.done ?? current.done,
              total: event.total ?? current.total,
              bytes: event.bytes ?? current.bytes,
              bytesTotal: event.bytesTotal ?? current.bytesTotal,
              path: event.path ?? current.path,
            };
            break;
          case 'done':
            outcome = 'done';
            failed = event.failed ?? 0;
            break;
          case 'error':
            outcome = 'error';
            message = event.message;
            break;
        }
        handlers.onProgress(current);
      },
      signal,
    );
  } catch (err) {
    if (signal?.aborted) return { outcome: 'interrupted', message: '已中止传输', failed: 0 };
    return { outcome: 'error', message: describe(err), failed: 0 };
  }

  if (outcome === null) {
    return { outcome: 'interrupted', message: '与面板的连接已断开', failed: 0 };
  }
  return { outcome, message, failed };
}
