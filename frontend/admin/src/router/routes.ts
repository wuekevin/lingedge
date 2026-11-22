import type { RouteRecordRaw } from 'vue-router';
import type { PageDef } from '../registry/types';

// 系统路由（外壳自身）：登录、修改口令、403、404、首页。业务页面来自「已交付页面注册表」，不在这里手写。
export const ROUTE = { login: 'login', changePw: 'change-password', forbidden: 'forbidden', notFound: 'not-found', catchAll: 'catch-all', home: 'home', shell: 'shell' } as const;

declare module 'vue-router' {
  interface RouteMeta {
    /** 登录前可访问。 */
    public?: boolean;
    /** 注册表页面键；系统路由无。 */
    pageKey?: string;
    titleKey?: string;
    /** 是否开标签页。 */
    tab?: boolean;
  }
}

export function buildRoutes(pages: readonly PageDef[]): RouteRecordRaw[] {
  const pageRoutes: RouteRecordRaw[] = pages.map((p) => ({
    name: p.key,
    path: p.path,
    component: p.component,
    meta: { pageKey: p.key, titleKey: p.titleKey, tab: true },
  }));
  return [
    { name: ROUTE.login, path: '/login', component: () => import('../views/LoginView.vue'), meta: { public: true } },
    { name: ROUTE.changePw, path: '/change-password', component: () => import('../views/ChangePasswordView.vue') },
    {
      name: ROUTE.shell,
      path: '/',
      component: () => import('../layout/AdminLayout.vue'),
      children: [
        { name: ROUTE.home, path: '', component: () => import('../views/HomeView.vue'), meta: { titleKey: 'home.title', tab: true } },
        { name: ROUTE.forbidden, path: '403', component: () => import('../views/ForbiddenView.vue'), meta: { titleKey: 'forbidden.title' } },
        { name: ROUTE.notFound, path: '404', component: () => import('../views/NotFoundView.vue'), meta: { titleKey: 'notFound.title' } },
        ...pageRoutes.map((r) => r),
        // 兜底：任何未登记的路径一律 404（不是 403：避免暗示「存在但无权」）；原样显示在原 URL 上，未登录时守卫先把它送去登录并保留回跳。
        { name: ROUTE.catchAll, path: ':pathMatch(.*)*', component: () => import('../views/NotFoundView.vue'), meta: { titleKey: 'notFound.title' } },
      ],
    },
  ];
}
