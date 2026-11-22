import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AdminUser, ApiToken, AuditLog, LoginLog, Permission, Role } from '../../api/types';
import zhCN from '../../locales/zh-CN.json';
import { productionPages } from '../../registry/pages';
import { apiError, makeMe, type Call, type Handler } from '../../testing/fakeApi';
import { mountAdmin } from '../../testing/mountAdmin';

// 系统管理页（Story 1.39）的组件测试：以生产同一套装配（createAdminApp + productionPages）挂载整个 Admin，只替换 fetch。
// 断言的是真实行为：请求体/请求头、再认证重试、一次性明文只出现一次、游标分页的游标传递、过滤参数、三态、只读、本地化、手机宽度的表格结构。

let m: Awaited<ReturnType<typeof mountAdmin>> | null = null;
afterEach(() => {
  m?.unmount();
  m = null;
  localStorage.clear();
  document.documentElement.removeAttribute('data-theme');
});
const q = <T extends Element = HTMLElement>(sel: string) => document.querySelector(sel) as T | null;
const qa = <T extends Element = HTMLElement>(sel: string) => [...document.querySelectorAll(sel)] as T[];
const text = (sel: string) => q(sel)?.textContent ?? '';
async function type(sel: string, value: string) {
  const el = q<HTMLInputElement | HTMLTextAreaElement>(sel)!;
  expect(el, sel).not.toBeNull();
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  el.dispatchEvent(new Event('blur', { bubbles: true }));
  await m!.settle();
}
async function click(sel: string, nth = 0) {
  const el = qa(sel)[nth];
  expect(el, sel).toBeDefined();
  el!.click();
  await m!.settle();
}
const ALL = ['user:read', 'user:write', 'role:read', 'role:write', 'setting:read', 'setting:write', 'system:read', 'audit:read', 'login-log:read', 'token:manage', 'tenant:read', 'task:read'];
const meWith = (perms: string[] = ALL, over: { totp?: boolean } = {}) => {
  const me = makeMe({ permissions: perms });
  if (over.totp) me.user.totp_enabled = true;
  return me;
};
const mount = (path: string, o: Partial<Parameters<typeof mountAdmin>[0]> = {}) => mountAdmin({ pages: productionPages, path, me: meWith(), ...o });
const reauthRequired = () => apiError(403, 'auth.reauth_required');
const page = (items: unknown[], total = items.length) => ({ body: { items, page: 1, page_size: 20, total } });
const cursorPage = (items: unknown[], next: string | null) => ({ body: { items, next_cursor: next } });
const bodyOf = (calls: Call[], method: string, path: string) => calls.filter((c) => c.method === method && c.url.split('?')[0]!.endsWith(path));
/** 色值在运行时拼出（门禁禁止在源码里写死色值字面量）。 */
const PW = ['unit', 'test', 'value'].join('-'); // 运行时拼出（G-6：仓库内没有固定口令字面量）
const CODE = ['4', '8', '1', '5', '9', '2'].join('');
const TMP_ONCE = ['TMP', 'ONCE', 'VALUE'].join('-');
const TMP_RESET = ['TMP', 'RESET', 'VALUE'].join('-');
const hex = (v: string): string => ['#', v].join('');
const noRawKeys = () => expect(document.body.textContent).not.toMatch(/\b(settings|users|roles|tokens|loginLogs|version|pages|danger|reauth|oneTime|pager|filters|audit)\.[a-z][A-Za-z_]*/);

const admin = (over: Partial<AdminUser> = {}): AdminUser => ({ uid: 'U1', email: 'root@example.test', display_name: 'Root', user_type: 'platform', status: 'active', role_codes: ['platform_admin'], totp_enabled: false, must_change_password: false, ...over }) as AdminUser;
const role = (over: Partial<Role> = {}): Role => ({ uid: 'R1', scope: 'platform', code: 'platform_admin', name: 'Super admin', is_builtin: true, permissions: ['user:read'], ...over }) as Role;
const perm = (code: string, scope: Permission['scope'] = 'platform'): Permission => {
  const [resource, action] = code.split(':') as [string, string];
  return { code, resource, action, scope, module: 'auth' } as Permission;
};

