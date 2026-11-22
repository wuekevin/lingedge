import { apiLogin, env, expect, prodUrl, recoveryCodes, shot, test, totpCode, watchConsole } from './helpers';
import type { Page } from '@playwright/test';

// 登录与会话（AC1）——生产构建 × 真实 server：口令 + TOTP、恢复码、租户账号被拒、须改口令、Cookie 与 CSRF、空菜单外壳。

const wrongSecret = () => `Wr${Math.random().toString(36).slice(2)}!x`;

async function fillLogin(page: Page, email: string, secret: string) {
  await page.getByTestId('login-email').fill(email);
  await page.getByTestId('login-pw').fill(secret);
  await page.getByTestId('login-submit').click();
}
async function cookies(page: Page) {
  return page.context().cookies();
}

test('登录页：未登录进入任何路径都落到登录页；空表单给出统一的必填提示且不发请求', async ({ page }) => {
  const c = watchConsole(page);
  let posted = false;
  page.on('request', (r) => {
    if (r.url().includes('/auth/login')) posted = true;
  });
  await page.goto(`${prodUrl()}/ops/anything`);
  await expect(page.getByTestId('login-page')).toBeVisible();
  expect(new URL(page.url()).searchParams.get('redirect')).toBe('/ops/anything');
  await page.getByTestId('login-submit').click();
  await expect(page.getByText('请填写邮箱')).toBeVisible();
  expect(posted).toBe(false);
  await shot(page, 'login-empty-validation');
  expect(c.problems).toEqual([]);
});

test('错口令与租户账号：同一条中性提示（不泄露账号是否存在），带 Request ID，且不留下任何会话 Cookie', async ({ page }) => {
  const c = watchConsole(page);
  await page.goto(`${prodUrl()}/login`);
  await fillLogin(page, env('LE_E2E_ADMIN_EMAIL'), wrongSecret());
  const wrong = page.getByTestId('login-error-text');
  await expect(wrong).toBeVisible();
  const wrongText = (await wrong.innerText()).trim();
  expect(wrongText).toBe('邮箱或口令不正确，或该账号无权访问此入口。');
  await expect(page.getByTestId('login-error')).toContainText(/请求 ID/);
  // 租户账号用正确口令在 Admin 入口登录：与错口令完全相同的提示
  await page.getByTestId('login-email').fill(env('LE_E2E_TENANT_EMAIL'));
  await page.getByTestId('login-pw').fill(env('LE_E2E_TENANT_PW'));
  await page.getByTestId('login-submit').click();
  await expect(page.getByTestId('login-error-text')).toHaveText(wrongText);
  await expect(page.getByTestId('login-page')).toBeVisible();
  expect((await cookies(page)).filter((k) => /^le_(admin|console)_sid$/.test(k.name))).toEqual([]);
  await shot(page, 'login-tenant-rejected');
  // 租户账号也不能经 API 拿到 admin 会话（服务端对两种失败的响应一致）
  const r = await page.context().request.post(`${prodUrl()}/api/v1/auth/login`, { data: { realm: 'admin', email: env('LE_E2E_TENANT_EMAIL'), password: env('LE_E2E_TENANT_PW') } });
  expect(r.status()).toBe(401);
  expect((await r.json()).error.code).toBe('auth.invalid_credentials');
  expect(c.problems).toEqual([]);
});

