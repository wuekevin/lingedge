import { defineConfig } from '@playwright/test';

// Admin E2E（Story 1.38）：被测对象是真实的 lingedge-server（正常模式，先经 setup 向导安装）+ 真实 MySQL/Redis + 真实 Chromium，
// 由 Go 侧 tests/integration/setup/adminweb_browser_test.go 拉起并通过环境变量传入地址与随机凭据（仓库内没有任何固定口令，G-6）。
// 两个静态服务（生产构建 / e2e 构建）由 webServer 拉起并在结束时回收。不要直接运行本目录的 spec；入口是 `make e2e`。
const env = (n: string) => process.env[n] ?? '';
const selftest = process.env.LE_E2E_SELFTEST === '1';

export default defineConfig({
  testDir: '.',
  testMatch: selftest ? 'selftest.spec.ts' : ['login.spec.ts', 'shell.spec.ts', 'mobile.spec.ts', 'theme.spec.ts', 'system.spec.ts', 'system-settings.spec.ts'],
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  use: {
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    viewport: { width: 1280, height: 900 },
    trace: 'off',
    video: 'off',
    screenshot: 'off',
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
  webServer: selftest
    ? undefined
    : [
        { command: 'node serve.mjs', url: `http://127.0.0.1:${env('LE_E2E_PORT_PROD')}/`, reuseExistingServer: false, timeout: 30_000, env: { ...(process.env as Record<string, string>), LE_E2E_SERVE_DIST: env('LE_E2E_DIST_PROD'), LE_E2E_PORT: env('LE_E2E_PORT_PROD'), LE_E2E_API: env('LE_E2E_API') } },
        { command: 'node serve.mjs', url: `http://127.0.0.1:${env('LE_E2E_PORT_FX')}/`, reuseExistingServer: false, timeout: 30_000, env: { ...(process.env as Record<string, string>), LE_E2E_SERVE_DIST: env('LE_E2E_DIST_FX'), LE_E2E_PORT: env('LE_E2E_PORT_FX'), LE_E2E_API: env('LE_E2E_API') } },
      ],
});
