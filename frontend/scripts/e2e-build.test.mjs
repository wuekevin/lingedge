// 端到端自测：用真实的 vite 生产构建产出“故意违规/合规”的 Console 产物，再跑隔离与 mock 检查，
// 证明检查对真实打包结果（压缩、模板字面量、tree-shaking 之后）同样有效，而不只是对手写字符串有效。
import fs from 'node:fs';
import path from 'node:path';
import { build } from 'vite';
import { afterAll, describe, expect, it } from 'vitest';
import { checkIsolation } from './lib/isolation.mjs';
import { checkDistForMock } from './lib/mock-check.mjs';
import { loadOpenapi, platformManifest } from './lib/openapi.mjs';
import { FRONTEND_ROOT, OPENAPI_YAML } from './lib/paths.mjs';

// 放在 node_modules 下：能解析依赖，且被 lint-guards/i18n/deps 检查的目录遍历跳过。
const base = path.join(FRONTEND_ROOT, 'node_modules', `.le-e2e-${process.pid}`);
afterAll(() => fs.rmSync(base, { recursive: true, force: true }));

const manifest = platformManifest(loadOpenapi(OPENAPI_YAML));
const adminPrefixes = ['/admin'];

async function buildApp(name, files, mode = 'production') {
  const root = path.join(base, name);
  for (const [rel, content] of Object.entries(files)) {
    const f = path.join(root, rel);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, content);
  }
  const outDir = path.join(root, 'dist');
  await build({ root, mode, logLevel: 'silent', configFile: false, build: { outDir, emptyOutDir: true, minify: true } });
  return outDir;
}
const html = '<!doctype html><html><body><script type="module" src="/src/main.ts"></script></body></html>';
const iso = (dist) => checkIsolation({ distDir: dist, role: 'console', manifest, adminPrefixes }).violations;

describe('真实 vite 构建 × 隔离检查', () => {
  it('Console 代码里调用 platform 端点（模板字面量，经压缩）→ 检出', async () => {
    const dist = await buildApp('iso-bad', {
      'index.html': html,
      'src/main.ts': 'const id = String(Date.now());\nfetch(`/api/v1/nodes/${id}`).then(() => fetch(`/api/v1/clusters/${id}/revisions`));\n',
    });
    const v = iso(dist);
    expect(v.map((x) => x.what).join('\n')).toContain('/nodes/{nodeId}');
    expect(v.map((x) => x.what).join('\n')).toContain('/clusters/{clusterId}/revisions');
  });
  it('Console 合法端点（/audit-logs、以 /clusters 结尾的租户路径）→ 不误报', async () => {
    const dist = await buildApp('iso-good', {
      'index.html': html,
      'src/main.ts': 'const id = String(Date.now());\nfetch("/api/v1/audit-logs");\nfetch(`/api/v1/sites/${id}/clusters`);\nfetch(`/api/v1/sites/${id}/nodes`);\nfetch("/api/v1/auth/me");\n',
    });
    expect(iso(dist)).toEqual([]);
  });
  it('Console 路由里带 Admin 路由前缀 → 检出', async () => {
    const dist = await buildApp('iso-admin-route', { 'index.html': html, 'src/main.ts': 'console.log([{ path: "/admin/users" }]);\n' });
    expect(iso(dist).some((x) => x.what.includes('Admin 路由前缀'))).toBe(true);
  });
});

describe('真实 vite 构建 × mock 检查', () => {
  // 约定：标记必须是“被执行代码里的字符串字面量”——只导出常量会被摇树掉，标记随之消失（本用例组第 3 条验证过这一点）。
  const mockModule = 'export const boot = () => console.warn("LE_MOCK_ENTRY active");\n';
  it('mock 入口受 import.meta.env.MODE 保护的动态 import：生产构建被摇树移除 → 通过', async () => {
    const dist = await buildApp('mock-guarded', {
      'index.html': html,
      'src/mocks/boot.ts': mockModule,
      'src/main.ts': 'if (import.meta.env.MODE === "mock") { void import("./mocks/boot"); }\nconsole.log("app");\n',
    });
    expect(checkDistForMock(dist).violations).toEqual([]);
  });
  it('同一份代码按 mock 模式构建会被检出（证明检查不是恒通过）', async () => {
    const dist = await buildApp(
      'mock-mode',
      { 'index.html': html, 'src/mocks/boot.ts': mockModule, 'src/main.ts': 'if (import.meta.env.MODE === "mock") { void import("./mocks/boot"); }\nconsole.log("app");\n' },
      'mock',
    );
    expect(checkDistForMock(dist).violations.some((v) => v.includes('LE_MOCK_ENTRY'))).toBe(true);
  });
  it('静态 import mock 入口：生产构建也带进产物 → 检出', async () => {
    const dist = await buildApp('mock-static', {
      'index.html': html,
      'src/mocks/boot.ts': mockModule,
      'src/main.ts': 'import { boot } from "./mocks/boot";\nboot();\n',
    });
    expect(checkDistForMock(dist).violations.some((v) => v.includes('LE_MOCK_ENTRY'))).toBe(true);
  });
});
