// 构建隔离检查（FR-37、ADR-014 §5、IMP-14）：
//   Console 产物不得出现任何 x-tenant-scope: platform operation 的路径模板（精确匹配，{param} 视为单段通配），
//   也不得出现 Admin 路由前缀与 Admin realm 常量；Admin 产物反向不得含 Console realm 常量。
//   清单由 openapi.yaml 自动生成；不使用 /clusters、/nodes 之类的子串（会被 /sites/{siteId}/clusters 等租户路径误报）。
import fs from 'node:fs';
import path from 'node:path';

const TEXT_EXT = new Set(['.js', '.mjs', '.cjs', '.css', '.html', '.json', '.map', '.txt', '.svg', '.webmanifest', '.xml']);
const API_PREFIX = '/api/v1';
export const REALM = { admin: 'le_admin_sid', console: 'le_console_sid' };

const esc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// 一个路径参数段：模板字面量插值 ${…} 或不含分隔符的任意文本
const PARAM = String.raw`(?:\$\{[^}\`]*\}|[^/"'\`\s?#<>{}$\\]+)`;
// 左边界：紧跟在引号/反引号/括号/空白/等号/逗号/冒号/方括号之后，或紧跟在 /api/v1 之后（baseURL 前缀）。
// 因此 "/sites/${id}/clusters" 里的 "/clusters"（前面是 }）与 "/x/clusters"（前面是字母）都不会被当作 /clusters。
const LEFT = String.raw`(?:(?<=["'\`(\s=,:\[])|(?<=${esc(API_PREFIX)}))`;
const RIGHT = String.raw`(?![A-Za-z0-9_\-/.{$])`;

/** 路径模板 → 正则片段（{param} 视为单段通配，也接受原样的 {param} 文本）。 */
export function bodyPattern(template) {
  return template
    .split(/(\{[^}/]+\})/)
    .map((part) => (/^\{[^}/]+\}$/.test(part) ? `(?:${PARAM}|${esc(part)})` : esc(part)))
    .join('');
}

/**
 * 路径模板 → 精确匹配的正则。
 * excludes：同形状、但属于其他（非 platform）operation 的更具体路径（如静态的 /tenants/current 之于 /tenants/{tenantId}），
 * OpenAPI/路由里静态段优先于参数段，所以这些字面量不是该模板的实例，不应命中。
 */
export function templateRegex(template, excludes = []) {
  const neg = excludes.length > 0 ? `(?!(?:${excludes.map(bodyPattern).join('|')})${RIGHT})` : '';
  return new RegExp(`${LEFT}${neg}${bodyPattern(template)}${RIGHT}`, 'g');
}

/** 前缀（如 /admin）→ 作为独立路径起点出现的正则：/admin、/admin/…、/admin?… 均命中，/administrator 不命中。 */
export function prefixRegex(prefix) {
  return new RegExp(`${LEFT}${esc(prefix.replace(/\/$/, ''))}(?=/|["'\`?#\\s]|$)`, 'g');
}

function* textFiles(dir) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) yield* textFiles(p);
    else if (TEXT_EXT.has(path.extname(e.name))) yield p;
  }
}

function lineCol(text, index) {
  const before = text.slice(0, index);
  return before.split('\n').length;
}

/**
 * @param {{distDir:string, role:'console'|'admin'|'setup', manifest:{platformOnly:Array<{template:string,operations:string[]}>}, adminPrefixes:string[]}} args
 * @returns {{violations:Array<{file:string,line:number,what:string,match:string}>, scannedFiles:number, templates:number}}
 */
export function checkIsolation({ distDir, role, manifest, adminPrefixes }) {
  if (!fs.existsSync(distDir) || !fs.statSync(distDir).isDirectory()) throw new Error(`产物目录不存在：${distDir}（先构建；拒绝空转通过）`);
  const files = [...textFiles(distDir)];
  if (files.length === 0 || !files.some((f) => /\.(js|mjs|html)$/.test(f))) throw new Error(`产物目录没有可扫描的 js/html：${distDir}`);
  const violations = [];
  const rules = [];
  if (role === 'console') {
    for (const t of manifest.platformOnly) rules.push({ what: `platform 端点 ${t.template}（${t.operations.join(', ')}）`, re: templateRegex(t.template, t.excludes ?? []) });
    for (const p of adminPrefixes) rules.push({ what: `Admin 路由前缀 ${p}`, re: prefixRegex(p) });
    rules.push({ what: `Admin realm 常量 ${REALM.admin}`, re: new RegExp(esc(REALM.admin), 'g') });
  } else if (role === 'setup') {
    // 安装向导（Story 1.37）不依赖 Admin Web：产物里不得有任何 platform 端点模板、Admin 路由前缀或任一 realm 会话常量（向导没有会话，只有 setup token）。
    for (const t of manifest.platformOnly) rules.push({ what: `platform 端点 ${t.template}（${t.operations.join(', ')}）`, re: templateRegex(t.template, t.excludes ?? []) });
    for (const p of adminPrefixes) rules.push({ what: `Admin 路由前缀 ${p}`, re: prefixRegex(p) });
    for (const c of Object.values(REALM)) rules.push({ what: `realm 会话常量 ${c}`, re: new RegExp(esc(c), 'g') });
  } else {
    rules.push({ what: `Console realm 常量 ${REALM.console}`, re: new RegExp(esc(REALM.console), 'g') });
  }
  for (const f of files) {
    const text = fs.readFileSync(f, 'utf8');
    for (const r of rules) {
      r.re.lastIndex = 0;
      for (const m of text.matchAll(r.re)) {
        violations.push({ file: path.relative(distDir, f), line: lineCol(text, m.index), what: r.what, match: m[0] });
      }
    }
  }
  return { violations, scannedFiles: files.length, templates: role === 'console' || role === 'setup' ? manifest.platformOnly.length : 0 };
}
