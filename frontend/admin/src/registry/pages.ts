import type { PageDef } from './types';

/**
 * 生产环境的已交付页面清单。
 *
 * Story 1.38 只交付外壳：登录、403/404、修改口令、外壳首页都是「系统路由」（router/routes.ts），不在清单里；
 * 业务页面（系统设置/用户与角色/API Token/审计，Story 1.39 起）在交付时**追加到这里**——追加之前它们不存在于路由表、菜单与搜索，
 * 也没有「敬请期待」之类的占位。故当前为空：外壳在没有任何业务页面时如实显示空态，而不是渲染假菜单。
 */
export const productionPages: readonly PageDef[] = [];
