// OpenAPI 元数据读取：x-tenant-scope 清单（构建隔离）与 x-permissions 集合（shared 权限常量守卫）。
import fs from 'node:fs';
import YAML from 'yaml';
import { bodyPattern } from './isolation.mjs';

const METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];

const cache = new Map();
export function loadOpenapi(file) {
  const st = fs.statSync(file);
  const key = `${file}:${st.mtimeMs}:${st.size}`;
  if (!cache.has(key)) cache.set(key, YAML.parse(fs.readFileSync(file, 'utf8')));
  return cache.get(key);
}

/** @returns {Array<{path:string, method:string, operationId:string, scope:string, permissions:string[]}>} */
export function listOperations(doc) {
  const ops = [];
  for (const [p, item] of Object.entries(doc.paths ?? {})) {
    for (const m of METHODS) {
      const op = item?.[m];
      if (!op) continue;
      ops.push({
        path: p,
        method: m,
        operationId: op.operationId ?? `${m} ${p}`,
        scope: op['x-tenant-scope'],
        permissions: op['x-permissions'] ?? [],
      });
    }
  }
  return ops;
}

/** 路径模板的“形状”：把 {param} 统一成 {}，用于判断两个 operation 的路径是否等价。 */
export const shapeOf = (tpl) => tpl.replace(/\{[^}/]+\}/g, '{}');

/**
 * 构建隔离清单（IMP-14、ADR-014 §5）：
 * - platformOnly：只有 platform operation 使用的路径模板 —— Console 产物中**不得出现**（按模板精确匹配）。
 * - shared：同一路径形状上既有 platform 又有 tenant/either/public operation（例如同路径不同方法）——
 *   字符串层面无法区分，不进入 grep 清单，单列报告（授权靠服务端，ADR-014 §4）。
 * - consoleAllowed：非 platform 的路径形状（用于对 platformOnly 做冲突检查）。
 */
export function platformManifest(doc) {
  const ops = listOperations(doc);
  const bad = ops.filter((o) => !['platform', 'tenant', 'either', 'public'].includes(o.scope));
  if (bad.length > 0) {
    throw new Error(`存在缺少/非法 x-tenant-scope 的 operation：${bad.map((o) => o.operationId).join(', ')}`);
  }
  const platformShapes = new Map(); // shape -> {templates:Set, ops:[]}
  const otherShapes = new Set();
  for (const o of ops) {
    const s = shapeOf(o.path);
    if (o.scope === 'platform') {
      if (!platformShapes.has(s)) platformShapes.set(s, { templates: new Set(), ops: [] });
      const e = platformShapes.get(s);
      e.templates.add(o.path);
      e.ops.push(o.operationId);
    } else {
      otherShapes.add(s);
    }
  }
  // 非 platform 的全部路径模板：用于识别“更具体的静态兄弟路径”（如 /tenants/current 之于 /tenants/{tenantId}）。
  const otherPaths = [...new Set(ops.filter((o) => o.scope !== 'platform').map((o) => o.path))];
  const excludesFor = (tpl) => {
    if (!tpl.includes('{')) return [];
    const re = new RegExp(`^${bodyPattern(tpl)}$`);
    return otherPaths.filter((p) => p !== tpl && re.test(p));
  };
  const platformOnly = [];
  const shared = [];
  for (const [shape, e] of platformShapes) {
    if (otherShapes.has(shape)) shared.push({ shape, templates: [...e.templates], operations: e.ops });
    else for (const tpl of e.templates) platformOnly.push({ template: tpl, operations: e.ops, excludes: excludesFor(tpl) });
  }
  platformOnly.sort((a, b) => a.template.localeCompare(b.template));
  return {
    platformOperationCount: ops.filter((o) => o.scope === 'platform').length,
    totalOperationCount: ops.length,
    platformOnly,
    shared,
  };
}

/** 全部权限串（x-permissions 的并集）。 */
export function allPermissions(doc) {
  return new Set(listOperations(doc).flatMap((o) => o.permissions));
}

/** 全部路径模板（含 /api/v1 前缀的形式也由调用方处理）。 */
export function allPathTemplates(doc) {
  return Object.keys(doc.paths ?? {});
}
