import { describe, expect, it } from 'vitest';
import type { Component } from 'vue';
import { makeMe } from '../testing/fakeApi';
import { GROUPS } from './groups';
import { filterSearch } from './search';
import { GROUP_KEYS, type PageDef } from './types';
import { accessFromMe, buildMenu, buildSearchIndex, evaluatePage } from './visibility';

const comp = () => Promise.resolve({} as Component);
const page = (key: string, group: PageDef['group'], module: string, permissions: string[], more: Partial<PageDef> = {}): PageDef => ({
  key,
  path: `/${group}/${key}`,
  group,
  titleKey: `t.${key}`,
  module,
  permissions,
  component: comp,
  ...more,
});
const PAGES: PageDef[] = [
  page('tasks', 'ops', 'tasks', ['task:read']),
  page('audit', 'ops', 'audit', ['audit:read']),
  page('rollback', 'ops', 'tasks', ['config:rollback']),
  page('nodes', 'nodes', 'nodes', ['node:read']), // 模块未交付
  page('maint', 'nodes', 'system', ['node:manage']),
  page('billing', 'billing', 'auth', ['billing:read'], { runModes: ['commercial'] }),
  page('alias', 'users', 'audit', ['audit:read'], { aliasOf: 'audit' }),
  page('detail', 'ops', 'tasks', ['task:read'], { menu: false }),
];
const meFull = makeMe({ permissions: ['task:read', 'audit:read', 'config:rollback', 'node:manage', 'billing:read', 'node:read'] });

describe('菜单可见性 = 模块已交付 ∧ 权限 ∧ 运行模式（UX-1）', () => {
  it('三项同时满足才可见；模块未交付与模式不符当作不存在（hidden），仅权限不足是 forbidden', () => {
    const a = accessFromMe(meFull);
    expect(evaluatePage(PAGES[0]!, a)).toEqual({ ok: true });
    expect(evaluatePage(PAGES[3]!, a)).toEqual({ ok: false, reason: 'hidden' }); // nodes 模块未交付，即使有 node:read
    expect(evaluatePage(PAGES[5]!, a)).toEqual({ ok: false, reason: 'hidden' }); // 自用模式隐藏商业页面，即使有权限
    const limited = accessFromMe(makeMe({ permissions: ['task:read'] }));
    expect(evaluatePage(PAGES[2]!, limited)).toEqual({ ok: false, reason: 'forbidden', missing: ['config:rollback'] });
  });

  it('商业模式下商业页面可见', () => {
    const a = accessFromMe(makeMe({ runMode: 'commercial', permissions: ['billing:read'] }));
    expect(evaluatePage(PAGES[5]!, a)).toEqual({ ok: true });
  });

  it('一级菜单下已交付页面数为 0 → 整个菜单不渲染；顺序按 17 个一级菜单的固定顺序；没有占位项', () => {
    const groups = buildMenu(PAGES, accessFromMe(meFull));
    expect(groups.map((g) => g.key)).toEqual(['nodes', 'users', 'ops']); // billing（模式）、…整组消失；nodes 因 maint 可见而保留
    expect(groups.find((g) => g.key === 'nodes')!.children.map((c) => c.key)).toEqual(['maint']); // 未交付模块的页面不在其中
    expect(JSON.stringify(groups)).not.toMatch(/soon|coming|disabled|placeholder|敬请期待/i);
    const none = buildMenu(PAGES, accessFromMe(makeMe({ permissions: [] })));
    expect(none).toEqual([]);
  });

  it('menu:false 的页面不进菜单；别名入口进菜单并标注规范页面', () => {
    const groups = buildMenu(PAGES, accessFromMe(meFull));
    const ops = groups.find((g) => g.key === 'ops')!;
    expect(ops.children.map((c) => c.key)).not.toContain('detail');
    expect(groups.find((g) => g.key === 'users')!.children[0]).toMatchObject({ key: 'alias', aliasOf: 'audit' });
  });

  it('全局搜索只索引已交付且可见的页面，别名与 menu:false 不重复索引', () => {
    const idx = buildSearchIndex(PAGES, accessFromMe(meFull));
    expect(idx.map((e) => e.key).sort()).toEqual(['audit', 'maint', 'rollback', 'tasks']);
    expect(buildSearchIndex(PAGES, accessFromMe(makeMe({ permissions: [] })))).toEqual([]);
  });

  it('搜索过滤按标题/菜单名/关键字不区分大小写匹配', () => {
    const idx = buildSearchIndex([page('tasks', 'ops', 'tasks', ['task:read'], { keywordKeys: ['kw.jobs'] })], accessFromMe(meFull));
    const tr = (k: string) => ({ 't.tasks': 'Task Status', 'menu.group.ops': 'Operations', 'kw.jobs': 'jobs' })[k] ?? k;
    expect(filterSearch(idx, 'status', tr)).toHaveLength(1);
    expect(filterSearch(idx, 'OPERA', tr)).toHaveLength(1);
    expect(filterSearch(idx, 'jobs', tr)).toHaveLength(1);
    expect(filterSearch(idx, 'zzz', tr)).toHaveLength(0);
    expect(filterSearch(idx, '  ', tr)).toHaveLength(1);
  });

  it('一级菜单与 ia.md §1 一致：17 个、固定顺序', () => {
    expect(GROUPS.map((g) => g.key)).toEqual(GROUP_KEYS);
    expect(GROUPS).toHaveLength(17);
    expect(GROUP_KEYS.slice(0, 3)).toEqual(['dashboard', 'sites', 'nodes']);
    expect(GROUP_KEYS[16]).toBe('settings');
  });
});
