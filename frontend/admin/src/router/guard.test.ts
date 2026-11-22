import { createPinia, setActivePinia } from 'pinia';
import { createMemoryHistory } from 'vue-router';
import { beforeEach, describe, expect, it } from 'vitest';
import type { Component } from 'vue';
import { createAdminRouter } from '.';
import { createRegistry } from '../registry/registry';
import type { PageDef } from '../registry/types';
import { useSessionStore } from '../stores/session';
import { makeMe } from '../testing/fakeApi';
import { safeRedirect } from './guard';

const comp = () => Promise.resolve({ render: () => null } as Component);
const pg = (key: string, p: string, module: string, perms: string[], more: Partial<PageDef> = {}): PageDef => ({ key, path: p, group: 'ops', titleKey: key, module, permissions: perms, component: comp, ...more });
const registry = createRegistry([pg('tasks', '/ops/tasks', 'tasks', ['task:read']), pg('rollback', '/ops/rollback', 'tasks', ['config:rollback']), pg('nodes', '/nodes/list', 'nodes', ['node:read']), pg('bill', '/billing/orders', 'auth', ['billing:read'], { runModes: ['commercial'] })]);

function setup(me: ReturnType<typeof makeMe> | null) {
  setActivePinia(createPinia());
  const s = useSessionStore();
  if (me) s.applyMe(me);
  else s.status = 'anon';
  return { s, router: createAdminRouter({ registry, history: createMemoryHistory() }) };
}
beforeEach(() => setActivePinia(createPinia()));

describe('路由守卫（FR-31、ia.md §4）', () => {
  it('未登录：任何受保护路由 → /login，并带回跳；登录页本身可达', async () => {
    const { router } = setup(null);
    await router.push('/ops/tasks?x=1');
    expect(router.currentRoute.value.name).toBe('login');
    expect(router.currentRoute.value.query.redirect).toBe('/ops/tasks?x=1');
    await router.push('/');
    expect(router.currentRoute.value.query.redirect).toBeUndefined();
  });

  it('已登录：有权限放行；权限不足 → 403（带所需权限点）；URL 直访同样 403，不渲染页面', async () => {
    const { router } = setup(makeMe({ permissions: ['task:read'] }));
    await router.push('/ops/tasks');
    expect(router.currentRoute.value.name).toBe('tasks');
    await router.push('/ops/rollback');
    expect(router.currentRoute.value.name).toBe('forbidden');
    expect(router.currentRoute.value.query).toMatchObject({ path: '/ops/rollback', need: 'config:rollback' });
  });

  it('模块未交付 / 运行模式不允许 / 未登记路径 → 404，而不是 403（不暗示存在）', async () => {
    const { router } = setup(makeMe({ permissions: ['node:read', 'billing:read', 'task:read'] }));
    for (const p of ['/nodes/list', '/billing/orders', '/does/not/exist']) {
      await router.push(p);
      expect(['not-found', 'catch-all'], p).toContain(router.currentRoute.value.name);
    }
  });

  it('须改口令的账号只能访问改口令页；无需改口令时进不了改口令页；已登录访问登录页回首页', async () => {
    const a = setup(makeMe({ mustChange: true }));
    await a.router.push('/ops/tasks');
    expect(a.router.currentRoute.value.name).toBe('change-password');
    const b = setup(makeMe());
    await b.router.push('/change-password');
    expect(b.router.currentRoute.value.name).toBe('home');
    await b.router.push('/login');
    expect(b.router.currentRoute.value.name).toBe('home');
  });

  it('权限在刷新身份后立即生效：同一路径从 403 变为放行（不依赖前端缓存的菜单）', async () => {
    const { s, router } = setup(makeMe({ permissions: ['task:read'] }));
    await router.push('/ops/rollback');
    expect(router.currentRoute.value.name).toBe('forbidden');
    s.applyMe(makeMe({ permissions: ['task:read', 'config:rollback'] }));
    await router.push('/ops/rollback');
    expect(router.currentRoute.value.name).toBe('rollback');
  });

  it('回跳目标只接受站内绝对路径（防开放重定向），且不会回到登录页', () => {
    expect(safeRedirect('/ops/tasks?a=1')).toBe('/ops/tasks?a=1');
    for (const bad of ['//evil.example', 'https://evil.example', '/\\evil', '/login', '/login?x', 'javascript:alert(1)', '', null]) expect(safeRedirect(bad), String(bad)).toBeNull();
  });
});
