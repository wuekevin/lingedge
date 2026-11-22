import vue from '@vitejs/plugin-vue';
import { defineConfig } from 'vitest/config';

// 两个 project：shared（组件/格式化/i18n/令牌，happy-dom）与 scripts（门禁脚本自测，node）。
export default defineConfig({
  test: {
    projects: [
      {
        plugins: [vue()],
        test: {
          name: 'shared',
          environment: 'happy-dom',
          include: ['shared/**/*.test.ts'],
          css: false,
        },
      },
      {
        test: {
          name: 'scripts',
          environment: 'node',
          include: ['scripts/**/*.test.mjs'],
          testTimeout: 60000,
        },
      },
    ],
  },
});
