import { describe, expect, it } from 'vitest';
import { createWizard } from './wizard';
import { apiError, fakeClient, status } from '../testing/fakeClient';

const mem = () => {
  const m = new Map<string, string>();
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v), removeItem: (k: string) => void m.delete(k), m };
};

describe('向导状态机', () => {
  it('校验 token 成功后进入向导，并回到第一个未完成的步骤（中断续做）', async () => {
    const c = fakeClient({ status: async () => status(['environment', 'database', 'redis']) });
    const w = createWizard(c as never, { storage: mem() });
    expect(w.state.phase).toBe('gate');
    await w.verifyToken('tok');
    expect(w.state.phase).toBe('wizard');
    expect(w.state.step).toBe('init_db');
    expect(c.token).toBe('tok');
  });

  it('错 token：抛出错误、清除内存中的 token、仍停在 gate', async () => {
    const c = fakeClient({
      status: async () => {
        throw apiError(401, 'setup.token_invalid');
      },
    });
    const w = createWizard(c as never, { storage: mem() });
    await expect(w.verifyToken('bad')).rejects.toMatchObject({ code: 'setup.token_invalid', requestId: 'req-test-1' });
    expect(w.state.phase).toBe('gate');
    expect(c.token).toBe('');
  });

  it('410（已安装）：转入 installed，并清除 token', async () => {
    const c = fakeClient({
      status: async () => {
        throw apiError(410, 'setup.already_installed');
      },
    });
    const w = createWizard(c as never, { storage: mem() });
    await expect(w.verifyToken('t')).rejects.toBeTruthy();
    expect(w.state.phase).toBe('installed');
    expect(w.state.installedError?.code).toBe('setup.already_installed');
    expect(c.token).toBe('');
  });

  it('步骤请求中途遇到 410：guard 转入 installed 并返回 true；其他错误返回 false', async () => {
    const c = fakeClient();
    const w = createWizard(c as never, { storage: mem() });
    await w.verifyToken('t');
    expect(w.guard(apiError(500, 'internal.error'))).toBe(false);
    expect(w.state.phase).toBe('wizard');
    expect(w.guard(apiError(410, 'setup.already_installed'))).toBe(true);
    expect(w.state.phase).toBe('installed');
  });

  it('已安装状态（status.installed=true）直接进入 installed', async () => {
    const w = createWizard(fakeClient({ status: async () => status([], true) }) as never, { storage: mem() });
    await w.verifyToken('t');
    expect(w.state.phase).toBe('installed');
  });

  it('不允许跳过未完成的步骤；已完成的步骤可回看；next/back 边界', async () => {
    const w = createWizard(fakeClient({ status: async () => status(['environment']) }) as never, { storage: mem() });
    await w.verifyToken('t');
    expect(w.state.step).toBe('database');
    w.go('admin');
    expect(w.state.step).toBe('database');
    w.go('environment');
    expect(w.state.step).toBe('environment');
    w.back();
    expect(w.state.step).toBe('environment');
    w.next();
    expect(w.state.step).toBe('database');
  });

  it('完成：token 立即清除；访问地址（非机密）存 sessionStorage，token 与口令从不进入任何存储', async () => {
    const st = mem();
    const c = fakeClient();
    const w = createWizard(c as never, { storage: st });
    await w.verifyToken('super-secret-token');
    w.setBaseUrl('https://admin.example.test');
    w.finish();
    expect(c.token).toBe('');
    expect(w.state.finished).toBe(true);
    expect([...st.m.values()]).toEqual(['https://admin.example.test']);
    expect(JSON.stringify([...st.m.entries()])).not.toContain('super-secret-token');
    // 新页面（刷新）：访问地址可恢复，token 不可恢复
    const w2 = createWizard(fakeClient() as never, { storage: st });
    expect(w2.state.baseUrl).toBe('https://admin.example.test');
    expect(w2.state.phase).toBe('gate');
  });
});
