import { describe, expect, it, vi } from 'vitest';
import { apiError, fakeFetch } from '../testing/fakeApi';
import { createApiClient } from './client';
import { ApiError, CODE_NETWORK } from './errors';
import { errorText } from './messages';

function setup(routes: Parameters<typeof fakeFetch>[0], over: Partial<Parameters<typeof createApiClient>[0]> = {}) {
  const ff = fakeFetch(routes);
  const state = { csrf: 'tok-1' as string | null };
  const onUnauthorized = vi.fn();
  const client = createApiClient({
    fetchImpl: ff.fetch,
    csrf: () => state.csrf,
    setCsrf: (t) => (state.csrf = t),
    actingTenantUid: () => '01TENANT',
    locale: () => 'en',
    onUnauthorized,
    ...over,
  });
  return { client, calls: ff.calls, state, onUnauthorized };
}

describe('API 客户端', () => {
  it('写请求自动携带 X-CSRF-Token；读请求不带；带 Accept-Language 与 Cookie 凭据模式', async () => {
    const { client, calls } = setup({ 'POST /things': { body: { ok: true } }, 'GET /things': { body: [] } });
    await client.post('/things', { body: { a: 1 } });
    await client.get('/things');
    expect(calls[0]!.headers['x-csrf-token']).toBe('tok-1');
    expect(calls[0]!.headers['accept-language']).toBe('en');
    expect(calls[0]!.body).toEqual({ a: 1 });
    expect(calls[1]!.headers['x-csrf-token']).toBeUndefined();
  });

  it('只有明确 actingAs 才带 X-Tenant-ID（平台级请求不带）', async () => {
    const { client, calls } = setup({ 'GET /a': { body: {} }, 'GET /b': { body: {} } });
    await client.get('/a');
    await client.get('/b', { actingAs: true });
    expect(calls[0]!.headers['x-tenant-id']).toBeUndefined();
    expect(calls[1]!.headers['x-tenant-id']).toBe('01TENANT');
  });

  it('403 auth.csrf_invalid：刷新令牌后重试一次；仍失败则抛出', async () => {
    let n = 0;
    const { client, calls, state } = setup({
      'POST /x': () => (++n === 1 ? apiError(403, 'auth.csrf_invalid') : { body: { done: true } }),
      'GET /auth/csrf': { body: { csrf_token: 'tok-2' } },
    });
    await expect(client.post('/x', {})).resolves.toEqual({ done: true });
    expect(state.csrf).toBe('tok-2');
    expect(calls.filter((c) => c.method === 'POST').map((c) => c.headers['x-csrf-token'])).toEqual(['tok-1', 'tok-2']);
    const bad = setup({ 'POST /x': apiError(403, 'auth.csrf_invalid'), 'GET /auth/csrf': { body: { csrf_token: 't' } } });
    await expect(bad.client.post('/x', {})).rejects.toMatchObject({ status: 403, code: 'auth.csrf_invalid' });
    expect(bad.calls.filter((c) => c.method === 'POST')).toHaveLength(2); // 只重试一次
  });

  it('401 通知会话层；登录相关请求（auth:false）的 401 不当作会话过期', async () => {
    const { client, onUnauthorized } = setup({ 'GET /auth/me': apiError(401, 'auth.unauthenticated'), 'POST /auth/login': apiError(401, 'auth.invalid_credentials') });
    await expect(client.get('/auth/me')).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
    await expect(client.post('/auth/login', { auth: false })).rejects.toMatchObject({ code: 'auth.invalid_credentials' });
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it('错误解析：状态码、错误码、Request ID、details、Retry-After；204 无响应体；网络中断', async () => {
    const { client } = setup({
      'GET /e': { status: 429, body: { error: { code: 'rate_limit.exceeded', message: 'm', request_id: 'R1', details: [{ field: 'f', reason: 'r' }] } }, headers: { 'Retry-After': '7' } },
      'DELETE /d': { status: 204 },
    });
    const e = await client.get('/e').catch((x: unknown) => x);
    expect(e).toMatchObject({ status: 429, code: 'rate_limit.exceeded', requestId: 'R1', retryAfterSeconds: 7, details: [{ field: 'f', reason: 'r' }] });
    await expect(client.delete('/d')).resolves.toBeUndefined();
    const net = createApiClient({ fetchImpl: () => Promise.reject(new TypeError('fail')), csrf: () => null, setCsrf: () => undefined, actingTenantUid: () => null, locale: () => 'en', onUnauthorized: () => undefined });
    await expect(net.get('/x')).rejects.toMatchObject({ status: 0, code: CODE_NETWORK });
  });

  it('文案按稳定错误码映射，未知错误码按状态类别给通用说明，不显示服务端原文', () => {
    const msgs: Record<string, string> = { 'errors.code.auth.invalid_credentials': 'bad creds', 'state.error.kind.server': 'server down', 'state.error.kind.unknown': 'unknown', 'errors.retryAfter': 'retry {seconds}' };
    const t = (k: string, n?: Record<string, unknown>) => (msgs[k] ?? k).replace('{seconds}', String(n?.seconds ?? ''));
    const te = (k: string) => k in msgs;
    expect(errorText(t, te, new ApiError({ status: 401, code: 'auth.invalid_credentials' }))).toBe('bad creds');
    expect(errorText(t, te, new ApiError({ status: 500, code: 'weird.code', message: 'SECRET INTERNALS' }))).toBe('server down');
    expect(errorText(t, te, new Error('x'))).toBe('unknown');
  });
});
