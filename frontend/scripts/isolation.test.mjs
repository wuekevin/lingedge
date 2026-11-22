import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { checkIsolation, prefixRegex, templateRegex } from './lib/isolation.mjs';
import { checkDistForMock, checkSourcesForMock } from './lib/mock-check.mjs';
import { listOperations, loadOpenapi, platformManifest, shapeOf } from './lib/openapi.mjs';
import { FRONTEND_ROOT, OPENAPI_YAML } from './lib/paths.mjs';

const dirs = [];
afterEach(() => {
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});
function tree(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'le-iso-'));
  dirs.push(root);
  for (const [rel, content] of Object.entries(files)) {
    const f = path.join(root, rel);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, content);
  }
  return root;
}
const doc = loadOpenapi(OPENAPI_YAML);
const manifest = platformManifest(doc);
const adminPrefixes = JSON.parse(fs.readFileSync(path.join(FRONTEND_ROOT, 'isolation/admin-route-prefixes.json'), 'utf8')).prefixes;
const consoleCheck = (files) => checkIsolation({ distDir: tree(files), role: 'console', manifest, adminPrefixes });

// 一个只含两个 platform 模板的小清单：精确语义的单元用例
const tiny = { platformOnly: [{ template: '/clusters', operations: ['listClusters'] }, { template: '/nodes/{nodeId}', operations: ['getNode'] }, { template: '/clusters/{clusterId}/revisions', operations: ['listRevisions'] }] };
const tinyCheck = (js) => checkIsolation({ distDir: tree({ 'assets/index.js': js, 'index.html': '<html></html>' }), role: 'console', manifest: tiny, adminPrefixes: [] }).violations.map((v) => v.match);

describe('清单由 openapi.yaml 自动生成', () => {
  it('platform operation 全部进入清单（数量与元数据一致），无手写', () => {
    const ops = listOperations(doc);
    const platform = ops.filter((o) => o.scope === 'platform');
    expect(manifest.platformOperationCount).toBe(platform.length);
    const covered = new Set([...manifest.platformOnly.map((t) => t.template), ...manifest.shared.flatMap((s) => s.templates)]);
    for (const o of platform) expect(covered.has(o.path), o.path).toBe(true);
    expect(manifest.platformOnly.length).toBeGreaterThan(50);
  });
  it('缺少/非法 x-tenant-scope 时生成失败（防止静默漏掉）', () => {
    const bad = { paths: { '/x': { get: { operationId: 'x' } } } };
    expect(() => platformManifest(bad)).toThrow(/x-tenant-scope/);
  });
  it('同一路径形状上混有非 platform operation 时不进 grep 清单并单列（避免误报）', () => {
    const d = { paths: { '/a/{id}': { get: { operationId: 'a', 'x-tenant-scope': 'platform' } }, '/a/{x}': { put: { operationId: 'b', 'x-tenant-scope': 'tenant' } }, '/b': { get: { operationId: 'c', 'x-tenant-scope': 'platform' } } } };
    const m = platformManifest(d);
    expect(m.platformOnly.map((t) => t.template)).toEqual(['/b']);
    expect(m.shared).toHaveLength(1);
    expect(shapeOf('/a/{id}/b/{c}')).toBe('/a/{}/b/{}');
  });
});

describe('路径模板精确匹配（IMP-14：不用 /clusters 之类子串）', () => {
  it.each([
    ['"/clusters"', ['"/clusters"'].map(() => '/clusters')],
    ["`/clusters`", ['/clusters']],
    ['fetch("/api/v1/clusters")', ['/clusters']],
    ['baseURL+"/api/v1/clusters?page=1"', ['/clusters']],
    ['get("/nodes/abc")', ['/nodes/abc']],
    ['get(`/nodes/${e.id}`)', ['/nodes/${e.id}']],
    ['get("/nodes/{nodeId}")', ['/nodes/{nodeId}']],
    ['u=`/api/v1/clusters/${a}/revisions`', ['/clusters/${a}/revisions']],
  ])('命中：%s', (js, want) => {
    expect(tinyCheck(js)).toEqual(want);
  });

  it.each([
    ['租户路径以 /clusters 结尾', 'get(`/sites/${id}/clusters`)'],
    ['租户路径以 /nodes 结尾', 'get(`/sites/${id}/nodes`)'],
    ['字母之后的 /clusters', 'x="my/clusters"'],
    ['更长的合法路径 /clusters-info', 'get("/clusters-info")'],
    ['更长的路径 /nodesx', 'get("/nodesx")'],
    ['/audit-logs 等 Console 合法路径', 'get("/audit-logs"); get(`/api/v1/audit-logs/${id}`)'],
    ['无关文本里的词 clusters / nodes', 'const a="clusters", b="nodes"; // 无斜杠'],
    ['/nodes/ 后跟子路径（不是 /nodes/{nodeId}）', 'get(`/nodes/${id}/status`)'],
  ])('不误报：%s', (_n, js) => {
    expect(tinyCheck(js)).toEqual([]);
  });

  it('templateRegex / prefixRegex 的边界：/admin 命中 /admin、/admin/x，不命中 /administrator', () => {
    const re = prefixRegex('/admin');
    const hit = (s) => [...s.matchAll(re)].length;
    expect(hit('"/admin/users"')).toBe(1);
    expect(hit('"/admin"')).toBe(1);
    expect(hit('u="/api/v1/admin/audit-logs"')).toBe(1);
    expect(hit('"/administrator"')).toBe(0);
    expect(hit('"/x/admin/y"')).toBe(0);
    expect(templateRegex('/a/{b}').test('"/a/1"')).toBe(true);
  });
});