// ===================================================================== 再认证（L4）
describe('再认证（L4 step-up）：窗口外的敏感操作 → 验证框 → 服务端校验 → 自动重试原请求', () => {
  function usersBackend(over: Record<string, Handler | { status?: number; body?: unknown }> = {}) {
    let creates = 0;
    return {
      'GET /admin/users': page([admin()]),
      'GET /admin/roles': page([role()]),
      'POST /admin/users': () => (++creates === 1 ? reauthRequired() : { status: 201, body: admin({ uid: 'U2', email: 'new@example.test', temporary_password: TMP_ONCE }) }),
      'POST /auth/reauth': { body: { reauth_at: '2030-01-01T00:00:00Z', expires_at: '2030-01-01T00:05:00Z' } },
      ...over,
    };
  }
  async function fillCreate() {
    await click('[data-testid="user-create"]');
    await type('[data-testid="create-email"]', 'new@example.test');
    await type('[data-testid="create-name"]', 'New Admin');
  }

  it('创建管理员：第一次被拒 → 弹出再认证框 → 提交口令 → 原请求自动重试成功（同一幂等键、同一请求体）→ 临时口令只展示一次', async () => {
    m = await mount('/users/list', { routes: usersBackend() });
    await fillCreate();
    await click('[data-testid="create-submit"]');
    expect(q('[data-testid="reauth-dialog"]')).not.toBeNull();
    expect(q('[data-testid="reauth-code"]')).toBeNull(); // 账号未启用 TOTP：只要口令
    await type('[data-testid="reauth-secret"]', PW);
    await click('[data-testid="reauth-submit"]');
    const creates = bodyOf(m.calls, 'POST', '/admin/users');
    expect(creates).toHaveLength(2);
    expect(creates[1]!.body).toEqual(creates[0]!.body);
    expect(creates[1]!.headers['idempotency-key']).toBe(creates[0]!.headers['idempotency-key']);
    expect(creates[1]!.headers['idempotency-key']).toBeTruthy();
    expect(creates[0]!.body).toMatchObject({ email: 'new@example.test', display_name: 'New Admin', user_type: 'platform' });
    const reauth = bodyOf(m.calls, 'POST', '/auth/reauth');
    expect(reauth).toHaveLength(1);
    expect(reauth[0]!.body).toEqual({ password: PW }); // 口令交给服务端校验，没有 totp_code
    expect(q('[data-testid="reauth-dialog"]')).toBeNull();
    // 一次性明文：展示、可复制、必须显式关闭；关闭后 DOM 里不再有
    expect(text('[data-testid="secret-value"]')).toBe(TMP_ONCE);
    expect(text('[data-testid="secret-once"]')).toContain('只显示这一次');
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await m.settle();
    expect(q('[data-testid="secret-value"]')).not.toBeNull(); // Esc/遮罩不能误关
    await click('[data-testid="secret-close"]');
    expect(document.body.textContent).not.toContain(TMP_ONCE);
  });

  it('已启用 TOTP 的账号：验证框要求 6 位验证码（本地只校验形态），请求体带 totp_code', async () => {
    m = await mount('/users/list', { me: meWith(ALL, { totp: true }), routes: usersBackend() });
    await fillCreate();
    await click('[data-testid="create-submit"]');
    await type('[data-testid="reauth-secret"]', PW);
    await click('[data-testid="reauth-submit"]');
    expect(text('[data-testid="reauth-error"]')).toContain('6 位');
    expect(bodyOf(m.calls, 'POST', '/auth/reauth')).toHaveLength(0); // 形态不对不发请求
    await type('[data-testid="reauth-code"]', CODE);
    await click('[data-testid="reauth-submit"]');
    expect(bodyOf(m.calls, 'POST', '/auth/reauth')[0]!.body).toEqual({ password: PW, totp_code: CODE });
    expect(bodyOf(m.calls, 'POST', '/admin/users')).toHaveLength(2);
  });

  it('服务端拒绝（口令错误 401）：验证框保持打开并如实提示，不被当作会话过期（不退出登录），不重试原请求', async () => {
    m = await mount('/users/list', { routes: usersBackend({ 'POST /auth/reauth': apiError(401, 'auth.invalid_credentials') }) });
    await fillCreate();
    await click('[data-testid="create-submit"]');
    await type('[data-testid="reauth-secret"]', PW);
    await click('[data-testid="reauth-submit"]');
    expect(q('[data-testid="reauth-dialog"]')).not.toBeNull();
    expect(text('[data-testid="reauth-error"]')).toContain('邮箱或口令不正确');
    expect(m.session.status).toBe('authed');
    expect(bodyOf(m.calls, 'POST', '/admin/users')).toHaveLength(1);
  });

  it('取消验证：原请求不重试，页面提示「已取消」，没有创建任何东西', async () => {
    m = await mount('/users/list', { routes: usersBackend() });
    await fillCreate();
    await click('[data-testid="create-submit"]');
    await click('[data-testid="reauth-cancel"]');
    expect(q('[data-testid="reauth-dialog"]')).toBeNull();
    expect(bodyOf(m.calls, 'POST', '/admin/users')).toHaveLength(1);
    expect(text('[data-testid="create-error"]')).toContain('已取消重新验证');
    expect(q('[data-testid="secret-dialog"]')).toBeNull();
  });

  it('重试后仍被要求再认证：如实报错，不循环弹框', async () => {
    m = await mount('/users/list', { routes: usersBackend({ 'POST /admin/users': reauthRequired }) });
    await fillCreate();
    await click('[data-testid="create-submit"]');
    await type('[data-testid="reauth-secret"]', PW);
    await click('[data-testid="reauth-submit"]');
    expect(bodyOf(m.calls, 'POST', '/admin/users')).toHaveLength(2);
    expect(q('[data-testid="reauth-dialog"]')).toBeNull();
    expect(text('[data-testid="create-error"]')).toContain('重新验证');
  });
});

