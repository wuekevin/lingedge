import ArcoVue from '@arco-design/web-vue';
import { createPinia, type Pinia } from 'pinia';
import { createApp, type App as VueApp } from 'vue';
import type { Router, RouterHistory } from 'vue-router';
import App from './App.vue';
import { REGISTRY_KEY } from './layout/keys';
import { createAuthApi } from './api/auth';
import { createApiClient } from './api/client';
import type { AdminI18n } from './i18n';
import type { PageDef } from './registry/types';
import { createRegistry, type Registry } from './registry/registry';
import { createAdminRouter } from './router';
import { ROUTE } from './router/routes';
import { bindAuthApi, useSessionStore } from './stores/session';
import { useTabsStore } from './stores/tabs';

export interface AdminAppOptions {
  pages: readonly PageDef[];
  i18n: AdminI18n;
  fetchImpl?: typeof fetch;
  history?: RouterHistory;
}

export interface AdminAppHandle {
  app: VueApp;
  router: Router;
  session: ReturnType<typeof useSessionStore>;
  tabs: ReturnType<typeof useTabsStore>;
  pinia: Pinia;
  registry: Registry;
}

/** 装配：pinia、i18n、API 客户端、会话、注册表、路由。生产（main.ts）与测试共用同一个装配，测试只替换 fetch。 */
export function createAdminApp(opts: AdminAppOptions): AdminAppHandle {
  const pinia = createPinia();
  const app = createApp(App);
  app.use(pinia);
  const session = useSessionStore(pinia);
  const tabs = useTabsStore(pinia);
  const registry = createRegistry(opts.pages);
  const router = createAdminRouter({ registry, ...(opts.history ? { history: opts.history } : {}) });

  const client = createApiClient({
    ...(opts.fetchImpl ? { fetchImpl: opts.fetchImpl } : {}),
    csrf: () => session.csrf,
    setCsrf: (t) => {
      session.csrf = t;
    },
    actingTenantUid: () => session.actingTenant?.uid ?? null,
    locale: () => String(opts.i18n.global.locale.value),
    onUnauthorized: () => {
      // 会话失效（过期/被吊销）：清空身份与全部标签，回到登录页并保留回跳。
      if (session.status !== 'authed') return;
      const back = router.currentRoute.value.fullPath;
      session.expire();
      tabs.closeAll();
      void router.replace({ name: ROUTE.login, query: back && back !== '/' ? { redirect: back } : {} });
    },
  });
  bindAuthApi(createAuthApi(client));

  app.provide(REGISTRY_KEY, registry);
  app.use(opts.i18n);
  app.use(ArcoVue);
  // 不在这里 app.use(router)：安装路由会立即发起首次导航，而此时身份还没读取。由调用方在 session.restore() 之后安装并挂载。
  return { app, router, session, tabs, pinia, registry };
}
