import { describe, expect, it, vi } from 'vitest';
import { stripTokenFromUrl } from './url';

function fakeWin(href: string) {
  const replaceState = vi.fn();
  return { win: { location: { href }, history: { replaceState } } as unknown as Pick<Window, 'location' | 'history'>, replaceState };
}

describe('token 不得出现在 URL', () => {
  it('带 token 类参数时丢弃并改写地址栏，保留其余参数', () => {
    const { win, replaceState } = fakeWin('http://127.0.0.1:8080/setup?token=SECRET&x=1&Setup_Token=S2');
    expect(stripTokenFromUrl(win)).toBe(true);
    expect(replaceState).toHaveBeenCalledWith(null, '', '/setup?x=1');
  });
  it('片段里带 token 同样清除；干净 URL 不改动', () => {
    const a = fakeWin('http://h/setup#token=SECRET');
    expect(stripTokenFromUrl(a.win)).toBe(true);
    expect(a.replaceState).toHaveBeenCalledWith(null, '', '/setup');
    const b = fakeWin('http://h/setup?lang=en');
    expect(stripTokenFromUrl(b.win)).toBe(false);
    expect(b.replaceState).not.toHaveBeenCalled();
  });
});
