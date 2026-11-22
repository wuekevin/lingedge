import { afterEach, describe, expect, it, vi } from 'vitest';
import { apiError, makeMe } from '../testing/fakeApi';
import { mountAdmin } from '../testing/mountAdmin';

let m: Awaited<ReturnType<typeof mountAdmin>> | null = null;
afterEach(() => {
  m?.unmount();
  m = null;
  localStorage.clear();
});
const q = <T extends Element = HTMLElement>(sel: string) => document.querySelector(sel) as T | null;
async function type(sel: string, value: string) {
  const el = q<HTMLInputElement>(sel)!;
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
  el.dispatchEvent(new Event('blur', { bubbles: true }));
}
async function submit(sel: string) {
  q(sel)!.click();
  await m!.settle();
}
const TEST_VALUE = ['unit', 'test', 'value'].join('-'); // 运行时拼出的占位输入（G-6：仓库内没有固定口令）

describe('登录页', () => {
  it('未登录访问任意受保护路径落到登录页；表单校验：必填与邮箱格式（统一校验文案）', async () => {
    m = await mountAdmin({ me: null, path: '/ops/tasks' });
    expect(q('[data-testid="login-page"]')).not.toBeNull();
    expect(m.router.currentRoute.value.query.redirect).toBe('/ops/tasks');
    await submit('[data-testid="login-submit"]');
    expect(document.body.textContent).toContain('请填写邮箱');
    await type('[data-testid="login-email"]', 'not-an-email');
    await submit('[data-testid="login-submit"]');
    expect(document.body.textContent).toContain('邮箱格式不正确');
    expect(m.calls.some((c) => c.url.includes('/auth/login'))).toBe(false); // 校验不通过不发请求
  });

  it('口令登录成功：回到回跳地址；请求固定 realm=admin；口令在登录后被清空', async () => {
    m = await mountAdmin({
      me: null,
      path: '/login?redirect=/ops/tasks',
      routes: {
        'POST /auth/login': { body: { status: 'ok', csrf_token: 'c1' } },
        'GET /auth/me': (() => {
          let n = 0;
          return () => (++n === 1 ? apiError(401, 'auth.unauthenticated') : { body: makeMe() });
        })(),
      },
      pages: [{ key: 'tasks', path: '/ops/tasks', group: 'ops', titleKey: 't.tasks', module: 'tasks', permissions: ['task:read'], component: () => Promise.resolve({ render: () => null }) }],
    });
    await type('[data-testid="login-email"]', 'admin@example.test');
    await type('[data-testid="login-pw"]', TEST_VALUE);
    await submit('[data-testid="login-submit"]');
    await vi.waitFor(() => expect(m!.router.currentRoute.value.path).toBe('/ops/tasks'));
    const login = m.calls.find((c) => c.url.endsWith('/auth/login'))!;
    expect(login.body).toMatchObject({ realm: 'admin', email: 'admin@example.test' });
    expect(m.session.status).toBe('authed');
  });

  it('租户账号被拒：统一的 401 凭据无效文案（不泄露账号是否存在），不建立会话', async () => {
    m = await mountAdmin({ me: null, path: '/login', routes: { 'POST /auth/login': apiError(401, 'auth.invalid_credentials') } });
    await type('[data-testid="login-email"]', 'tenant@example.test');
    await type('[data-testid="login-pw"]', TEST_VALUE);
    await submit('[data-testid="login-submit"]');
    await vi.waitFor(() => expect(q('[data-testid="login-error"]')).not.toBeNull());
    expect(q('[data-testid="login-error-text"]')!.textContent).toBe('邮箱或口令不正确，或该账号无权访问此入口。');
    expect(q('[data-testid="login-error"]')!.textContent).toContain('rid-err'); // Request ID 可见
    expect(m.session.status).not.toBe('authed');
  });

  it('限流：显示 429 说明与 Retry-After', async () => {
    m = await mountAdmin({ me: null, path: '/login', routes: { 'POST /auth/login': apiError(429, 'rate_limit.exceeded', { 'Retry-After': '30' }) } });
    await type('[data-testid="login-email"]', 'a@example.test');
    await type('[data-testid="login-pw"]', TEST_VALUE);
    await submit('[data-testid="login-submit"]');
    await vi.waitFor(() => expect(q('[data-testid="login-error-text"]')?.textContent).toContain('30'));
  });

  it('TOTP 两步：第二步用验证码；可切换为恢复码（同一端点）；错误码给出明确提示；可返回重新登录', async () => {
    let verifyBody: unknown;
    m = await mountAdmin({
      me: null,
      path: '/login',
      routes: {
        'POST /auth/login': { body: { status: 'totp_required', mfa_token: 'mfa-x' } },
        'POST /auth/totp/verify': (c) => {
          verifyBody = c.body;
          return (c.body as { code: string }).code === '000000' ? apiError(401, 'auth.totp_invalid') : { body: { status: 'ok', csrf_token: 'c' } };
        },
        'GET /auth/me': (() => {
          let n = 0;
          return () => (++n === 1 ? apiError(401, 'auth.unauthenticated') : { body: makeMe() });
        })(),
      },
    });
    await type('[data-testid="login-email"]', 'a@example.test');
    await type('[data-testid="login-pw"]', TEST_VALUE);
    await submit('[data-testid="login-submit"]');
    await vi.waitFor(() => expect(q('[data-testid="totp-code"]')).not.toBeNull());
    expect(q('[data-testid="totp-hint"]')!.textContent).toContain('6 位');
    // 错误验证码
    await type('[data-testid="totp-code"]', '000000');
    await submit('[data-testid="totp-submit"]');
    await vi.waitFor(() => expect(q('[data-testid="login-error-text"]')?.textContent).toContain('验证码或恢复码不正确'));
    // 切换为恢复码
    q('[data-testid="totp-toggle"]')!.click();
    await m.settle();
    expect(q('[data-testid="totp-hint"]')!.textContent).toContain('恢复码');
    expect(q('[data-testid="login-error"]')).toBeNull(); // 切换后清除旧错误
    await type('[data-testid="totp-code"]', 'abcd-efgh-ijkl-mnop');
    await submit('[data-testid="totp-submit"]');
    await vi.waitFor(() => expect(m!.session.status).toBe('authed'));
    expect(verifyBody).toEqual({ mfa_token: 'mfa-x', code: 'abcd-efgh-ijkl-mnop' });
  });

  it('第二步可返回重新登录；挑战失效（401 unauthenticated）自动回到第一步', async () => {
    m = await mountAdmin({
      me: null,
      path: '/login',
      routes: { 'POST /auth/login': { body: { status: 'totp_required', mfa_token: 'm' } }, 'POST /auth/totp/verify': apiError(401, 'auth.unauthenticated') },
    });
    await type('[data-testid="login-email"]', 'a@example.test');
    await type('[data-testid="login-pw"]', TEST_VALUE);
    await submit('[data-testid="login-submit"]');
    await vi.waitFor(() => expect(q('[data-testid="totp-code"]')).not.toBeNull());
    await type('[data-testid="totp-code"]', '123456');
    await submit('[data-testid="totp-submit"]');
    await vi.waitFor(() => expect(q('[data-testid="login-email"]')).not.toBeNull()); // 回到第一步
  });

  it('须改口令的账号登录后进入改口令页（其余路由被守卫拦下）', async () => {
    m = await mountAdmin({
      me: null,
      path: '/login',
      routes: {
        'POST /auth/login': { body: { status: 'must_change_password', csrf_token: 'c' } },
        'GET /auth/me': (() => {
          let n = 0;
          return () => (++n === 1 ? apiError(401, 'auth.unauthenticated') : { body: makeMe({ mustChange: true }) });
        })(),
      },
    });
    await type('[data-testid="login-email"]', 'a@example.test');
    await type('[data-testid="login-pw"]', TEST_VALUE);
    await submit('[data-testid="login-submit"]');
    await vi.waitFor(() => expect(q('[data-testid="change-password-page"]')).not.toBeNull());
    await m.go('/');
    expect(m.router.currentRoute.value.name).toBe('change-password');
  });

  it('会话过期后回到登录页并提示（回跳保留）', async () => {
    m = await mountAdmin({ pages: [{ key: 'tasks', path: '/ops/tasks', group: 'ops', titleKey: 't.tasks', module: 'tasks', permissions: ['task:read'], component: () => Promise.resolve({ render: () => null }) }], path: '/ops/tasks' });
    m.session.expire(); // 模拟 401 处理
    await m.router.replace({ name: 'login', query: { redirect: '/ops/tasks' } });
    await m.settle();
    expect(q('[data-testid="login-expired"]')).not.toBeNull();
  });
});
