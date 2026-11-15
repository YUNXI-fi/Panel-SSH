/**
 * 文件预览的类型判定。
 *
 * 判断只依赖文件名，不读内容：双击一个文件时要在发请求之前就知道该开哪种
 * 查看器 —— 图片/音视频走预览接口，文本走正文接口，其余直接下载。
 * 猜错了的代价是用户看到一个打不开的查看器，所以宁可不认。
 */

export type PreviewKind =
  | 'code'
  | 'text'
  | 'markdown'
  | 'html'
  | 'image'
  | 'audio'
  | 'video'
  | 'pdf'
  | 'none';

/** 超过这个字符数就不做语法着色：着色是纯前端的正则扫描，
 *  上百万字符会把窗口卡住好几秒，而那种体量的文件本来也没法读。 */
export const MAX_HIGHLIGHT_CHARS = 200_000;

const IMAGE_EXT = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'ico', 'avif', 'svg']);

const AUDIO_EXT = new Set(['mp3', 'wav', 'ogg', 'oga', 'opus', 'flac', 'm4a', 'aac']);

const VIDEO_EXT = new Set(['mp4', 'm4v', 'webm', 'ogv', 'mov', 'mkv']);

/** 扩展名 → highlight.js 语言名。 */
const LANGUAGE_BY_EXT: Record<string, string> = {
  js: 'javascript',
  jsx: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  ts: 'typescript',
  tsx: 'typescript',
  json: 'json',
  jsonc: 'json',
  html: 'xml',
  htm: 'xml',
  xml: 'xml',
  svg: 'xml',
  vue: 'xml',
  svelte: 'xml',
  css: 'css',
  scss: 'css',
  less: 'css',
  md: 'markdown',
  markdown: 'markdown',
  sh: 'bash',
  bash: 'bash',
  zsh: 'bash',
  fish: 'bash',
  env: 'bash',
  yml: 'yaml',
  yaml: 'yaml',
  go: 'go',
  py: 'python',
  java: 'java',
  c: 'c',
  h: 'c',
  cc: 'cpp',
  cpp: 'cpp',
  cxx: 'cpp',
  hpp: 'cpp',
  cs: 'csharp',
  rs: 'rust',
  php: 'php',
  rb: 'ruby',
  sql: 'sql',
  // highlight.js 没有独立的 TOML 语法；INI 的"节 + 键值"规则与它最接近，
  // 着色结果至少能把节名与值分开。
  toml: 'ini',
  ini: 'ini',
  conf: 'ini',
  cfg: 'ini',
  properties: 'ini',
  service: 'ini',
  timer: 'ini',
  socket: 'ini',
  ps1: 'powershell',
};

/** 没有扩展名、但确定是文本的常见文件。 */
const LANGUAGE_BY_NAME: Record<string, string> = {
  dockerfile: 'dockerfile',
  makefile: 'makefile',
  nginx: 'nginx',
};

/** 已知是文本、但没有专门语法着色的扩展名。 */
const TEXT_EXT = new Set([
  'txt',
  'log',
  'csv',
  'tsv',
  'lock',
  'rules',
  'list',
  'gitignore',
  'editorconfig',
  'gradle',
]);

/** 常见的无扩展名文本文件（含点开头的配置）。 */
const TEXT_NAMES = new Set([
  'readme',
  'license',
  'changelog',
  'authors',
  'notice',
  '.gitignore',
  '.gitconfig',
  '.editorconfig',
  '.bashrc',
  '.zshrc',
  '.profile',
  '.vimrc',
  '.env',
]);

function extensionOf(name: string): string {
  const lower = name.toLowerCase();
  const index = lower.lastIndexOf('.');
  // `.env` 这种"点开头的名字"不是扩展名。
  return index > 0 ? lower.slice(index + 1) : '';
}

export function previewKindOf(name: string): PreviewKind {
  const lower = name.toLowerCase();
  const ext = extensionOf(lower);

  if (IMAGE_EXT.has(ext)) return 'image';
  if (AUDIO_EXT.has(ext)) return 'audio';
  if (VIDEO_EXT.has(ext)) return 'video';
  if (ext === 'pdf') return 'pdf';
  if (ext === 'md' || ext === 'markdown') return 'markdown';
  if (ext === 'html' || ext === 'htm') return 'html';

  if (LANGUAGE_BY_EXT[ext]) return 'code';
  if (LANGUAGE_BY_NAME[lower]) return 'code';
  if (TEXT_EXT.has(ext) || TEXT_NAMES.has(lower)) return 'text';

  return 'none';
}

/** 该文件用哪个语言着色；没有对应语言时返回 null（按纯文本显示）。 */
export function languageOf(name: string): string | null {
  const lower = name.toLowerCase();
  return LANGUAGE_BY_EXT[extensionOf(lower)] ?? LANGUAGE_BY_NAME[lower] ?? null;
}

/** 走正文接口、可以预览与编辑的类型。 */
export function isTextKind(kind: PreviewKind): boolean {
  return kind === 'code' || kind === 'text' || kind === 'markdown' || kind === 'html';
}

/** 可以编辑的类型：二进制文件没有"编辑"这回事。 */
export function isEditable(kind: PreviewKind): boolean {
  return isTextKind(kind);
}

/**
 * 把 Markdown 里的相对图片路径解析成远端绝对路径。
 *
 * 只处理相对/绝对路径，`http(s)` 与 `data:image` 原样返回，其它协议一律拒绝 ——
 * `![](javascript:...)` 在 markdown 里是合法写法。
 */
export function resolveMarkdownAsset(filePath: string, raw: string): string | null {
  const src = raw.trim().split(/[?#]/)[0];
  if (src === '') return null;
  if (/^(https?:\/\/)/i.test(src) || /^data:image\//i.test(src)) return src;
  if (/^[a-z][a-z0-9+.-]*:/i.test(src)) return null;

  // 绝对路径直接用；相对路径以 Markdown 文件所在目录为基准。
  if (src.startsWith('/')) return src;
  const base = filePath.slice(0, filePath.lastIndexOf('/')) || '/';
  const parts = base.split('/').filter(Boolean);
  for (const segment of src.split('/')) {
    if (segment === '' || segment === '.') continue;
    if (segment === '..') parts.pop();
    else parts.push(segment);
  }
  return `/${parts.join('/')}`;
}
