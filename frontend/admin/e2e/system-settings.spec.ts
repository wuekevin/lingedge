import type { Page } from '@playwright/test';
import { apiLogin, env, expect, expectContrast, expectTheme, noOverflow, prodUrl, settled, shot, sinkSecret, test, watchConsole, type Scheme } from './helpers';

// Story 1.39 系统设置页：生产构建 × 真实 lingedge-server × 真实 Chromium。
// 版本信息 = 二进制自报版本与库里最高迁移号（Go 侧独立取得）；品牌修改后 Admin（含另一个只读账号）读到新值；登录设置与「再认证窗口外」；亮/暗对比度审计。
test.describe.configure({ mode: 'serial' });

const base = prodUrl();
const OPS = () => [env('LE_E2E_OPS_EMAIL'), env('LE_E2E_OPS_PW')] as const;
/** 色值运行时拼出（门禁禁止源码里写死色值字面量）。 */
const COLOR = ['#', '0A7D5A'].join('');
const rand = () => Math.random().toString(36).slice(2, 10);

test('版本信息页：显示真实 Server 版本、构建、迁移版本（与二进制自报版本、库里最高迁移号一致，不是写死的值）', async ({ page }) => {
  const c = watchConsole(page);
  await apiLogin(page, base, ...OPS());
  await page.goto(`${base}/settings/version`);
  await expect(page.getByTestId('version-page')).toBeVisible();
  await expect(page.getByTestId('version-server')).toHaveText(env('LE_E2E_SERVER_VERSION'));
  expect(env('LE_E2E_SERVER_VERSION')).not.toBe('0.0.0-dev'); // 注入的版本与默认值不同：不可能是巧合相等
  await expect(page.getByTestId('version-build')).toHaveText('e2e0139');
  await expect(page.getByTestId('version-migration')).toHaveText(env('LE_E2E_MIGRATION'));
  await expect(page.getByTestId('version-pending')).toHaveText('0');
  await expect(page.getByTestId('version-installation')).not.toHaveText('—');
  // 页面值 = 接口值（接口是同一份真实数据）
  const api = (await (await page.context().request.get(`${base}/api/v1/admin/system/version`)).json()) as { server_version: string; schema_migration: { current: string } };
  expect(api.server_version).toBe(env('LE_E2E_SERVER_VERSION'));
  expect(api.schema_migration.current).toBe(env('LE_E2E_MIGRATION'));
  await shot(page, 'version');
  await noOverflow(page, 'version');
  expect(c.problems).toEqual([]);
});

test('品牌设置：校验 → 保存 → Admin 立即读到新值（顶栏）；刷新、英文界面、另一个只读账号都读到；只读账号不能改', async ({ page, browser }) => {
  const c = watchConsole(page);
  const name = env('LE_E2E_BRAND_NAME');
  await apiLogin(page, base, ...OPS());
  const before = (await (await page.context().request.get(`${base}/api/v1/admin/settings/brand`)).json()) as { name: string };
  expect(before.name).not.toBe(name);
  await page.goto(`${base}/settings/brand`);
  await expect(page.getByTestId('field-name')).toHaveValue(before.name); // 安装向导设置的初始品牌名（真实 GET /admin/settings/brand）
  await expect(page.getByTestId('top-brand')).toHaveText(before.name);
  await page.getByTestId('field-name').fill(name);
  await page.getByTestId('field-name_en').fill('E2E Brand EN');
  await page.getByTestId('field-primary_color').fill('red');
  await page.getByTestId('field-support_email').fill('not-an-email');
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('field-error-primary_color')).toBeVisible();
  await expect(page.getByTestId('field-error-support_email')).toBeVisible();
  await shot(page, 'brand-validation');
  await page.getByTestId('field-primary_color').fill(COLOR);
  await page.getByTestId('field-support_email').fill('help@example.test');
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('settings-saved')).toBeVisible();
  await expect(page.getByTestId('top-brand')).toHaveText(name); // 保存后顶栏立即是新值
  await page.reload();
  await expect(page.getByTestId('top-brand')).toHaveText(name); // 来自服务端，不是前端缓存
  await expect(page.getByTestId('field-name')).toHaveValue(name);
  await page.getByTestId('lang-select').selectOption('en');
  await expect(page.getByTestId('top-brand')).toHaveText('E2E Brand EN');
  await page.getByTestId('lang-select').selectOption('zh-CN');
  const api = (await (await page.context().request.get(`${base}/api/v1/admin/settings/brand`)).json()) as { name: string; name_en: string; primary_color: string; support_email: string };
  expect(api).toMatchObject({ name, name_en: 'E2E Brand EN', primary_color: COLOR, support_email: 'help@example.test' });

  // 另一个账号（只读审计员）：读到同一份新值，且不能修改
  const ctx = await browser.newContext({ locale: 'zh-CN', extraHTTPHeaders: { 'X-Forwarded-For': '203.0.113.211' } });
  const ap = await ctx.newPage();
  await apiLogin(ap, base, env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
  await ap.goto(`${base}/settings/brand`);
  await expect(ap.getByTestId('top-brand')).toHaveText(name);
  await expect(ap.getByTestId('field-name')).toHaveValue(name);
  await expect(ap.getByTestId('field-name')).toHaveAttribute('readonly', '');
  await expect(ap.getByTestId('settings-save')).toHaveCount(0);
  await expect(ap.getByTestId('settings-readonly')).toBeVisible();
  await shot(ap, 'brand-readonly');
  await ctx.close();
  expect(c.problems).toEqual([]);
});