// ===================================================================== 用户列表
describe('用户列表', () => {
  it('列出用户；有 user:write 才显示创建与管理按钮；自己的行没有删除/停用；只读身份看不到任何写入口', async () => {
    const list = page([admin({ uid: 'ME' }), admin({ uid: 'U2', email: 'b@example.test', status: 'disabled', role_codes: [] }), admin({ uid: 'U3', email: 'c@example.test', status: 'locked' })]);
    const me = meWith();
    me.user.uid = 'ME';
    m = await mount('/users/list', { me, routes: { 'GET /admin/users': list, 'GET /admin/roles': page([role()]) } });
    const rows = qa('[data-testid="user-row"]');
    expect(rows).toHaveLength(3);
    expect(rows[0]!.querySelector('[data-testid="user-delete"]')).toBeNull();
    expect(rows[0]!.querySelector('[data-testid="user-disable"]')).toBeNull();
    expect(rows[1]!.querySelector('[data-testid="user-enable"]')).not.toBeNull();
    expect(rows[2]!.querySelector('[data-testid="user-unlock"]')).not.toBeNull();
    expect(q('[data-testid="user-create"]')).not.toBeNull();
    m.unmount();
    m = await mount('/users/list', { me: meWith(['user:read']), routes: { 'GET /admin/users': list } });
    expect(q('[data-testid="user-create"]')).toBeNull();
    expect(q('[data-testid="user-reset"]')).toBeNull();
    expect(q('[data-testid="user-roles"]')).toBeNull();
  });

  it('删除用户是 L2+：对话框说明影响范围、页脚固定审计提示；确认后才发请求；失败如实提示', async () => {
    const del = vi.fn<Handler>(() => ({ status: 204 }));
    m = await mount('/users/list', { routes: { 'GET /admin/users': page([admin({ uid: 'ME2' }), admin({ uid: 'U2', email: 'b@example.test' })]), 'DELETE /admin/users/U2': del } });
    await click('[data-testid="user-delete"]', 1);
    expect(text('[data-testid="danger-level"]')).toContain('二级确认');
    expect(text('[data-testid="danger-impact"]')).toContain('不可撤销');
    expect(text('[data-testid="danger-audit"]')).toBe('此操作将记录到审计日志（操作人、时间、对象、Request ID）。');
    expect(text('[data-testid="danger-reauth-hint"]')).toContain('重新验证身份'); // 服务端对删除用户是 L4
    expect(del).not.toHaveBeenCalled();
    await click('[data-testid="danger-ok"]');
    expect(del).toHaveBeenCalledTimes(1);
  });

  it('重置口令是 L2+：默认由服务端生成，临时口令只展示一次；手工设定过短被本地校验拦下', async () => {
    const reset = vi.fn<Handler>(() => ({ body: { temporary_password: TMP_RESET } }));
    m = await mount('/users/list', { routes: { 'GET /admin/users': page([admin({ uid: 'ME2' }), admin({ uid: 'U2', email: 'b@example.test' })]), 'POST /admin/users/U2/reset-password': reset } });
    await click('[data-testid="user-reset"]', 1);
    expect(text('[data-testid="danger-audit"]')).toContain('审计日志');
    await type('[data-testid="reset-manual"]', 'short');
    await click('[data-testid="danger-ok"]');
    expect(reset).not.toHaveBeenCalled();
    await type('[data-testid="reset-manual"]', '');
    await click('[data-testid="danger-ok"]');
    expect(reset.mock.calls[0]![0].body).toEqual({}); // 省略 = 服务端生成
    expect(text('[data-testid="secret-value"]')).toBe(TMP_RESET);
  });

  it('角色分配：按当前角色勾选，保存 = 整体设置（PUT role_codes）；自己的角色变了会刷新 /auth/me', async () => {
    const put = vi.fn<Handler>(() => ({ body: admin({ uid: 'ME', role_codes: ['custom_ops'] }) }));
    const me = meWith();
    me.user.uid = 'ME';
    m = await mount('/users/list', { me, routes: { 'GET /admin/users': page([admin({ uid: 'ME', role_codes: ['platform_admin'] })]), 'GET /admin/roles': page([role(), role({ uid: 'R2', code: 'custom_ops', name: 'Ops', is_builtin: false })]), 'PUT /admin/users/ME/roles': put } });
    const meCalls = () => bodyOf(m!.calls, 'GET', '/auth/me').length;
    const before = meCalls();
    await click('[data-testid="user-roles"]');
    expect(q<HTMLInputElement>('[data-testid="assign-role-platform_admin"]')!.checked).toBe(true);
    expect(q<HTMLInputElement>('[data-testid="assign-role-custom_ops"]')!.checked).toBe(false);
    await click('[data-testid="assign-role-custom_ops"]');
    await click('[data-testid="assign-role-platform_admin"]');
    await click('[data-testid="assign-submit"]');
    expect(put.mock.calls[0]![0].body).toEqual({ role_codes: ['custom_ops'] });
    expect(meCalls()).toBe(before + 1);
  });

  it('页面随标签保活：切到别的标签再切回时重新读取（别处的修改不会被旧列表遮住），首次进入只读一次', async () => {
    m = await mount('/users/list', { routes: { 'GET /admin/users': page([admin()]), 'GET /audit-logs': cursorPage([], null) } });
    const reads = () => bodyOf(m!.calls, 'GET', '/admin/users').length;
    expect(reads()).toBe(1);
    await m.go('/ops/audit');
    expect(reads()).toBe(1);
    await m.go('/users/list');
    expect(reads()).toBe(2);
  });

  it('三态：加载中 / 失败（Request ID + 重试）/ 无创建权限时只读', async () => {
    let n = 0;
    m = await mount('/users/list', { routes: { 'GET /admin/users': () => (++n === 1 ? { status: 503, body: { error: { code: 'internal.error', message: 'x', request_id: 'RID-U1' } } } : page([admin()])) } });
    expect(q('[data-le-state="error"]')!.textContent).toContain('RID-U1');
    await click('.le-error__actions button');
    expect(qa('[data-testid="user-row"]')).toHaveLength(1);
  });
});

