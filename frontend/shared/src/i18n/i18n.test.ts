import { describe, expect, it, vi } from 'vitest';
import { createLeI18n, flattenMessages, mergeMessages, resolveLocale, sharedMessages } from './index';

describe('resolveLocale', () => {
  it.each([
    ['zh', 'zh-CN'],
    ['zh-Hans-CN', 'zh-CN'],
    ['zh_CN', 'zh-CN'],
    ['en', 'en'],
    ['en-US', 'en'],
    ['fr', 'zh-CN'],
    ['', 'zh-CN'],
    [undefined, 'zh-CN'],
    [null, 'zh-CN'],
  ])('%s -> %s', (input, want) => {
    expect(resolveLocale(input as string | undefined)).toBe(want);
  });
});

describe('共享词条', () => {
  it('zh-CN 与 en 键集完全一致、占位符一致（与 CI 脚本同口径）', () => {
    const zh = flattenMessages(sharedMessages['zh-CN']);
    const en = flattenMessages(sharedMessages.en);
    expect(Object.keys(zh).sort()).toEqual(Object.keys(en).sort());
    const ph = (s: string) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();
    for (const k of Object.keys(zh)) expect(ph(zh[k]!), k).toEqual(ph(en[k]!));
  });
});

describe('createLeI18n', () => {
  it('按语言取词与插值', () => {
    const zh = createLeI18n({ locale: 'zh-CN' });
    const en = createLeI18n({ locale: 'en-GB' });
    expect(zh.global.t('state.error.title', { subject: '站点' })).toBe('无法加载站点');
    expect(en.global.t('state.error.title', { subject: 'sites' })).toBe('Unable to load sites');
  });
  it('应用词条深合并且覆盖共享词条', () => {
    const i18n = createLeI18n({
      locale: 'en',
      messages: { en: { app: { name: 'LingEdge' }, state: { loading: { label: 'Please wait…' } } } },
    });
    expect(i18n.global.t('app.name')).toBe('LingEdge');
    expect(i18n.global.t('state.loading.label')).toBe('Please wait…');
    expect(i18n.global.t('state.loading.cancel')).toBe('Cancel'); // 未覆盖的共享词条保留
  });
  it('缺键：回退 en；仍缺则返回键并上报', () => {
    const onMissing = vi.fn();
    const i18n = createLeI18n({ locale: 'zh-CN', messages: { en: { only: { en: 'only-en' } } }, onMissing });
    expect(i18n.global.t('only.en')).toBe('only-en');
    expect(i18n.global.t('no.such.key')).toBe('no.such.key');
    // 回退链 zh-CN → zh → en：每一级未命中都会回调；最后一级（en）仍未命中即“全局缺键”。
    expect(onMissing).toHaveBeenCalledWith('en', 'no.such.key');
    expect(onMissing).toHaveBeenCalledWith('zh', 'only.en');
  });
  it('mergeMessages 不修改入参', () => {
    const a = { x: { y: '1' } };
    const out = mergeMessages(a, { x: { z: '2' } });
    expect(a).toEqual({ x: { y: '1' } });
    expect(out).toEqual({ x: { y: '1', z: '2' } });
  });
});