describe('静态兄弟路径优先于参数段（/tenants/current vs /tenants/{tenantId}）', () => {
  const m = { platformOnly: [{ template: '/tenants/{tenantId}', operations: ['getTenant'], excludes: ['/tenants/current'] }] };
  const run = (js) => checkIsolation({ distDir: tree({ 'a.js': js, 'index.html': '<html></html>' }), role: 'console', manifest: m, adminPrefixes: [] }).violations.map((v) => v.match);
  it('非 platform 的静态路径 /tenants/current 不是该模板的实例', () => {
    expect(run('get("/tenants/current")')).toEqual([]);
  });
  it('其他具体值仍命中', () => {
    expect(run('get("/tenants/abc")')).toEqual(['/tenants/abc']);
    expect(run('get(`/tenants/${id}`)')).toEqual(['/tenants/${id}']);
  });
  it('真实清单里存在此类排除项（由 openapi 自动算出，非手写）', () => {
    expect(manifest.platformOnly.some((t) => t.excludes.length > 0)).toBe(true);
  });
});

describe('Console 产物隔离（对真实 openapi 清单）', () => {
  const toLiteral = (p) => p.replace(/\{[^}]+\}/g, '${e}');

  it('每个 platform 路径模板出现在 Console 产物里都会被检出（含 /api/v1 前缀与反引号两种写法）', () => {
    for (const t of manifest.platformOnly) {
      const js = `fetch(\`${toLiteral(t.template)}\`);fetch("/api/v1${t.template}");`;
      const r = consoleCheck({ 'assets/index.js': js, 'index.html': '<html></html>' });
      expect(r.violations.length, t.template).toBeGreaterThanOrEqual(1);
    }
  });

  it('每个非 platform（tenant/either/public）路径模板出现在 Console 产物里都不会误报', () => {
    const platformShapes = new Set(manifest.platformOnly.map((t) => shapeOf(t.template)));
    const tenantPaths = [...new Set(listOperations(doc).filter((o) => o.scope !== 'platform').map((o) => o.path))].filter((p) => !platformShapes.has(shapeOf(p)));
    expect(tenantPaths.length).toBeGreaterThan(50);
    const js = tenantPaths.map((p) => `f(\`${toLiteral(p)}\`);f("/api/v1${p}");`).join('\n');
    const r = consoleCheck({ 'assets/index.js': js, 'index.html': '<html></html>' });
    expect(r.violations).toEqual([]);
  });

  it('Admin 路由前缀与 Admin realm 常量出现在 Console 产物里即失败', () => {
    const r = consoleCheck({ 'assets/index.js': 'route("/admin/users");const c="le_admin_sid";', 'index.html': '<html></html>' });
    expect(r.violations.map((v) => v.what).join('|')).toMatch(/Admin 路由前缀 \/admin/);
    expect(r.violations.map((v) => v.what).join('|')).toMatch(/Admin realm 常量/);
  });

  it('违规信息带文件与行号；css/html/map 也被扫描', () => {
    const r = consoleCheck({ 'assets/a.js': 'x=1;\ny="/api/v1/nodes";', 'assets/a.css': '/* "/clusters" */', 'index.html': '<script>g("/nodes")</script>', 'assets/a.js.map': '{"sourcesContent":["get(\\"/clusters\\")"]}' });
    const js = r.violations.find((v) => v.file.endsWith('a.js'));
    expect(js).toMatchObject({ line: 2, match: '/nodes' });
    expect(r.violations.map((v) => path.extname(v.file)).sort()).toEqual(['.css', '.html', '.js', '.map']);
  });

  it('Admin 产物反向：含 Console realm 常量失败；含 platform 端点则是合法的', () => {
    const bad = checkIsolation({ distDir: tree({ 'a.js': 'c="le_console_sid"', 'index.html': '<html></html>' }), role: 'admin', manifest, adminPrefixes });
    expect(bad.violations).toHaveLength(1);
    const ok = checkIsolation({ distDir: tree({ 'a.js': 'f("/clusters");f("/admin/users")', 'index.html': '<html></html>' }), role: 'admin', manifest, adminPrefixes });
    expect(ok.violations).toEqual([]);
  });

  it('产物目录不存在/没有 js 或 html → 抛错（不空转通过）', () => {
    expect(() => checkIsolation({ distDir: path.join(os.tmpdir(), 'le-no-such-dist'), role: 'console', manifest, adminPrefixes })).toThrow(/不存在/);
    expect(() => checkIsolation({ distDir: tree({ 'a.png': 'x' }), role: 'console', manifest, adminPrefixes })).toThrow(/没有可扫描/);
  });
});