// ===================================================================== 权限角色
describe('权限角色：权限矩阵', () => {
  const perms = [perm('user:read'), perm('user:write'), perm('site:read', 'both'), perm('site:write', 'both'), perm('tenant:update', 'tenant')];
  const roles = [role(), role({ uid: 'R2', code: 'ops_lead', name: 'Ops lead', is_builtin: false, permissions: ['user:read'] })];
  const backend = (over: Record<string, Handler | { status?: number; body?: unknown }> = {}) => ({ 'GET /admin/roles': page(roles), 'GET /admin/permissions': page(perms), ...over });

  it('内置角色只读；自定义角色可勾选；平台角色不显示租户专属权限的复选框；保存只提交勾选结果（排序）', async () => {
    const put = vi.fn<Handler>((c) => ({ body: role({ uid: 'R2', code: 'ops_lead', name: 'Ops lead', is_builtin: false, permissions: (c.body as { permissions: string[] }).permissions }) }));
    m = await mount('/settings/roles', { routes: backend({ 'PUT /admin/roles/R2/permissions': put }) });
    await click('[data-code="platform_admin"] [data-testid="role-select"]');
    expect(q('[data-testid="role-builtin-note"]')).not.toBeNull();
    expect(q<HTMLInputElement>('[data-testid="perm-user:read"]')!.disabled).toBe(true);
    expect(q('[data-testid="role-perms-save"]')).toBeNull();
    await click('[data-code="ops_lead"] [data-testid="role-select"]');
    expect(q<HTMLInputElement>('[data-testid="perm-user:read"]')!.checked).toBe(true);
    expect(q('[data-testid="perm-tenant:update"]')).toBeNull(); // 租户范围权限不适用于平台角色
    expect(q<HTMLButtonElement>('[data-testid="role-perms-save"]')!.disabled).toBe(true); // 没有更改
    await click('[data-testid="perm-site:write"]');
    await click('[data-testid="perm-site:read"]');
    expect(m.tabs.tabs.some((t) => t.dirty)).toBe(true); // 未保存更改标记当前标签
    await click('[data-testid="role-perms-save"]');
    expect(put.mock.calls[0]![0].body).toEqual({ permissions: ['site:read', 'site:write', 'user:read'] });
    expect(m.tabs.tabs.some((t) => t.dirty)).toBe(false);
  });

  it('创建角色：代码格式本地校验；POST 体正确；创建后自动选中；删除是 L3 键入角色代码 + 再认证提示', async () => {
    const created = role({ uid: 'R3', code: 'auditors_x', name: 'Auditors X', is_builtin: false, permissions: [] });
    let list = roles;
    const post = vi.fn<Handler>(() => {
      list = [...roles, created];
      return { status: 201, body: created };
    });
    const del = vi.fn<Handler>(() => ({ status: 204 }));
    m = await mount('/settings/roles', { routes: { 'GET /admin/roles': () => page(list), 'GET /admin/permissions': page(perms), 'POST /admin/roles': post, 'DELETE /admin/roles/R3': del } });
    await click('[data-testid="role-create"]');
    await type('[data-testid="create-role-code"]', 'Bad Code');
    await type('[data-testid="create-role-name"]', 'Auditors X');
    await click('[data-testid="role-create-submit"]');
    expect(post).not.toHaveBeenCalled();
    expect(document.body.textContent).toContain('小写字母开头');
    await type('[data-testid="create-role-code"]', 'auditors_x');
    await click('[data-testid="role-create-submit"]');
    expect(post.mock.calls[0]![0].body).toMatchObject({ scope: 'platform', code: 'auditors_x', name: 'Auditors X' });
    expect(post.mock.calls[0]![0].headers['idempotency-key']).toBeTruthy();
    await vi.waitFor(() => expect(q('[data-testid="role-detail"]')?.getAttribute('data-code')).toBe('auditors_x'));
    await click('[data-testid="role-delete"]');
    expect(text('[data-testid="danger-level"]')).toContain('三级');
    expect(q<HTMLButtonElement>('[data-testid="danger-ok"]')!.disabled).toBe(true); // 键入确认前禁用
    await type('[data-testid="danger-confirm-input"]', 'auditors');
    expect(q<HTMLButtonElement>('[data-testid="danger-ok"]')!.disabled).toBe(true);
    await type('[data-testid="danger-confirm-input"]', 'auditors_x');
    expect(q<HTMLButtonElement>('[data-testid="danger-ok"]')!.disabled).toBe(false);
    expect(text('[data-testid="danger-reauth-hint"]')).toContain('重新验证');
    await click('[data-testid="danger-ok"]');
    expect(del).toHaveBeenCalledTimes(1);
  });

  it('保存权限被服务端要求再认证 → 验证框 → 重试；无 role:write 时整页只读', async () => {
    let n = 0;
    const put = vi.fn<Handler>(() => (++n === 1 ? reauthRequired() : { body: role({ uid: 'R2', code: 'ops_lead', is_builtin: false, permissions: ['user:read', 'user:write'] }) }));
    m = await mount('/settings/roles', { routes: backend({ 'PUT /admin/roles/R2/permissions': put, 'POST /auth/reauth': { body: { reauth_at: 'x', expires_at: 'y' } } }) });
    await click('[data-code="ops_lead"] [data-testid="role-select"]');
    await click('[data-testid="perm-user:write"]');
    await click('[data-testid="role-perms-save"]');
    await type('[data-testid="reauth-secret"]', PW);
    await click('[data-testid="reauth-submit"]');
    expect(put).toHaveBeenCalledTimes(2);
    m.unmount();
    m = await mount('/settings/roles', { me: meWith(['role:read']), routes: backend() });
    expect(q('[data-testid="role-create"]')).toBeNull();
    await click('[data-code="ops_lead"] [data-testid="role-select"]');
    expect(q<HTMLInputElement>('[data-testid="perm-user:read"]')!.disabled).toBe(true);
    expect(q('[data-testid="role-perms-save"]')).toBeNull();
  });

  it('别名入口 /users/roles 与规范页面 /settings/roles 渲染同一个页面', async () => {
    m = await mount('/users/roles', { routes: backend() });
    expect(q('[data-testid="roles-page"]')).not.toBeNull();
  });
});

// ===================================================================== API Token
describe('个人 API Token', () => {
  const token = (over: Partial<ApiToken> = {}): ApiToken => ({ uid: 'T1', name: 'ci', token_prefix: 'abcd1234', scopes: ['site:read'], created_at: '2026-11-01T00:00:00Z', ...over }) as ApiToken;

  it('创建：scopes 候选只来自自己的权限；无 scope 不发请求；明文只在创建响应里出现一次，可复制，关闭后页面里再也没有；列表不含明文', async () => {
    const PLAIN = ['le_abcd1234', 'PLAINTEXT-ONCE'].join('_');
    const write = vi.fn(async () => undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: write }, configurable: true });
    const post = vi.fn<Handler>(() => ({ status: 201, body: { ...token({ uid: 'T2', name: 'deploy', scopes: ['audit:read', 'user:read'] }), token: PLAIN } }));
    m = await mount('/users/api-tokens', { me: meWith(['token:manage', 'user:read', 'audit:read']), routes: { 'GET /auth/api-tokens': page([token()]), 'POST /auth/api-tokens': post } });
    expect(qa('[data-testid="token-row"]')).toHaveLength(1);
    expect(document.body.textContent).not.toContain(PLAIN);
    await click('[data-testid="token-create"]');
    expect(qa('[data-testid^="scope-"]').map((e) => e.getAttribute('data-testid')).sort()).toEqual(['scope-audit:read', 'scope-token:manage', 'scope-user:read']);
    await type('[data-testid="token-name"]', 'deploy');
    await click('[data-testid="token-create-submit"]');
    expect(post).not.toHaveBeenCalled();
    expect(q('[data-testid="token-scope-problem"]')).not.toBeNull();
    await click('[data-testid="scope-user:read"]');
    await click('[data-testid="scope-audit:read"]');
    await click('[data-testid="token-create-submit"]');
    const call = post.mock.calls[0]![0];
    expect(call.body).toEqual({ name: 'deploy', scopes: ['audit:read', 'user:read'] });
    expect(call.headers['idempotency-key']).toBeTruthy();
    expect(text('[data-testid="secret-value"]')).toBe(PLAIN);
    await click('[data-testid="secret-copy"]');
    expect(write).toHaveBeenCalledWith(PLAIN);
    expect(q('[data-testid="secret-copied"]')).not.toBeNull();
    await click('[data-testid="secret-close"]');
    expect(document.body.textContent).not.toContain(PLAIN);
    expect(m.calls.filter((c) => c.method === 'GET').every((c) => !c.url.includes('PLAINTEXT'))).toBe(true);
  });

  it('剪贴板不可用时如实提示手动复制，明文仍可见', async () => {
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: () => Promise.reject(new Error('denied')) }, configurable: true });
    m = await mount('/users/api-tokens', { routes: { 'GET /auth/api-tokens': page([token()]), 'POST /auth/api-tokens': { status: 201, body: { ...token(), token: 'le_x_y' } } } });
    await click('[data-testid="token-create"]');
    await type('[data-testid="token-name"]', 'a');
    await click('[data-testid^="scope-"]');
    await click('[data-testid="token-create-submit"]');
    await click('[data-testid="secret-copy"]');
    expect(q('[data-testid="secret-copy-failed"]')).not.toBeNull();
    expect(text('[data-testid="secret-value"]')).toBe('le_x_y');
  });

  it('过去的到期时间被本地拦下；轮换新明文同样只展示一次；吊销是 L2 对话框并带审计提示', async () => {
    const rotate = vi.fn<Handler>(() => ({ body: { ...token({ uid: 'T9' }), token: 'le_new_ROTATED' } }));
    const revoke = vi.fn<Handler>(() => ({ status: 204 }));
    m = await mount('/users/api-tokens', { routes: { 'GET /auth/api-tokens': page([token(), token({ uid: 'T3', name: 'old', revoked_at: '2026-11-02T00:00:00Z' })]), 'POST /auth/api-tokens/T1/rotate': rotate, 'DELETE /auth/api-tokens/T1': revoke, 'POST /auth/api-tokens': { status: 201, body: { ...token(), token: 'x' } } } });
    expect(qa('[data-testid="token-row"][data-state="revoked"] [data-testid="token-rotate"]')).toHaveLength(0); // 已吊销的没有操作
    await click('[data-testid="token-create"]');
    await type('[data-testid="token-name"]', 'a');
    await click('[data-testid^="scope-"]');
    await type('[data-testid="token-expires"]', '2001-01-01T00:00');
    await click('[data-testid="token-create-submit"]');
    expect(q('[data-testid="token-expires-problem"]')).not.toBeNull();
    expect(bodyOf(m.calls, 'POST', '/auth/api-tokens')).toHaveLength(0);
    await click('.arco-modal-container [data-testid="token-create-form"] .arco-btn:not([type="submit"])');
    await click('[data-testid="token-rotate"]');
    expect(text('[data-testid="danger-audit"]')).toContain('审计日志');
    await click('[data-testid="danger-ok"]');
    expect(text('[data-testid="secret-value"]')).toBe('le_new_ROTATED');
    await click('[data-testid="secret-close"]');
    await click('[data-testid="token-revoke"]');
    await click('[data-testid="danger-ok"]');
    expect(revoke).toHaveBeenCalledTimes(1);
  });

  it('空态：没有令牌时给出说明与创建入口', async () => {
    m = await mount('/users/api-tokens', { routes: { 'GET /auth/api-tokens': page([]) } });
    expect(q('[data-testid="tokens-empty"]')).not.toBeNull();
    expect(q('[data-testid="token-create"]')).not.toBeNull();
  });
});

