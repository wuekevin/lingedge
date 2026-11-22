import { request as pwRequest, type Page } from '@playwright/test';
import { apiLogin, csrfOf, env, expect, freshTotp, noOverflow, prodUrl, recoveryCodes, settled, shot, sidebarGroups, sidebarItems, sinkSecret, test, uiLogin, watchConsole } from './helpers';

// Story 1.39 系统管理页主流程：生产构建 × 真实 lingedge-server（正常模式）× 真实 MySQL/Redis × 真实 Chromium。
// 同一个世界里按顺序执行（后面的用例依赖前面创建的角色/用户/审计记录）；一次性明文逐个写入 Go 侧 sink，由 Go 扫描日志/审计/库并核对库内只存哈希。
test.describe.configure({ mode: 'serial' });

const base = prodUrl();
const OPS = () => [env('LE_E2E_OPS_EMAIL'), env('LE_E2E_OPS_PW')] as const;
const rand = () => Math.random().toString(36).slice(2, 10);
const viewer = { email: `viewer${rand()}@example.test`, temp: '', next: `Vw${rand()}${rand()}!7` };

async function grantClipboard(page: Page): Promise<void> {
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin: base });
}
const toast = (page: Page, text: string) => expect(page.locator('.arco-message').filter({ hasText: text }).first()).toBeVisible();

