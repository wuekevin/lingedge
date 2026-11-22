import { createPinia, setActivePinia } from 'pinia';
import { beforeEach, describe, expect, it } from 'vitest';
import { MAX_TABS, useTabsStore } from './tabs';

beforeEach(() => {
  setActivePinia(createPinia());
  sessionStorage.clear();
});

describe('多标签页', () => {
  it('同一 fullPath 不重复开标签，只激活', () => {
    const t = useTabsStore();
    t.open('/a', 'a');
    t.open('/b', 'b');
    t.open('/a', 'a');
    expect(t.tabs.map((x) => x.fullPath)).toEqual(['/a', '/b']);
    expect(t.active).toBe('/a');
    t.open('/a?id=1', 'a'); // 参数不同 = 不同页面
    expect(t.tabs).toHaveLength(3);
  });

  it('超过 12 个：关闭最久未用的「无更改」标签；有未保存更改的不被淘汰', () => {
    const t = useTabsStore();
    for (let i = 0; i < MAX_TABS; i++) t.open(`/p${i}`, 'x', 1000 + i);
    t.setDirty('/p0', true); // 最久未用但有更改
    const evicted = t.open('/new', 'x', 5000);
    expect(evicted.map((e) => e.fullPath)).toEqual(['/p1']);
    expect(t.tabs).toHaveLength(MAX_TABS);
    expect(t.tabs.some((x) => x.fullPath === '/p0')).toBe(true);
  });

  it('关闭有未保存更改的标签需确认；force 才真正关闭', () => {
    const t = useTabsStore();
    t.open('/a', 'a');
    t.setDirty('/a', true);
    expect(t.close('/a')).toBe('needs-confirm');
    expect(t.tabs).toHaveLength(1);
    expect(t.close('/a', true)).toBe('closed');
    expect(t.close('/zzz')).toBe('not-found');
  });

  it('首页标签固定不可关闭；关闭其他保留首页与当前页，并保留有更改的标签', () => {
    const t = useTabsStore();
    t.open('/', 'home');
    t.open('/a', 'a');
    t.open('/b', 'b');
    t.open('/c', 'c');
    expect(t.close('/')).toBe('not-found');
    t.setDirty('/a', true);
    expect(t.closeOthers('/c')).toBe(1);
    expect(t.tabs.map((x) => x.fullPath).sort()).toEqual(['/', '/a', '/c']);
  });

  it('刷新恢复：只恢复仍可访问的标签，且不恢复未保存状态；退出/切换身份关闭全部并清除持久化', () => {
    const t = useTabsStore();
    t.open('/a', 'ta');
    t.open('/gone', 'tg');
    t.setDirty('/a', true);
    setActivePinia(createPinia());
    const r = useTabsStore();
    r.restore((p) => (p === '/a' ? 'ta' : null));
    expect(r.tabs.map((x) => [x.fullPath, x.dirty])).toEqual([['/a', false]]);
    r.closeAll();
    expect(r.tabs).toEqual([]);
    expect(sessionStorage.getItem('le.admin.tabs')).toBeNull();
    setActivePinia(createPinia());
    const again = useTabsStore();
    again.restore(() => 'x');
    expect(again.tabs).toEqual([]);
  });

  it('持久化内容只含路径与标题键（无业务数据）', () => {
    const t = useTabsStore();
    t.open('/a', 'ta');
    t.setDirty('/a', true);
    t.persist();
    expect(JSON.parse(sessionStorage.getItem('le.admin.tabs')!)).toEqual([{ fullPath: '/a', titleKey: 'ta' }]);
  });
});
