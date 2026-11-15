/**
 * SSH 密钥生成的纯函数。
 *
 * 从设置窗口里抽出来，一是让公钥 wire 格式与指纹能被单元测试直接覆盖，
 * 二是后续接真实后端时，预览页与写入端可以共用同一套编码逻辑。
 */

export interface SshKey {
  publicKey: string;
  fingerprint: string;
  note: string;
  createdAt: string;
}

/** 32 字节随机数：ed25519 公钥的密钥体长度。 */
export function randomBytes(length: number): Uint8Array {
  const raw = new Uint8Array(length);
  globalThis.crypto.getRandomValues(raw);
  return raw;
}

export function toBase64(bytes: Uint8Array): string {
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

/**
 * 按 OpenSSH wire 格式拼公钥 blob。
 *
 * 结构是 string algorithm + string key，每个 string 自带 4 字节大端长度前缀。
 * 少了这层前缀，base64 出来只有 44 字符，写进 authorized_keys 会被 sshd 拒绝。
 */
export function ed25519Blob(rawKey: Uint8Array): Uint8Array {
  const algorithm = new TextEncoder().encode('ssh-ed25519');
  const blob = new Uint8Array(4 + algorithm.length + 4 + rawKey.length);
  const view = new DataView(blob.buffer);
  view.setUint32(0, algorithm.length);
  blob.set(algorithm, 4);
  view.setUint32(4 + algorithm.length, rawKey.length);
  blob.set(rawKey, 8 + algorithm.length);
  return blob;
}

/** 指纹是公钥 blob 的 SHA-256；拿不到 WebCrypto 时退回随机演示值。 */
export async function fingerprintOf(blob: Uint8Array): Promise<string> {
  try {
    const digest = new Uint8Array(
      await globalThis.crypto.subtle.digest('SHA-256', blob.buffer as ArrayBuffer),
    );
    return `SHA256:${toBase64(digest).replace(/=+$/, '')}`;
  } catch {
    return `SHA256:${toBase64(randomBytes(32)).replace(/=+$/, '')}`;
  }
}

/** 补零成本地时间：2026-11-14 09:05:03。 */
export function formatTimestamp(now: Date): string {
  const pad = (value: number) => String(value).padStart(2, '0');
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())} ${pad(now.getHours())}:${pad(now.getMinutes())}:${pad(now.getSeconds())}`;
}

/** 生成一把 ed25519 公钥与其指纹。now 可注入，便于断言时间格式。 */
export async function createEd25519Key(note: string, now: Date = new Date()): Promise<SshKey> {
  const blob = ed25519Blob(randomBytes(32));
  return {
    publicKey: `ssh-ed25519 ${toBase64(blob)} ${note}`,
    fingerprint: await fingerprintOf(blob),
    note,
    createdAt: formatTimestamp(now),
  };
}

/**
 * 预览页的私钥占位文件。
 *
 * 不是可用的 OpenSSH 私钥：文件头明确标注 DEMO，避免被误当成真实凭据使用。
 */
export function demoPrivateKey(publicKey: string): string {
  return [
    '# DEMO ONLY — 预览页生成的占位私钥，不能用于真实登录。',
    '-----BEGIN OPENSSH PRIVATE KEY-----',
    toBase64(randomBytes(96)),
    toBase64(randomBytes(96)),
    '-----END OPENSSH PRIVATE KEY-----',
    '',
    `# 对应公钥：${publicKey}`,
  ].join('\n');
}