test('创建角色 → 分配 → 无权限用户验证（含首个 L4 操作触发再认证、一次性临时口令、服务端真实拒绝）', async ({ page, browser }) => {
  const c = watchConsole(page);
  await grantClipboard(page);
  await apiLogin(page, base, ...OPS());
  await page.goto(`${base}/settings/roles`);
  await expect(page.getByTestId('roles-page')).toBeVisible();

  // 内置角色只读
  await page.locator('[data-code="platform_admin"] [data-testid="role-select"]').click();
  await expect(page.getByTestId('role-builtin-note')).toBeVisible();
  await expect(page.getByTestId('perm-user:read')).toBeDisabled();
  await expect(page.getByTestId('role-perms-save')).toHaveCount(0);
  // 权限矩阵：租户范围专属的权限不适用于平台角色，不渲染复选框（取自真实的 /admin/permissions）
  const perms = (await (await page.context().request.get(`${base}/api/v1/admin/permissions?page_size=200`)).json()) as { items: { code: string; scope: string }[] };
  const tenantOnly = perms.items.filter((p) => p.scope === 'tenant').map((p) => p.code);
  expect(tenantOnly.length).toBeGreaterThan(0);
  const platformApplicable = perms.items.find((p) => p.scope === 'platform' || p.scope === 'both')!.code; // 正向对照：适用的权限确实有复选框

  // 创建（新会话的第一个 L4 操作）：服务端要求再认证 → 验证框；口令错误如实提示且不被当作会话过期；通过后原请求自动重试
  await page.getByTestId('role-create').click();
  await page.getByTestId('create-role-code').fill('Bad Code');
  await page.getByTestId('create-role-name').fill('E2E Viewer');
  await page.getByTestId('role-create-submit').click();
  await expect(page.getByText('代码必须以小写字母开头')).toBeVisible(); // 本地校验，不发请求
  await expect(page.getByTestId('reauth-dialog')).toHaveCount(0);
  await page.getByTestId('create-role-code').fill('e2e_viewer');
  await page.getByTestId('role-create-submit').click();
  await expect(page.getByTestId('reauth-dialog')).toBeVisible();
  await expect(page.getByTestId('reauth-code')).toHaveCount(0); // 该账号未启用 TOTP：只要口令
  await expect(page.getByTestId('reauth-audit')).toContainText('此操作将记录到审计日志');
  await shot(page, 'reauth-dialog');
  await page.getByTestId('reauth-secret').fill(`wrong${rand()}`);
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('reauth-error')).toContainText('邮箱或口令不正确');
  await expect(page.getByTestId('reauth-dialog')).toBeVisible();
  expect((await page.context().request.get(`${base}/api/v1/auth/me`)).status()).toBe(200); // 会话仍然有效
  await page.getByTestId('reauth-secret').fill(env('LE_E2E_OPS_PW'));
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('reauth-dialog')).toHaveCount(0);
  await expect(page.getByTestId('role-detail')).toHaveAttribute('data-code', 'e2e_viewer'); // 原请求已自动重试并选中新角色

  // 分配权限：只勾 audit:read（窗口内，不再询问）
  await expect(page.getByTestId(`perm-${platformApplicable}`)).toHaveCount(1);
  for (const code of tenantOnly.slice(0, 3)) await expect(page.getByTestId(`perm-${code}`)).toHaveCount(0);
  await page.getByTestId('perm-filter').fill('audit:');
  await page.getByTestId('perm-audit:read').check();
  await expect(page.getByTestId('role-perms-save')).toBeEnabled();
  await page.getByTestId('role-perms-save').click();
  await toast(page, '权限已保存');
  await expect(page.getByTestId('reauth-dialog')).toHaveCount(0);
  await expect(page.locator('[data-code="e2e_viewer"] td').nth(4)).toHaveText('1');
  await shot(page, 'roles-matrix');

  // 创建用户（不填口令：服务端生成一次性临时口令）
  await page.goto(`${base}/users/list`);
  await page.getByTestId('user-create').click();
  await page.getByTestId('create-email').fill(viewer.email);
  await page.getByTestId('create-name').fill('E2E Viewer');
  await expect(page.getByTestId('create-reauth-hint')).toBeVisible();
  await page.getByTestId('create-submit').click();
  await expect(page.getByTestId('secret-dialog')).toBeVisible();
  viewer.temp = ((await page.getByTestId('secret-value').textContent()) ?? '').trim();
  expect(viewer.temp.length).toBeGreaterThanOrEqual(16);
  sinkSecret(viewer.temp);
  await expect(page.getByTestId('secret-once')).toContainText('只显示这一次');
  await page.getByTestId('secret-copy').click();
  await expect(page.getByTestId('secret-copied')).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(viewer.temp);
  await page.keyboard.press('Escape');
  await expect(page.getByTestId('secret-dialog')).toBeVisible(); // 不能误关
  await shot(page, 'secret-once');
  await page.getByTestId('secret-close').click();
  await expect(page.getByTestId('secret-dialog')).toHaveCount(0);
  expect(await page.locator('body').innerText()).not.toContain(viewer.temp); // 关闭后页面里再也没有
  const row = page.locator(`[data-testid="user-row"][data-email="${viewer.email}"]`);
  await expect(row).toBeVisible();
  await expect(row.getByTestId('user-roles-cell')).toHaveText('无角色');
  await expect(row).toContainText('须改口令');

  // 分配角色
  await row.getByTestId('user-roles').click();
  await page.getByTestId('assign-role-e2e_viewer').check();
  await page.getByTestId('assign-submit').click();
  await expect(row.getByTestId('user-roles-cell')).toHaveText('e2e_viewer');

  // 无权限用户验证：新浏览器上下文，用临时口令登录 → 强制改口令 → 登录
  const ctx = await browser.newContext({ locale: 'zh-CN', extraHTTPHeaders: { 'X-Forwarded-For': '203.0.113.201' } });
  const vp = await ctx.newPage();
  const vc = watchConsole(vp);
  await uiLogin(vp, base, viewer.email, viewer.temp);
  await expect(vp.getByTestId('change-password-page')).toBeVisible();
  await vp.getByTestId('cp-current').fill(viewer.temp);
  await vp.getByTestId('cp-next').fill(viewer.next);
  await vp.getByTestId('cp-confirm').fill(viewer.next);
  await vp.getByTestId('cp-submit').click();
  await expect(vp.getByTestId('login-page')).toBeVisible();
  await uiLogin(vp, base, viewer.email, viewer.next);
  await expect(vp.getByTestId('admin-shell')).toBeVisible();
  // 菜单只剩 audit:read 允许的页面（用户操作审计别名 + 审计日志）；没有任何设置/用户列表/令牌入口
  expect(await sidebarGroups(vp)).toEqual(['users', 'ops']);
  expect(await sidebarItems(vp)).toEqual(['users.audit', 'ops.audit']);
  await shot(vp, 'viewer-menu');
  const needs: [string, string][] = [
    ['/users/list', 'user:read'],
    ['/users/roles', 'role:read'],
    ['/settings/roles', 'role:read'],
    ['/settings/basic', 'setting:read'],
    ['/settings/version', 'system:read'],
    ['/users/api-tokens', 'token:manage'],
    ['/users/login-logs', 'login-log:read'],
  ];
  for (const [p, perm] of needs) {
    await vp.goto(`${base}${p}`);
    await expect(vp.getByTestId('forbidden-page'), p).toBeVisible();
    await expect(vp.getByTestId('forbidden-need')).toContainText(perm);
  }
  await shot(vp, 'viewer-403');
  await vp.goto(`${base}/ops/audit`);
  await expect(vp.getByTestId('audit-page')).toBeVisible();
  await expect(vp.getByTestId('audit-row').first()).toBeVisible();
  // 服务端真实裁决（不是前端隐藏）：越权的 API 一律 403，被审计为 denied；有权的 API 200
  expect((await vp.context().request.get(`${base}/api/v1/admin/users`)).status()).toBe(403);
  expect((await vp.context().request.get(`${base}/api/v1/admin/roles`)).status()).toBe(403);
  expect((await vp.context().request.get(`${base}/api/v1/audit-logs?limit=5`)).status()).toBe(200);
  const csrf = await csrfOf(vp, base);
  const mk = await vp.context().request.post(`${base}/api/v1/admin/roles`, { headers: { 'X-CSRF-Token': csrf }, data: { scope: 'platform', code: 'sneaky', name: 'x' } });
  expect(mk.status()).toBe(403);
  expect(vc.problems).toEqual([]);
  await ctx.close();
  expect(c.problems).toEqual([]);
});

