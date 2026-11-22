// 仅供 vitest 使用的 SetupClient 替身：不被任何生产代码引用（生产构建经 check:mock 验证；真实流程由 Playwright + 真实 server 覆盖）。
import { vi } from 'vitest';
import type { SetupClient } from '../api/client';
import { SetupApiError } from '../api/errors';
import type { SetupStatus, SetupStepResult } from '../api/types';

/** 测试夹具里的凭据一律运行期随机生成（G-6：仓库内不出现固定口令字面量）。 */
export const rnd = (prefix = ''): string => `${prefix}${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;

export type FakeClient = { [K in keyof SetupClient]: ReturnType<typeof vi.fn> } & { token: string };

export function ok(step: string, extra: Partial<SetupStepResult> = {}): SetupStepResult {
  return { step, ok: true, message: `${step} ok`, next: null, ...extra };
}

export function status(done: string[] = [], installed = false): SetupStatus {
  const keys = ['environment', 'database', 'redis', 'init_db', 'admin', 'system', 'keys', 'complete'] as const;
  return { installed, server_version: 'test', steps: keys.map((key) => ({ key, status: done.includes(key) ? 'done' : 'pending' })) };
}

export function apiError(status: number, code: string, requestId = 'req-test-1', details: { field?: string; reason: string }[] = [], retryAfterSeconds: number | null = null): SetupApiError {
  return new SetupApiError({ status, code, message: code, requestId, details, retryAfterSeconds });
}

export function fakeClient(overrides: Partial<Record<keyof SetupClient, unknown>> = {}): FakeClient {
  const c = {
    token: '',
    setToken: vi.fn((t: string) => {
      c.token = t;
    }),
    hasToken: vi.fn(() => c.token !== ''),
    status: vi.fn(async () => status()),
    checks: vi.fn(async () => ({ items: [] })),
    database: vi.fn(async () => ok('database')),
    redis: vi.fn(async () => ok('redis')),
    initialize: vi.fn(async () => ok('init_db', { migrations: [] })),
    admin: vi.fn(async () => ok('admin')),
    system: vi.fn(async () => ok('system')),
    keys: vi.fn(async () => ok('keys')),
    complete: vi.fn(async () => ok('complete')),
  } as unknown as FakeClient;
  // 覆盖实现同样包成 spy，测试可断言调用次数与参数。
  for (const [k, v] of Object.entries(overrides)) (c as unknown as Record<string, unknown>)[k] = typeof v === 'function' ? vi.fn(v as (...a: never[]) => unknown) : v;
  return c;
}
