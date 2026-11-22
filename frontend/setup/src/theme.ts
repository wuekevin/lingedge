import { applyTheme, type LeTheme } from '@lingedge/shared';

/** 主题偏好：auto = 跟随系统 prefers-color-scheme；light/dark = 用户显式选择（同一标签页内保留，非敏感）。 */
export type ThemePref = 'auto' | 'light' | 'dark';
export const THEME_PREFS: readonly ThemePref[] = ['auto', 'light', 'dark'];
const THEME_KEY = 'le.setup.theme';

export function parseThemePref(v: string | null | undefined): ThemePref {
  return v === 'light' || v === 'dark' ? v : 'auto';
}

export function initialThemePref(storage: Pick<Storage, 'getItem'> | null): ThemePref {
  try {
    return parseThemePref(storage?.getItem(THEME_KEY));
  } catch {
    return 'auto';
  }
}

export function persistThemePref(storage: Pick<Storage, 'setItem'> | null, pref: ThemePref): void {
  try {
    storage?.setItem(THEME_KEY, pref);
  } catch {
    /* 存储不可用：仅内存 */
  }
}

export function resolveTheme(pref: ThemePref, systemDark: boolean): LeTheme {
  if (pref === 'auto') return systemDark ? 'dark' : 'light';
  return pref;
}

const QUERY = '(prefers-color-scheme: dark)';
export const systemPrefersDark = (win: Pick<Window, 'matchMedia'> = window): boolean => win.matchMedia?.(QUERY).matches ?? false;

/** 把解析后的主题写到 <html data-theme>（颜色全部来自 shared 令牌，组件里没有主题分支）。 */
export function applyResolvedTheme(pref: ThemePref, doc: Document = document, win: Pick<Window, 'matchMedia'> = window): LeTheme {
  const theme = resolveTheme(pref, systemPrefersDark(win));
  applyTheme(theme, doc);
  return theme;
}

/** 监听系统配色变化（仅 auto 时生效）；返回取消订阅函数。 */
export function watchSystemTheme(onChange: () => void, win: Pick<Window, 'matchMedia'> = window): () => void {
  const mq = win.matchMedia?.(QUERY);
  if (!mq) return () => undefined;
  mq.addEventListener('change', onChange);
  return () => mq.removeEventListener('change', onChange);
}
