import { afterEach, describe, expect, it } from 'vitest';
import { applyResolvedTheme, initialThemePref, parseThemePref, persistThemePref, resolveTheme, watchSystemTheme } from './theme';
import App from './App.vue';
import { mountStep, tid } from './testing/mount';

afterEach(() => {
  document.body.replaceChildren();
  document.documentElement.removeAttribute('data-theme');
  document.body.removeAttribute('arco-theme');
});

/** 可控的 matchMedia 替身（只模拟浏览器 API，不涉及被测逻辑）。 */
function fakeWin(dark: boolean) {
  const listeners = new Set<() => void>();
  const state = { dark };
  const win = {
    matchMedia: () => ({
      get matches() {
        return state.dark;
      },
      addEventListener: (_: string, l: () => void) => listeners.add(l),
      removeEventListener: (_: string, l: () => void) => listeners.delete(l),
    }),
  } as unknown as Pick<Window, 'matchMedia'>;
  return { win, flip: (d: boolean) => { state.dark = d; listeners.forEach((l) => l()); }, listeners };
}

describe('向导主题（Story 1.37b）', () => {
  it('偏好解析：未知值回落 auto；auto 跟随系统，显式选择优先', () => {
    expect(parseThemePref('dark')).toBe('dark');
    expect(parseThemePref('purple')).toBe('auto');
    expect(parseThemePref(null)).toBe('auto');
    expect(resolveTheme('auto', true)).toBe('dark');
    expect(resolveTheme('auto', false)).toBe('light');
    expect(resolveTheme('light', true)).toBe('light');
    expect(resolveTheme('dark', false)).toBe('dark');
  });

  it('存取：存储不可用时不抛错', () => {
    const store = new Map<string, string>();
    persistThemePref({ setItem: (k, v) => void store.set(k, v) }, 'dark');
    expect(initialThemePref({ getItem: (k) => store.get(k) ?? null })).toBe('dark');
    expect(() => persistThemePref({ setItem: () => { throw new Error('denied'); } }, 'dark')).not.toThrow();
    expect(initialThemePref({ getItem: () => { throw new Error('denied'); } })).toBe('auto');
    expect(initialThemePref(null)).toBe('auto');
  });

  it('applyResolvedTheme：写 <html data-theme> 与 <body arco-theme>', () => {
    expect(applyResolvedTheme('auto', document, fakeWin(true).win)).toBe('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    expect(document.body.getAttribute('arco-theme')).toBe('dark');
    expect(applyResolvedTheme('auto', document, fakeWin(false).win)).toBe('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    expect(document.body.hasAttribute('arco-theme')).toBe(false);
  });

  it('系统配色变化被监听，取消订阅后不再触发', () => {
    const f = fakeWin(false);
    let n = 0;
    const stop = watchSystemTheme(() => n++, f.win);
    f.flip(true);
    expect(n).toBe(1);
    stop();
    f.flip(false);
    expect(n).toBe(1);
    expect(f.listeners.size).toBe(0);
  });

  it('App：顶栏主题选择器可切换并写到 <html>；选择为 dark 后不受系统影响', async () => {
    const m = await mountStep(App, { enter: false });
    const sel = tid(m.wrapper, 'theme-select');
    expect(sel.exists()).toBe(true);
    await sel.setValue('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    await sel.setValue('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    expect(sel.findAll('option').map((o) => o.attributes('value'))).toEqual(['auto', 'light', 'dark']);
  });
});
