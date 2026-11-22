import ArcoVue from '@arco-design/web-vue';
import '@arco-design/web-vue/dist/arco.css';
import { createLeI18n } from '@lingedge/shared';
import '@lingedge/shared/tokens.css';
import '@lingedge/shared/arco-theme.css';
import { createApp } from 'vue';
import App from './App.vue';

// 基座探针：确认 shared（令牌/i18n）与 Arco 在本应用的 Vite 构建里可用；console 自己的路由、菜单、会话、API 层由后续 Story 交付。
createApp(App).use(ArcoVue).use(createLeI18n({ locale: navigator.language })).mount('#app');
