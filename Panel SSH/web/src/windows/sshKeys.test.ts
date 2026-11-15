import { describe, expect, it } from 'vitest';

import {
  createEd25519Key,
  demoPrivateKey,
  ed25519Blob,
  fingerprintOf,
  formatTimestamp,
  randomBytes,
  toBase64,
} from './sshKeys';

/** 合法 ed25519 公钥的 base64 前缀：字符串长度 11 + 密钥长度 32 的 wire 头。 */
const ED25519_PREFIX = 'AAAAC3NzaC1lZDI1NTE5AAAAI';

function decodeBase64(text: string): Uint8Array {
  return Uint8Array.from(atob(text), (char) => char.charCodeAt(0));
}

function blobOf(publicKey: string): Uint8Array {
  const base64 = publicKey.split(' ')[1] ?? '';
  return decodeBase64(base64);
}

describe('ed25519Blob', () => {
  it('按 OpenSSH wire 格式写入算法名与密钥长度', () => {
    const raw = new Uint8Array(32).fill(7);
    const blob = ed25519Blob(raw);
    const view = new DataView(blob.buffer);

    expect(blob).toHaveLength(51);
    expect(view.getUint32(0)).toBe(11);
    expect(new TextDecoder().decode(blob.slice(4, 15))).toBe('ssh-ed25519');
    expect(view.getUint32(15)).toBe(32);
    expect(Array.from(blob.slice(19))).toEqual(Array.from(raw));
  });
});

describe('createEd25519Key', () => {
  it('生成的公钥结构合法且长度正确', async () => {
    const key = await createEd25519Key('panel@web-01');
    const [algorithm, base64, note] = key.publicKey.split(' ');

    expect(algorithm).toBe('ssh-ed25519');
    expect(note).toBe('panel@web-01');
    // 51 字节的 wire blob base64 后正好 68 个字符。
    expect(base64).toHaveLength(68);
    expect(base64?.startsWith(ED25519_PREFIX)).toBe(true);
    expect(blobOf(key.publicKey)).toHaveLength(51);
  });

  it('指纹与公钥 blob 的 SHA-256 一致', async () => {
    const key = await createEd25519Key('panel@web-01');
    const expected = await fingerprintOf(blobOf(key.publicKey));

    expect(key.fingerprint).toBe(expected);
    expect(key.fingerprint).toMatch(/^SHA256:[A-Za-z0-9+/]{43}$/);
  });

  it('两次生成不会重复', async () => {
    const [first, second] = await Promise.all([
      createEd25519Key('panel@web-01'),
      createEd25519Key('panel@web-01'),
    ]);

    expect(first.publicKey).not.toBe(second.publicKey);
    expect(first.fingerprint).not.toBe(second.fingerprint);
  });

  it('createdAt 使用传入时间并补零', async () => {
    const key = await createEd25519Key('n', new Date(2026, 10, 14, 9, 5, 3));

    expect(key.createdAt).toBe('2026-11-14 09:05:03');
  });

  it('原样保留备注，由调用方决定是否 trim', async () => {
    const key = await createEd25519Key('  spaced note  ');

    expect(key.note).toBe('  spaced note  ');
    expect(key.publicKey.endsWith('   spaced note  ')).toBe(true);
  });
});

describe('formatTimestamp', () => {
  it('月日时分秒都补齐两位', () => {
    expect(formatTimestamp(new Date(2026, 0, 2, 3, 4, 5))).toBe('2026-01-02 03:04:05');
  });
});

describe('randomBytes / toBase64', () => {
  it('随机字节长度正确且不是全零', () => {
    const bytes = randomBytes(32);

    expect(bytes).toHaveLength(32);
    expect(bytes.some((byte) => byte !== 0)).toBe(true);
  });

  it('base64 能还原出原始字节', () => {
    const bytes = new Uint8Array([0, 1, 2, 253, 254, 255]);

    expect(Array.from(decodeBase64(toBase64(bytes)))).toEqual(Array.from(bytes));
  });
});

describe('demoPrivateKey', () => {
  it('带 DEMO 标记、PEM 边界与对应公钥', () => {
    const text = demoPrivateKey('ssh-ed25519 AAA test');
    const lines = text.split('\n');

    expect(lines[0]).toContain('DEMO ONLY');
    expect(lines[1]).toBe('-----BEGIN OPENSSH PRIVATE KEY-----');
    expect(lines).toContain('-----END OPENSSH PRIVATE KEY-----');
    expect(text.endsWith('# 对应公钥：ssh-ed25519 AAA test')).toBe(true);
  });

  it('每次生成的占位内容不同', () => {
    expect(demoPrivateKey('k')).not.toBe(demoPrivateKey('k'));
  });
});
