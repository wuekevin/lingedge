import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { checkNoCommandEntry } from './lib/no-command-entry.mjs';
import { FRONTEND_ROOT } from './lib/paths.mjs';

const dirs = [];
afterEach(() => {
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});
function tree(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'le-nocmd-'));
  dirs.push(root);
  for (const [rel, content] of Object.entries(files)) {
    const f = path.join(root, rel);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, typeof content === 'string' ? content : JSON.stringify(content));
  }
  return root;
}
const ok = { 'admin/src/locales/zh-CN.json': { a: { b: '查看审计日志' } }, 'admin/src/locales/en.json': { a: { b: 'View audit log' } }, 'admin/src/registry/pages.ts': "export const p = [{ key: 'ops.audit', path: '/ops/audit' }];", 'admin/package.json': { dependencies: { vue: '1.0.0' } } };
const run = (over) => checkNoCommandEntry(tree({ ...ok, ...over }));

describe('UX-27 静态检查：没有「执行命令」入口', () => {
  it('真实仓库：admin / console 的词条、页面注册表、依赖都干净，且确实扫描了内容', () => {
    const r = checkNoCommandEntry(FRONTEND_ROOT);
    expect(r.violations).toEqual([]);
    expect(r.scanned).toBeGreaterThanOrEqual(5);
  });
  it('正向：说明性文字（命令行指引、CLI、Console 一词）不误报', () => {
    const r = run({
      'admin/src/locales/zh-CN.json': { a: '升级由命令行工具 lingedge 执行，网页只显示当前版本与指引', b: 'Console 入口网址' },
      'admin/src/locales/en.json': { a: 'Upgrades are performed by the lingedge command line; this page only shows versions', b: 'Console URL' },
    });
    expect(r.violations).toEqual([]);
  });
  it.each([
    ['中文：执行命令', { 'admin/src/locales/zh-CN.json': { x: '在这里执行命令' } }],
    ['中文：运行脚本', { 'admin/src/locales/zh-CN.json': { x: { y: '运行脚本' } } }],
    ['中文：网页终端', { 'admin/src/locales/zh-CN.json': { x: '打开网页终端' } }],
    ['中文：远程 Shell', { 'admin/src/locales/zh-CN.json': { x: '远程 Shell' } }],
    ['英文：run command', { 'admin/src/locales/en.json': { x: 'Run command on node' } }],
    ['英文：execute arbitrary script', { 'admin/src/locales/en.json': { x: 'Execute an arbitrary script' } }],
    ['英文：web terminal', { 'console/src/locales/en.json': { x: 'Open web terminal' } }],
    ['console 的词条同样检查', { 'console/src/locales/zh-CN.json': { x: '执行命令' } }],
  ])('词条：%s', (_n, files) => {
    const r = run(files);
    expect(r.violations.map((v) => v.rule)).toContain('no-command-entry');
  });
  it.each([
    ["key: 'ops.exec'", "export const p = [{ key: 'ops.exec', path: '/ops/tools' }];"],
    ["path: '/nodes/terminal'", "export const p = [{ key: 'nodes.x', path: '/nodes/terminal' }];"],
    ["path: '/ops/run-command'", "export const p = [{ key: 'ops.x', path: '/ops/run-command' }];"],
    ["path: '/settings/shell'", "export const p = [{ key: 'a', path: '/settings/shell' }];"],
    ["key: 'ops.scripts'", "export const p = [{ key: 'ops.scripts', path: '/ops/s' }];"],
  ])('页面注册表：%s', (_n, src) => {
    const r = run({ 'admin/src/registry/pages.ts': src });
    expect(r.violations.some((v) => v.file.endsWith('pages.ts'))).toBe(true);
  });
  it('页面注册表：切分按词段，不误伤包含这些字母的正常词（description、scripture 不是 script 段）', () => {
    const r = run({ 'admin/src/registry/pages.ts': "export const p = [{ key: 'a.description', path: '/ops/shelly' }, { key: 'a.transcript', path: '/ops/commands-log-x' }];" });
    expect(r.violations.filter((v) => v.message.includes('/ops/shelly'))).toEqual([]);
  });
  it.each(['xterm', '@xterm/xterm', 'ssh2', 'node-pty', 'webssh2'])('依赖：%s', (dep) => {
    const r = run({ 'admin/package.json': { dependencies: { [dep]: '1.0.0' } } });
    expect(r.violations.some((v) => v.message.includes(dep))).toBe(true);
  });
});
