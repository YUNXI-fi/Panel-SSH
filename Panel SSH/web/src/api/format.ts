/**
 * 展示层共用的格式化函数。
 * 抽在这里是因为文件管理与资源监控都要用，重复实现迟早会出现口径不一致。
 */

/** 字节数 -> 易读字符串。 */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';

  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  // 字节本身没有小数意义；其余单位在小于 10 时保留一位，避免 1 MB 与 1.9 MB 看起来一样。
  const digits = unit === 0 ? 0 : value >= 10 ? 0 : 1;
  return `${value.toFixed(digits)} ${units[unit]}`;
}

/** 字节/秒 -> 易读速率。 */
export function formatRate(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return '0 B/s';
  return `${formatBytes(bytesPerSecond)}/s`;
}

/** 秒 -> “12 天 3 小时”这类可读时长。 */
export function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return '—';

  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);

  if (days > 0) return `${days} 天 ${hours} 小时`;
  if (hours > 0) return `${hours} 小时 ${minutes} 分`;
  return `${minutes} 分`;
}

/** 时间字符串 -> 本地时间显示。 */
export function formatTime(raw: string): string {
  const date = new Date(raw);
  if (Number.isNaN(date.getTime())) return raw;
  return date.toLocaleString('zh-CN', { hour12: false });
}

/**
 * 延时读数：两位小数，800 毫秒以上改用秒。
 *
 * 留小数是必须的：本机与同网段的 TCP 建连常在 0.2～0.5 毫秒之间，
 * 取整后这些读数会全部塌成 "0 ms"，看着像没测到数据。
 *
 * 800 毫秒换单位：再往上（跨地域线路）毫秒位已经没有判断价值，
 * "1234.56 ms" 远不如 "1.23 s" 直观；800 也正好是 0.8 秒，边界好记。
 *
 * 这条规则与后端 internal/api/notifications.go 的 formatLatency 必须一致 ——
 * 同一个读数在通知里和界面上是两个样子，用户会怀疑哪一个才是对的。
 */
export function formatLatency(ms: number): string {
  if (ms >= 800) return `${(ms / 1000).toFixed(2)} s`;
  return `${ms.toFixed(2)} ms`;
}

/**
 * 把一次 SSH 连接测试的结果拼成可直接展示的一行文案。
 *
 * 成功时带上握手耗时：它是对"这台机器网络通不通畅"最直接的量化，
 * 值得在保存之前看到。失败时不显示耗时，只给原因 —— 那时耗时没有诊断意义，
 * 混在错误信息里反而会干扰阅读。
 */
export function formatTestResult(result: { ok: boolean; message: string; latencyMs: number }): string {
  if (!result.ok) return result.message;
  return `${result.message} · ${formatLatency(result.latencyMs)}`;
}
