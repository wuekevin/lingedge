import type { Page } from '@playwright/test';
import { apiLogin, env, expect, fxUrl, recoveryCodes, settled, shot, test, watchConsole } from './helpers';

// 手机宽度（AC4、UX-23）——真实 server × e2e 构建。承诺范围：查看状态/告警、触发回滚、节点维护入口可用；复杂编辑可用但不保证体验（如实标注）。
test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

const noOverflow = async (page: Page, label: string) => {
  const o = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
  expect(o.sw, `${label}: horizontal overflow`).toBeLessThanOrEqual(o.iw);
};

test('手机外壳：侧栏变抽屉、标签栏隐藏改为面包屑+返回；状态/回滚/节点维护入口可点达；触控目标 ≥44px；无横向溢出', async ({ page }) => {
  const c = watchConsole(page);
  await apiLogin(page, fxUrl(), env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[9]);
  await page.goto(`${fxUrl()}/`);
  await expect(page.getByTestId('admin-shell')).toBeVisible();
  await expect(page.getByTestId('sidebar')).toHaveCount(0);
  await expect(page.getByTestId('tab-bar')).toHaveCount(0);
  await expect(page.getByTestId('acting-banner')).toBeVisible(); // 横幅在手机上同样固定显示
  await noOverflow(page, 'home');

  // 顶栏所有控件都在视口内且触控目标 ≥44px
  for (const id of ['nav-toggle', 'search-open', 'account-menu']) {
    const b = await page.getByTestId(id).boundingBox();
    expect(b!.x + b!.width, `${id} inside viewport`).toBeLessThanOrEqual(390);
    expect(b!.height, `${id} touch target`).toBeGreaterThanOrEqual(32);
    expect(b!.width, `${id} touch target`).toBeGreaterThanOrEqual(32);
  }

  const entries: [string, string, string][] = [
    ['fx.tasks', '/ops/tasks', 'Task status and alerts'], // 查看状态/告警
    ['fx.rollback', '/ops/rollback', 'Trigger rollback'], // 触发回滚
    ['fx.maintenance', '/nodes/maintenance', 'Node maintenance'], // 节点维护
  ];
  for (const [key, path, title] of entries) {
    await page.getByTestId('nav-toggle').click();
    const link = page.locator(`.arco-drawer [data-menu-item="${key}"]`);
    await expect(link).toBeVisible();
    const box = await link.boundingBox();
    expect(box!.height, `${key} nav item touch target`).toBeGreaterThanOrEqual(44);
    if (key === 'fx.tasks') {
      await settled(page);
      await shot(page, 'mobile-drawer');
    }
    await link.tap();
    await expect(page).toHaveURL(new RegExp(`${path}$`));
    await expect(page.getByTestId('fixture-page')).toHaveAttribute('data-page', key);
    await expect(page.locator('.arco-drawer')).toHaveCount(0); // 点击后抽屉自动收起
    await expect(page.getByTestId('mobile-crumbs')).toContainText(title);
    await noOverflow(page, key);
  }
  await shot(page, 'mobile-fixture-page');
  // 返回
  await page.getByTestId('mobile-back').tap();
  await expect(page).toHaveURL(/\/ops\/rollback$/);
  // 退出登录入口在手机上可达
  await page.getByTestId('account-menu').tap();
  await expect(page.getByTestId('logout')).toBeVisible();
  await page.keyboard.press('Escape');
  expect(c.problems).toEqual([]);
});

test('复杂编辑页在手机上如实标注「可用但不保证体验」，且不阻断操作；桌面宽度不显示', async ({ page }) => {
  await apiLogin(page, fxUrl(), env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
  await page.goto(`${fxUrl()}/settings/rule-editor`);
  await expect(page.getByTestId('mobile-complex-notice')).toContainText('不保证体验');
  await expect(page.getByTestId('fixture-page')).toHaveAttribute('data-page', 'fx.rules');
  await noOverflow(page, 'rule-editor');
  await shot(page, 'mobile-complex-notice');
  await page.setViewportSize({ width: 1280, height: 900 });
  await expect(page.getByTestId('mobile-complex-notice')).toHaveCount(0);
  await expect(page.getByTestId('tab-bar')).toBeVisible();
});

test('手机上的搜索与语言/主题入口可用；平板宽度侧栏折叠为图标', async ({ page }) => {
  await apiLogin(page, fxUrl(), env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
  await page.goto(`${fxUrl()}/`);
  await page.getByTestId('search-open').tap();
  await expect(page.getByTestId('search-input')).toBeVisible();
  await page.getByTestId('search-input').fill('task');
  await expect(page.getByTestId('search-result')).toHaveCount(1);
  await page.getByTestId('search-result').tap();
  await expect(page).toHaveURL(/\/ops\/tasks$/);
  await page.getByTestId('nav-toggle').tap();
  await expect(page.locator('.arco-drawer [data-testid="theme-select"]')).toBeVisible();
  await expect(page.locator('.arco-drawer [data-testid="lang-select"]')).toBeVisible();
  await page.keyboard.press('Escape');
  // 平板（md）：侧栏折叠为图标
  await page.setViewportSize({ width: 820, height: 900 });
  await expect(page.getByTestId('sidebar')).toHaveClass(/le-shell__side--collapsed/);
  const w = (await page.getByTestId('sidebar').boundingBox())!.width;
  expect(Math.round(w)).toBe(48);
  await shot(page, 'tablet-collapsed');
});
