import { useI18n } from 'vue-i18n';
import type { LeLocale } from '@lingedge/shared';
import { writeLocalePref } from '../i18n';
import { localStore } from '../theme/theme';

/** 切换界面语言：写偏好并同步 <html lang>。 */
export function useLocale() {
  const { locale } = useI18n();
  function setLocale(l: LeLocale): void {
    locale.value = l;
    writeLocalePref(localStore(), l);
    document.documentElement.setAttribute('lang', l);
  }
  return { locale, setLocale };
}