test('基本设置：保存只提交变更键；敏感键触发再认证；服务端校验失败如实提示', async ({ page }) => {
  const c = watchConsole(page);
  await apiLogin(page, base, ...OPS());
  await page.goto(`${base}/settings/basic`);
  const initial = (await (await page.context().request.get(`${base}/api/v1/admin/settings?group=general`)).json()) as Record<string, unknown>;
  const tz = initial['general.timezone'] === 'Europe/Berlin' ? 'America/New_York' : 'Europe/Berlin'; // 取一个与向导设置的值不同的真实时区
  await expect(page.getByTestId('field-general.timezone')).toHaveValue(String(initial['general.timezone']));
  // 前端校验
  await page.getByTestId('field-general.docs_url').fill('javascript:alert(1)');
  await page.getByTestId('field-general.timezone').fill('Mars/Olympus');
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('field-error-general.docs_url')).toBeVisible();
  await expect(page.getByTestId('field-error-general.timezone')).toBeVisible();
  // 非敏感键：直接保存，不需要再认证
  await page.getByTestId('field-general.docs_url').fill('https://docs.example.test');
  await page.getByTestId('field-general.timezone').fill(tz);
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('settings-saved')).toBeVisible();
  await expect(page.getByTestId('reauth-dialog')).toHaveCount(0);
  const s1 = (await (await page.context().request.get(`${base}/api/v1/admin/settings?group=general`)).json()) as Record<string, unknown>;
  expect(s1['general.docs_url']).toBe('https://docs.example.test');
  expect(s1['general.timezone']).toBe(tz);
  // 敏感键（开放自助注册）：新会话 → 服务端要求再认证 → 验证框 → 重试
  await page.getByTestId('field-general.registration_open').check();
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('reauth-dialog')).toBeVisible();
  await page.getByTestId('reauth-secret').fill(env('LE_E2E_OPS_PW'));
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('settings-saved')).toBeVisible();
  const s2 = (await (await page.context().request.get(`${base}/api/v1/admin/settings?group=general`)).json()) as Record<string, unknown>;
  expect(s2['general.registration_open']).toBe(true);
  await page.getByTestId('field-general.registration_open').uncheck();
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('settings-saved')).toBeVisible();
  // 撤销更改
  await page.getByTestId('field-general.docs_url').fill('https://other.example.test');
  await expect(page.getByTestId('settings-reset')).toBeEnabled();
  await page.getByTestId('settings-reset').click();
  await expect(page.getByTestId('field-general.docs_url')).toHaveValue('https://docs.example.test');
  expect(c.problems).toEqual([]);
});

