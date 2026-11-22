import { createLeI18n, resolveLocale, type LeLocale, type MessageTree } from '@lingedge/shared';
import en from './locales/en.json';
import zhCN from './locales/zh-CN.json';

/** 语言偏好（非敏感）可在同一标签页内跨刷新保留。 */
const LOCALE_KEY = 'le.setup.locale';

export function initialLocale(storage: Pick<Storage, 'getItem'> | null, navigatorLanguage: string | undefined): LeLocale {
  const saved = storage?.getItem(LOCALE_KEY);
  return resolveLocale(saved ?? navigatorLanguage);
}

export function persistLocale(storage: Pick<Storage, 'setItem'> | null, locale: LeLocale): void {
  try {
    storage?.setItem(LOCALE_KEY, locale);
  } catch {
    /* 存储不可用：仅内存 */
  }
}

export function createSetupI18n(locale: string | null | undefined) {
  return createLeI18n({ locale, messages: { 'zh-CN': zhCN as MessageTree, en: en as MessageTree } });
}
