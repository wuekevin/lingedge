import type { PageDef } from './types';

/**
 * 生产环境的已交付页面清单（「已交付模块注册表」）。
 *
 * Story 1.38 只交付外壳（清单为空）；Story 1.39 起业务页面在交付时追加到这里——登记后菜单、搜索、路由、403/404 守卫与对账测试（registry.test.ts）自动生效，
 * 没有登记的页面不存在于路由表/菜单/搜索，也没有「敬请期待」占位。
 *
 * 一个能力只有一个规范页面（UX-3）：用户列表 = /users/list（「设置 → 管理员管理」是别名）、权限角色 = /settings/roles（「用户管理 → 用户角色」是别名）、
 * 审计日志 = /ops/audit（「用户管理 → 操作审计」是别名，预置「用户操作」筛选）。别名共用同一个组件，不复制页面；搜索只索引规范页面。
 * ia.md §1.17 中邮件/API/系统安全/数据保留/系统日志属 P2：这里不登记即不渲染。
 */
const users = () => import('../views/system/UsersView.vue');
const roles = () => import('../views/system/RolesView.vue');
const audit = () => import('../views/system/AuditView.vue');

export const productionPages: readonly PageDef[] = [
  { key: 'users.list', path: '/users/list', group: 'users', titleKey: 'pages.usersList', module: 'auth', permissions: ['user:read'], component: users, keywordKeys: ['pages.kw.users'] },
  { key: 'users.roles', path: '/users/roles', group: 'users', titleKey: 'pages.usersRoles', module: 'auth', permissions: ['role:read'], component: roles, aliasOf: 'settings.roles' },
  { key: 'users.api-tokens', path: '/users/api-tokens', group: 'users', titleKey: 'pages.usersTokens', module: 'auth', permissions: ['token:manage'], component: () => import('../views/system/ApiTokensView.vue'), keywordKeys: ['pages.kw.tokens'] },
  { key: 'users.login-logs', path: '/users/login-logs', group: 'users', titleKey: 'pages.usersLoginLogs', module: 'audit', permissions: ['login-log:read'], component: () => import('../views/system/LoginLogsView.vue') },
  { key: 'users.audit', path: '/users/audit', group: 'users', titleKey: 'pages.usersAudit', module: 'audit', permissions: ['audit:read'], component: audit, aliasOf: 'ops.audit' },
  { key: 'ops.audit', path: '/ops/audit', group: 'ops', titleKey: 'pages.opsAudit', module: 'audit', permissions: ['audit:read'], component: audit, keywordKeys: ['pages.kw.audit'] },
  { key: 'settings.basic', path: '/settings/basic', group: 'settings', titleKey: 'pages.settingsBasic', module: 'system', permissions: ['setting:read'], component: () => import('../views/system/SettingsBasicView.vue'), keywordKeys: ['pages.kw.runMode'] },
  { key: 'settings.brand', path: '/settings/brand', group: 'settings', titleKey: 'pages.settingsBrand', module: 'system', permissions: ['setting:read'], component: () => import('../views/system/SettingsBrandView.vue') },
  { key: 'settings.login', path: '/settings/login', group: 'settings', titleKey: 'pages.settingsLogin', module: 'system', permissions: ['setting:read'], component: () => import('../views/system/SettingsLoginView.vue') },
  { key: 'settings.admins', path: '/settings/admins', group: 'settings', titleKey: 'pages.settingsAdmins', module: 'auth', permissions: ['user:read'], component: users, aliasOf: 'users.list' },
  { key: 'settings.roles', path: '/settings/roles', group: 'settings', titleKey: 'pages.settingsRoles', module: 'auth', permissions: ['role:read'], component: roles, keywordKeys: ['pages.kw.roles'] },
  { key: 'settings.version', path: '/settings/version', group: 'settings', titleKey: 'pages.settingsVersion', module: 'system', permissions: ['system:read'], component: () => import('../views/system/VersionView.vue') },
];
