import { describe, expect, it } from 'vitest';
import { applyResolvedTheme, parseThemePref, readThemePref, resolveTheme, writeThemePref } from './theme';

const win = (dark: boolean) => ({ matchMedia: () => ({ matches: dark }) as MediaQueryList });

describe('主题（亮 / 暗 / 跟随系统）', () => {
  it('偏好解析：非法值回落为跟随系统', () => {
    expect(parseThemePref('dark')).toBe('dark');
    expect(parseThemePref('light')).toBe('light');
    for (const v of ['auto', 'x', '', null, undefined]) expect(parseThemePref(v)).toBe('auto');
  });
  it('auto 跟随系统，显式选择优先', () => {
    expect(resolveTheme('auto', true)).toBe('dark');
    expect(resolveTheme('auto', false)).toBe('light');
    expect(resolveTheme('light', true)).toBe('light');
    expect(resolveTheme('dark', false)).toBe('dark');
  });
  it('写 <html data-theme> 与 body[arco-theme]（颜色全部来自令牌）', () => {
    expect(applyResolvedTheme('auto', document, win(true))).toBe('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    expect(document.body.getAttribute('arco-theme')).toBe('dark');
    applyResolvedTheme('light', document, win(true));
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    expect(document.body.hasAttribute('arco-theme')).toBe(false);
  });
  it('存储不可用时降级为跟随系统，不抛错', () => {
    const broken = {
      getItem: () => {
        throw new Error('denied');
      },
      setItem: () => {
        throw new Error('denied');
      },
    };
    expect(readThemePref(broken)).toBe('auto');
    expect(() => writeThemePref(broken, 'dark')).not.toThrow();
    expect(readThemePref(null)).toBe('auto');
  });
});