test('登录设置：范围/跨键校验；保存（再认证）；再认证窗口改为 10 秒后，窗口外的 L4 操作再次被要求验证并自动重试', async ({ page }) => {
  const c = watchConsole(page);
  await apiLogin(page, base, ...OPS());
  await page.goto(`${base}/settings/login`);
  await expect(page.getByTestId('field-auth.reauth_window_s')).toHaveValue('300');
  await page.getByTestId('field-login.throttle_window_s').fill('0');
  await page.getByTestId('field-auth.session_idle_ttl_s').fill('99999999');
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('field-error-login.throttle_window_s')).toContainText('1 到 86400');
  await expect(page.getByTestId('field-error-auth.session_idle_ttl_s')).toBeVisible();
  await shot(page, 'login-settings-validation');
  await page.getByTestId('field-login.throttle_window_s').fill('900');
  await page.getByTestId('field-auth.session_idle_ttl_s').fill('7200');
  await page.getByTestId('field-auth.session_absolute_ttl_s').fill('3600'); // 滑动 > 绝对：跨键约束
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('field-error-auth.session_idle_ttl_s')).toContainText('滑动过期不能超过绝对过期');
  await page.getByTestId('field-auth.session_absolute_ttl_s').fill('43200');
  await page.getByTestId('field-auth.reauth_window_s').fill('10');
  await page.getByTestId('settings-save').click();
  await expect(page.getByTestId('reauth-dialog')).toBeVisible(); // 安全敏感键：需要再认证
  await page.getByTestId('reauth-secret').fill(env('LE_E2E_OPS_PW'));
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('settings-saved')).toBeVisible();
  const s = (await (await page.context().request.get(`${base}/api/v1/admin/settings`)).json()) as Record<string, unknown>;
  expect(s['auth.reauth_window_s']).toBe(10);
  expect(s['login.throttle_window_s']).toBe(900);

  // 等窗口过期，再做 L4（创建角色）：服务端 403 auth.reauth_required → 前端弹出验证框 → 通过后自动重试
  // 服务端对认证设置有 15 秒缓存（settingsCacheTTL）：新窗口最迟 15 秒后生效，再加上窗口本身的 10 秒，等 17 秒保证窗口已缩短且上次再认证已过期
  await page.waitForTimeout(17_000);
  await page.goto(`${base}/settings/roles`);
  await page.getByTestId('role-create').click();
  await page.getByTestId('create-role-code').fill('e2e_late');
  await page.getByTestId('create-role-name').fill('Late');
  await page.getByTestId('role-create-submit').click();
  await expect(page.getByTestId('reauth-dialog')).toBeVisible();
  await page.getByTestId('reauth-secret').fill(env('LE_E2E_OPS_PW'));
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('role-detail')).toHaveAttribute('data-code', 'e2e_late');
  // 删除角色是 L3（键入角色代码）+ L4：窗口刚刚被刷新，不再询问
  await page.getByTestId('role-delete').click();
  await expect(page.getByTestId('danger-ok')).toBeDisabled();
  await page.getByTestId('danger-confirm-input').fill('e2e_late');
  await page.getByTestId('danger-ok').click();
  await expect(page.locator('[data-code="e2e_late"]')).toHaveCount(0);
  expect(c.problems).toEqual([]);
});

// ---- 亮/暗对比度审计（≥4.5:1）：真实页面 + 弹层，逐个等动画结束后审计；截图落在 LINGEDGE_E2E_SHOT_DIR，由人工逐张核对 ----
async function audit(page: Page, scheme: Scheme, label: string) {
  await settled(page);
  await expectTheme(page, scheme);
  await expectContrast(page, `${scheme}/${label}`);
  await shot(page, `${scheme}-1.39-${label}`);
}