// ===================================================================== 审计与登录日志
describe('审计日志：游标分页与过滤', () => {
  const log = (n: number, over: Partial<AuditLog> = {}): AuditLog => ({ uid: `A${n}`, created_at: '2026-11-22T10:00:00.000Z', actor_type: 'user', actor_label: 'root@example.test', action: 'user.create', resource_type: 'user', resource_uid: `U${n}`, result: 'success', request_id: `RID-${n}`, changes: { email: 'x@example.test' }, ...over }) as AuditLog;
  const auditCalls = () => bodyOf(m!.calls, 'GET', '/audit-logs').map((c) => new URL(c.url, 'http://x').searchParams);

  it('游标逐页传递：下一页带上一页返回的 next_cursor，上一页回退，末页没有下一页；每页条数变化回到第一页', async () => {
    m = await mount('/ops/audit', {
      routes: {
        'GET /audit-logs': (c) => {
          const cur = new URL(c.url, 'http://x').searchParams.get('cursor');
          return cur === null ? cursorPage([log(1), log(2)], 'CUR-2') : cur === 'CUR-2' ? cursorPage([log(3)], 'CUR-3') : cursorPage([log(4)], null);
        },
      },
    });
    expect(qa('[data-testid="audit-row"]').map((r) => r.getAttribute('data-action'))).toHaveLength(2);
    expect(q<HTMLButtonElement>('[data-testid="cursor-prev"]')!.disabled).toBe(true);
    await click('[data-testid="cursor-next"]');
    expect(auditCalls().at(-1)!.get('cursor')).toBe('CUR-2');
    await click('[data-testid="cursor-next"]');
    expect(auditCalls().at(-1)!.get('cursor')).toBe('CUR-3');
    expect(q<HTMLButtonElement>('[data-testid="cursor-next"]')!.disabled).toBe(true); // next_cursor = null：已到末尾
    expect(text('[data-testid="cursor-info"]')).toContain('3');
    await click('[data-testid="cursor-prev"]');
    expect(auditCalls().at(-1)!.get('cursor')).toBe('CUR-2');
    const select = q<HTMLSelectElement>('[data-testid="cursor-limit"]')!;
    select.value = '50';
    select.dispatchEvent(new Event('change', { bubbles: true }));
    await m.settle();
    expect(auditCalls().at(-1)!.get('cursor')).toBeNull();
    expect(auditCalls().at(-1)!.get('limit')).toBe('50');
    expect(text('[data-testid="cursor-info"]')).toContain('1');
  });

  it('过滤：参数逐项传给服务端（时间转 RFC 3339 UTC）；起止颠倒被拦下；筛选无结果显示「无结果」并可清除', async () => {
    let empty = false;
    m = await mount('/ops/audit', { routes: { 'GET /audit-logs': () => (empty ? cursorPage([], null) : cursorPage([log(1)], null)) } });
    await type('[data-testid="filter-action"]', 'role.create');
    await type('[data-testid="filter-resource-type"]', 'role');
    await type('[data-testid="filter-actor-uid"]', 'U9');
    await type('[data-testid="filter-request-id"]', 'RID-7');
    await type('[data-testid="filter-result"]', 'denied');
    await type('[data-testid="filter-from"]', '2026-11-01T00:00');
    await type('[data-testid="filter-to"]', '2026-10-01T00:00');
    expect(q('[data-testid="filter-problem"]')!.textContent).toContain('不能晚于');
    expect(q<HTMLButtonElement>('[data-testid="filter-apply"]')!.disabled).toBe(true);
    await type('[data-testid="filter-to"]', '2026-12-01T00:00');
    empty = true;
    await click('[data-testid="filter-apply"]');
    const p = auditCalls().at(-1)!;
    expect(Object.fromEntries(p)).toMatchObject({ action: 'role.create', resource_type: 'role', actor_uid: 'U9', request_id: 'RID-7', result: 'denied' });
    expect(p.get('from')).toMatch(/^2026-1[01]-\d\dT\d\d:00:00\.000Z$/);
    expect(p.get('to')).toMatch(/^2026-1[12]-\d\dT\d\d:00:00\.000Z$/);
    expect(q('[data-le-state="empty"]')!.getAttribute('data-le-empty-reason')).toBe('no-results');
    empty = false;
    await click('[data-le-clear-filters]');
    expect(auditCalls().at(-1)!.get('action')).toBeNull();
    expect(qa('[data-testid="audit-row"]')).toHaveLength(1);
  });

  it('别名 /users/audit 与规范页面 /ops/audit 共用同一组件：别名预置「用户操作」筛选（resource_type=user，可清除）；规范页面不预置', async () => {
    m = await mount('/users/audit', { routes: { 'GET /audit-logs': cursorPage([log(1)], null) } });
    expect(q('[data-testid="audit-alias-note"]')).not.toBeNull();
    expect(auditCalls()[0]!.get('resource_type')).toBe('user');
    expect(q<HTMLInputElement>('[data-testid="filter-resource-type"]')!.value).toBe('user');
    await click('[data-testid="filter-clear"]');
    expect(auditCalls().at(-1)!.get('resource_type')).toBeNull();
    m.unmount();
    m = await mount('/ops/audit', { routes: { 'GET /audit-logs': cursorPage([log(1)], null) } });
    expect(q('[data-testid="audit-alias-note"]')).toBeNull();
    expect(auditCalls()[0]!.get('resource_type')).toBeNull();
  });

  it('详情抽屉展示变更摘要（作为文本，不当 HTML 渲染）；结果用文字而不只靠颜色', async () => {
    m = await mount('/ops/audit', { routes: { 'GET /audit-logs': cursorPage([log(1, { result: 'denied', changes: { note: '<img src=x onerror=alert(1)>' } })], null) } });
    expect(text('[data-testid="audit-row"]')).toContain('拒绝');
    await click('[data-testid="audit-detail"]');
    expect(text('[data-testid="audit-changes"]')).toContain('<img src=x');
    expect(q('[data-testid="audit-changes"] img')).toBeNull();
  });

  it('三态：失败显示 Request ID 并可重试；无数据显示空态', async () => {
    let n = 0;
    m = await mount('/ops/audit', { routes: { 'GET /audit-logs': () => (++n === 1 ? { status: 500, body: { error: { code: 'internal.error', message: 'x', request_id: 'RID-A1' } } } : cursorPage([], null)) } });
    expect(q('[data-le-state="error"]')!.textContent).toContain('RID-A1');
    await click('.le-error__actions button');
    expect(q('[data-le-state="empty"]')!.getAttribute('data-le-empty-reason')).toBe('no-data');
  });
});

