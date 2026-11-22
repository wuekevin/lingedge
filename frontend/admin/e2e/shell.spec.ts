import type { Page } from '@playwright/test';
import { apiLogin, env, expect, fxUrl, recoveryCodes, settled, shot, test, watchConsole } from './helpers';

// 外壳（AC2、AC3）——e2e 构建（带夹具页面）× 真实 server：模块与权限来自真实 /auth/me，夹具页面只是占位内容。
// 夹具页面的模块/权限（见 src/mocks/e2e-pages.mock.ts）：tasks/audit/auth/system 是后端真实已交付模块；nodes/scheduler 未交付；billing 页只在商业模式显示（本环境是自用模式）。

const items = (page: Page) => page.locator('[data-menu-item]').evaluateAll((els) => els.map((e) => e.getAttribute('data-menu-item')));
const groups = (page: Page) => page.locator('[data-menu-group]').evaluateAll((els) => els.map((e) => e.getAttribute('data-menu-group')));

test.describe('platform_admin（拥有全部权限）', () => {
  test('菜单 = 模块已交付 ∧ 权限 ∧ 运行模式：按 17 菜单顺序渲染；未交付模块/商业模式页面不出现；整组为空则一级菜单不渲染', async ({ page }) => {
    const c = watchConsole(page);
    await apiLogin(page, fxUrl(), env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[3]);
    await page.goto(`${fxUrl()}/`);
    await expect(page.getByTestId('admin-shell')).toBeVisible();
    expect(await groups(page)).toEqual(['nodes', 'users', 'ops', 'settings']); // scheduling（模块未交付）、billing（商业模式）整组不渲染
    // 真实页面（Story 1.39，登记在 registry/pages.ts）+ 仍无真实页面的菜单组的夹具（节点维护、任务、回滚、未保存更改、复杂编辑）
    expect(await items(page)).toEqual([
      'fx.maintenance',
      'users.list', 'users.roles', 'users.api-tokens', 'users.login-logs', 'users.audit',
      'ops.audit', 'fx.tasks', 'fx.rollback', 'fx.dirty',
      'settings.basic', 'settings.brand', 'settings.login', 'settings.admins', 'settings.roles', 'settings.version', 'fx.rules',
    ]);
    // P2 才交付的设置页（邮件/API/系统安全/数据保留/系统日志）不渲染：菜单里没有，直访 404（FR-36）
    for (const p of ['mail', 'api', 'security', 'retention', 'logs']) {
      await expect(page.locator(`[data-menu-item="settings.${p}"]`)).toHaveCount(0);
    }
    // nodelist（nodes 模块未交付）不在 nodes 组里；没有灰显/占位
    await expect(page.locator('[data-menu-item="fx.nodelist"]')).toHaveCount(0);
    await expect(page.locator('[data-testid="side-menu"] [disabled], [data-testid="side-menu"] [aria-disabled="true"]')).toHaveCount(0);
    expect(await page.getByTestId('side-menu').innerText()).not.toMatch(/敬请期待|即将|Coming soon/i);
    await shot(page, 'shell-admin-menu');
    // 英文界面：分组名与横幅走词条
    await page.getByTestId('lang-select').selectOption('en');
    await expect(page.locator('[data-menu-group="ops"]')).toContainText('Operations');
    await expect(page.getByTestId('acting-banner')).toContainText('Acting as administrator for tenant');
    await expect(page.locator('html')).toHaveAttribute('lang', 'en');
    await page.getByTestId('lang-select').selectOption('zh-CN');
    expect(c.problems).toEqual([]);
  });

  test('直访：模块未交付 / 商业模式页面 / 未知路径 → 404（不暗示存在）；页面不渲染', async ({ page }) => {
    await apiLogin(page, fxUrl(), env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[4]);
    for (const p of ['/nodes/list', '/scheduling/policies', '/billing/orders', '/settings/mail', '/settings/security', '/settings/retention', '/zzz']) {
      await page.goto(`${fxUrl()}${p}`);
      await expect(page.getByTestId('not-found-page'), p).toBeVisible();
      await expect(page.getByTestId('fixture-page')).toHaveCount(0);
      await expect(page.getByTestId('forbidden-page')).toHaveCount(0);
      await expect(page.getByTestId('acting-banner')).toBeVisible();
    }
  });

  test('全局搜索：Ctrl+K 打开，只索引已交付且可见的页面（别名不重复）；Enter 打开；无匹配时显示无结果态', async ({ page }) => {
    await apiLogin(page, fxUrl(), env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[5]);
    await page.goto(`${fxUrl()}/`);
    await expect(page.getByTestId('admin-shell')).toBeVisible();
    await page.keyboard.press('Control+k');
    await expect(page.getByTestId('search-input')).toBeFocused();
    const rows = page.getByTestId('search-result');
    await expect(rows).toHaveCount(14); // 17 个菜单页面去掉 3 个别名（用户角色、操作审计、管理员管理）
    const texts = (await rows.allInnerTexts()).join('\n');
    for (const hidden of ['Node list', 'Scheduling', 'Billing', '用户角色', '操作审计', '管理员管理']) expect(texts).not.toContain(hidden);
    for (const shown of ['用户列表', '权限角色', '审计日志', '版本信息']) expect(texts).toContain(shown);
    await settled(page);
    await shot(page, 'search-open');
    await page.getByTestId('search-input').fill('rollback');
    await expect(rows).toHaveCount(1);
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/\/ops\/rollback$/);
    await expect(page.getByTestId('fixture-page')).toHaveAttribute('data-page', 'fx.rollback');
    await page.keyboard.press('Control+k');
    await page.getByTestId('search-input').fill('billing');
    await expect(rows).toHaveCount(0);
    await expect(page.locator('.arco-modal [data-le-state="empty"]')).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(page.getByTestId('search-input')).toHaveCount(0);
  });

  test('多标签：同页不重复；关闭；未保存更改先确认；刷新恢复标签；退出关闭全部且重新登录后只剩首页', async ({ page }) => {
    const c = watchConsole(page);
    await apiLogin(page, fxUrl(), env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[6]);
    await page.goto(`${fxUrl()}/`);
    const tab = page.getByTestId('tab');
    const click = (k: string) => page.locator(`[data-menu-item="${k}"]`).click();
    await click('fx.tasks');
    await click('ops.audit');
    await click('fx.tasks');
    await expect(tab).toHaveCount(3); // 首页 + 任务 + 审计，任务没有重复
    await click('fx.dirty');
    await expect(tab).toHaveCount(4);
    // 未保存更改：输入后标签带标记；关闭先确认，取消则保留
    await page.getByTestId('dirty-input').fill('draft');
    await expect(tab.filter({ hasText: 'Dirty form' }).getByTitle('有未保存的更改')).toBeVisible();
    await tab.filter({ hasText: 'Dirty form' }).getByTestId('tab-close').click();
    await expect(page.getByTestId('tab-dirty-confirm')).toContainText('Dirty form');
    await page.getByRole('button', { name: '取消' }).click();
    await expect(tab).toHaveCount(4);
    // 切到别的标签再回来：页面保活，输入内容仍在（标签即工作区）
    await click('ops.audit');
    await click('fx.dirty');
    await expect(page.getByTestId('dirty-input')).toHaveValue('draft');
    // 刷新：标签恢复（只恢复路径，不恢复未保存内容）
    await page.reload();
    await expect(tab).toHaveCount(4);
    await expect(page.getByTestId('dirty-input')).toHaveValue('');
    // 关闭审计标签
    await tab.filter({ hasText: '审计日志' }).getByTestId('tab-close').click();
    await expect(tab).toHaveCount(3);
    // 退出：再次有未保存更改时先确认，确认后关闭全部标签并回到登录页
    await page.getByTestId('dirty-input').fill('again');
    await page.getByTestId('account-menu').click();
    await page.getByTestId('logout').click();
    await expect(page.getByTestId('logout-confirm')).toBeVisible();
    await page.getByRole('button', { name: '退出登录' }).last().click();
    await expect(page.getByTestId('login-page')).toBeVisible();
    expect(await page.evaluate(() => sessionStorage.getItem('le.admin.tabs'))).toBeNull();
    // 重新登录后只有首页标签（不继承旧标签）
    await apiLogin(page, fxUrl(), env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[7]);
    await page.goto(`${fxUrl()}/`);
    await expect(tab).toHaveCount(1);
    expect(c.problems).toEqual([]);
  });

  test('代管横幅：每个页面固定显示「默认租户」，不可关闭；滚动时保持在顶栏下方', async ({ page }) => {
    await apiLogin(page, fxUrl(), env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[8]);
    for (const p of ['/', '/ops/tasks', '/403', '/nope']) {
      await page.goto(`${fxUrl()}${p}`);
      const b = page.getByTestId('acting-banner');
      await expect(b).toContainText('正在以管理员身份操作租户：默认租户');
      await expect(b.locator('button, [role="button"]')).toHaveCount(0);
    }
    await page.goto(`${fxUrl()}/ops/tasks`);
    await page.evaluate(() => {
      document.body.style.minHeight = '3000px';
      window.scrollTo(0, 800);
    });
    const topbar = await page.getByTestId('top-bar').boundingBox();
    const banner = await page.getByTestId('acting-banner').boundingBox();
    expect(Math.round(banner!.y)).toBe(Math.round(topbar!.y + topbar!.height));
  });
});

