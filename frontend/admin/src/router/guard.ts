import type { RouteLocationNormalized, RouteLocationRaw } from 'vue-router';
import type { Registry } from '../registry/registry';
import { evaluatePage } from '../registry/visibility';
import type { useSessionStore } from '../stores/session';
import { ROUTE } from './routes';

export interface GuardDeps {
  registry: Registry;
}

/**
 * 路由守卫的决策（纯逻辑，便于测试）：返回 true 放行，或重定向目标。
 * 顺序：未登录 → 登录页（带回跳）；须改密 → 只允许改密页；页面可见性：模块未交付/运行模式不符 → 404；权限不足 → 403（带所需权限）。
 */
export function decide(to: RouteLocationNormalized, session: ReturnType<typeof useSessionStore>, deps: GuardDeps): true | RouteLocationRaw {
  const authed = session.status === 'authed';
  if (to.meta.public) {
    if (authed && to.name === ROUTE.login) return redirectTarget(to) ?? { name: ROUTE.home };
    return true;
  }
  if (!authed) return { name: ROUTE.login, query: to.fullPath && to.fullPath !== '/' ? { redirect: to.fullPath } : {} };
  if (session.mustChangePassword) return to.name === ROUTE.changePw ? true : { name: ROUTE.changePw };
  if (to.name === ROUTE.changePw) return { name: ROUTE.home }; // 无需改密时不进入改密页
  const key = to.meta.pageKey;
  if (key) {
    const page = deps.registry.byKey(key);
    const access = session.access;
    if (!page || !access) return { name: ROUTE.notFound, query: { path: to.fullPath } };
    const r = evaluatePage(page, access);
    if (!r.ok) {
      if (r.reason === 'hidden') return { name: ROUTE.notFound, query: { path: to.fullPath } };
      return { name: ROUTE.forbidden, query: { path: to.fullPath, need: r.missing.join(',') } };
    }
  }
  return true;
}

/** /login?redirect=… 的回跳目标：只接受站内绝对路径（防开放重定向），且不能回到登录页。 */
export function redirectTarget(to: RouteLocationNormalized): string | null {
  const r = to.query.redirect;
  return safeRedirect(typeof r === 'string' ? r : null);
}
export function safeRedirect(r: string | null): string | null {
  if (!r || !r.startsWith('/') || r.startsWith('//') || r.startsWith('/\\') || r.startsWith('/login')) return null;
  return r;
}

export type { GuardDeps as Deps };