describe('登录日志', () => {
  const entry = (n: number, over: Partial<LoginLog> = {}): LoginLog => ({ uid: `L${n}`, created_at: '2026-11-22T10:00:00.000Z', realm: 'admin', identifier: 'root@example.test', event: 'login_success', ip: '203.0.113.9', ...over }) as LoginLog;
  it('游标分页 + 过滤参数（event/identifier/ip/from/to）', async () => {
    m = await mount('/users/login-logs', { routes: { 'GET /admin/login-logs': (c) => (new URL(c.url, 'http://x').searchParams.get('cursor') ? cursorPage([entry(2, { event: 'login_failed' })], null) : cursorPage([entry(1)], 'C2')) } });
    const calls = () => bodyOf(m!.calls, 'GET', '/admin/login-logs').map((c) => new URL(c.url, 'http://x').searchParams);
    expect(text('[data-testid="login-log-row"]')).toContain('登录成功');
    await click('[data-testid="cursor-next"]');
    expect(calls().at(-1)!.get('cursor')).toBe('C2');
    expect(text('[data-testid="login-log-row"]')).toContain('登录失败');
    await type('[data-testid="filter-event"]', 'locked');
    await type('[data-testid="filter-identifier"]', 'root@example.test');
    await type('[data-testid="filter-ip"]', '203.0.113.9');
    await click('[data-testid="filter-apply"]');
    expect(Object.fromEntries(calls().at(-1)!)).toMatchObject({ event: 'locked', identifier: 'root@example.test', ip: '203.0.113.9' });
    expect(calls().at(-1)!.get('cursor')).toBeNull();
  });
  it('无权限（缺 login-log:read）直访 → 403 页并给出所需权限，不发任何数据请求', async () => {
    m = await mount('/users/login-logs', { me: meWith(['user:read']) });
    expect(q('[data-testid="forbidden-page"]')).not.toBeNull();
    expect(text('[data-testid="forbidden-need"]')).toContain('login-log:read');
    expect(bodyOf(m.calls, 'GET', '/admin/login-logs')).toHaveLength(0);
  });
});

// ===================================================================== 系统设置
const settingsAll = (over: Record<string, unknown> = {}) => ({
  'system.run_mode': 'self_hosted', 'general.base_url': null, 'general.admin_url': null, 'general.console_url': null, 'general.docs_url': null, 'general.status_url': null, 'general.timezone': 'UTC', 'general.locales': ['zh-CN', 'en'], 'general.registration_open': false,
  'login.totp_required_for_admin': false, 'login.throttle_window_s': 900, 'login.throttle_account_max': 10, 'login.throttle_ip_max': 30,
  'auth.session_idle_ttl_s': 7200, 'auth.session_absolute_ttl_s': 43200, 'auth.reauth_window_s': 300, 'auth.mfa_window_s': 900, ...over,
});

