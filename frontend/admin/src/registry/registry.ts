import { GROUP_KEYS, type PageDef } from './types';

export interface Registry {
  pages: readonly PageDef[];
  byKey(key: string): PageDef | undefined;
}

/** 校验并建立注册表：键/路径唯一、路径合法、权限串形态、别名指向存在的规范页面。有问题直接抛错（构建/启动即暴露，而不是运行时悄悄丢页面）。 */
export function createRegistry(pages: readonly PageDef[]): Registry {
  const keys = new Map<string, PageDef>();
  const paths = new Set<string>();
  const groups = new Set<string>(GROUP_KEYS);
  const reserved = new Set(['/', '/login', '/403', '/404', '/change-password']);
  for (const p of pages) {
    if (keys.has(p.key)) throw new Error(`registry: duplicate page key ${p.key}`);
    if (!p.path.startsWith('/') || reserved.has(p.path)) throw new Error(`registry: invalid or reserved path ${p.path} (${p.key})`);
    if (paths.has(p.path)) throw new Error(`registry: duplicate path ${p.path}`);
    if (!groups.has(p.group)) throw new Error(`registry: unknown group ${p.group} (${p.key})`);
    for (const perm of p.permissions) if (!/^[a-z][a-z0-9-]*(:[a-z0-9-]+)+$/.test(perm)) throw new Error(`registry: malformed permission ${perm} (${p.key})`);
    keys.set(p.key, p);
    paths.add(p.path);
  }
  for (const p of pages) if (p.aliasOf && !keys.has(p.aliasOf)) throw new Error(`registry: alias ${p.key} points to unknown page ${p.aliasOf}`);
  return { pages, byKey: (k) => keys.get(k) };
}
