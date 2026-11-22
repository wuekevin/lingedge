import type { Me } from '../api/types';

// 单元/组件测试用的假后端（只在 vitest 里使用，生产代码不引用；E2E 一律走真实 server，不用它）。

export function makeMe(over: Partial<Me> & { permissions?: string[]; modules?: string[]; runMode?: 'self_hosted' | 'commercial'; mustChange?: boolean } = {}): Me {
  const modules = over.modules ?? ['auth', 'tenants', 'tasks', 'audit', 'system'];
  const runMode = over.runMode ?? 'self_hosted';
  return {
    user: { uid: '01TESTUSER', email: 'admin@example.test', display_name: 'Test Admin', user_type: 'platform', locale: 'zh-CN', totp_enabled: false, must_change_password: over.mustChange ?? false },
    realm: 'admin',
    tenant: null,
    acting_tenant: null,
    roles: ['platform_admin'],
    permissions: over.permissions ?? ['task:read', 'audit:read', 'user:read', 'role:read', 'setting:read', 'config:rollback', 'node:manage', 'tenant:read'],
    menu_keys: [],
    capabilities: { run_mode: runMode, modules, features: {} },
    features: { run_mode: runMode, modules_delivered: modules, capability_status: {} },
    csrf_token: 'csrf-from-me',
    session_expires_at: '2030-01-01T00:00:00.000Z',
    ...(over.roles ? { roles: over.roles } : {}),
  } as Me;
}

export interface Call {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: unknown;
}
export type Handler = (call: Call) => { status?: number; body?: unknown; headers?: Record<string, string> } | undefined;

/** 按「方法 路径」匹配的假 fetch；记录全部请求。未匹配的请求返回 404（测试里会暴露意外调用）。 */
export function fakeFetch(routes: Record<string, Handler | { status?: number; body?: unknown; headers?: Record<string, string> }>): { fetch: typeof fetch; calls: Call[] } {
  const calls: Call[] = [];
  const f = (async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = new URL(String(input), 'http://localhost');
    const method = (init?.method ?? 'GET').toUpperCase();
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries((init?.headers ?? {}) as Record<string, string>)) headers[k.toLowerCase()] = v;
    const call: Call = { method, url: url.pathname + url.search, headers, body: init?.body ? JSON.parse(String(init.body)) : undefined };
    calls.push(call);
    const key = `${method} ${url.pathname.replace('/api/v1', '')}`;
    const r = routes[key];
    const res = typeof r === 'function' ? r(call) : r;
    if (!res) return new Response(JSON.stringify({ error: { code: 'resource.not_found', message: 'not found', request_id: 'rid-404' } }), { status: 404, headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-404' } });
    const status = res.status ?? 200;
    return new Response(status === 204 ? null : JSON.stringify(res.body ?? {}), { status, headers: { 'Content-Type': 'application/json', 'X-Request-ID': 'rid-test', ...(res.headers ?? {}) } });
  }) as typeof fetch;
  return { fetch: f, calls };
}

export const apiError = (status: number, code: string, extra: Record<string, string> = {}) => ({ status, body: { error: { code, message: code, request_id: 'rid-err' } }, headers: extra });