test('个人 API Token：创建时明文只展示一次并可复制；库内只存哈希；轮换新明文同样一次；吊销立即失效；令牌权限受 scopes 约束', async ({ page }) => {
  const c = watchConsole(page);
  await grantClipboard(page);
  await apiLogin(page, base, ...OPS());
  await page.goto(`${base}/users/api-tokens`);
  await expect(page.getByTestId('tokens-empty')).toBeVisible(); // 空态
  await shot(page, 'tokens-empty');

  await page.getByTestId('token-create').click();
  await page.getByTestId('token-name').fill('e2e-ci');
  await page.getByTestId('token-create-submit').click();
  await expect(page.getByTestId('token-scope-problem')).toBeVisible(); // 至少一项权限
  await page.getByTestId('token-scope-filter').fill('audit:read');
  await page.getByTestId('scope-audit:read').check();
  await page.getByTestId('token-create-submit').click();
  await expect(page.getByTestId('secret-dialog')).toBeVisible();
  const token1 = ((await page.getByTestId('secret-value').textContent()) ?? '').trim();
  expect(token1).toMatch(/^le_[A-Za-z0-9]{8}_\S{16,}$/);
  sinkSecret(token1);
  await page.getByTestId('secret-copy').click();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(token1);
  await page.getByTestId('secret-close').click();
  expect(await page.locator('body').innerText()).not.toContain(token1);
  const prefix1 = token1.split('_')[1]!;
  const row = () => page.locator('[data-testid="token-row"][data-name="e2e-ci"]');
  await expect(row().first().getByTestId('token-prefix')).toHaveText(prefix1);
  await expect(row().first().getByTestId('token-scopes')).toHaveText('audit:read');
  // 刷新后与列表接口里都没有明文
  await page.reload();
  expect(await page.locator('body').innerText()).not.toContain(token1);
  const listBody = await (await page.context().request.get(`${base}/api/v1/auth/api-tokens`)).text();
  expect(listBody).not.toContain(token1);

  // 令牌真能用，且只能做 scopes 允许的事
  const api = await pwRequest.newContext({ extraHTTPHeaders: { Authorization: `Bearer ${token1}`, 'X-Forwarded-For': '203.0.113.202' } });
  expect((await api.get(`${base}/api/v1/audit-logs?limit=1`)).status()).toBe(200);
  expect((await api.get(`${base}/api/v1/admin/users`)).status()).toBe(403);

  // 轮换：L2 对话框（影响范围 + 审计提示）→ 新明文只显示一次，旧令牌立即失效
  await row().first().getByTestId('token-rotate').click();
  await expect(page.getByTestId('danger-audit')).toBeVisible();
  await expect(page.getByTestId('danger-impact')).toContainText('立即吊销旧令牌');
  await page.getByTestId('danger-ok').click();
  await expect(page.getByTestId('secret-dialog')).toBeVisible();
  const token2 = ((await page.getByTestId('secret-value').textContent()) ?? '').trim();
  expect(token2).toMatch(/^le_/);
  expect(token2).not.toBe(token1);
  sinkSecret(token2);
  await page.getByTestId('secret-close').click();
  expect((await api.get(`${base}/api/v1/audit-logs?limit=1`)).status()).toBe(401); // 旧令牌已吊销
  await expect(page.locator('[data-testid="token-row"][data-state="revoked"]')).toHaveCount(1);
  const api2 = await pwRequest.newContext({ extraHTTPHeaders: { Authorization: `Bearer ${token2}`, 'X-Forwarded-For': '203.0.113.203' } });
  expect((await api2.get(`${base}/api/v1/audit-logs?limit=1`)).status()).toBe(200);

  // 吊销（L2）
  await page.locator('[data-testid="token-row"][data-state="active"]').getByTestId('token-revoke').click();
  await page.getByTestId('danger-ok').click();
  await expect(page.locator('[data-testid="token-row"][data-state="active"]')).toHaveCount(0);
  expect((await api2.get(`${base}/api/v1/audit-logs?limit=1`)).status()).toBe(401);
  await api.dispose();
  await api2.dispose();
  expect(c.problems).toEqual([]);
});