test('管理员：口令 → TOTP（错码被拒，正确码通过）→ le_admin_sid 会话；写请求自动带 X-CSRF-Token；退出后会话失效', async ({ page }) => {
  const c = watchConsole(page);
  const csrfOnLogout: (string | undefined)[] = [];
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith('/auth/logout')) csrfOnLogout.push(r.headers()['x-csrf-token']);
  });
  await page.goto(`${prodUrl()}/login`);
  await fillLogin(page, env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'));
  await expect(page.getByTestId('totp-code')).toBeVisible(); // 第二步
  await expect(page.getByTestId('login-email')).toHaveCount(0);
  await shot(page, 'login-totp-step');
  const good = totpCode(env('LE_E2E_TOTP_KEY'));
  const bad = good === '000000' ? '000001' : '000000';
  await page.getByTestId('totp-code').fill(bad);
  await page.getByTestId('totp-submit').click();
  await expect(page.getByTestId('login-error-text')).toContainText('验证码或恢复码不正确');
  await page.getByTestId('totp-code').fill(good);
  await page.getByTestId('totp-submit').click();
  await expect(page.getByTestId('admin-shell')).toBeVisible();

  // 会话 Cookie：le_admin_sid、HttpOnly；没有 Console 的 realm；脚本读不到 Cookie
  const ck = await cookies(page);
  const sid = ck.find((k) => k.name === 'le_admin_sid');
  expect(sid, 'le_admin_sid').toBeTruthy();
  expect(sid!.httpOnly).toBe(true);
  expect(ck.some((k) => k.name === 'le_console_sid')).toBe(false);
  expect(await page.evaluate(() => document.cookie)).not.toContain('le_admin_sid');
  // 浏览器存储里没有会话令牌/CSRF/TOTP 相关内容
  const dump = JSON.stringify(await page.evaluate(() => ({ l: { ...localStorage }, s: { ...sessionStorage } })));
  expect(dump).not.toContain(sid!.value);
  expect(dump).not.toMatch(/csrf|mfa|totp/i);

  // 代管横幅：固定、不可关闭，写出真实默认租户名
  const banner = page.getByTestId('acting-banner');
  await expect(banner).toContainText('正在以管理员身份操作租户：默认租户');
  await expect(banner.locator('button')).toHaveCount(0);
  await shot(page, 'shell-home-prod');

  // 退出：POST 带 CSRF 令牌；之后 /auth/me 为 401
  await page.getByTestId('account-menu').click();
  await page.getByTestId('logout').click();
  await expect(page.getByTestId('login-page')).toBeVisible();
  expect(csrfOnLogout).toHaveLength(1);
  expect(csrfOnLogout[0], 'X-CSRF-Token on logout').toMatch(/\S{16,}/);
  expect((await page.context().request.get(`${prodUrl()}/api/v1/auth/me`)).status()).toBe(401);
  expect((await cookies(page)).some((k) => k.name === 'le_admin_sid' && k.value !== '')).toBe(false);
  expect(c.problems).toEqual([]);
});

