import vue from '@vitejs/plugin-vue';
import { defineConfig } from 'vite';

// console 是独立的 Vite 工程与构建产物（ADR-014）：输出到 frontend/dist/console，供 scripts/check-isolation.mjs 与 check-mock.mjs 检查。
export default defineConfig({
  plugins: [vue()],
  // 基座探针整包引入 Arco（实际应用改为按需引入），故放宽单包体积告警。
  build: { outDir: '../dist/console', emptyOutDir: true, chunkSizeWarningLimit: 1200 },
});
