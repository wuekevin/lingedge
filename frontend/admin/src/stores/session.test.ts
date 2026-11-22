import { createPinia, setActivePinia } from 'pinia';
import { beforeEach, describe, expect, it } from 'vitest';
import { createAuthApi } from '../api/auth';
import { createApiClient } from '../api/client';
import { apiError, fakeFetch, makeMe } from '../testing/fakeApi';
import { bindAuthApi, useSessionStore } from './session';

function setup(routes: Parameters<typeof fakeFetch>[0]) {
  setActivePinia(createPinia());
  const ff = fakeFetch(routes);
  const s = useSessionStore();
  bindAuthApi(createAuthApi(createApiClient({ fetchImpl: ff.fetch, csrf: () => s.csrf, setCsrf: (t) => (s.csrf = t), actingTenantUid: () => null, locale: () => 'zh-CN', onUnauthorized: () => s.expire() })));
  return { s, calls: ff.calls };
}
beforeEach(() => setActivePinia(createPinia()));

describe('会话（登录 / TOTP / 恢复码 / 退出）', () => {
  it('登录请求固定 realm=admin；成功后以 /auth/me 为准建立身份与 CSRF', async () => {
    const { s, calls } = setup({ 'POST /auth/login': { body: { status: 'ok', csrf_token: 'c1' } }, 'GET /auth/me': { body: makeMe() } });
    expect(await s.login('a@example.test', 'runtime-generated-value')).toBe('ok');
    expect(calls[0]!.body).toMatchObject({ realm: 'admin', email: 'a@example.test' });
    expect(s.status).toBe('authed');
    expect(s.csrf).toBe('csrf-from-me');
    expect(s.access?.permissions.has('task:read')).toBe(true);
  });

  it('TOTP：第一步返回 mfa_token，第二步用验证码或恢复码完成；挑战只在内存', async () => {
    const { s, calls } = setup({
      'POST /auth/login': { body: { status: 'totp_required', mfa_token: 'mfa-1' } },
      'POST /auth/totp/verify': { body: { status: 'ok', csrf_token: 'c2' } },
      'GET /auth/me': { body: makeMe() },
    });
    expect(await s.login('a@example.test', 'x')).toBe('totp_required');
    expect(s.status).not.toBe('authed');
    expect(s.mfa?.token).toBe('mfa-1');
    expect(await s.verifyTotp(' abcd-efgh-ijkl-mnop ')).toBe('ok'); // 恢复码走同一端点，去首尾空白
    expect(calls[1]!.body).toEqual({ mfa_token: 'mfa-1', code: 'abcd-efgh-ijkl-mnop' });
    expect(s.mfa).toBeNull();
    expect(s.status).toBe('authed');
    expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain('mfa-1');
  });

  it('must_change_password 登录结果被如实返回；租户账号被拒（统一 401）且不建立会话', async () => {
    const a = setup({ 'POST /auth/login': { body: { status: 'must_change_password', csrf_token: 'c' } }, 'GET /auth/me': { body: makeMe({ mustChange: true }) } });
    expect(await a.s.login('a@example.test', 'x')).toBe('must_change');
    expect(a.s.mustChangePassword).toBe(true);
    const b = setup({ 'POST /auth/login': apiError(401, 'auth.invalid_credentials') });
    await expect(b.s.login('tenant@example.test', 'x')).rejects.toMatchObject({ status: 401, code: 'auth.invalid_credentials' });
    expect(b.s.status).toBe('unknown');
    expect(b.s.me).toBeNull();
  });

  it('realm 不是 admin 的身份不被接受（防御）', async () => {
    const { s } = setup({ 'GET /auth/me': { body: { ...makeMe(), realm: 'console' } } });
    await s.restore();
    expect(s.status).toBe('anon');
    expect(s.me).toBeNull();
  });

  it('restore：401 = 匿名；网络/5xx 向上抛出而不是误判为已退出', async () => {
    const a = setup({ 'GET /auth/me': apiError(401, 'auth.unauthenticated') });
    await a.s.restore();
    expect(a.s.status).toBe('anon');
    const b = setup({ 'GET /auth/me': apiError(503, 'internal.error') });
    await expect(b.s.restore()).rejects.toMatchObject({ status: 503 });
    expect(b.s.status).toBe('unknown');
  });

  it('退出：服务端不可达也清理本地身份；过期会话标记 expired', async () => {
    const { s } = setup({ 'GET /auth/me': { body: makeMe() }, 'POST /auth/logout': apiError(500, 'internal.error') });
    await s.restore();
    s.expire();
    expect(s.expired).toBe(true);
    expect(s.me).toBeNull();
    await s.restore();
    await s.logout();
    expect(s.status).toBe('anon');
    expect(s.expired).toBe(false);
  });

  it('默认租户（代管目标）：有 tenant:read 才解析；没有则退回通用文案（null）', async () => {
    const list = { items: [{ uid: 'T1', name: 'Other', slug: 'o', is_default: false }, { uid: 'T2', name: 'Default Tenant', slug: 'd', is_default: true }] };
    const a = setup({ 'GET /auth/me': { body: makeMe() }, 'GET /tenants': { body: list } });
    await a.s.restore();
    await a.s.resolveActingTenant();
    expect(a.s.actingTenant).toMatchObject({ uid: 'T2', name: 'Default Tenant' });
    const b = setup({ 'GET /auth/me': { body: makeMe({ permissions: ['task:read'] }) }, 'GET /tenants': { body: list } });
    await b.s.restore();
    await b.s.resolveActingTenant();
    expect(b.s.actingTenant).toBeNull();
    expect(b.calls.some((c) => c.url.startsWith('/api/v1/tenants'))).toBe(false);
  });
});
