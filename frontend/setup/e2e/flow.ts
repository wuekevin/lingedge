// 向导各步骤的页面驱动（真实 DOM、真实 server）。选择器用 data-testid（语言无关），关键文案另行按语言断言。
import { expect, type Page } from '@playwright/test';
import { env, expectContrast, expectTokenNotPersisted, optEnv, shot, totpCode, writeOut } from './helpers';

const T = (page: Page, id: string) => page.getByTestId(id);

export async function openGate(page: Page): Promise<void> {
  await page.goto('/setup');
  await expect(T(page, 'gate')).toBeVisible();
}

export async function enterToken(page: Page, token = env('LE_E2E_TOKEN')): Promise<void> {
  await T(page, 'f-setup_token').fill(token);
  await T(page, 'gate-submit').click();
}

export async function passGate(page: Page, scenario: string): Promise<void> {
  await openGate(page);
  await shot(page, `${scenario}-00-gate`);
  await expectContrast(page, 'gate');
  await enterToken(page);
  await expect(T(page, 'stepper')).toBeVisible();
  await expectTokenNotPersisted(page, env('LE_E2E_TOKEN'));
}

export async function stepEnv(page: Page, scenario: string): Promise<void> {
  await expect(T(page, 'step-environment')).toBeVisible();
  await expect(T(page, 'env-items')).toBeVisible();
  await expect(T(page, 'env-blocking')).toHaveCount(0);
  await shot(page, `${scenario}-01-environment`);
  await expectContrast(page, 'environment');
  await T(page, 'nav-next').click();
}

export async function fillDbForm(page: Page, opts: { dbName: string; conn?: 'tcp' | 'socket'; skipSubmit?: boolean }): Promise<void> {
  await expect(T(page, 'step-database')).toBeVisible();
  if (opts.conn === 'socket') {
    await T(page, 'r-conn-socket').check();
    await T(page, 'f-socket').fill(env('LE_E2E_DB_SOCKET'));
  } else {
    await T(page, 'f-host').fill(env('LE_E2E_DB_HOST'));
    await T(page, 'f-port').fill(env('LE_E2E_DB_PORT'));
  }
  await T(page, 'f-database').fill(opts.dbName);
  await T(page, 'f-admin_username').fill(env('LE_E2E_DB_ADMIN_USER'));
  await T(page, 'f-admin_password').fill(env('LE_E2E_DB_ADMIN_PW'));
  if (!opts.skipSubmit) await T(page, 'db-submit').click();
}

export async function stepDb(page: Page, scenario: string, conn: 'tcp' | 'socket'): Promise<void> {
  await expect(T(page, 'step-database')).toBeVisible();
  await expect(T(page, 'db-empty')).toBeVisible(); // 三态：尚未提交的空态
  await shot(page, `${scenario}-02-database-empty`);
  await expectContrast(page, 'database(empty)');
  await fillDbForm(page, { dbName: env('LE_E2E_DB_NAME'), conn });
  await expect(T(page, 'db-ok')).toBeVisible();
  // 口令字段已被清空（不回显、不保留）
  await expect(T(page, 'f-admin_password')).toHaveValue('');
  await shot(page, `${scenario}-02-database-ok`);
  await expectContrast(page, 'database(ok)');
  await T(page, 'nav-next').click();
}

export async function stepRedis(page: Page, scenario: string): Promise<void> {
  await expect(T(page, 'step-redis')).toBeVisible();
  await T(page, 'f-address').fill(env('LE_E2E_REDIS_ADDR'));
  await T(page, 'redis-submit').click();
  await expect(T(page, 'redis-ok')).toBeVisible();
  await shot(page, `${scenario}-03-redis`);
  await T(page, 'nav-next').click();
}

export async function stepInit(page: Page, scenario: string): Promise<void> {
  await expect(T(page, 'step-init')).toBeVisible();
  await shot(page, `${scenario}-04-init-before`);
  await T(page, 'init-run').click();
  await expect(T(page, 'init-ok')).toBeVisible({ timeout: 120_000 });
  const rows = page.locator('[data-testid="init-migrations"] tbody tr');
  expect(await rows.count()).toBeGreaterThanOrEqual(18);
  const classes = new Set(await rows.evaluateAll((els) => els.map((e) => e.getAttribute('data-category'))));
  for (const c of classes) expect(['reversible', 'expand', 'irreversible']).toContain(c);
  await shot(page, `${scenario}-04-init-ok`);
  await expectContrast(page, 'init');
  await T(page, 'nav-next').click();
}

