import { createApp } from 'vue';
import '@lingedge/shared/tokens.css';
import App from './App.vue';
import { createSetupClient } from './api/client';
import { createSetupI18n, initialLocale } from './i18n';
import { stripTokenFromUrl } from './url';
import { createWizard, WIZARD_KEY } from './state/wizard';
import { applyResolvedTheme, initialThemePref } from './theme';
import './styles/app.css';

const urlTokenIgnored = stripTokenFromUrl();
function sessionStore(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}
// 首帧前先应用主题（跟随系统或本标签页的显式选择），避免暗色系统下先闪一下亮色。
applyResolvedTheme(initialThemePref(sessionStore()));
const i18n = createSetupI18n(initialLocale(sessionStore(), navigator.language));
const wizard = createWizard(createSetupClient());

createApp(App, { urlTokenIgnored }).use(i18n).provide(WIZARD_KEY, wizard).mount('#app');
