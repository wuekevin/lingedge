import { applyTheme, type LeTheme } from '@lingedge/shared';

/** 主题偏好：auto = 跟随系统 prefers-color-scheme（缺省）；light/dark = 用户显式选择。
 * [ASSUMPTION] EXPERIENCE.md 希望偏好存服务端用户偏好；当前契约没有对应字段（不改 OpenAPI），先存 localStorage（非敏感），失败降级为跟随系统。 */
export type ThemePref = 'auto' | 'light' | 'dark';
export const THEME_PREFS: readonly ThemePref[] = ['auto', 'light', 'dark'];
const THEME_KEY = 'le.admin.theme';

export function parseThemePref(v: string | null | undefined): ThemePref {
  return v === 'light' || v === 'dark' ? v : 'auto';
}

export function readThemePref(storage: Pick<Storage, 'getItem'> | null): ThemePref {
  try {
    return parseThemePref(storage?.getItem(THEME_KEY));
  } catch {
    return 'auto';
  }
}

export function writeThemePref(storage: Pick<Storage, 'setItem'> | null, pref: ThemePref): void {
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

export function applyResolvedTheme(pref: ThemePref, doc: Document = document, win: Pick<Window, 'matchMedia'> = window): LeTheme {
  const theme = resolveTheme(pref, systemPrefersDark(win));
  applyTheme(theme, doc);
  return theme;
}

/** 监听系统配色变化；返回取消订阅函数。 */
export function watchSystemTheme(onChange: () => void, win: Pick<Window, 'matchMedia'> = window): () => void {
  const mq = win.matchMedia?.(QUERY);
  if (!mq) return () => undefined;
  mq.addEventListener('change', onChange);
  return () => mq.removeEventListener('change', onChange);
}

export function localStore(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}
