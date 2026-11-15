import { ref } from 'vue';

/**
 * 桌面壁纸。
 *
 * 与主题一样只保存在当前浏览器：壁纸是"这台设备看着顺眼"的事，
 * 不该写进服务端配置，也不该让管理员替所有人决定。
 *
 * 内置预设只输出背景图层（见 global.css 的 .wp--*），颜色全部引用主题变量，
 * 因此切主题时壁纸会跟着换配色；上传的图片则是固定像素，靠遮罩保证窗口可读。
 */

export type WallpaperId = 'default' | 'plain' | 'grid' | 'aurora';

export interface WallpaperPreset {
  id: WallpaperId;
  name: string;
}

export const WALLPAPER_PRESETS: WallpaperPreset[] = [
  { id: 'default', name: '默认' },
  { id: 'plain', name: '纯色' },
  { id: 'grid', name: '细网格' },
  { id: 'aurora', name: '双色辉光' },
];

const STORAGE_KEY = 'panelssh.wallpaper';

/**
 * localStorage 在 Chrome 里按 UTF-16 计费：5MB 配额约等于 250 万字符。
 * 留出余量后按 200 万字符卡上限，超出的图片直接拒绝，而不是让存档静默失败。
 */
const MAX_DATA_URL = 2_000_000;

/** 上传图片统一压到 1920px 长边，再按 WebP 编码，兼顾清晰度与体积。 */
const MAX_EDGE = 1920;

export const wallpaperPreset = ref<WallpaperId>('default');
export const wallpaperImage = ref('');

function isWallpaperId(value: unknown): value is WallpaperId {
  return WALLPAPER_PRESETS.some((item) => item.id === value);
}

// 模块加载时同步读档：Desktop 挂载时壁纸已经是最终值，不会先闪一下默认底衬。
try {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (raw) {
    const saved = JSON.parse(raw) as { preset?: unknown; image?: unknown };
    if (typeof saved.image === 'string' && saved.image.startsWith('data:image/')) {
      wallpaperImage.value = saved.image;
    } else if (isWallpaperId(saved.preset)) {
      wallpaperPreset.value = saved.preset;
    }
  }
} catch {
  // 隐私模式下访问 localStorage 会抛错，存档损坏也可能抛：都退回默认壁纸。
}

function persist(): boolean {
  const payload = wallpaperImage.value
    ? { image: wallpaperImage.value }
    : { preset: wallpaperPreset.value };
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(payload));
    return true;
  } catch {
    return false;
  }
}

/** 选内置预设：预设置为当前项，同时清掉已上传的图片（壁纸只有一个槽位）。 */
export function selectWallpaper(id: WallpaperId): void {
  wallpaperPreset.value = id;
  wallpaperImage.value = '';
  persist();
}

/** 移除自定义图片，回到最近一次选中的预设。 */
export function clearWallpaper(): void {
  wallpaperImage.value = '';
  persist();
}

/**
 * 把上传的文件变成壁纸：先等比缩到长边 1920，再编码成 WebP。
 * 体积仍超上限时只降一次质量；还不行就报错，交给界面提示用户换图。
 */
async function fileToDataUrl(file: File): Promise<string> {
  let bitmap: ImageBitmap;
  try {
    bitmap = await createImageBitmap(file);
  } catch {
    throw new Error('无法读取这张图片，请换一张');
  }

  const scale = Math.min(1, MAX_EDGE / Math.max(bitmap.width, bitmap.height));
  const canvas = document.createElement('canvas');
  canvas.width = Math.max(1, Math.round(bitmap.width * scale));
  canvas.height = Math.max(1, Math.round(bitmap.height * scale));

  const ctx = canvas.getContext('2d');
  if (!ctx) {
    bitmap.close();
    throw new Error('浏览器无法处理这张图片');
  }
  ctx.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
  bitmap.close();

  const encode = (quality: number): string => canvas.toDataURL('image/webp', quality);
  let data = encode(0.82);
  if (data.length > MAX_DATA_URL) data = encode(0.62);
  if (data.length > MAX_DATA_URL) throw new Error('这张图片太大，请换一张分辨率更低的');
  return data;
}

/** 设置自定义壁纸；失败时保持原壁纸不变，并把原因抛给调用方提示。 */
export async function useWallpaperFile(file: File): Promise<void> {
  if (!file.type.startsWith('image/')) throw new Error('请选择图片文件');

  const data = await fileToDataUrl(file);
  const previous = wallpaperImage.value;
  wallpaperImage.value = data;
  if (!persist()) {
    wallpaperImage.value = previous;
    throw new Error('这张图片太大，浏览器存不下，请换一张');
  }
}
