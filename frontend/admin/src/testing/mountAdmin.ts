import { flushPromises } from '@vue/test-utils';
import { createMemoryHistory } from 'vue-router';
import { createAdminApp } from '../app';
import { createAdminI18n } from '../i18n';
import type { PageDef } from '../registry/types';
import { fakeFetch, makeMe, type Call } from './fakeApi';
import type { Me } from '../api/types';

export interface MountOptions {
  pages?: readonly PageDef[];
  me?: Me | null;
  path?: string;
  width?: number;
  locale?: string;
  routes?: Parameters<typeof fakeFetch>[0];
  messages?: Record<string, string>;
}

/** 以生产同一套装配（createAdminApp）挂载整个 Admin，只替换 fetch 与历史记录。 */
export async function mountAdmin(o: MountOptions = {}) {
  Object.defineProperty(window, 'innerWidth', { value: o.width ?? 1280, configurable: true, writable: true });
  const i18n = createAdminI18n(o.locale ?? 'zh-CN');
  for (const l of ['zh-CN', 'en'] as const) i18n.global.mergeLocaleMessage(l, { t: o.messages ?? {} });
  const me = o.me === undefined ? makeMe() : o.me;
  const ff = fakeFetch({
    'GET /auth/me': me ? { body: me } : { status: 401, body: { error: { code: 'auth.unauthenticated', message: 'x', request_id: 'r' } } },
    'GET /tenants': { body: { items: [{ uid: 'TDEF', name: 'Default Tenant', slug: 'default', is_default: true }] } },
    'POST /auth/logout': { status: 204 },
    ...(o.routes ?? {}),
  });
  const handle = createAdminApp({ pages: o.pages ?? [], i18n, fetchImpl: ff.fetch, history: createMemoryHistory() });
  await handle.session.restore();
  await handle.session.resolveActingTenant();
  handle.app.use(handle.router);
  await handle.router.push(o.path ?? '/');
  await handle.router.isReady();
  const el = document.createElement('div');
  document.body.appendChild(el);
  handle.app.mount(el);
  await flushPromises();
  const calls: Call[] = ff.calls;
  return {
    ...handle,
    el,
    calls,
    i18n,
    async settle() {
      await flushPromises();
      await new Promise((r) => setTimeout(r, 0));
      await flushPromises();
    },
    async go(p: string) {
      await handle.router.push(p);
      await flushPromises();
    },
    unmount() {
      handle.app.unmount();
      el.remove();
      document.body.querySelectorAll('.arco-modal-container, .arco-drawer-container, .arco-trigger-popup, .arco-message-list').forEach((n) => n.remove());
    },
  };
}
