import type { Component } from 'vue';
import type { RunMode } from '../api/types';

/** 17 个一级菜单（ia.md §1，基线 §6.1 顺序）。只有「已交付页面数 > 0」的菜单才会渲染。 */
export const GROUP_KEYS = [
  'dashboard',
  'sites',
  'nodes',
  'scheduling',
  'dns',
  'cache',
  'security',
  'l4',
  'certificates',
  'monitoring',
  'users',
  'billing',
  'notifications',
  'plugins',
  'ops',
  'website',
  'settings',
] as const;
export type GroupKey = (typeof GROUP_KEYS)[number];

export interface GroupDef {
  key: GroupKey;
  /** i18n 键：menu.group.<key> */
  titleKey: string;
}

/**
 * 已交付页面（「已交付模块注册表」的一项）。
 * 页面只有在这里登记后才存在于路由表、菜单与全局搜索——没有登记 = 不渲染（FR-31、NFR-1）；不存在占位页。
 * 可见性 = 页面已登记 ∧ 后端模块已交付（/auth/me）∧ 权限 ∧ 运行模式（ia.md §0）。
 */
export interface PageDef {
  /** 全局唯一键（路由名、标签键、搜索键）。 */
  key: string;
  /** 绝对路径，如 /ops/tasks。 */
  path: string;
  group: GroupKey;
  /** i18n 键（页面标题）。 */
  titleKey: string;
  /** 后端模块键：必须出现在 /auth/me 的 features.modules_delivered 中，否则页面不可达（404）。 */
  module: string;
  /** 须全部具备的权限点（resource:action）。 */
  permissions: readonly string[];
  /** 允许的运行模式；缺省 = 全部。 */
  runModes?: readonly RunMode[];
  component: () => Promise<Component>;
  /** 搜索关键字（i18n 键列表，可选）。 */
  keywordKeys?: readonly string[];
  /** false：不进侧栏（如详情页）；仍受路由守卫约束。缺省 true。 */
  menu?: boolean;
  /** UX-3：该页面是另一个规范页面的别名入口（共用组件，不复制页面）。 */
  aliasOf?: string;
  /** 复杂编辑页（规则条件、CMS 装修）：手机宽度下显示「可用但不保证体验」的如实标注（UX-23）。 */
  complexEdit?: boolean;
}
