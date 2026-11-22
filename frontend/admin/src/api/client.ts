import { ApiError, CODE_BAD_RESPONSE, CODE_NETWORK, CODE_REAUTH_CANCELED, CODE_REAUTH_REQUIRED, type ApiErrorDetail } from './errors';

// Admin 的 API 客户端（ADR-014 §2：会话/API 客户端只放 admin，不进 shared）。
// - 同源相对路径 /api/v1（Nginx 反代；开发时由 Vite 代理）；Cookie 会话 `le_admin_sid` 是 HttpOnly，由浏览器自动携带（credentials: 'include'）。
// - 写请求（非 GET/HEAD/OPTIONS）自动携带 X-CSRF-Token；遇到 403 auth.csrf_invalid 时刷新令牌并重试一次。
// - 代租户（actingAs）只在调用方明确要求时才附带 X-Tenant-ID（M1 为默认租户，D-13）：平台级 operation 不应带它。
// - 401（会话失效）统一通知会话层；登录相关请求（auth:false）不触发。
// - 403 auth.reauth_required（L4 再认证窗口外，IMP-12、EXPERIENCE.md L4）：交给 deps.reauth 弹出验证框（POST /auth/reauth 由服务端校验口令/TOTP，前端不伪造），
//   成功后自动重试原请求一次；用户取消 → CODE_REAUTH_CANCELED。

export const API_BASE = '/api/v1';

export interface RequestOptions {
  query?: Record<string, string | number | boolean | undefined>;
  body?: unknown;
  /** 以默认租户代管：附带 X-Tenant-ID（须有 tenant:impersonate，服务端判定）。 */
  actingAs?: boolean;
  signal?: AbortSignal;
  /** false：登录/第二步等未认证请求，401 不当作会话过期处理，也不触发再认证流程。缺省 true。 */
  auth?: boolean;
  /** 创建类请求的幂等键（Idempotency-Key，≤128 字符）。 */
  idempotencyKey?: string;
}

export interface ApiClient {
  get<T>(path: string, opts?: RequestOptions): Promise<T>;
  post<T>(path: string, opts?: RequestOptions): Promise<T>;
  put<T>(path: string, opts?: RequestOptions): Promise<T>;
  patch<T>(path: string, opts?: RequestOptions): Promise<T>;
  delete<T>(path: string, opts?: RequestOptions): Promise<T>;
}

export interface ClientDeps {
  fetchImpl?: typeof fetch;
  /** 当前会话的 CSRF 令牌（内存）。 */
  csrf: () => string | null;
  /** 刷新并返回新的 CSRF 令牌（GET /auth/csrf）。 */
  setCsrf: (token: string) => void;
  actingTenantUid: () => string | null;
  locale: () => string;
  onUnauthorized: () => void;
  /** 需要再认证时调用：弹出验证框；返回 true = 已再认证（客户端重试原请求），false = 用户取消。 */
  reauth?: () => Promise<boolean>;
}

interface ErrorBody {
  error?: { code?: unknown; message?: unknown; details?: unknown; request_id?: unknown };
}

const SAFE = new Set(['GET', 'HEAD', 'OPTIONS']);

function parseDetails(v: unknown): ApiErrorDetail[] {
  if (!Array.isArray(v)) return [];
  const out: ApiErrorDetail[] = [];
  for (const d of v) {
    if (d && typeof d === 'object' && typeof (d as { reason?: unknown }).reason === 'string') {
      const o = d as { field?: unknown; reason: string; code?: unknown };
      out.push({ reason: o.reason, ...(typeof o.field === 'string' ? { field: o.field } : {}), ...(typeof o.code === 'string' ? { code: o.code } : {}) });
    }
  }
  return out;
}

function buildUrl(path: string, query?: RequestOptions['query']): string {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) if (v !== undefined) qs.set(k, String(v));
  const s = qs.toString();
  return `${API_BASE}${path}${s ? `?${s}` : ''}`;
}