test('运行模式开关：L3（键入目标模式 + 审计提示）+ L4（已启用 TOTP 的账号须重新输入口令和 TOTP；错误如实提示；窗口内不再询问）', async ({ page }) => {
  const c = watchConsole(page);
  await apiLogin(page, base, env('LE_E2E_ADMIN_EMAIL'), env('LE_E2E_ADMIN_PW'), recoveryCodes()[0]);
  await page.goto(`${base}/settings/basic`);
  await expect(page.getByTestId('run-mode-current')).toHaveAttribute('data-mode', 'self_hosted');
  await page.getByTestId('run-mode-switch').click();
  await expect(page.getByTestId('danger-level')).toContainText('三级');
  await expect(page.getByTestId('danger-audit')).toHaveText('此操作将记录到审计日志（操作人、时间、对象、Request ID）。');
  await expect(page.getByTestId('danger-ok')).toBeDisabled();
  await page.getByTestId('danger-confirm-input').fill('commerc');
  await expect(page.getByTestId('danger-ok')).toBeDisabled();
  await shot(page, 'run-mode-l3');
  await page.getByTestId('danger-confirm-input').fill('commercial');
  await expect(page.getByTestId('danger-ok')).toBeEnabled();
  await page.getByTestId('danger-ok').click();

  // L4：再认证。该账号启用了 TOTP → 口令 + 验证码；先空提交 / 口令错 / 验证码错，最后正确
  await expect(page.getByTestId('reauth-dialog')).toBeVisible();
  await expect(page.getByTestId('reauth-code')).toBeVisible();
  await shot(page, 'reauth-totp');
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('reauth-error')).toBeVisible(); // 本地必填校验
  await page.getByTestId('reauth-secret').fill(`wrong${rand()}`);
  await page.getByTestId('reauth-code').fill('000000');
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('reauth-error')).toContainText('邮箱或口令不正确');
  await page.getByTestId('reauth-secret').fill(env('LE_E2E_ADMIN_PW'));
  await page.getByTestId('reauth-code').fill('000000');
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('reauth-error')).toContainText('验证码');
  await expect(page.getByTestId('reauth-dialog')).toBeVisible();
  expect((await page.context().request.get(`${base}/api/v1/auth/me`)).status()).toBe(200);
  await page.getByTestId('reauth-code').fill(await freshTotp(env('LE_E2E_TOTP_KEY')));
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('reauth-dialog')).toHaveCount(0);
  await expect(page.getByTestId('run-mode-current')).toHaveAttribute('data-mode', 'commercial'); // 原请求自动重试成功
  await page.reload();
  await expect(page.getByTestId('run-mode-current')).toHaveAttribute('data-mode', 'commercial');
  expect(((await (await page.context().request.get(`${base}/api/v1/admin/system/run-mode`)).json()) as { run_mode: string }).run_mode).toBe('commercial');
  await page.goto(`${base}/`);
  await expect(page.getByTestId('home-run-mode')).toHaveText('商业模式'); // /auth/me 刷新后的运行模式

  // 切回自用：窗口内不再询问
  await page.goto(`${base}/settings/basic`);
  await page.getByTestId('run-mode-switch').click();
  await page.getByTestId('danger-confirm-input').fill('self_hosted');
  await page.getByTestId('danger-ok').click();
  await expect(page.getByTestId('run-mode-current')).toHaveAttribute('data-mode', 'self_hosted');
  await expect(page.getByTestId('reauth-dialog')).toHaveCount(0);
  expect(c.problems).toEqual([]);
});

