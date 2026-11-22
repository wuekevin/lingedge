import { expect, test } from '@playwright/test';
import { env, expectContrast, optEnv, shot } from './helpers';
import { fillDbForm, passGate, stepEnv } from './flow';

// 非空库（FR-6）：目标库已有数据时拒绝并给出稳定错误码与 Request ID，不覆盖；换成空库后可继续。
test('非空库被拒绝（setup.database_not_empty），换空库后成功', async ({ page }) => {
  const scenario = optEnv('LE_E2E_SCENARIO') || 'nonempty';
  await passGate(page, scenario);
  await stepEnv(page, scenario);
  await fillDbForm(page, { dbName: env('LE_E2E_NONEMPTY_DB') });
  await expect(page.getByTestId('error-code')).toHaveText('setup.database_not_empty');
  await expect(page.locator('[data-le-request-id]')).toBeVisible();
  await expect(page.getByTestId('error-hint')).toBeVisible();
  await expect(page.getByTestId('error-message')).toHaveAttribute('data-localized', 'true'); // 已知错误码走词条，不是服务端原文
  await expect(page.getByTestId('error-reason').first()).toContainText('already contains'); // 英文界面下的详细原因（真实 server 的 reason 句式）
  await expect(page.getByTestId('db-ok')).toHaveCount(0);
  await expect(page.getByTestId('nav-next')).toBeDisabled(); // 失败不放行
  await shot(page, `${scenario}-01-not-empty`);
  await expectContrast(page, 'not empty');
  // 表单仍在，口令已不回显；换成空库名后成功
  await page.getByTestId('f-database').fill(env('LE_E2E_DB_NAME'));
  await page.getByTestId('f-admin_password').fill(env('LE_E2E_DB_ADMIN_PW'));
  await page.getByTestId('db-submit').click();
  await expect(page.getByTestId('db-ok')).toBeVisible();
  await shot(page, `${scenario}-02-fresh-db-ok`);
});
