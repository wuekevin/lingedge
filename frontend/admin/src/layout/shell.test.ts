import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Component } from 'vue';
import { h } from 'vue';
import { makeMe } from '../testing/fakeApi';
import { mountAdmin } from '../testing/mountAdmin';
import type { PageDef } from '../registry/types';

const view = (name: string): (() => Promise<Component>) => () => Promise.resolve({ render: () => h('div', { 'data-testid': 'page', 'data-page': name }, name) });
const pg = (key: string, group: PageDef['group'], module: string, perms: string[], more: Partial<PageDef> = {}): PageDef => ({
  key,
  path: `/${group}/${key}`,
  group,
  titleKey: `t.${key}`,
  module,
  permissions: perms,
  component: view(key),
  ...more,
});
const PAGES: PageDef[] = [
  pg('tasks', 'ops', 'tasks', ['task:read']),
  pg('rollback', 'ops', 'tasks', ['config:rollback']),
  pg('nodes', 'nodes', 'nodes', ['node:read']),
  pg('maint', 'nodes', 'system', ['node:manage']),
  pg('editor', 'settings', 'system', ['setting:read'], { complexEdit: true }),
];
const MSG = { tasks: 'Task status', rollback: 'Rollback', nodes: 'Node list', maint: 'Node maintenance', editor: 'Rule editor' };

let m: Awaited<ReturnType<typeof mountAdmin>> | null = null;
afterEach(() => {
  m?.unmount();
  m = null;
});
const q = (sel: string) => document.querySelector(sel);
const qa = (sel: string) => [...document.querySelectorAll(sel)];

describe('外壳：动态菜单', () => {
  it('只渲染已交付 ∧ 有权限 ∧ 模式允许的页面；整组为空则一级菜单不渲染；没有灰显/占位', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, me: makeMe({ permissions: ['task:read', 'node:manage', 'node:read'] }) });
    const items = qa('[data-menu-item]').map((e) => e.getAttribute('data-menu-item'));
    expect(items).toEqual(['maint', 'tasks']); // rollback 无权限、nodes 模块未交付、editor 无 setting:read
    expect(qa('[data-menu-group]').map((e) => e.getAttribute('data-menu-group'))).toEqual(['nodes', 'ops']); // settings 整组不渲染
    const nav = q('[data-testid="side-menu"]')!.textContent ?? '';
    expect(nav).not.toMatch(/敬请期待|即将|soon|coming/i);
    expect(qa('[disabled],[aria-disabled="true"]').filter((e) => q('[data-testid="side-menu"]')!.contains(e))).toEqual([]);
  });

  it('没有任何已交付页面时：外壳首页如实显示空态，侧栏只有首页入口', async () => {
    m = await mountAdmin({ pages: [], messages: MSG });
    expect(qa('[data-menu-group]')).toEqual([]);
    expect(q('[data-testid="home-pages"]')!.textContent).toBe('0');
    expect(q('[data-le-state="empty"]')).not.toBeNull();
  });

  it('权限刷新后菜单随之变化（以 /auth/me 为准，不依赖缓存）', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, me: makeMe({ permissions: ['task:read'] }) });
    expect(qa('[data-menu-item]')).toHaveLength(1);
    m.session.applyMe(makeMe({ permissions: ['task:read', 'config:rollback'] }));
    await m.settle();
    expect(qa('[data-menu-item]').map((e) => e.getAttribute('data-menu-item'))).toEqual(['tasks', 'rollback']);
  });
});

describe('外壳：直访与 403 / 404', () => {
  it('权限不足直访：显示 403 页与所需权限，不渲染该页面', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, me: makeMe({ permissions: ['task:read'] }), path: '/ops/rollback' });
    expect(q('[data-testid="forbidden-page"]')).not.toBeNull();
    expect(q('[data-testid="forbidden-need"]')!.textContent).toContain('config:rollback');
    expect(q('[data-page="rollback"]')).toBeNull();
  });
  it('模块未交付 / 未知路径：显示 404（不是 403）', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, me: makeMe({ permissions: ['node:read'] }), path: '/nodes/nodes' });
    expect(q('[data-testid="not-found-page"]')).not.toBeNull();
    await m.go('/whatever/else');
    expect(q('[data-testid="not-found-page"]')).not.toBeNull();
    expect(q('[data-testid="forbidden-page"]')).toBeNull();
  });
});

describe('外壳：代管横幅（UX-24）', () => {
  it('固定显示「代管默认租户」横幅，不可关闭；有 tenant:read 时显示真实的默认租户名', async () => {
    m = await mountAdmin({ pages: [], me: makeMe() });
    const b = q('[data-testid="acting-banner"]')!;
    expect(b.textContent).toContain('Default Tenant');
    expect(b.querySelector('button')).toBeNull(); // 没有关闭按钮
    m.unmount();
    m = await mountAdmin({ pages: [], me: makeMe({ permissions: ['task:read'] }) });
    expect(q('[data-testid="acting-banner"]')!.textContent).toContain('默认租户'); // 无法解析时退回通用文案
  });
  it('每个页面都带横幅（外壳固定顶栏下）', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, path: '/ops/tasks' });
    expect(q('[data-testid="acting-banner"]')).not.toBeNull();
    await m.go('/403');
    expect(q('[data-testid="acting-banner"]')).not.toBeNull();
  });
});