test('新会话里创建管理员触发再认证；取消则不创建；通过后创建成功', async ({ page }) => {
  await apiLogin(page, base, ...OPS());
  await page.goto(`${base}/users/list`);
  const email = `second${rand()}@example.test`;
  await page.getByTestId('user-create').click();
  await page.getByTestId('create-email').fill(email);
  await page.getByTestId('create-name').fill('Second Admin');
  await page.getByTestId('create-submit').click();
  await expect(page.getByTestId('reauth-dialog')).toBeVisible();
  await page.getByTestId('reauth-cancel').click();
  await expect(page.getByTestId('create-error')).toContainText('已取消重新验证');
  const list = await (await page.context().request.get(`${base}/api/v1/admin/users?page_size=200`)).text();
  expect(list).not.toContain(email); // 取消 = 没有创建
  await page.getByTestId('create-submit').click();
  await page.getByTestId('reauth-secret').fill(env('LE_E2E_OPS_PW'));
  await page.getByTestId('reauth-submit').click();
  await expect(page.getByTestId('secret-dialog')).toBeVisible();
  sinkSecret(((await page.getByTestId('secret-value').textContent()) ?? '').trim());
  await page.getByTestId('secret-close').click();
  await expect(page.locator(`[data-testid="user-row"][data-email="${email}"]`)).toBeVisible();
  // L2+：重置口令与删除——对话框页脚固定审计提示，并提前告知需要重新验证
  const row = page.locator(`[data-testid="user-row"][data-email="${email}"]`);
  await row.getByTestId('user-delete').click();
  await expect(page.getByTestId('danger-level')).toContainText('二级确认');
  await expect(page.getByTestId('danger-audit')).toContainText('此操作将记录到审计日志');
  await expect(page.getByTestId('danger-reauth-hint')).toBeVisible();
  await page.getByTestId('danger-ok').click(); // 窗口内：直接执行
  await expect(row).toHaveCount(0);
});

