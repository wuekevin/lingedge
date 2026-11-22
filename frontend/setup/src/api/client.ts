import { CODE_BAD_RESPONSE, CODE_NETWORK, SetupApiError, type ApiErrorDetail } from './errors';
import type { SetupAdminCreate, SetupCheckResult, SetupDbConfig, SetupRedisConfig, SetupStatus, SetupStepResult, SetupSystemConfig } from './types';

// 端点写完整路径（不拼接相对段）：构建隔离检查按路径模板精确匹配，裸的“/admin”段会被判为 Admin 路由前缀（向导的 /public/setup/admin 是公开的向导操作，不是 Admin 路由）。

export interface SetupClient {
  /** 设置 setup token（只保存在本闭包的内存里：不写 URL、不写任何 Web Storage、不写 cookie）。传空串即清除。 */
  setToken(token: string): void;
  hasToken(): boolean;
  status(signal?: AbortSignal): Promise<SetupStatus>;
  checks(signal?: AbortSignal): Promise<SetupCheckResult>;
  database(body: SetupDbConfig, signal?: AbortSignal): Promise<SetupStepResult>;
  redis(body: SetupRedisConfig, signal?: AbortSignal): Promise<SetupStepResult>;
  initialize(signal?: AbortSignal): Promise<SetupStepResult>;
  admin(body: SetupAdminCreate, signal?: AbortSignal): Promise<SetupStepResult>;
  system(body: SetupSystemConfig, signal?: AbortSignal): Promise<SetupStepResult>;
  keys(signal?: AbortSignal): Promise<SetupStepResult>;
  complete(signal?: AbortSignal): Promise<SetupStepResult>;
}

interface ErrorBody {
  error?: { code?: unknown; message?: unknown; details?: unknown; request_id?: unknown };
}

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

/** fetch 的可替换实现只用于单元测试（vitest）；生产构建始终使用浏览器的 fetch。 */
export function createSetupClient(fetchImpl: typeof fetch = (...a) => fetch(...a)): SetupClient {
  let token = '';

  async function call<T>(method: 'GET' | 'POST' | 'PUT', path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json', 'X-Setup-Token': token };
    const init: RequestInit = { method, headers, credentials: 'omit', cache: 'no-store', referrerPolicy: 'no-referrer', ...(signal ? { signal } : {}) };
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    let res: Response;
    try {
      res = await fetchImpl(path, init);
    } catch (e) {
      if (e instanceof DOMException && e.name === 'AbortError') throw e;
      throw new SetupApiError({ status: 0, code: CODE_NETWORK, message: 'network error' });
    }
    const headerRid = res.headers.get('X-Request-ID') ?? '';
    let parsed: unknown;
    try {
      parsed = await res.json();
    } catch {
      parsed = undefined;
    }
    if (!res.ok) {
      const e = (parsed as ErrorBody | undefined)?.error;
      const retry = Number.parseInt(res.headers.get('Retry-After') ?? '', 10);
      throw new SetupApiError({
        status: res.status,
        code: typeof e?.code === 'string' && e.code !== '' ? e.code : `http.${res.status}`,
        message: typeof e?.message === 'string' ? e.message : res.statusText,
        requestId: typeof e?.request_id === 'string' && e.request_id !== '' ? e.request_id : headerRid,
        details: parseDetails(e?.details),
        retryAfterSeconds: Number.isFinite(retry) ? retry : null,
      });
    }
    if (parsed === undefined || parsed === null || typeof parsed !== 'object') {
      throw new SetupApiError({ status: res.status, code: CODE_BAD_RESPONSE, message: 'unexpected response body', requestId: headerRid });
    }
    return parsed as T;
  }

  return {
    setToken: (t) => {
      token = t;
    },
    hasToken: () => token !== '',
    status: (s) => call('GET', '/api/v1/public/setup/status', undefined, s),
    checks: (s) => call('POST', '/api/v1/public/setup/checks', undefined, s),
    database: (b, s) => call('PUT', '/api/v1/public/setup/database', b, s),
    redis: (b, s) => call('PUT', '/api/v1/public/setup/redis', b, s),
    initialize: (s) => call('POST', '/api/v1/public/setup/initialize', undefined, s),
    admin: (b, s) => call('POST', '/api/v1/public/setup/admin', b, s),
    system: (b, s) => call('PUT', '/api/v1/public/setup/system', b, s),
    keys: (s) => call('POST', '/api/v1/public/setup/keys', undefined, s),
    complete: (s) => call('POST', '/api/v1/public/setup/complete', undefined, s),
  };
}