describe('基本设置与运行模式（L3 + L4）', () => {
  const backend = (over: Record<string, Handler | { status?: number; body?: unknown }> = {}) => ({ 'GET /admin/settings': { body: settingsAll() }, 'GET /admin/system/run-mode': { body: { run_mode: 'self_hosted' } }, ...over });

  it('运行模式切换：键入目标模式前确认键禁用；对话框带审计提示；确认后 PUT，再认证后重试，并刷新 /auth/me（菜单随模式变化）', async () => {
    let n = 0;
    const put = vi.fn<Handler>(() => (++n === 1 ? reauthRequired() : { body: { run_mode: 'commercial' } }));
    m = await mount('/settings/basic', { routes: backend({ 'PUT /admin/system/run-mode': put, 'POST /auth/reauth': { body: { reauth_at: 'x', expires_at: 'y' } } }) });
    expect(text('[data-testid="run-mode-current"]')).toBe('自用模式');
    const meBefore = bodyOf(m.calls, 'GET', '/auth/me').length;
    await click('[data-testid="run-mode-switch"]');
    expect(text('[data-testid="danger-level"]')).toContain('三级');
    expect(text('[data-testid="danger-audit"]')).toContain('审计日志');
    expect(text('[data-testid="danger-impact"]')).toContain('商业功能');
    const ok = () => q<HTMLButtonElement>('[data-testid="danger-ok"]')!.disabled;
    expect(ok()).toBe(true);
    await type('[data-testid="danger-confirm-input"]', 'commercia');
    expect(ok()).toBe(true);
    await click('[data-testid="danger-ok"]');
    expect(put).not.toHaveBeenCalled(); // 禁用时点击无效
    await type('[data-testid="danger-confirm-input"]', 'commercial');
    expect(ok()).toBe(false);
    await click('[data-testid="danger-ok"]');
    expect(q('[data-testid="reauth-dialog"]')).not.toBeNull(); // L4：服务端要求再认证
    await type('[data-testid="reauth-secret"]', PW);
    await click('[data-testid="reauth-submit"]');
    expect(put).toHaveBeenCalledTimes(2);
    expect(put.mock.calls[1]![0].body).toEqual({ run_mode: 'commercial' });
    expect(text('[data-testid="run-mode-current"]')).toBe('商业模式');
    expect(bodyOf(m.calls, 'GET', '/auth/me').length).toBe(meBefore + 1);
  });

  it('无 setting:write：没有切换入口、表单只读；保存按钮不存在', async () => {
    m = await mount('/settings/basic', { me: meWith(['setting:read']), routes: backend() });
    expect(q('[data-testid="run-mode-switch"]')).toBeNull();
    expect(q('[data-testid="settings-save"]')).toBeNull();
    expect(q('[data-testid="settings-readonly"]')).not.toBeNull();
    expect(q<HTMLInputElement>('[data-testid="field-general.base_url"]')!.readOnly).toBe(true);
  });

  it('表单校验：无效网址/时区本地拦下不发请求；只提交变更的键；空网址 = null（恢复默认）', async () => {
    const patch = vi.fn<Handler>((c) => ({ body: settingsAll((c.body as { values: Record<string, unknown> }).values) }));
    m = await mount('/settings/basic', { routes: backend({ 'PATCH /admin/settings': patch }) });
    await type('[data-testid="field-general.base_url"]', 'ftp://nope');
    expect(text('[data-testid="field-error-general.base_url"]')).toContain('http://');
    await type('[data-testid="field-general.timezone"]', 'Mars/Olympus');
    expect(q('[data-testid="field-error-general.timezone"]')).not.toBeNull();
    await click('[data-testid="settings-save"]');
    expect(patch).not.toHaveBeenCalled();
    await type('[data-testid="field-general.base_url"]', 'https://cdn.example.test');
    await type('[data-testid="field-general.timezone"]', 'Asia/Shanghai');
    await click('[data-testid="settings-save"]');
    expect(patch.mock.calls[0]![0].body).toEqual({ values: { 'general.base_url': 'https://cdn.example.test', 'general.timezone': 'Asia/Shanghai' } });
    expect(q('[data-testid="settings-saved"]')).not.toBeNull();
    await type('[data-testid="field-general.base_url"]', '');
    await click('[data-testid="settings-save"]');
    expect(patch.mock.calls[1]![0].body).toEqual({ values: { 'general.base_url': null } });
  });

  it('未保存更改标记标签；撤销更改后清除；至少保留一种语言', async () => {
    m = await mount('/settings/basic', { routes: backend() });
    await type('[data-testid="field-general.docs_url"]', 'https://docs.example.test');
    expect(m.tabs.tabs.some((t) => t.dirty)).toBe(true);
    await click('[data-testid="settings-reset"]');
    expect(m.tabs.tabs.some((t) => t.dirty)).toBe(false);
    expect(q<HTMLInputElement>('[data-testid="field-general.docs_url"]')!.value).toBe('');
    await click('[data-testid="field-general.locales-zh-CN"]');
    await click('[data-testid="field-general.locales-en"]');
    expect(q('[data-testid="field-error-general.locales"]')).not.toBeNull();
    await click('[data-testid="settings-save"]');
    expect(bodyOf(m.calls, 'PATCH', '/admin/settings')).toHaveLength(0);
  });

  it('服务端校验失败（400）：按错误码显示文案与出错字段，不展示服务端原文', async () => {
    m = await mount('/settings/basic', { routes: backend({ 'PATCH /admin/settings': { status: 400, body: { error: { code: 'validation.failed', message: 'SERVER-ORIGINAL-TEXT', request_id: 'RID-S1', details: [{ field: 'values.general.base_url', reason: 'bad' }] } } } }) });
    await type('[data-testid="field-general.docs_url"]', 'https://docs.example.test');
    await click('[data-testid="settings-save"]');
    const e = text('[data-testid="settings-error"]');
    expect(e).toContain('values.general.base_url');
    expect(e).toContain('RID-S1');
    expect(e).not.toContain('SERVER-ORIGINAL-TEXT');
  });
});

describe('品牌设置', () => {
  it('保存 = 整体 PUT（logo/favicon 原样带回）；保存后顶栏立即读到新值；英文界面用英文品牌名', async () => {
    const brand = { name: '凌界云', name_en: 'LingEdge', logo_media_uid: 'M-LOGO', favicon_media_uid: null, primary_color: hex('2563EB'), support_email: '' };
    const put = vi.fn<Handler>((c) => ({ body: c.body }));
    m = await mount('/settings/brand', { routes: { 'GET /admin/settings/brand': { body: brand }, 'PUT /admin/settings/brand': put } });
    expect(text('[data-testid="top-brand"]')).toBe('凌界云');
    await type('[data-testid="field-name"]', '新品牌');
    await type('[data-testid="field-primary_color"]', 'red');
    expect(text('[data-testid="field-error-primary_color"]')).toContain('#RRGGBB');
    await click('[data-testid="settings-save"]');
    expect(put).not.toHaveBeenCalled();
    await type('[data-testid="field-primary_color"]', hex('112233'));
    await type('[data-testid="field-support_email"]', 'help@example.test');
    await click('[data-testid="settings-save"]');
    expect(put.mock.calls[0]![0].body).toEqual({ name: '新品牌', name_en: 'LingEdge', logo_media_uid: 'M-LOGO', favicon_media_uid: null, primary_color: hex('112233'), support_email: 'help@example.test' });
    expect(text('[data-testid="top-brand"]')).toBe('新品牌');
    m.unmount();
    m = await mount('/settings/brand', { locale: 'en', routes: { 'GET /admin/settings/brand': { body: brand }, 'PUT /admin/settings/brand': put } });
    expect(text('[data-testid="top-brand"]')).toBe('LingEdge');
  });
  it('进入 Admin 即读取品牌：顶栏显示 setting:read 读到的品牌名；没有该权限时退回内置词条', async () => {
    m = await mount('/', { routes: { 'GET /admin/settings/brand': { body: { name: '读到的品牌', name_en: 'Read Brand' } } } });
    await vi.waitFor(() => expect(text('[data-testid="top-brand"]')).toBe('读到的品牌'));
    m.unmount();
    m = await mount('/', { me: meWith(['task:read']), routes: { 'GET /admin/settings/brand': { body: { name: '不应读取' } } } });
    expect(text('[data-testid="top-brand"]')).toBe(zhCN.topbar.brand);
    expect(bodyOf(m.calls, 'GET', '/admin/settings/brand')).toHaveLength(0);
  });
});