test('审计日志：游标分页（UI 与 API 逐页一致、无重复）、过滤、别名预置、详情；登录日志过滤', async ({ page }) => {
  const c = watchConsole(page);
  await apiLogin(page, base, ...OPS());
  const csrf = await csrfOf(page, base);
  // 造数据：25 次真实的品牌写入（每次一条审计），保证超过一页
  for (let i = 0; i < 25; i++) {
    const r = await page.context().request.put(`${base}/api/v1/admin/settings/brand`, { headers: { 'X-CSRF-Token': csrf }, data: { name: 'Audit Brand', support_email: `a${i}@example.test` } });
    expect(r.status()).toBe(200);
  }
  // API 逐页走完作为对照
  const apiIds: string[] = [];
  let cursor = '';
  for (let guard = 0; guard < 50; guard++) {
    const r = await page.context().request.get(`${base}/api/v1/audit-logs?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`);
    const j = (await r.json()) as { items: { request_id: string; uid: string }[]; next_cursor: string | null };
    apiIds.push(...j.items.map((i) => i.uid));
    if (!j.next_cursor) break;
    cursor = j.next_cursor;
  }
  expect(apiIds.length).toBeGreaterThan(45);
  expect(new Set(apiIds).size).toBe(apiIds.length);

  await page.goto(`${base}/ops/audit`);
  await expect(page.getByTestId('audit-page')).toHaveAttribute('data-preset', '');
  const rows = page.getByTestId('audit-row');
  await expect(rows).toHaveCount(20);
  await expect(page.getByTestId('cursor-prev')).toBeDisabled();
  await expect(page.getByTestId('cursor-info')).toContainText('1');
  await shot(page, 'audit-page1');
  const uiSeen: string[] = [];
  const collect = async () => uiSeen.push(...(await rows.evaluateAll((els) => els.map((e) => e.querySelector('td:nth-child(6)')?.textContent ?? ''))));
  await collect();
  const firstPage = [...uiSeen];
  let pages = 1;
  while (await page.getByTestId('cursor-next').isEnabled()) {
    const before = (await rows.first().innerText()) + pages;
    await page.getByTestId('cursor-next').click();
    pages++;
    await expect(page.getByTestId('cursor-info')).toContainText(String(pages));
    await expect.poll(async () => (await rows.count()) > 0).toBe(true);
    await collect();
    expect(before).toBeTruthy();
    expect(pages).toBeLessThan(40);
  }
  expect(pages).toBeGreaterThanOrEqual(3);
  expect(uiSeen.length).toBe(apiIds.length); // UI 逐页走完 = API 逐页走完
  await expect(page.getByTestId('cursor-next')).toBeDisabled(); // 末页
  // 上一页回退到前一页，回到第一页后与最初一致
  for (let i = 1; i < pages; i++) await page.getByTestId('cursor-prev').click();
  await expect(page.getByTestId('cursor-info')).toContainText('1');
  await expect.poll(async () => (await rows.evaluateAll((els) => els.map((e) => e.querySelector('td:nth-child(6)')?.textContent ?? ''))).join()).toBe(firstPage.join());

  // 过滤：动作
  await page.getByTestId('filter-action').fill('setting.brand.update');
  await page.getByTestId('filter-apply').click();
  await expect
    .poll(async () => {
      const a = await rows.evaluateAll((els) => els.map((e) => e.getAttribute('data-action')));
      return a.length === 20 && a.every((x) => x === 'setting.brand.update'); // 非空且全部匹配（不能让空列表空真通过）
    })
    .toBe(true);
  await expect(page.getByTestId('cursor-info')).toContainText('1'); // 过滤回到第一页
  // 过滤：结果 = 拒绝（无权限用户的越权请求）
  await page.getByTestId('filter-clear').click();
  await page.getByTestId('filter-result').selectOption('denied');
  await page.getByTestId('filter-apply').click();
  await expect.poll(async () => (await rows.allInnerTexts()).length > 0 && (await rows.allInnerTexts()).every((t) => t.includes('拒绝'))).toBe(true);
  // 过滤：Request ID 精确到一条
  await page.getByTestId('filter-clear').click();
  await expect(rows).toHaveCount(20);
  const rid = ((await rows.nth(3).locator('td:nth-child(6)').textContent()) ?? '').trim();
  await page.getByTestId('filter-request-id').fill(rid);
  await page.getByTestId('filter-apply').click();
  await expect.poll(async () => await rows.count()).toBeGreaterThanOrEqual(1);
  expect(await rows.locator('td:nth-child(6)').allTextContents()).toEqual(expect.arrayContaining([rid]));
  // 过滤：时间（未来 = 无结果态，可清除）与起止颠倒
  await page.getByTestId('filter-clear').click();
  await page.getByTestId('filter-from').fill('2099-01-01T00:00');
  await page.getByTestId('filter-apply').click();
  await expect(page.locator('[data-le-state="empty"]')).toHaveAttribute('data-le-empty-reason', 'no-results');
  await page.locator('[data-le-clear-filters]').click();
  await expect(rows).toHaveCount(20);
  await page.getByTestId('filter-from').fill('2030-01-01T00:00');
  await page.getByTestId('filter-to').fill('2029-01-01T00:00');
  await expect(page.getByTestId('filter-problem')).toBeVisible();
  await expect(page.getByTestId('filter-apply')).toBeDisabled();
  await page.getByTestId('filter-clear').click();
  // 每页条数
  await page.getByTestId('cursor-limit').selectOption('50');
  await expect(rows).toHaveCount(50);
  // 详情
  await rows.first().getByTestId('audit-detail').click();
  await expect(page.getByTestId('audit-detail-panel')).toBeVisible();
  await shot(page, 'audit-detail');
  await page.keyboard.press('Escape');

  // 别名入口：同一组件，预置「用户操作」筛选
  await page.goto(`${base}/users/audit`);
  await expect(page.getByTestId('audit-alias-note')).toBeVisible();
  await expect(page.getByTestId('audit-page')).toHaveAttribute('data-preset', 'user');
  await expect.poll(async () => await rows.count()).toBeGreaterThan(0);
  const types = page.getByTestId('audit-resource-type');
  const resources = await types.allTextContents();
  expect([...new Set(resources)]).toEqual(['user']);
  await page.getByTestId('filter-clear').click();
  await expect.poll(async () => (await types.allTextContents()).some((r) => r !== 'user')).toBe(true); // 清除预置后出现非 user 的记录

  // 登录日志：过滤 + 游标；失败事件真实存在（前面的口令错误）
  await page.goto(`${base}/users/login-logs`);
  const logs = page.getByTestId('login-log-row');
  await expect.poll(async () => await logs.count()).toBeGreaterThan(0);
  await page.getByTestId('filter-event').selectOption('login_failed');
  await page.getByTestId('filter-apply').click();
  await expect
    .poll(async () => {
      const ev = await logs.evaluateAll((els) => els.map((e) => e.getAttribute('data-event')));
      return ev.length > 0 && ev.every((v) => v === 'login_failed');
    })
    .toBe(true);
  await page.getByTestId('filter-identifier').fill(env('LE_E2E_OPS_EMAIL'));
  await page.getByTestId('filter-apply').click();
  await expect.poll(async () => await logs.count()).toBeGreaterThanOrEqual(1);
  await shot(page, 'login-logs');
  expect(c.problems).toEqual([]);
});

