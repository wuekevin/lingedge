import { expect, test } from '@playwright/test';
import { env, expectContrast, expectTokenNotPersisted, optEnv, shot } from './helpers';
import { enterToken, openGate } from './flow';

// 错 token 路径（AC1/AC2/AC4）：错误码与 Request ID、令牌不在 URL/存储、刷新后重输、限流（含之后携带正确 token 的请求）。
test('错 token：稳定错误码 + Request ID；URL 里的 token 被忽略；限流后正确 token 也被拒', async ({ page }) => {
  const scenario = optEnv('LE_E2E_SCENARIO') || 'token';
  await openGate(page);
  await shot(page, `${scenario}-00-gate`);

  // 1) 错误 token
  await enterToken(page, 'definitely-the-wrong-token-0123456789abcdef');
  await expect(page.getByTestId('error-code')).toHaveText('setup.token_invalid');
  const rid = (await page.locator('[data-le-request-id]').innerText()).trim();
  expect(rid.length).toBeGreaterThan(0);
  await expect(page.getByTestId('error-hint')).toBeVisible();
  await expect(page.getByTestId('f-setup_token')).toHaveValue(''); // 输入已清空
  await expect(page.getByTestId('stepper')).toHaveCount(0); // 没有进入向导
  await shot(page, `${scenario}-01-bad-token`);
  await expectContrast(page, 'bad token');
  await expectTokenNotPersisted(page, 'definitely-the-wrong-token-0123456789abcdef');

  // 2) token 放在 URL 里：被忽略并从地址栏移除，不会自动登录
  await page.goto(`/setup?token=${env('LE_E2E_TOKEN')}&x=1`);
  await expect(page.getByTestId('url-token-ignored')).toBeVisible();
  expect(page.url()).not.toContain(env('LE_E2E_TOKEN'));
  expect(page.url()).toContain('x=1');
  await expect(page.getByTestId('gate')).toBeVisible();
  await shot(page, `${scenario}-02-url-token-ignored`);

  // 3) 正确 token 进入；刷新后需要重新输入
  await enterToken(page);
  await expect(page.getByTestId('stepper')).toBeVisible();
  await expectTokenNotPersisted(page, env('LE_E2E_TOKEN'));
  await page.reload();
  await expect(page.getByTestId('gate')).toBeVisible();
  await expect(page.getByTestId('stepper')).toHaveCount(0);

  // 4) 限流：连续错误尝试超过上限（缺省 5 分钟 10 次）→ 429，此后携带正确 token 的请求也被拒
  for (let i = 0; i < 12; i++) {
    const r = await page.request.get('/api/v1/public/setup/status', { headers: { 'X-Setup-Token': `bad-${i}` } });
    expect([401, 429]).toContain(r.status());
  }
  await enterToken(page);
  await expect(page.getByTestId('error-code')).toHaveText('rate_limit.exceeded');
  await expect(page.locator('[data-le-request-id]')).toBeVisible();
  await expect(page.getByTestId('stepper')).toHaveCount(0);
  await shot(page, `${scenario}-03-rate-limited`);
  await expectContrast(page, 'rate limited');
});
