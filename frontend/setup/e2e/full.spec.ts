import { expect, test } from '@playwright/test';
import { env, optEnv, shot, watchConsole } from './helpers';
import { flags, passGate, stepAdmin, stepComplete, stepDb, stepEnv, stepInit, stepKeys, stepRedis, stepSystem } from './flow';

// 全流程（AC1–AC5）：验证令牌 → 8 步 → 完成页；TCP 或 Unix socket（LE_E2E_CONN）、可选 TOTP（LE_E2E_TOTP）、中/英文（LE_E2E_LOCALE）。
test('向导全流程（真实 server setup mode + 真实 MySQL/Redis）', async ({ page }) => {
  const scenario = optEnv('LE_E2E_SCENARIO') || 'full';
  const con = watchConsole(page);
  await passGate(page, scenario);
  await stepEnv(page, scenario);
  await stepDb(page, scenario, flags.socket() ? 'socket' : 'tcp');
  await stepRedis(page, scenario);
  await stepInit(page, scenario);
  await stepAdmin(page, scenario, { totp: flags.totp() });
  await stepSystem(page, scenario);
  await stepKeys(page, scenario);
  await stepComplete(page, scenario);
  expect(con.problems, '向导流程中的控制台/CSP 问题').toEqual([]);

  // 刷新后需要重新输入 token（token 不在任何存储里）；此时 server 已切到正常模式，/setup 是 410。
  await expect
    .poll(
      async () => {
        // 模式切换瞬间监听会短暂关闭（ECONNREFUSED），视为「尚未就绪」继续轮询。
        try {
          return (await page.request.get('/setup')).status();
        } catch {
          return 0;
        }
      },
      { timeout: 60_000, message: '/setup 在 complete 之后应永久 410' },
    )
    .toBe(410);
  const gone = await page.request.get('/setup');
  expect((await gone.json()).error.code).toBe('setup.already_installed');
  const rid = (await gone.json()).error.request_id as string;
  expect(rid.length).toBeGreaterThan(0);
  const api = await page.request.get('/api/v1/public/setup/status', { headers: { 'X-Setup-Token': env('LE_E2E_TOKEN') } });
  expect(api.status()).toBe(410);
  expect((await api.json()).error.code).toBe('setup.already_installed');
  // 浏览器里直接打开 /setup 得到 410（JSON 错误体，含稳定错误码）
  const resp = await page.goto('/setup');
  expect(resp?.status()).toBe(410);
  await shot(page, `${scenario}-09-after-complete-410`);
});
