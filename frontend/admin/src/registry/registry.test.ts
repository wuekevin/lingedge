import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parse } from 'yaml';
import { describe, expect, it } from 'vitest';
import type { Component } from 'vue';
import { createAdminI18n } from '../i18n';
import { bootE2EPages } from '../mocks/e2e-pages.mock';
import { productionPages } from './pages';
import { createRegistry } from './registry';
import type { PageDef } from './types';

const here = path.dirname(fileURLToPath(import.meta.url));
const REPO = path.resolve(here, '../../../..');
const openapi = parse(fs.readFileSync(path.join(REPO, 'docs/api/openapi.yaml'), 'utf8')) as { paths: Record<string, Record<string, { 'x-permissions'?: string[]; 'x-tenant-scope'?: string }>> };
const comp = () => Promise.resolve({} as Component);
const mk = (key: string, p: string, more: Partial<PageDef> = {}): PageDef => ({ key, path: p, group: 'ops', titleKey: `t.${key}`, module: 'tasks', permissions: ['task:read'], component: comp, ...more });

describe('已交付页面注册表', () => {
  it('合法清单通过；重复键/路径、保留路径、未知菜单、畸形权限串、悬空别名都会抛错', () => {
    expect(createRegistry([mk('a', '/ops/a'), mk('b', '/ops/b', { aliasOf: 'a' })]).byKey('a')?.path).toBe('/ops/a');
    expect(() => createRegistry([mk('a', '/ops/a'), mk('a', '/ops/b')])).toThrow(/duplicate page key/);
    expect(() => createRegistry([mk('a', '/ops/a'), mk('b', '/ops/a')])).toThrow(/duplicate path/);
    expect(() => createRegistry([mk('a', '/login')])).toThrow(/reserved/);
    expect(() => createRegistry([mk('a', '/403')])).toThrow(/reserved/);
    expect(() => createRegistry([mk('a', 'ops/a')])).toThrow(/invalid/);
    expect(() => createRegistry([mk('a', '/ops/a', { group: 'nope' as never })])).toThrow(/unknown group/);
    expect(() => createRegistry([mk('a', '/ops/a', { permissions: ['TaskRead'] })])).toThrow(/malformed permission/);
    expect(() => createRegistry([mk('a', '/ops/a', { aliasOf: 'zzz' })])).toThrow(/unknown page/);
  });

  // 守卫：页面登记时，其模块必须是后端「已交付模块」（sysconfig.DeliveredModules），权限必须是 OpenAPI 声明过的 x-permissions。
  const goSrc = fs.readFileSync(path.join(REPO, 'internal/platform/sysconfig/capability.go'), 'utf8');
  const delivered = new Set([...goSrc.matchAll(/\{Name: "([a-z_]+)"/g)].map((m) => m[1]!));
  const knownPerms = new Set<string>();
  for (const ops of Object.values(openapi.paths)) for (const op of Object.values(ops)) for (const p of op['x-permissions'] ?? []) knownPerms.add(p);

  it('能读到后端已交付模块与 OpenAPI 权限（对账来源不是空的）', () => {
    expect(delivered.size).toBeGreaterThanOrEqual(5);
    expect(delivered.has('auth')).toBe(true);
    expect(knownPerms.size).toBeGreaterThan(30);
  });

  it('生产清单里的每个页面：模块已交付、权限真实存在、键/路径唯一（1.39 起追加页面时自动受检）', () => {
    createRegistry(productionPages);
    for (const p of productionPages) {
      expect(delivered, `${p.key} 的模块 ${p.module} 后端尚未交付`).toContain(p.module);
      for (const perm of p.permissions) expect(knownPerms, `${p.key} 的权限 ${perm} 不在 OpenAPI`).toContain(perm);
    }
  });

  // Story 1.38 交付外壳时这里断言「清单为空」；Story 1.39 起业务页面登记进来，该断言按预期被替换为下面对 1.39 页面集合的精确对账。
  it('Story 1.39：生产清单恰好是 ia.md §1.11/§1.15/§1.17 的 M1 页面（不多不少），键与路径一一对应', () => {
    expect(productionPages.map((p) => `${p.group}:${p.key}:${p.path}`).sort()).toEqual(
      [
        'ops:ops.audit:/ops/audit',
        'settings:settings.admins:/settings/admins',
        'settings:settings.basic:/settings/basic',
        'settings:settings.brand:/settings/brand',
        'settings:settings.login:/settings/login',
        'settings:settings.roles:/settings/roles',
        'settings:settings.version:/settings/version',
        'users:users.api-tokens:/users/api-tokens',
        'users:users.audit:/users/audit',
        'users:users.list:/users/list',
        'users:users.login-logs:/users/login-logs',
        'users:users.roles:/users/roles',
      ].sort(),
    );
  });

  it('FR-36：邮件/API/系统安全/数据保留/系统日志（P2）不登记 = 不渲染；设置菜单只有 M1 的六页', () => {
    const settings = productionPages.filter((p) => p.group === 'settings').map((p) => p.key.replace('settings.', ''));
    expect(settings.sort()).toEqual(['admins', 'basic', 'brand', 'login', 'roles', 'version']);
    for (const p of productionPages) expect(p.path).not.toMatch(/mail|api-settings|security|retention|syslog|system-log/);
  });

  it('UX-3：别名与规范页面共用同一个组件（不复制页面），规范页面本身不是别名，搜索不重复索引别名', () => {
    const by = new Map(productionPages.map((p) => [p.key, p]));
    const aliases = productionPages.filter((p) => p.aliasOf);
    expect(aliases.map((p) => `${p.key}->${p.aliasOf}`).sort()).toEqual(['settings.admins->users.list', 'users.audit->ops.audit', 'users.roles->settings.roles']);
    for (const a of aliases) {
      const canonical = by.get(a.aliasOf!)!;
      expect(canonical.aliasOf, `${a.key} 的规范页面不能再是别名`).toBeUndefined();
      expect(a.component, `${a.key} 与 ${canonical.key} 必须共用同一个组件加载器`).toBe(canonical.component);
    }
  });

  it('e2e 夹具页面使用的权限点都是 OpenAPI 里真实存在的（夹具不发明权限）', () => {
    const pages = bootE2EPages(createAdminI18n('en'));
    createRegistry(pages);
    for (const p of pages) for (const perm of p.permissions) expect(knownPerms, `${p.key}: ${perm}`).toContain(perm);
  });
});

describe('Admin 路由前缀登记（ADR-014 §5）', () => {
  const prefixes: string[] = JSON.parse(fs.readFileSync(path.join(REPO, 'frontend/isolation/admin-route-prefixes.json'), 'utf8')).prefixes;
  // 首段只出现在 platform 端点里的路径（构建隔离检查已按 platform 端点模板精确覆盖它们）。
  const platformOnlyFirstSegments = new Set<string>();
  const scopesBySegment = new Map<string, Set<string>>();
  for (const [p, ops] of Object.entries(openapi.paths)) {
    const seg = p.split('/')[1]!;
    for (const op of Object.values(ops)) {
      const s = scopesBySegment.get(seg) ?? new Set<string>();
      s.add((op as { 'x-tenant-scope'?: string })['x-tenant-scope'] ?? '');
      scopesBySegment.set(seg, s);
    }
  }
  for (const [seg, scopes] of scopesBySegment) if (scopes.size === 1 && scopes.has('platform')) platformOnlyFirstSegments.add(seg);
  const adminOnlyGroups = new Set(['nodes', 'scheduling', 'ops']);

  it('只属于 Admin 的一级菜单（节点管理/智能调度/自动化运维）下的页面：路径前缀已登记，或已被 platform 端点模板覆盖，Console 构建因此不得含有它们', () => {
    const pages = [...productionPages, ...bootE2EPages(createAdminI18n('en'))].filter((p) => adminOnlyGroups.has(p.group));
    expect(pages.length).toBeGreaterThan(0);
    for (const p of pages) {
      const seg = p.path.split('/')[1]!;
      expect(prefixes.includes(`/${seg}`) || platformOnlyFirstSegments.has(seg), `${p.key} ${p.path}`).toBe(true);
    }
  });
  it('对账来源不是空的：nodes 被 platform 模板覆盖，ops 需要登记', () => {
    expect(platformOnlyFirstSegments.has('nodes')).toBe(true);
    expect(platformOnlyFirstSegments.has('ops')).toBe(false);
    expect(prefixes).toContain('/ops');
  });
});