export interface AdminOpts {
  totp: boolean;
}
export async function stepAdmin(page: Page, scenario: string, o: AdminOpts): Promise<void> {
  await expect(T(page, 'step-admin')).toBeVisible();
  await T(page, 'f-email').fill(env('LE_E2E_ADMIN_EMAIL'));
  await T(page, 'f-display_name').fill('E2E Admin');
  // 口令强度提示随输入变化
  await T(page, 'f-new_password').fill('short');
  await expect(T(page, 'strength-text')).toHaveAttribute('data-score', '1');
  await T(page, 'f-new_password').fill(env('LE_E2E_ADMIN_PW'));
  expect(Number(await T(page, 'strength-text').getAttribute('data-score'))).toBeGreaterThanOrEqual(2);
  // 二次确认：不一致被拦截
  await T(page, 'f-confirm_password').fill(`${env('LE_E2E_ADMIN_PW')}x`);
  await T(page, 'admin-submit').click();
  await expect(page.locator('.field__error').first()).toBeVisible();
  await shot(page, `${scenario}-05-admin-mismatch`);
  await T(page, 'f-confirm_password').fill(env('LE_E2E_ADMIN_PW'));
  // 不回显：口令框是 password 类型，页面文本里没有口令
  await expect(T(page, 'f-new_password')).toHaveAttribute('type', 'password');
  expect(await page.locator('body').innerText()).not.toContain(env('LE_E2E_ADMIN_PW'));
  await shot(page, `${scenario}-05-admin-form`);
  await expectContrast(page, 'admin(form)');
  if (o.totp) await T(page, 'c-enable_totp').check();
  await T(page, 'admin-submit').click();
  if (o.totp) {
    await expect(T(page, 'admin-totp')).toBeVisible();
    const secret = (await T(page, 'totp-secret').innerText()).trim();
    expect(secret).toMatch(/^[A-Z2-7]{16,}$/);
    await expect(T(page, 'totp-qr')).toBeVisible();
    await shot(page, `${scenario}-05-admin-totp`);
    await expectContrast(page, 'admin(totp)');
    await T(page, 'f-totp_code').fill(totpCode(secret));
    await T(page, 'totp-confirm').click();
    await expect(T(page, 'admin-recovery')).toBeVisible();
    const codes = (await page.locator('[data-testid="recovery-codes"] li').allInnerTexts()).map((s) => s.trim());
    expect(codes.length).toBeGreaterThanOrEqual(8);
    await expect(T(page, 'recovery-continue')).toBeDisabled();
    await shot(page, `${scenario}-05-admin-recovery`);
    await expectContrast(page, 'admin(recovery)');
    await T(page, 'c-recovery_ack').check();
    await T(page, 'recovery-continue').click();
    writeOut({ totpSecret: secret, recoveryCodes: codes });
  }
  await expect(T(page, 'admin-ok')).toBeVisible();
  await expect(T(page, 'f-new_password')).toHaveCount(0);
  await shot(page, `${scenario}-05-admin-ok`);
  await T(page, 'nav-next').click();
}

export async function stepSystem(page: Page, scenario: string): Promise<void> {
  await expect(T(page, 'step-system')).toBeVisible();
  await T(page, 'f-base_url').fill(env('LE_E2E_ADMIN_URL'));
  await T(page, 'f-brand_name').fill('E2E Cloud');
  await T(page, 'f-timezone').fill('Asia/Shanghai');
  await shot(page, `${scenario}-06-system`);
  await expectContrast(page, 'system');
  await T(page, 'system-submit').click();
  await expect(T(page, 'system-ok')).toBeVisible();
  await T(page, 'nav-next').click();
}

export async function stepKeys(page: Page, scenario: string): Promise<void> {
  await expect(T(page, 'step-keys')).toBeVisible();
  await expect(T(page, 'master-key-notice')).toContainText('master.key');
  await T(page, 'keys-run').click();
  await expect(T(page, 'keys-ok')).toBeVisible({ timeout: 60_000 });
  await shot(page, `${scenario}-07-keys`);
  await expectContrast(page, 'keys');
  await T(page, 'nav-next').click();
}

export async function stepComplete(page: Page, scenario: string): Promise<{ migratePassword: string }> {
  await expect(T(page, 'step-complete')).toBeVisible();
  await expect(T(page, 'master-key-notice')).toBeVisible();
  await expect(T(page, 'complete-submit')).toBeDisabled(); // 未确认 master.key 前不能完成
  await shot(page, `${scenario}-08-complete-before`);
  await expectContrast(page, 'complete(before)');
  await T(page, 'c-master_key_ack').check();
  await T(page, 'complete-submit').click();
  await expect(T(page, 'complete-done')).toBeVisible();
  const href = await T(page, 'admin-url').getAttribute('href');
  expect(href).toBe(env('LE_E2E_ADMIN_URL'));
  await expect(T(page, 'master-key-notice')).toBeVisible(); // 完成后依然强提示
  let migratePassword = '';
  if (await T(page, 'migrate-password').count()) {
    migratePassword = (await T(page, 'migrate-password').innerText()).trim();
    expect(migratePassword.length).toBeGreaterThanOrEqual(16);
    await expect(T(page, 'migrate-command')).toHaveText('sudo lingedge-server migrate credential init');
  }
  await shot(page, `${scenario}-08-complete-done`);
  await expectContrast(page, 'complete(done)');
  await expectTokenNotPersisted(page, env('LE_E2E_TOKEN'));
  writeOut({ migratePassword, adminUrl: href });
  return { migratePassword };
}

export const flags = { socket: () => optEnv('LE_E2E_CONN') === 'socket', totp: () => optEnv('LE_E2E_TOTP') === '1' };
