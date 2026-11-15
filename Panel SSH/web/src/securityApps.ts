/**
 * 网络安全应用的图标映射。
 *
 * 应用中心、桌面图标与安全应用窗口共用这一份：同一款工具在三处必须
 * 长得一样，否则用户会以为桌面上多了一个陌生应用。
 * 目录里出现未登记的新条目时回退到盾牌。
 */
const SECURITY_GLYPHS: Record<string, string> = {
  nmap: 'sec-scan',
  tcpdump: 'sec-packet',
  ufw: 'sec-shield',
  fail2ban: 'sec-ban',
  clamav: 'sec-bug',
  'wireguard-tools': 'sec-tunnel',
  openvpn: 'sec-vpn',
  nethogs: 'sec-traffic',
  tor: 'sec-onion',
  gnupg: 'sec-key',
  pass: 'sec-lock',
  lynis: 'sec-audit',
  rkhunter: 'sec-magnifier',
  chkrootkit: 'sec-crosshair',
  firewalld: 'sec-wall',
};

export function securityGlyph(slug: string): string {
  return SECURITY_GLYPHS[slug] ?? 'sec-shield';
}