describe('外壳：多标签页', () => {
  it('打开页面即开标签、同页不重复；关闭标签回到上一个；首页标签固定', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, path: '/ops/tasks' });
    await m.go('/ops/rollback');
    await m.go('/ops/tasks');
    const labels = () => qa('[data-testid="tab"]').map((e) => e.textContent?.replace(/×/g, '').trim());
    expect(labels()).toEqual(['首页', 'Task status', 'Rollback']);
    (qa('[data-testid="tab-close"]').at(-1) as HTMLElement).click();
    await m.settle();
    expect(labels()).toEqual(['首页', 'Task status']);
    expect(qa('[data-testid="tab"]')[0]!.querySelector('[data-testid="tab-close"]')).toBeNull();
  });

  it('关闭有未保存更改的标签先确认', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, path: '/ops/tasks' });
    m.tabs.setDirty('/ops/tasks', true);
    await m.settle();
    (qa('[data-testid="tab-close"]')[0] as HTMLElement).click();
    await m.settle();
    expect(document.querySelector('[data-testid="tab-dirty-confirm"]')?.textContent).toContain('Task status');
    expect(m.tabs.tabs.some((t) => t.fullPath === '/ops/tasks')).toBe(true);
  });

  it('退出登录关闭全部标签并回到登录页', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, path: '/ops/tasks' });
    expect(m.tabs.tabs.length).toBeGreaterThan(0);
    (q('[data-testid="account-menu"]') as HTMLElement).click();
    await m.settle();
    (document.querySelector('[data-testid="logout"]') as HTMLElement).click();
    await vi.waitFor(() => expect(m!.router.currentRoute.value.name).toBe('login'));
    expect(m.tabs.tabs).toEqual([]);
    expect(m.session.status).toBe('anon');
  });
});

describe('外壳：全局搜索壳', () => {
  it('Ctrl+K 打开；只列出已交付且可见的页面；Enter 打开选中页；Esc 关闭', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, me: makeMe({ permissions: ['task:read', 'node:manage'] }) });
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'k', ctrlKey: true }));
    await m.settle();
    const rows = qa('[data-testid="search-result"]').map((e) => e.textContent);
    expect(rows).toHaveLength(2);
    expect(rows.join()).toContain('Task status');
    expect(rows.join()).not.toContain('Rollback'); // 无权限的页面不被索引
    expect(rows.join()).not.toContain('Node list'); // 模块未交付
    const input = q('[data-testid="search-input"]') as HTMLInputElement;
    input.value = 'maint';
    input.dispatchEvent(new Event('input'));
    await m.settle();
    expect(qa('[data-testid="search-result"]')).toHaveLength(1);
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await m.settle();
    expect(m.router.currentRoute.value.path).toBe('/nodes/maint');
  });
  it('没有可搜索页面时显示空态说明', async () => {
    m = await mountAdmin({ pages: [] });
    (q('[data-testid="search-open"]') as HTMLElement).click();
    await m.settle();
    expect(qa('[data-testid="search-result"]')).toHaveLength(0);
    expect(document.querySelector('.arco-modal [data-le-state="empty"]')).not.toBeNull();
  });
});

describe('外壳：响应式（UX-23）', () => {
  it('桌面：侧栏 + 标签栏；手机宽度：侧栏变抽屉、隐藏标签栏、改面包屑 + 返回', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, path: '/ops/tasks', width: 1280 });
    expect(q('[data-testid="sidebar"]')).not.toBeNull();
    expect(q('[data-testid="tab-bar"]')).not.toBeNull();
    m.unmount();
    m = await mountAdmin({ pages: PAGES, messages: MSG, path: '/ops/tasks', width: 390 });
    expect(q('[data-testid="sidebar"]')).toBeNull();
    expect(q('[data-testid="tab-bar"]')).toBeNull();
    expect(q('[data-testid="mobile-crumbs"]')!.textContent).toContain('Task status');
    (q('[data-testid="nav-toggle"]') as HTMLElement).click();
    await m.settle();
    expect(document.querySelector('.arco-drawer [data-menu-item="tasks"]')).not.toBeNull(); // 抽屉里可达导航
  });
  it('复杂编辑页在手机宽度下如实标注「可用但不保证体验」，桌面不显示', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, path: '/settings/editor', width: 390 });
    expect(q('[data-testid="mobile-complex-notice"]')).not.toBeNull();
    expect(q('[data-page="editor"]')).not.toBeNull(); // 不阻断
    m.unmount();
    m = await mountAdmin({ pages: PAGES, messages: MSG, path: '/settings/editor', width: 1280 });
    expect(q('[data-testid="mobile-complex-notice"]')).toBeNull();
  });
});

describe('外壳：中英文与主题', () => {
  it('英文界面：横幅/菜单分组/首页全部走词条', async () => {
    m = await mountAdmin({ pages: PAGES, messages: MSG, locale: 'en', me: makeMe() });
    expect(q('[data-testid="acting-banner"]')!.textContent).toContain('Acting as administrator');
    expect(q('[data-menu-group="ops"]')!.textContent).toContain('Operations');
  });
  it('切换主题写 <html data-theme> 并记忆；切换语言写 <html lang>', async () => {
    m = await mountAdmin({ pages: [] });
    const theme = q('[data-testid="theme-select"]') as HTMLSelectElement;
    theme.value = 'dark';
    theme.dispatchEvent(new Event('change'));
    await m.settle();
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    expect(localStorage.getItem('le.admin.theme')).toBe('dark');
    const lang = q('[data-testid="lang-select"]') as HTMLSelectElement;
    lang.value = 'en';
    lang.dispatchEvent(new Event('change'));
    await m.settle();
    expect(document.documentElement.getAttribute('lang')).toBe('en');
    expect(q('[data-testid="acting-banner"]')!.textContent).toContain('Acting as');
    localStorage.clear();
  });
});
