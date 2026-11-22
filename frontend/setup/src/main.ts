import { createApp } from 'vue';
import '@lingedge/shared/tokens.css';
import App from './App.vue';
import { createSetupClient } from './api/client';
import { createSetupI18n, initialLocale } from './i18n';
import { stripTokenFromUrl } from './url';
import { createWizard, WIZARD_KEY } from './state/wizard';
import './styles/app.css';

const urlTokenIgnored = stripTokenFromUrl();
function sessionStore(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}
const i18n = createSetupI18n(initialLocale(sessionStore(), navigator.language));
const wizard = createWizard(createSetupClient());

createApp(App, { urlTokenIgnored }).use(i18n).provide(WIZARD_KEY, wizard).mount('#app');
