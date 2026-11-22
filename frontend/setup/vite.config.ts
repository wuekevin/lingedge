import vue from '@vitejs/plugin-vue';
import { defineConfig } from 'vite';

// 安装向导是独立的 Vite 工程（Story 1.37）：产物输出到 frontend/dist/setup，再由 scripts/sync-setup-embed.mjs 同步到
// internal/platform/setup/webdist 供 go:embed。base 固定 /setup/：server 在 /setup 与 /setup/assets/* 提供它。
// assetsInlineLimit=0：不把任何资源内联为 data: URI，服务端 CSP 因此可以保持 default-src 'none' 起步（只放行 self）。
export default defineConfig({
  base: '/setup/',
  plugins: [vue()],
  build: { outDir: '../dist/setup', emptyOutDir: true, assetsInlineLimit: 0, sourcemap: false, cssCodeSplit: false, chunkSizeWarningLimit: 700 },
});
