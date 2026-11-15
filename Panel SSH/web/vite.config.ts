import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

// 后端默认监听地址，开发态由 Vite 反向代理转发 API 与 WebSocket。
const BACKEND = process.env.PANELSSH_BACKEND ?? 'http://127.0.0.1:8080';

export default defineConfig({
  plugins: [vue()],
  build: {
    // 直接产出到 Go 的 embed 目录，省掉一次拷贝步骤。
    outDir: '../internal/web/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': {
        target: BACKEND,
        // 必须保持 false：后端 WebSocket 使用默认的同源 Origin 校验，
        // 一旦改写 Host，Origin 与 Host 不一致会导致握手被拒。
        changeOrigin: false,
        ws: true,
      },
    },
  },
});
