import type { Me, RunMode } from '../api/types';
import { GROUPS } from './groups';
import type { GroupDef, PageDef } from './types';

// 菜单/页面可见性（ia.md §0；FR-31、UX-1）：已登记 ∧ 后端模块已交付 ∧ 运行模式允许 ∧ 权限。
// 权限不足与「不存在」要区分：模块未交付/模式不允许 → 当作不存在（404，避免暗示存在）；仅权限不足 → 403。

export interface Access {
  permissions: ReadonlySet<string>;
  modules: ReadonlySet<string>;
  runMode: RunMode;
}

export function accessFromMe(me: Me): Access {
  const delivered = me.features?.modules_delivered ?? me.capabilities.modules;
  return {
    permissions: new Set(me.permissions),
    modules: new Set(delivered),
    runMode: me.features?.run_mode ?? me.capabilities.run_mode,
  };
}

export type PageAccess = { ok: true } | { ok: false; reason: 'hidden' } | { ok: false; reason: 'forbidden'; missing: string[] };

export function evaluatePage(page: PageDef, access: Access): PageAccess {
  if (!access.modules.has(page.module)) return { ok: false, reason: 'hidden' };
  if (page.runModes && !page.runModes.includes(access.runMode)) return { ok: false, reason: 'hidden' };
  const missing = page.permissions.filter((p) => !access.permissions.has(p));
  if (missing.length > 0) return { ok: false, reason: 'forbidden', missing };
  return { ok: true };
}

export interface MenuItem {
  key: string;
  titleKey: string;
  path: string;
  aliasOf?: string;
}
export interface MenuGroup {
  key: string;
  titleKey: string;
  children: MenuItem[];
}

/** 动态菜单：只含可见页面；一级菜单下可见页面数为 0 → 整个一级菜单不渲染（没有灰显占位）。顺序 = GROUPS 顺序，组内按登记顺序。 */
export function buildMenu(pages: readonly PageDef[], access: Access, groups: readonly GroupDef[] = GROUPS): MenuGroup[] {
  const out: MenuGroup[] = [];
  for (const g of groups) {
    const children: MenuItem[] = pages
      .filter((p) => p.group === g.key && p.menu !== false && evaluatePage(p, access).ok)
      .map((p) => ({ key: p.key, titleKey: p.titleKey, path: p.path, ...(p.aliasOf ? { aliasOf: p.aliasOf } : {}) }));
    if (children.length > 0) out.push({ key: g.key, titleKey: g.titleKey, children });
  }
  return out;
}

export interface SearchEntry {
  key: string;
  titleKey: string;
  groupTitleKey: string;
  path: string;
  keywordKeys: readonly string[];
}

/** 全局搜索索引：只索引「已交付且当前身份可见」的页面；别名页面不重复索引（同一能力一个规范入口，UX-3）。 */
export function buildSearchIndex(pages: readonly PageDef[], access: Access): SearchEntry[] {
  return pages
    .filter((p) => !p.aliasOf && p.menu !== false && evaluatePage(p, access).ok)
    .map((p) => ({ key: p.key, titleKey: p.titleKey, groupTitleKey: `menu.group.${p.group}`, path: p.path, keywordKeys: p.keywordKeys ?? [] }));
}
