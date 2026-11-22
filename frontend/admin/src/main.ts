import '@arco-design/web-vue/dist/arco.css';
import '@lingedge/shared/tokens.css';
import '@lingedge/shared/arco-theme.css';
import { resolveLocale } from '@lingedge/shared';
import { createApp, h } from 'vue';
import { createAdminApp } from './app';
import { createAdminI18n, readLocalePref } from './i18n';
import { productionPages } from './registry/pages';
import type { PageDef } from './registry/types';
import { applyResolvedTheme, localStore, readThemePref } from './theme/theme';
import BootFailure from './views/BootFailure.vue';
import './styles/app.css';

// 首帧前先应用主题（跟随系统或用户的显式选择），避免暗色系统下先闪一下亮色。
applyResolvedTheme(readThemePref(localStore()));
const locale = readLocalePref(localStore()) ?? resolveLocale(navigator.language);
document.documentElement.setAttribute('lang', locale);
const i18n = createAdminI18n(locale);

async function loadPages(): Promise<PageDef[]> {
  const pages = [...productionPages];
  // 仅 e2e 构建（--mode e2e）会包含测试夹具页面；生产构建里这个分支被编译期常量折叠，夹具模块不进产物（check:mock 校验）。
  if (import.meta.env.MODE === 'e2e') {
    const m = await import('./mocks/e2e-pages.mock');
    pages.push(...m.bootE2EPages(i18n));
  }
  return pages;
}

async function start(): Promise<void> {
  const handle = createAdminApp({ pages: await loadPages(), i18n });
  try {
    // 先读取一次当前身份：401 = 匿名（进入登录）；网络/5xx 失败不能误判为「已退出」，显示启动失败页并允许重试。
    await handle.session.restore();
    await handle.session.resolveActingTenant();
  } catch (e) {
    createApp({ render: () => h(BootFailure, { error: e }) })
      .use(i18n)
      .mount('#app');
    return;
  }
  handle.app.use(handle.router);
  handle.app.mount('#app');
}
void start();
