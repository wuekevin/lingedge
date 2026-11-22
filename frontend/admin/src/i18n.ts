import { createLeI18n, resolveLocale, type LeLocale, type MessageTree } from '@lingedge/shared';
import en from './locales/en.json';
import zhCN from './locales/zh-CN.json';

/** 界面语言偏好（非敏感）存 localStorage；缺省取登录用户的 locale，再取浏览器语言。 */
const LOCALE_KEY = 'le.admin.locale';

export function readLocalePref(storage: Pick<Storage, 'getItem'> | null): LeLocale | null {
  try {
    const v = storage?.getItem(LOCALE_KEY);
    return v ? resolveLocale(v) : null;
  } catch {
    return null;
  }
}
export function writeLocalePref(storage: Pick<Storage, 'setItem'> | null, locale: LeLocale): void {
  try {
    storage?.setItem(LOCALE_KEY, locale);
  } catch {
    /* 存储不可用：仅内存 */
  }
}

export function createAdminI18n(locale: string | null | undefined, onMissing?: (locale: string, key: string) => void) {
  return createLeI18n({ locale, messages: { 'zh-CN': zhCN as MessageTree, en: en as MessageTree }, ...(onMissing ? { onMissing } : {}) });
}
export type AdminI18n = ReturnType<typeof createAdminI18n>;
