import vue from '@vitejs/plugin-vue';
import { defineConfig } from 'vite';

// admin 是独立的 Vite 工程与构建产物（ADR-014）：
//   - 生产构建（vite build）→ frontend/dist/admin：由 scripts/check-isolation.mjs（不含 Console realm 常量）与 check-mock.mjs（不含 mock 入口）检查；
//   - e2e 构建（vite build --mode e2e）→ frontend/dist/admin-e2e：额外包含 e2e 夹具页面（src/mocks），只供 Playwright 使用，不是发行物，不被任何检查当作生产产物。
export default defineConfig(({ mode }) => ({
  base: '/',
  plugins: [vue()],
  build: {
    outDir: mode === 'e2e' ? '../dist/admin-e2e' : '../dist/admin',
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 1200,
  },
  // 开发服务器：把同源的 /api 代理到本机 lingedge-server（Cookie 为 host-only，必须同源；生产由 Nginx 反代，见 docs/architecture/tools-design.md）。
  server: { proxy: { '/api': { target: process.env.LE_ADMIN_DEV_API ?? 'http://127.0.0.1:8080', changeOrigin: false } } },
}));
