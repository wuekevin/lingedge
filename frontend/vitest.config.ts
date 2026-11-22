import vue from '@vitejs/plugin-vue';
import { defineConfig } from 'vitest/config';

// 四个 project：shared（组件/格式化/i18n/令牌，happy-dom）、setup（安装向导组件，happy-dom）、admin（Admin 外壳，happy-dom）与 scripts（门禁脚本自测，node）。
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
        // 安装向导（Story 1.37）：组件测试（happy-dom）。E2E 在 setup/e2e，由 Playwright 运行，不属于 vitest。
        plugins: [vue()],
        test: {
          name: 'setup',
          environment: 'happy-dom',
          include: ['setup/src/**/*.test.ts'],
          css: false,
        },
      },
      {
        // Admin（Story 1.38）：守卫/菜单裁剪/注册表/三态/标签/搜索/横幅/登录等的单元与组件测试（happy-dom）。E2E 在 admin/e2e，由 Playwright 运行。
        plugins: [vue()],
        test: {
          name: 'admin',
          environment: 'happy-dom',
          include: ['admin/src/**/*.test.ts'],
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
