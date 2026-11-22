import { describe, expect, it, vi } from 'vitest';
import { createSetupClient } from './client';
import { CODE_BAD_RESPONSE, CODE_NETWORK, SetupApiError } from './errors';

const pw = `${Math.random().toString(36).slice(2)}x`;

function res(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
}

describe('setup client', () => {
  it('token 只经 X-Setup-Token 头发送：不进 URL、不进请求体、不带 cookie、不缓存', async () => {
    const f = vi.fn(async () => res(200, { installed: false, steps: [], server_version: 'v' }));
    const c = createSetupClient(f as unknown as typeof fetch);
    c.setToken('tok-secret-123');
    await c.status();
    const [url, init] = f.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('/api/v1/public/setup/status');
    expect(url).not.toContain('tok-secret-123');
    expect((init.headers as Record<string, string>)['X-Setup-Token']).toBe('tok-secret-123');
    expect(init.body).toBeUndefined();
    expect(init.credentials).toBe('omit');
    expect(init.cache).toBe('no-store');
    expect(init.referrerPolicy).toBe('no-referrer');
  });

  it('各 operation 的方法与路径与 OpenAPI 一致，请求体为 JSON', async () => {
    const f = vi.fn(async () => res(200, { step: 'x', ok: true }));
    const c = createSetupClient(f as unknown as typeof fetch);
    c.setToken('t');
    await c.checks();
    await c.database({ database: 'd', username: 'u', password: pw, socket: '/s' });
    await c.redis({ address: 'a:1' });
    await c.initialize();
    await c.admin({ email: 'e', display_name: 'n', password: pw });
    await c.system({ run_mode: 'self_hosted' });
    await c.keys();
    await c.complete();
    const seen = f.mock.calls.map((a) => `${(a as unknown as [string, RequestInit])[1].method} ${(a as unknown as [string])[0]}`);
    expect(seen).toEqual([
      'POST /api/v1/public/setup/checks',
      'PUT /api/v1/public/setup/database',
      'PUT /api/v1/public/setup/redis',
      'POST /api/v1/public/setup/initialize',
      'POST /api/v1/public/setup/admin',
      'PUT /api/v1/public/setup/system',
      'POST /api/v1/public/setup/keys',
      'POST /api/v1/public/setup/complete',
    ]);
    const dbInit = (f.mock.calls[1] as unknown as [string, RequestInit])[1];
    expect(JSON.parse(String(dbInit.body))).toMatchObject({ socket: '/s' });
    expect((dbInit.headers as Record<string, string>)['Content-Type']).toBe('application/json');
  });

  it('错误：保留稳定错误码、Request ID、逐字段详情与 Retry-After', async () => {
    const f = vi.fn(async () =>
      res(429, { error: { code: 'rate_limit.exceeded', message: 'too many', request_id: 'rid-1', details: [{ field: 'x', reason: 'r' }, { bad: 1 }] } }, { 'Retry-After': '42' }),
    );
    const c = createSetupClient(f as unknown as typeof fetch);
    const e = await c.status().catch((x: unknown) => x);
    expect(e).toBeInstanceOf(SetupApiError);
    const err = e as SetupApiError;
    expect(err.status).toBe(429);
    expect(err.code).toBe('rate_limit.exceeded');
    expect(err.requestId).toBe('rid-1');
    expect(err.details).toEqual([{ field: 'x', reason: 'r' }]);
    expect(err.retryAfterSeconds).toBe(42);
    expect(err.fieldReason('x')).toBe('r');
    expect(err.kind).toBe('rateLimited');
  });

  it('Request ID 缺省取响应头；非 JSON 错误体按状态码生成码；401 归为无权限类', async () => {
    const f = vi.fn(async () => new Response('<html>bad gateway</html>', { status: 502, headers: { 'X-Request-ID': 'hdr-rid' } }));
    const err = (await createSetupClient(f as unknown as typeof fetch).status().catch((x: unknown) => x)) as SetupApiError;
    expect(err.code).toBe('http.502');
    expect(err.requestId).toBe('hdr-rid');
    expect(err.kind).toBe('server');
    const e401 = (await createSetupClient((async () => res(401, { error: { code: 'setup.token_invalid', message: 'm', request_id: 'r' } })) as unknown as typeof fetch).status().catch((x: unknown) => x)) as SetupApiError;
    expect(e401.kind).toBe('forbidden');
  });

  it('网络中断 → status 0 / client.network_error；成功但响应体不是对象 → client.bad_response', async () => {
    const down = (await createSetupClient((async () => {
      throw new TypeError('fetch failed');
    }) as unknown as typeof fetch).status().catch((x: unknown) => x)) as SetupApiError;
    expect(down.status).toBe(0);
    expect(down.code).toBe(CODE_NETWORK);
    expect(down.kind).toBe('network');
    const bad = (await createSetupClient((async () => new Response('null', { status: 200 })) as unknown as typeof fetch).status().catch((x: unknown) => x)) as SetupApiError;
    expect(bad.code).toBe(CODE_BAD_RESPONSE);
  });

  it('setToken 空串清除；hasToken 反映状态', () => {
    const c = createSetupClient((async () => res(200, {})) as unknown as typeof fetch);
    expect(c.hasToken()).toBe(false);
    c.setToken('a');
    expect(c.hasToken()).toBe(true);
    c.setToken('');
    expect(c.hasToken()).toBe(false);
  });
});
