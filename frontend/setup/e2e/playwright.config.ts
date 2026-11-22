import { defineConfig } from '@playwright/test';

// 安装向导 E2E（Story 1.37）：被测对象是真实的 lingedge-server（setup mode）+ 真实 MySQL/Redis，由 Go 侧
// tests/integration/setup/browser_test.go 拉起并通过环境变量传入地址与随机凭据（仓库内没有任何固定口令，G-6）。
// 不要直接运行本目录的 spec（缺环境变量会立即失败）；入口是 `make e2e`。
export default defineConfig({
  testDir: '.',
  testMatch: '**/*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 180_000,
  expect: { timeout: 20_000 },
  reporter: [['list']],
  use: {
    baseURL: process.env.LE_E2E_BASE_URL,
    locale: process.env.LE_E2E_LOCALE ?? 'zh-CN',
    // 模拟系统配色偏好（Story 1.37b）：向导跟随系统 prefers-color-scheme。
    colorScheme: process.env.LE_E2E_SCHEME === 'dark' ? 'dark' : 'light',
    timezoneId: 'Asia/Shanghai',
    viewport: { width: 1280, height: 1000 },
    trace: 'off',
    video: 'off',
    screenshot: 'off',
    // 向导页面只允许同源：任何 CSP 违规都会在 console 里出现，由 helpers.watchConsole 断言为零。
    ignoreHTTPSErrors: false,
  },
  projects: [{ name: 'chromium', use: { browserName: 'chromium' } }],
});