test('恢复码登录：同一恢复码只能用一次（第二次被拒），另一枚可用', async ({ page }) => {
  const c = watchConsole(page);
  const codes = recoveryCodes();
  const useRecovery = async (code: string) => {
    await page.goto(`${prodUrl()}/login`);
    await fillLogin(page, env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'));
    await expect(page.getByTestId('totp-code')).toBeVisible();
    await page.getByTestId('totp-toggle').click();
    await expect(page.getByTestId('totp-hint')).toContainText('恢复码');
    await page.getByTestId('totp-code').fill(code);
    await page.getByTestId('totp-submit').click();
  };
  await useRecovery(codes[0]!);
  await expect(page.getByTestId('admin-shell')).toBeVisible();
  await page.getByTestId('account-menu').click();
  await page.getByTestId('logout').click();
  await expect(page.getByTestId('login-page')).toBeVisible();
  await useRecovery(codes[0]!); // 已用过
  await expect(page.getByTestId('login-error-text')).toContainText('验证码或恢复码不正确');
  await expect(page.getByTestId('admin-shell')).toHaveCount(0);
  await shot(page, 'login-recovery-reuse-rejected');
  await page.getByTestId('totp-back').click(); // 返回重新登录
  await expect(page.getByTestId('login-email')).toBeVisible();
  await useRecovery(codes[1]!);
  await expect(page.getByTestId('admin-shell')).toBeVisible();
  expect(c.problems).toEqual([]);
});

test('须改口令的账号：登录后只能到改口令页；表单校验；改完回到登录页，用新口令登录成功', async ({ page }) => {
  const c = watchConsole(page);
  await page.goto(`${prodUrl()}/login`);
  await fillLogin(page, env('LE_E2E_MUST_EMAIL'), env('LE_E2E_MUST_PW'));
  await expect(page.getByTestId('change-password-page')).toBeVisible();
  // 其余路由被守卫拦回改口令页
  await page.goto(`${prodUrl()}/`);
  await expect(page.getByTestId('change-password-page')).toBeVisible();
  const next = `Nw${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}!9`;
  await page.getByTestId('cp-current').fill(env('LE_E2E_MUST_PW'));
  await page.getByTestId('cp-next').fill('short');
  await page.getByTestId('cp-confirm').fill('different');
  await page.getByTestId('cp-submit').click();
  await expect(page.getByText('新口令至少 12 个字符')).toBeVisible();
  await expect(page.getByText('两次输入的确认新口令不一致')).toBeVisible();
  await page.getByTestId('cp-next').fill(next);
  await page.getByTestId('cp-confirm').fill(next);
  await page.getByTestId('cp-submit').click();
  await expect(page.getByTestId('login-page')).toBeVisible();
  await fillLogin(page, env('LE_E2E_MUST_EMAIL'), next);
  await expect(page.getByTestId('admin-shell')).toBeVisible();
  expect(c.problems).toEqual([]);
});

test('生产外壳：没有任何已交付业务页面时侧栏无菜单、首页如实空态；夹具路由与未知路径一律 404；无「敬请期待」', async ({ page }) => {
  const c = watchConsole(page);
  await apiLogin(page, prodUrl(), env('LE_E2E_AUDITOR_EMAIL'), env('LE_E2E_AUDITOR_PW'));
  await page.goto(`${prodUrl()}/`);
  await expect(page.getByTestId('admin-shell')).toBeVisible();
  await expect(page.locator('[data-menu-group]')).toHaveCount(0);
  await expect(page.locator('[data-menu-item]')).toHaveCount(0);
  await expect(page.getByTestId('home-pages')).toHaveText('0');
  await expect(page.getByTestId('home-page').locator('[data-le-state="empty"]')).toBeVisible();
  await expect(page.getByTestId('home-roles')).toContainText('platform_auditor');
  await expect(page.getByTestId('home-modules')).toContainText('auth');
  await expect(page.getByTestId('home-run-mode')).toHaveText('自用模式');
  expect(await page.locator('body').innerText()).not.toMatch(/敬请期待|即将推出|Coming soon/i);
  // 夹具页面只存在于 e2e 构建：生产构建下它们和任何未知路径一样是 404（不是 403）
  for (const p of ['/ops/tasks', '/nodes/maintenance', '/definitely/not/here']) {
    await page.goto(`${prodUrl()}${p}`);
    await expect(page.getByTestId('not-found-page'), p).toBeVisible();
    await expect(page.getByTestId('forbidden-page')).toHaveCount(0);
  }
  // 刷新保持会话；清除会话 Cookie 后刷新回到登录页
  await page.goto(`${prodUrl()}/`);
  await page.reload();
  await expect(page.getByTestId('admin-shell')).toBeVisible();
  await page.context().clearCookies();
  await page.reload();
  await expect(page.getByTestId('login-page')).toBeVisible();
  expect(c.problems).toEqual([]);
});

test('生产产物运行时检查：运行时加载的全部 JS 不含 Console realm 常量、mock 入口与夹具', async ({ page }) => {
  const loaded: Promise<string>[] = [];
  page.on('response', (r) => {
    if (r.url().endsWith('.js')) loaded.push(r.text());
  });
  await page.goto(`${prodUrl()}/login`);
  await expect(page.getByTestId('login-email')).toBeVisible();
  await page.waitForLoadState('networkidle');
  const all = (await Promise.all(loaded)).join('\n');
  expect(all.length).toBeGreaterThan(1000);
  expect(all).not.toContain('le_console_sid');
  expect(all).not.toContain('LE_MOCK_ENTRY');
  expect(all).not.toContain('E2E fixture');
});
