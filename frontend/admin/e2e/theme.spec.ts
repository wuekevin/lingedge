import type { Page } from '@playwright/test';
import { apiLogin, env, expect, expectContrast, expectTheme, fxUrl, prodUrl, recoveryCodes, settled, shot, test, watchConsole, type Scheme } from './helpers';

// 主题与对比度（AC3）：亮/暗/跟随系统；各页面、弹层、抽屉逐个做 WCAG 文字对比度审计（≥4.5:1）；审计器自测见 selftest.spec.ts。

async function audit(page: Page, scheme: Scheme, label: string) {
  await settled(page);
  await expectTheme(page, scheme);
  await expectContrast(page, `${scheme}/${label}`);
  await shot(page, `${scheme}-${label}`);
}

for (const scheme of ['light', 'dark'] as const) {
  test.describe(`${scheme}：跟随系统配色并逐页审计`, () => {
    test.use({ colorScheme: scheme });

    test(`${scheme}：登录页、TOTP 步骤、错误提示、改口令页`, async ({ page }) => {
      const c = watchConsole(page);
      await page.goto(`${prodUrl()}/login`);
      await expect(page.getByTestId('login-page')).toBeVisible();
      await audit(page, scheme, 'login');
      await page.getByTestId('login-submit').click(); // 校验错误态
      await expect(page.getByText('请填写邮箱')).toBeVisible();
      await audit(page, scheme, 'login-validation');
      await page.getByTestId('login-email').fill(env('LE_E2E_ADMIN_EMAIL'));
      await page.getByTestId('login-pw').fill(`Wr${Math.random().toString(36).slice(2)}!x`);
      await page.getByTestId('login-submit').click();
      await expect(page.getByTestId('login-error')).toBeVisible();
      await audit(page, scheme, 'login-error');
      await page.getByTestId('login-pw').fill(env('LE_E2E_ADMIN_PW'));
      await page.getByTestId('login-submit').click();
      await expect(page.getByTestId('totp-code')).toBeVisible();
      await audit(page, scheme, 'totp');
      await page.getByTestId('totp-toggle').click();
      await audit(page, scheme, 'totp-recovery');
      expect(c.problems).toEqual([]);
    });

    test(`${scheme}：外壳首页（生产构建空态）与 404`, async ({ page }) => {
      const c = watchConsole(page);
      await apiLogin(page, prodUrl(), env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
      await page.goto(`${prodUrl()}/`);
      await expect(page.getByTestId('home-page')).toBeVisible();
      await audit(page, scheme, 'prod-home');
      await page.goto(`${prodUrl()}/nope`);
      await expect(page.getByTestId('not-found-page')).toBeVisible();
      await audit(page, scheme, 'prod-404');
      expect(c.problems).toEqual([]);
    });

    test(`${scheme}：外壳（菜单、标签、夹具页、403、搜索弹层、标签确认弹层、账号菜单）`, async ({ page }) => {
      const c = watchConsole(page);
      const idx = scheme === 'light' ? 0 : 1;
      await apiLogin(page, fxUrl(), env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[idx]);
      await page.goto(`${fxUrl()}/ops/dirty-form`);
      await expect(page.getByTestId('fixture-page')).toBeVisible();
      await page.locator('[data-menu-item="fx.tasks"]').click();
      await page.locator('[data-menu-item="fx.audit"]').click();
      await audit(page, scheme, 'shell');
      // 搜索弹层
      await page.keyboard.press('Control+k');
      await expect(page.getByTestId('search-result').first()).toBeVisible();
      await audit(page, scheme, 'search');
      await page.getByTestId('search-input').fill('zzzz');
      await expect(page.locator('.arco-modal [data-le-state="empty"]')).toBeVisible();
      await audit(page, scheme, 'search-empty');
      await page.keyboard.press('Escape');
      // 有未保存更改的关闭确认弹层
      await page.locator('[data-menu-item="fx.dirty"]').click();
      await page.getByTestId('dirty-input').fill('x');
      await page.getByTestId('tab').filter({ hasText: 'Dirty form' }).getByTestId('tab-close').click();
      await expect(page.getByTestId('tab-dirty-confirm')).toBeVisible();
      await audit(page, scheme, 'tab-confirm');
      await expect(page.getByTestId('tab-dirty-confirm'), 'confirm dialog must still be open after the audit').toBeVisible();
      await page.getByRole('button', { name: '取消' }).click();
      // 账号菜单
      await page.getByTestId('account-menu').click();
      await expect(page.getByTestId('logout')).toBeVisible();
      await audit(page, scheme, 'account-menu');
      await expect(page.getByTestId('logout'), 'account menu must still be open after the audit').toBeVisible();
      await page.keyboard.press('Escape');
      // 404
      await page.goto(`${fxUrl()}/nodes/list`);
      await expect(page.getByTestId('not-found-page')).toBeVisible();
      await audit(page, scheme, 'fx-404');
      expect(c.problems).toEqual([]);
    });

    test(`${scheme}：403 页与手机抽屉`, async ({ page }) => {
      const c = watchConsole(page);
      await apiLogin(page, fxUrl(), env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
      await page.goto(`${fxUrl()}/ops/rollback`);
      await expect(page.getByTestId('forbidden-page')).toBeVisible();
      await audit(page, scheme, '403');
      await page.setViewportSize({ width: 390, height: 844 });
      await page.goto(`${fxUrl()}/settings/rule-editor`);
      await expect(page.getByTestId('mobile-complex-notice')).toBeVisible();
      await audit(page, scheme, 'mobile-notice');
      await page.getByTestId('nav-toggle').click();
      await expect(page.locator('.arco-drawer [data-menu-item]').first()).toBeVisible();
      await audit(page, scheme, 'mobile-drawer');
      expect(c.problems).toEqual([]);
    });
  });
}

test.describe('主题切换与记忆', () => {
  test.use({ colorScheme: 'light' });
  test('显式选择暗色：立即生效、刷新后保持；选回「跟随系统」后随系统配色实时变化', async ({ page }) => {
    await page.goto(`${prodUrl()}/login`);
    await expectTheme(page, 'light');
    await page.getByTestId('theme-select').selectOption('dark');
    await expectTheme(page, 'dark');
    await page.reload();
    await expectTheme(page, 'dark'); // 首帧即暗色（已记忆）
    await expect(page.getByTestId('theme-select')).toHaveValue('dark');
    await page.getByTestId('theme-select').selectOption('auto');
    await page.emulateMedia({ colorScheme: 'light' });
    await expectTheme(page, 'light');
    await page.emulateMedia({ colorScheme: 'dark' });
    await expectTheme(page, 'dark'); // 跟随系统实时切换
    await page.getByTestId('theme-select').selectOption('light');
    await page.emulateMedia({ colorScheme: 'dark' });
    await expectTheme(page, 'light'); // 显式选择优先于系统
  });
});