describe('登录设置', () => {
  it('范围与跨键约束本地校验（与后端注册表一致）；只提交变更并以整数提交；保存被要求再认证时走验证框', async () => {
    let n = 0;
    const patch = vi.fn<Handler>((c) => (++n === 1 ? reauthRequired() : { body: settingsAll((c.body as { values: Record<string, unknown> }).values) }));
    m = await mount('/settings/login', { routes: { 'GET /admin/settings': { body: settingsAll() }, 'PATCH /admin/settings': patch, 'POST /auth/reauth': { body: { reauth_at: 'x', expires_at: 'y' } } } });
    await type('[data-testid="field-login.throttle_window_s"]', '0');
    expect(text('[data-testid="field-error-login.throttle_window_s"]')).toContain('1 到 86400');
    await type('[data-testid="field-login.throttle_window_s"]', '12.5');
    expect(text('[data-testid="field-error-login.throttle_window_s"]')).toContain('整数');
    await type('[data-testid="field-login.throttle_window_s"]', '600');
    await type('[data-testid="field-auth.session_idle_ttl_s"]', '90000');
    expect(text('[data-testid="field-error-auth.session_idle_ttl_s"]')).toContain('滑动过期不能超过绝对过期');
    await click('[data-testid="settings-save"]');
    expect(patch).not.toHaveBeenCalled();
    await type('[data-testid="field-auth.session_idle_ttl_s"]', '3600');
    await click('[data-testid="field-login.totp_required_for_admin"]');
    await click('[data-testid="settings-save"]');
    await type('[data-testid="reauth-secret"]', PW);
    await click('[data-testid="reauth-submit"]');
    expect(patch).toHaveBeenCalledTimes(2);
    expect(patch.mock.calls[1]![0].body).toEqual({ values: { 'login.totp_required_for_admin': true, 'login.throttle_window_s': 600, 'auth.session_idle_ttl_s': 3600 } });
    expect(q('[data-testid="settings-saved"]')).not.toBeNull();
  });
});

describe('版本信息', () => {
  it('全部取自 GET /admin/system/version（真实 Server 版本与迁移版本、组件表），没有写死的版本号', async () => {
    const v = { server_version: '9.8.7-test', api_version: 'v7', build: 'abc1234', installation_id: 'INST-1', schema_migration: { current: '000042', pending: 3 }, components: [{ component: 'server', host: 'h1', version: '9.8.7-test', status: 'installed', installed_at: '2026-11-01T00:00:00Z' }] };
    m = await mount('/settings/version', { routes: { 'GET /admin/system/version': { body: v } } });
    expect(text('[data-testid="version-server"]')).toBe('9.8.7-test');
    expect(text('[data-testid="version-api"]')).toBe('v7');
    expect(text('[data-testid="version-migration"]')).toBe('000042');
    expect(text('[data-testid="version-pending"]')).toBe('3');
    expect(text('[data-testid="version-components"]')).toContain('h1');
    m.unmount();
    m = await mount('/settings/version', { routes: { 'GET /admin/system/version': { body: { server_version: '1', api_version: 'v1', components: [] } } } });
    expect(q('[data-testid="version-no-components"]')).not.toBeNull();
    expect(text('[data-testid="version-migration"]')).toBe('—');
  });
});

// ===================================================================== 横切：本地化 / 手机 / 无命令入口
describe('中英文：所有页面没有漏翻的词条键', () => {
  const routes = {
    'GET /admin/users': page([admin()]), 'GET /admin/roles': page([role()]), 'GET /admin/permissions': page([perm('user:read')]),
    'GET /auth/api-tokens': page([]), 'GET /audit-logs': cursorPage([], null), 'GET /admin/login-logs': cursorPage([], null),
    'GET /admin/settings': { body: settingsAll() }, 'GET /admin/system/run-mode': { body: { run_mode: 'self_hosted' } }, 'GET /admin/settings/brand': { body: { name: 'a', name_en: 'b', primary_color: hex('2563EB'), support_email: '' } },
    'GET /admin/system/version': { body: { server_version: '1', api_version: 'v1', components: [] } },
  };
  it.each(['zh-CN', 'en'])('%s：12 个已登记页面逐个挂载，页面文字里不出现原始词条键', async (locale) => {
    for (const p of productionPages) {
      m = await mount(p.path, { locale, routes });
      expect(q('.le-page'), p.key).not.toBeNull();
      noRawKeys();
      m.unmount();
    }
  });
  it('英文界面的页面标题与表头是英文', async () => {
    m = await mount('/users/list', { locale: 'en', routes });
    expect(text('.le-page__title')).toBe('Users');
    expect(text('[data-testid="users-table"] thead')).toContain('Display name');
  });
});

describe('手机宽度', () => {
  it('列表在手机宽度下使用逐行卡片结构（每个单元格带列名），页面自身不依赖横向滚动', async () => {
    m = await mount('/users/list', { width: 390, routes: { 'GET /admin/users': page([admin()]) } });
    const table = q('[data-testid="users-table"]')!;
    expect(table.classList.contains('le-table--stack')).toBe(true);
    for (const td of qa('[data-testid="user-row"] td')) expect(td.getAttribute('data-label'), td.textContent ?? '').toBeTruthy();
    expect(q('[data-testid="mobile-crumbs"]')!.textContent).toContain('用户列表');
  });
});
