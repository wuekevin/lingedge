// Admin 真实生产构建的反向检查自测（Story 1.38 AC5、ADR-014 §5）：用真实 vite 构建 frontend/admin，
// 证明 ① 生产产物不含 mock 入口/e2e 夹具 ② 不含 Console 专属 realm 常量 ③ 同一份代码按 e2e 模式构建会被 mock 检查检出 ④ 往真实产物里放进 Console 常量会被隔离检查检出。
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { build } from 'vite';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { checkIsolation } from './lib/isolation.mjs';
import { checkDistForMock } from './lib/mock-check.mjs';
import { loadOpenapi, platformManifest } from './lib/openapi.mjs';
import { FRONTEND_ROOT, OPENAPI_YAML } from './lib/paths.mjs';

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'le-admin-build-'));
afterAll(() => fs.rmSync(tmp, { recursive: true, force: true }));
const manifest = platformManifest(loadOpenapi(OPENAPI_YAML));
const adminPrefixes = JSON.parse(fs.readFileSync(path.join(FRONTEND_ROOT, 'isolation/admin-route-prefixes.json'), 'utf8')).prefixes;
const root = path.join(FRONTEND_ROOT, 'admin');

async function buildAdmin(mode, name) {
  const outDir = path.join(tmp, name);
  await build({ root, mode, logLevel: 'silent', build: { outDir, emptyOutDir: true } });
  return outDir;
}
const textOf = (dir) => {
  let s = '';
  const walk = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (/\.(js|css|html)$/.test(e.name)) s += fs.readFileSync(p, 'utf8');
    }
  };
  walk(dir);
  return s;
};

describe('Admin 真实构建 × 反向检查', () => {
  let prod;
  let e2e;
  beforeAll(async () => {
    prod = await buildAdmin('production', 'prod');
    e2e = await buildAdmin('e2e', 'e2e');
  }, 120000);

  it('生产构建：不含 mock 入口、夹具页面、Console realm 常量；隔离与 mock 检查通过', () => {
    expect(checkDistForMock(prod).violations).toEqual([]);
    expect(checkIsolation({ distDir: prod, role: 'admin', manifest, adminPrefixes }).violations).toEqual([]);
    const txt = textOf(prod);
    expect(txt).not.toContain('le_console_sid');
    expect(txt).not.toContain('LE_MOCK_ENTRY');
    expect(txt).not.toContain('E2E fixture');
    expect(fs.readdirSync(prod, { recursive: true }).join('\n')).not.toMatch(/mock|e2e/i);
  });

  it('同一份代码按 e2e 模式构建：夹具被打进产物，mock 检查必须检出（证明检查对真实 Admin 构建有效）', () => {
    expect(textOf(e2e)).toContain('LE_MOCK_ENTRY');
    expect(checkDistForMock(e2e).violations.some((v) => v.includes('LE_MOCK_ENTRY'))).toBe(true);
  });

  it('往真实 Admin 产物里放进 Console realm 常量 → 隔离检查检出；Admin 自己的会话常量不触发', () => {
    const dir = path.join(tmp, 'tainted');
    fs.cpSync(prod, dir, { recursive: true });
    fs.writeFileSync(path.join(dir, 'assets', 'extra.js'), 'export const c = "le_console_sid";\n');
    const v = checkIsolation({ distDir: dir, role: 'admin', manifest, adminPrefixes }).violations;
    expect(v.map((x) => x.what).join()).toContain('le_console_sid');
    fs.writeFileSync(path.join(dir, 'assets', 'extra.js'), 'export const c = "le_admin_sid";\n');
    expect(checkIsolation({ distDir: dir, role: 'admin', manifest, adminPrefixes }).violations).toEqual([]);
  });

  it('生产产物使用同源 /api/v1 与 Cookie 会话：不含 localStorage/sessionStorage 里存令牌的痕迹字段（会话令牌 HttpOnly，脚本不接触）', () => {
    const txt = textOf(prod);
    expect(txt).toContain('/api/v1');
    expect(txt).toContain('X-CSRF-Token');
    expect(txt).not.toMatch(/localStorage\.setItem\(["'](token|csrf|session)/i);
  });
});