describe('mock 检查（生产构建不含 mock 入口）', () => {
  const dist = (files) => checkDistForMock(tree({ 'index.html': '<html></html>', 'assets/index.js': 'console.log(1)', ...files })).violations;

  it('干净产物通过', () => {
    expect(dist({})).toEqual([]);
  });
  it.each([
    ['mock 标记常量 LE_MOCK_ENTRY', { 'assets/index.js': 'const LE_MOCK_ENTRY=1' }],
    ['msw service worker 文件', { 'mockServiceWorker.js': '//' }],
    ['mocks/ 目录', { 'assets/mocks/handlers.js': 'x' }],
    ['*.mock.js 文件', { 'assets/users.mock.js': 'x' }],
    ['msw setupWorker', { 'assets/index.js': 'import("msw/browser");setupWorker(a)' }],
    ['编译期开关残留 __LE_MOCK__', { 'assets/index.js': 'if(__LE_MOCK__){}' }],
    ['VITE_ENABLE_MOCK 被内联', { 'assets/index.js': '{"VITE_ENABLE_MOCK":"true"}' }],
    ['sourcemap 引用 mock 源文件', { 'assets/index.js.map': JSON.stringify({ version: 3, sources: ['../src/mocks/handlers.ts'] }) }],
  ])('负向：%s', (_n, files) => {
    expect(dist(files).length).toBeGreaterThan(0);
  });
  it('目录缺失抛错', () => {
    expect(() => checkDistForMock(path.join(os.tmpdir(), 'le-no-such'))).toThrow(/不存在/);
  });

  it('源码：静态 import mock 失败；动态 import() 与测试文件允许', () => {
    const root = tree({
      'admin/src/main.ts': "import { setup } from './mocks/boot';\nimport x from './api.mock';\nexport { y } from 'msw/browser';\n",
      'console/src/main.ts': "if (import.meta.env.MODE === 'mock') { await import('./mocks/boot'); }\n",
      'console/src/a.test.ts': "import { h } from './mocks/handlers';\n",
      'console/src/mocks/boot.ts': "import { z } from './handlers';\n",
    });
    const v = checkSourcesForMock([path.join(root, 'admin'), path.join(root, 'console')]).violations;
    expect(v.filter((m) => m.startsWith('admin/'))).toHaveLength(3);
    expect(v.filter((m) => m.startsWith('console/'))).toEqual([]);
  });
  it('环境文件：生产相关 .env 打开 mock 开关失败；development/mock 模式允许', () => {
    const root = tree({
      'admin/.env.production': 'VITE_ENABLE_MOCK=true\n',
      'console/.env': 'VITE_USE_MOCK=1\n',
      'website/.env.development': 'VITE_ENABLE_MOCK=true\n',
      'admin/.env.mock': 'VITE_ENABLE_MOCK=true\n',
    });
    const v = checkSourcesForMock(['admin', 'console', 'website'].map((a) => path.join(root, a))).violations;
    expect(v.some((m) => m.includes('admin/.env.production'))).toBe(true);
    expect(v.some((m) => m.includes('console/.env'))).toBe(true);
    expect(v.some((m) => m.includes('.env.development') || m.includes('.env.mock'))).toBe(false);
  });
});
