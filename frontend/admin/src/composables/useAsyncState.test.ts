import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/errors';
import { useAsyncState } from './useAsyncState';

const deferred = <T>() => {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => ((resolve = res), (reject = rej)));
  return { promise, resolve, reject };
};

describe('统一三态', () => {
  it('loading → ready / empty / error，并给出失败分类与 Request ID', async () => {
    const s = useAsyncState(() => Promise.resolve([1]), (d) => d.length === 0);
    expect(s.status.value).toBe('loading');
    await s.run();
    expect(s.status.value).toBe('ready');
    const e = useAsyncState(() => Promise.resolve([] as number[]), (d) => d.length === 0);
    await e.run();
    expect(e.status.value).toBe('empty');
    const f = useAsyncState(() => Promise.reject(new ApiError({ status: 403, code: 'auth.forbidden', requestId: 'R9' })));
    await f.run();
    expect(f.status.value).toBe('error');
    expect(f.errorKind.value).toBe('forbidden');
    expect(f.requestId.value).toBe('R9');
  });

  it('重试：失败后再次 run 回到 loading 并以最新结果为准；旧响应不覆盖新状态', async () => {
    const d1 = deferred<string>();
    const d2 = deferred<string>();
    const queue = [d1, d2];
    const s = useAsyncState(() => queue.shift()!.promise);
    const p1 = s.run();
    const p2 = s.run();
    d2.resolve('new');
    await p2;
    d1.resolve('old');
    await p1;
    expect(s.data.value).toBe('new');
    expect(s.status.value).toBe('ready');
  });

  it('取消：不再更新状态', async () => {
    const d = deferred<string>();
    const s = useAsyncState(() => d.promise);
    const p = s.run();
    s.cancel();
    d.resolve('late');
    await p;
    expect(s.data.value).toBeNull();
  });

  it('网络中断归类为 network', async () => {
    const s = useAsyncState(() => Promise.reject(new ApiError({ status: 0, code: 'client.network' })));
    await s.run();
    expect(s.errorKind.value).toBe('network');
  });
});