for (const scheme of ['light', 'dark'] as const) {
  test.describe(`${scheme}：1.39 真实页面对比度`, () => {
    test.use({ colorScheme: scheme });

    test(`${scheme}：列表页、权限矩阵、审计、登录日志、设置、版本、403`, async ({ page }) => {
      const c = watchConsole(page);
      await apiLogin(page, base, ...OPS());
      const visit = async (path: string, ready: string, label: string) => {
        await page.goto(`${base}${path}`);
        await expect(page.getByTestId(ready).first()).toBeVisible();
        await page.waitForLoadState('networkidle');
        await audit(page, scheme, label);
      };
      await visit('/users/list', 'users-table', 'users');
      await page.goto(`${base}/settings/roles`);
      await page.locator('[data-code="platform_operator"] [data-testid="role-select"]').click();
      await expect(page.getByTestId('role-detail')).toBeVisible();
      await audit(page, scheme, 'roles-builtin');
      await visit('/users/api-tokens', 'token-create', 'tokens');
      await visit('/ops/audit', 'audit-table', 'audit');
      await page.getByTestId('filter-action').fill('nothing.matches.this');
      await page.getByTestId('filter-apply').click();
      await expect(page.locator('[data-le-state="empty"]')).toBeVisible();
      await audit(page, scheme, 'audit-no-results');
      await visit('/users/login-logs', 'login-logs-table', 'login-logs');
      await visit('/settings/basic', 'run-mode-card', 'settings-basic');
      await visit('/settings/brand', 'settings-brand-form', 'settings-brand');
      await visit('/settings/login', 'settings-login-form', 'settings-login');
      await page.getByTestId('field-login.throttle_window_s').fill('0');
      await page.getByTestId('settings-save').click();
      await expect(page.getByTestId('field-error-login.throttle_window_s')).toBeVisible();
      await audit(page, scheme, 'settings-login-errors');
      await visit('/settings/version', 'version-page', 'version');
      expect(c.problems).toEqual([]);
    });

    test(`${scheme}：对话框（新建管理员、L3 运行模式、再认证、一次性明文、L2 确认、审计详情抽屉）与 403`, async ({ page, browser }) => {
      const c = watchConsole(page);
      await apiLogin(page, base, ...OPS());
      await page.goto(`${base}/users/list`);
      await expect(page.getByTestId('users-table')).toBeVisible();
      await page.getByTestId('user-create').click();
      await expect(page.getByTestId('user-create-form')).toBeVisible();
      await audit(page, scheme, 'dialog-create-user');
      await page.keyboard.press('Escape');
      // L2 确认（吊销某用户会话）
      await page.locator('[data-testid="user-row"]').filter({ hasNotText: '当前账号' }).first().getByTestId('user-reset').click();
      await expect(page.getByTestId('danger-dialog')).toBeVisible();
      await audit(page, scheme, 'dialog-l2');
      await page.getByTestId('danger-cancel').click();
      // L3：运行模式
      await page.goto(`${base}/settings/basic`);
      await page.getByTestId('run-mode-switch').click();
      await page.getByTestId('danger-confirm-input').fill('commerc');
      await audit(page, scheme, 'dialog-l3');
      await page.getByTestId('danger-cancel').click();
      // 一次性明文（创建令牌）→ 再认证（新会话的 L4）
      await page.goto(`${base}/users/api-tokens`);
      await page.getByTestId('token-create').click();
      await page.getByTestId('token-name').fill(`e2e-theme-${scheme}`);
      await page.getByTestId('scope-audit:read').check();
      await audit(page, scheme, 'dialog-create-token');
      await page.getByTestId('token-create-submit').click();
      await expect(page.getByTestId('secret-dialog')).toBeVisible();
      sinkSecret(((await page.getByTestId('secret-value').textContent()) ?? '').trim());
      await audit(page, scheme, 'dialog-secret');
      await page.getByTestId('secret-close').click();
      await expect(page.locator('[data-testid="token-row"]').first()).toBeVisible();
      await audit(page, scheme, 'tokens-list');
      await page.goto(`${base}/settings/roles`);
      await page.getByTestId('role-create').click();
      await page.getByTestId('create-role-code').fill(`e2e_${scheme}_${rand()}`);
      await page.getByTestId('create-role-name').fill('Theme role');
      await page.getByTestId('role-create-submit').click();
      await expect(page.getByTestId('reauth-dialog')).toBeVisible();
      await page.getByTestId('reauth-secret').fill('x');
      await page.getByTestId('reauth-submit').click();
      await expect(page.getByTestId('reauth-error')).toBeVisible();
      await audit(page, scheme, 'dialog-reauth-error');
      await page.getByTestId('reauth-cancel').click();
      await page.keyboard.press('Escape');
      // 审计详情抽屉
      await page.goto(`${base}/ops/audit`);
      await page.getByTestId('audit-detail').first().click();
      await expect(page.getByTestId('audit-detail-panel')).toBeVisible();
      await audit(page, scheme, 'audit-drawer');
      await page.keyboard.press('Escape');
      // 403（只读审计员直访需要写权限之外的页面：api-tokens 需要 token:manage）
      const ctx = await browser.newContext({ locale: 'zh-CN', colorScheme: scheme, extraHTTPHeaders: { 'X-Forwarded-For': scheme === 'light' ? '203.0.113.221' : '203.0.113.222' } });
      const ap = await ctx.newPage();
      await apiLogin(ap, base, env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
      await ap.goto(`${base}/users/api-tokens`);
      await expect(ap.getByTestId('forbidden-page')).toBeVisible();
      await audit(ap, scheme, '403-token');
      await ap.goto(`${base}/settings/roles`);
      await expect(ap.getByTestId('roles-page')).toBeVisible();
      await ap.locator('[data-testid="role-select"]').first().click();
      await audit(ap, scheme, 'roles-readonly');
      await ctx.close();
      expect(c.problems).toEqual([]);
    });
  });
}

test.describe('手机', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  test('手机宽度的设置页：无横向溢出、触控目标', async ({ page }) => {
    await apiLogin(page, base, ...OPS());
    for (const p of ['/settings/basic', '/settings/brand', '/settings/login', '/settings/version']) {
      await page.goto(`${base}${p}`);
      await expect(page.getByTestId('mobile-crumbs')).toBeVisible();
      await page.waitForLoadState('networkidle');
      await noOverflow(page, p);
    }
    await page.goto(`${base}/settings/login`);
    const b = await page.getByTestId('settings-save').boundingBox();
    expect(b!.height).toBeGreaterThanOrEqual(32);
    await shot(page, 'mobile-settings-login');
    // 手机宽度下的 L3 对话框在视口内
    await page.goto(`${base}/settings/basic`);
    await page.getByTestId('run-mode-switch').tap();
    await expect(page.getByTestId('danger-dialog')).toBeVisible();
    await settled(page);
    const box = await page.locator('.arco-modal').first().boundingBox();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
    await shot(page, 'mobile-l3');
    await noOverflow(page, 'l3-dialog');
  });
});