test.describe('手机', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  test('手机宽度：真实页面无横向溢出、表格改为逐行卡片、触控目标 ≥44px、对话框在视口内', async ({ page }) => {
    await apiLogin(page, base, ...OPS());
    for (const [path, testid] of [
      ['/users/list', 'users-table'],
      ['/settings/roles', 'roles-table'],
      ['/ops/audit', 'audit-table'],
      ['/users/login-logs', 'login-logs-table'],
      ['/users/api-tokens', 'tokens-table'],
      ['/settings/version', 'version-components'],
    ] as const) {
      await page.goto(`${base}${path}`);
      const table = page.getByTestId(testid);
      if (testid === 'tokens-table') {
        await expect(page.getByTestId('token-create')).toBeVisible();
      } else await expect(table.first()).toBeVisible();
      await noOverflow(page, path);
      if (await table.count()) {
        expect(await table.first().locator('tbody tr').first().evaluate((e) => getComputedStyle(e).display)).toBe('block'); // 逐行卡片
        const label = await table.first().locator('tbody tr td').first().getAttribute('data-label');
        expect(label).toBeTruthy();
      }
      await expect(page.getByTestId('mobile-crumbs')).toBeVisible();
    }
    await page.goto(`${base}/users/list`);
    await expect(page.getByTestId('user-row').first()).toBeVisible();
    await shot(page, 'mobile-users');
    const act = page.locator('[data-testid="user-row"] [data-testid="user-reset"]').first();
    expect((await act.boundingBox())!.height).toBeGreaterThanOrEqual(44);
    await page.getByTestId('user-create').tap();
    await expect(page.getByTestId('user-create-form')).toBeVisible();
    await settled(page);
    const modal = page.locator('.arco-modal').first();
    const box = await modal.boundingBox();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
    await shot(page, 'mobile-create-user');
    await noOverflow(page, 'create-dialog');
  });
});

test('UX-27：运行中的界面没有任何「执行命令」入口（菜单、各页面文字）', async ({ page }) => {
  await apiLogin(page, base, ...OPS());
  const banned = /(执行|运行)(任意|远程|系统|自定义)?(命令|脚本)|网页终端|远程\s*Shell|\b(run|execute)\s+(a\s+|an\s+|any\s+)?(command|script)\b|\b(web|remote)\s+(terminal|shell)\b/i;
  await page.goto(`${base}/`);
  await expect(page.locator('[data-menu-item]')).toHaveCount(12); // 生产清单的 12 个页面（含 3 个别名入口）
  const items = await sidebarItems(page);
  for (const key of items) expect(key).not.toMatch(/exec|terminal|shell|command|script|ssh/i);
  for (const p of ['/', '/users/list', '/users/roles', '/users/api-tokens', '/users/login-logs', '/users/audit', '/ops/audit', '/settings/basic', '/settings/brand', '/settings/login', '/settings/admins', '/settings/roles', '/settings/version']) {
    await page.goto(`${base}${p}`);
    await expect(page.getByTestId('admin-shell')).toBeVisible();
    await page.waitForLoadState('networkidle');
    expect(await page.locator('body').innerText(), p).not.toMatch(banned);
  }
});