/** fetch 的可替换实现只用于单元测试（vitest）；生产构建始终使用浏览器的 fetch。 */
export function createApiClient(deps: ClientDeps): ApiClient {
  const doFetch: typeof fetch = deps.fetchImpl ?? ((...a) => fetch(...a));

  async function once<T>(method: string, path: string, opts: RequestOptions): Promise<{ value: T } | { error: ApiError }> {
    const headers: Record<string, string> = { Accept: 'application/json', 'Accept-Language': deps.locale() };
    const init: RequestInit = { method, headers, credentials: 'include', cache: 'no-store', ...(opts.signal ? { signal: opts.signal } : {}) };
    if (!SAFE.has(method)) {
      const csrf = deps.csrf();
      if (csrf) headers['X-CSRF-Token'] = csrf;
    }
    if (opts.idempotencyKey) headers['Idempotency-Key'] = opts.idempotencyKey;
    if (opts.actingAs) {
      const uid = deps.actingTenantUid();
      if (uid) headers['X-Tenant-ID'] = uid;
    }
    if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.body);
    }
    let res: Response;
    try {
      res = await doFetch(buildUrl(path, opts.query), init);
    } catch (e) {
      if (e instanceof DOMException && e.name === 'AbortError') throw e;
      return { error: new ApiError({ status: 0, code: CODE_NETWORK, message: 'network error' }) };
    }
    const requestId = res.headers.get('X-Request-ID') ?? '';
    if (res.status === 204) return { value: undefined as T };
    let parsed: unknown;
    try {
      parsed = await res.json();
    } catch {
      parsed = undefined;
    }
    if (res.ok) {
      if (parsed === undefined) return { error: new ApiError({ status: res.status, code: CODE_BAD_RESPONSE, message: 'response is not JSON', requestId }) };
      return { value: parsed as T };
    }
    const e = (parsed as ErrorBody | undefined)?.error;
    const retryAfter = Number(res.headers.get('Retry-After'));
    return {
      error: new ApiError({
        status: res.status,
        code: typeof e?.code === 'string' ? e.code : CODE_BAD_RESPONSE,
        message: typeof e?.message === 'string' ? e.message : `HTTP ${res.status}`,
        requestId: (typeof e?.request_id === 'string' && e.request_id) || requestId,
        details: parseDetails(e?.details),
        retryAfterSeconds: Number.isFinite(retryAfter) && retryAfter > 0 ? retryAfter : null,
      }),
    };
  }

  async function call<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
    let r = await once<T>(method, path, opts);
    // CSRF 令牌可能因会话轮换而过期：刷新一次再试（仅写请求，且只重试一次）。
    if ('error' in r && r.error.status === 403 && r.error.code === 'auth.csrf_invalid' && !SAFE.has(method) && opts.auth !== false) {
      const t = await once<{ csrf_token?: string }>('GET', '/auth/csrf', {});
      if ('value' in t && typeof t.value.csrf_token === 'string') {
        deps.setCsrf(t.value.csrf_token);
        r = await once<T>(method, path, opts);
      }
    }
    // L4 再认证：窗口外的敏感操作返回 403 auth.reauth_required；验证通过后原请求只重试一次（再失败就如实报错，不循环）。
    if ('error' in r && r.error.status === 403 && r.error.code === CODE_REAUTH_REQUIRED && opts.auth !== false && deps.reauth) {
      const original = r.error;
      if (await deps.reauth()) r = await once<T>(method, path, opts);
      else throw new ApiError({ status: 403, code: CODE_REAUTH_CANCELED, message: 'reauthentication cancelled', requestId: original.requestId });
    }
    if ('error' in r) {
      if (r.error.status === 401 && opts.auth !== false) deps.onUnauthorized();
      throw r.error;
    }
    return r.value;
  }

  return {
    get: (p, o) => call('GET', p, o),
    post: (p, o) => call('POST', p, o),
    put: (p, o) => call('PUT', p, o),
    patch: (p, o) => call('PATCH', p, o),
    delete: (p, o) => call('DELETE', p, o),
  };
}