test.describe('platform_auditor（只读角色）', () => {
  test('无权限的菜单隐藏：看不到回滚、节点维护（整个节点菜单不渲染）；其余只读页面可见', async ({ page }) => {
    const c = watchConsole(page);
    await apiLogin(page, fxUrl(), env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
    await page.goto(`${fxUrl()}/`);
    await expect(page.getByTestId('admin-shell')).toBeVisible();
    expect(await groups(page)).toEqual(['users', 'ops', 'settings']);
    // 只读审计员拥有全部 :read 权限，没有 token:manage（API Token 页）与任何写权限；节点维护/回滚（node:manage、config:rollback）不可见
    expect(await items(page)).toEqual([
      'users.list', 'users.roles', 'users.login-logs', 'users.audit',
      'ops.audit', 'fx.tasks', 'fx.dirty',
      'settings.basic', 'settings.brand', 'settings.login', 'settings.admins', 'settings.roles', 'settings.version', 'fx.rules',
    ]);
    await expect(page.locator('[data-menu-item="users.api-tokens"]')).toHaveCount(0);
    await expect(page.locator('[data-menu-item="fx.rollback"], [data-menu-item="fx.maintenance"]')).toHaveCount(0);
    await expect(page.locator('[data-menu-group="nodes"]')).toHaveCount(0);
    // 搜索同样不索引无权限页面
    await page.keyboard.press('Control+k');
    await page.getByTestId('search-input').fill('rollback');
    await expect(page.getByTestId('search-result')).toHaveCount(0);
    await page.getByTestId('search-input').fill('maintenance');
    await expect(page.getByTestId('search-result')).toHaveCount(0);
    await page.keyboard.press('Escape');
    expect(c.problems).toEqual([]);
  });

  test('直访无权页面：显示 403（所需权限 + 联系管理员 + 返回首页），不渲染页面；不开标签', async ({ page }) => {
    await apiLogin(page, fxUrl(), env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
    await page.goto(`${fxUrl()}/ops/rollback`);
    await expect(page.getByTestId('forbidden-page')).toBeVisible();
    await expect(page.getByTestId('forbidden-need')).toContainText('config:rollback');
    await expect(page.getByTestId('forbidden-page')).toContainText('联系管理员');
    await expect(page.getByTestId('fixture-page')).toHaveCount(0);
    await shot(page, 'forbidden-page');
    await page.goto(`${fxUrl()}/users/api-tokens`); // 真实页面：auth 模块已交付，但缺 token:manage → 403（不是 404）
    await expect(page.getByTestId('forbidden-need')).toContainText('token:manage');
    await page.goto(`${fxUrl()}/nodes/maintenance`); // system 模块已交付，但缺 node:manage → 403（不是 404）
    await expect(page.getByTestId('forbidden-need')).toContainText('node:manage');
    await expect(page.getByTestId('tab').filter({ hasText: 'Trigger rollback' })).toHaveCount(0);
    await page.getByRole('link', { name: '返回首页' }).click();
    await expect(page.getByTestId('home-page')).toBeVisible();
    // 有权限的页面可进
    await page.goto(`${fxUrl()}/ops/tasks`);
    await expect(page.getByTestId('fixture-page')).toHaveAttribute('data-page', 'fx.tasks');
  });

  test('别名入口（UX-3）：三个别名与各自的规范页面共用同一页面组件；搜索只索引规范页面', async ({ page }) => {
    await apiLogin(page, fxUrl(), env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
    await page.goto(`${fxUrl()}/users/audit`);
    await expect(page.getByTestId('audit-page')).toHaveAttribute('data-preset', 'user'); // 别名：预置「用户操作」筛选
    await expect(page.locator('[data-menu-group="users"] [data-menu-item="users.audit"]')).toBeVisible();
    await page.goto(`${fxUrl()}/ops/audit`);
    await expect(page.getByTestId('audit-page')).toHaveAttribute('data-preset', ''); // 规范页面：不预置
    await page.goto(`${fxUrl()}/users/roles`);
    await expect(page.getByTestId('roles-page')).toBeVisible();
    await page.goto(`${fxUrl()}/settings/admins`);
    await expect(page.getByTestId('users-page')).toBeVisible();
    await page.keyboard.press('Control+k');
    await page.getByTestId('search-input').fill('审计');
    await expect(page.getByTestId('search-result')).toHaveCount(1); // 只有规范页面「审计日志」，别名「操作审计」不重复索引
    await expect(page.getByTestId('search-result')).toContainText('审计日志');
  });
});
