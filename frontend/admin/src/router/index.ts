import { createRouter, createWebHistory, type Router, type RouterHistory } from 'vue-router';
import type { Registry } from '../registry/registry';
import { useSessionStore } from '../stores/session';
import { decide } from './guard';
import { buildRoutes } from './routes';

export interface CreateRouterOptions {
  registry: Registry;
  history?: RouterHistory;
}

export function createAdminRouter({ registry, history }: CreateRouterOptions): Router {
  const router = createRouter({ history: history ?? createWebHistory('/'), routes: buildRoutes(registry.pages) });
  // 身份在启动时已读取（main.ts 的 bootstrap；网络/5xx 失败由启动失败页处理，不进入路由）。守卫只做决策。
  router.beforeEach((to) => decide(to, useSessionStore(), { registry }));
  return router;
}
