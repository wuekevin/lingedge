import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { afterEach, describe, expect, it } from 'vitest';
import { diffTrees, syncTrees } from './lib/embed-sync.mjs';
import { checkIsolation } from './lib/isolation.mjs';
import { FRONTEND_ROOT, SETUP_DIST } from './lib/paths.mjs';

const dirs = [];
afterEach(() => {
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});
function tmp() {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), 'le-embed-'));
  dirs.push(d);
  return d;
}
function put(root, files) {
  for (const [rel, content] of Object.entries(files)) {
    const f = path.join(root, rel);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, content);
  }
  return root;
}
const GOOD = { 'index.html': '<html></html>', 'assets/index-abc.js': 'console.log(1)', 'assets/style-abc.css': 'a{}' };

describe('安装向导嵌入同步（漂移/缺失检查）', () => {
  it('同步后逐字节一致；再次比对通过', () => {
    const src = put(tmp(), GOOD);
    const dst = path.join(tmp(), 'webdist');
    syncTrees(src, dst);
    const r = diffTrees(src, dst);
    expect(r.ok).toBe(true);
    expect(r.files).toBe(3);
  });
  it('嵌入目录缺文件、多文件、内容被改，三种漂移都被报告', () => {
    const src = put(tmp(), GOOD);
    const dst = path.join(tmp(), 'webdist');
    syncTrees(src, dst);
    fs.rmSync(path.join(dst, 'assets/style-abc.css'));
    fs.writeFileSync(path.join(dst, 'assets/old-9.js'), 'x');
    fs.writeFileSync(path.join(dst, 'index.html'), '<html>tampered</html>');
    const r = diffTrees(src, dst);
    expect(r.ok).toBe(false);
    expect(r.missing).toEqual(['assets/style-abc.css']);
    expect(r.extra).toEqual(['assets/old-9.js']);
    expect(r.changed).toEqual(['index.html']);
  });
  it('嵌入目录整体缺失 = 全部缺失（不是通过）', () => {
    const src = put(tmp(), GOOD);
    const r = diffTrees(src, path.join(tmp(), 'nope'));
    expect(r.ok).toBe(false);
    expect(r.missing).toHaveLength(3);
  });
  it('产物目录缺失或不完整 → 抛错（不空转通过）', () => {
    expect(() => diffTrees(path.join(tmp(), 'nope'), tmp())).toThrow(/不存在/);
    expect(() => diffTrees(put(tmp(), { 'index.html': 'x' }), tmp())).toThrow(/不完整/);
    expect(() => syncTrees(put(tmp(), { 'assets/a.js': 'x' }), path.join(tmp(), 'd'))).toThrow(/不完整/);
  });
  it('同步会清掉旧哈希文件（无残留）', () => {
    const src = put(tmp(), GOOD);
    const dst = put(tmp(), { 'assets/stale-1.js': 'old' });
    syncTrees(src, dst);
    expect(fs.existsSync(path.join(dst, 'assets/stale-1.js'))).toBe(false);
  });
  it('脚本 --check：产物缺失退出码 2', () => {
    const r = spawnSync(process.execPath, ['scripts/sync-setup-embed.mjs', '--check'], { cwd: FRONTEND_ROOT, encoding: 'utf8', env: { ...process.env } });
    // 本仓库正常情况下 dist/setup 存在；若不存在则必须是 2 而不是 0。
    if (!fs.existsSync(SETUP_DIST)) expect(r.status).toBe(2);
    else expect([0, 1]).toContain(r.status);
  });
});

describe('安装向导产物隔离（不依赖 Admin Web）', () => {
  const manifest = { platformOnly: [{ template: '/clusters', operations: ['listClusters'] }] };
  const check = (files) => checkIsolation({ distDir: put(tmp(), { 'index.html': '<html></html>', ...files }), role: 'setup', manifest, adminPrefixes: ['/admin'] }).violations.map((v) => v.what);
  it('合规产物通过（只含 /api/v1/public/setup/*）', () => {
    expect(check({ 'assets/a.js': 'fetch("/api/v1/public/setup/status")' })).toEqual([]);
  });
  it('含 Admin 路由前缀、platform 端点、任一 realm 会话常量即失败', () => {
    expect(check({ 'assets/a.js': 'location="/admin/login"' }).join()).toMatch(/Admin 路由前缀/);
    expect(check({ 'assets/a.js': 'fetch("/api/v1/clusters")' }).join()).toMatch(/platform 端点/);
    expect(check({ 'assets/a.js': 'document.cookie="le_admin_sid"' }).join()).toMatch(/realm/);
    expect(check({ 'assets/a.js': 'x="le_console_sid"' }).join()).toMatch(/realm/);
  });
});
