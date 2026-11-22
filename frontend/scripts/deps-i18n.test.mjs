import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { checkDeps, specProblem } from './lib/deps-check.mjs';
import { checkI18n } from './lib/i18n-check.mjs';
import { FRONTEND_ROOT } from './lib/paths.mjs';

const dirs = [];
afterEach(() => {
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});
function tree(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'le-chk-'));
  dirs.push(root);
  for (const [rel, content] of Object.entries(files)) {
    const f = path.join(root, rel);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, typeof content === 'string' ? content : JSON.stringify(content, null, 2));
  }
  return root;
}

// ---------------- 依赖锁定 ----------------
const goodFiles = () => ({
  'package.json': { name: 'root', version: '0.1.0', private: true, packageManager: 'pnpm@12.18.0', devDependencies: { vue: '3.5.46' } },
  'pnpm-workspace.yaml': 'packages:\n  - app\n  - shared\nignoreScripts: true\n',
  '.npmrc': 'ignore-scripts=true\nsave-exact=true\n',
  'pnpm-lock.yaml': "lockfileVersion: '9.0'\n\nimporters:\n  .:\n    devDependencies:\n      vue:\n        specifier: 3.5.46\n        version: 3.5.46\n  app:\n    dependencies:\n      '@x/shared':\n        specifier: workspace:0.1.0\n        version: link:../shared\n  shared: {}\n",
  'app/package.json': { name: '@x/app', version: '0.1.0', dependencies: { '@x/shared': 'workspace:0.1.0', vue: '3.5.46' } },
  'shared/package.json': { name: '@x/shared', version: '0.1.0' },
});
const deps = (patch = {}) => {
  const files = { ...goodFiles(), ...patch };
  for (const [k, v] of Object.entries(files)) if (v === null) delete files[k];
  return checkDeps(tree(files)).errors;
};

describe('specProblem：精确版本判定', () => {
  it.each(['1.2.3', '0.0.0-rc.1', '10.0.1', 'workspace:0.1.0', 'npm:vue@3.5.46', '1.0.0-beta.2+build5'])('合格：%s', (s) => {
    expect(specProblem(s)).toBeNull();
  });
  it.each(['^1.2.3', '~1.2.3', '*', 'latest', 'next', '1.x', '1.2', '>=1.0.0', '1.2.3 - 2.0.0', '1.2.3 || 2.0.0', 'workspace:*', 'workspace:^', 'git+https://x/y.git', 'github:a/b', 'file:../x', 'link:../x', 'https://x/y.tgz', '', 'npm:vue@^3.0.0'])('不合格：%j', (s) => {
    expect(specProblem(s)).not.toBeNull();
  });
});

describe('依赖锁定检查', () => {
  it('合格的最小工作区通过', () => {
    expect(deps()).toEqual([]);
  });
  it('真实 frontend/ 通过', () => {
    expect(checkDeps(FRONTEND_ROOT).errors).toEqual([]);
  });
  it('负向：^ / ~ / latest / * 各自被检出，且点名依赖与字段', () => {
    const e = deps({ 'app/package.json': { name: '@x/app', version: '0.1.0', dependencies: { a: '^1.0.0', b: '~2.0.0', c: 'latest', d: '*' }, devDependencies: { e: '>=1.0.0' }, peerDependencies: { f: '1.x' } } });
    for (const n of ['a', 'b', 'c', 'd', 'e', 'f']) expect(e.some((m) => m.includes(`${n} 的版本不合格`)), n).toBe(true);
  });
  it('负向：overrides / catalog 里的浮动版本', () => {
    expect(deps({ 'package.json': { name: 'root', version: '0.1.0', packageManager: 'pnpm@12.18.0', pnpm: { overrides: { x: '^1.0.0' } } } }).some((m) => m.includes('pnpm.overrides'))).toBe(true);
    expect(deps({ 'pnpm-workspace.yaml': 'packages:\n  - app\n  - shared\nignoreScripts: true\ncatalog:\n  vue: ^3.5.0\n' }).some((m) => m.includes('catalog'))).toBe(true);
  });
  it('负向：packageManager 缺失 / 浮动', () => {
    expect(deps({ 'package.json': { name: 'root', version: '0.1.0' } }).some((m) => m.includes('packageManager'))).toBe(true);
    expect(deps({ 'package.json': { name: 'root', version: '0.1.0', packageManager: 'pnpm@^12.0.0' } }).some((m) => m.includes('packageManager'))).toBe(true);
    expect(deps({ 'package.json': { name: 'root', version: '0.1.0', packageManager: 'pnpm' } }).some((m) => m.includes('packageManager'))).toBe(true);
  });
  it('负向：.npmrc 缺失 / 未关闭脚本 / 显式打开脚本', () => {
    expect(deps({ '.npmrc': null }).some((m) => m.includes('.npmrc 缺失'))).toBe(true);
    expect(deps({ '.npmrc': 'save-exact=true\n' }).some((m) => m.includes('ignore-scripts=true'))).toBe(true);
    expect(deps({ '.npmrc': 'ignore-scripts=false\nsave-exact=true\n' }).some((m) => m.includes('ignore-scripts=true'))).toBe(true);
    expect(deps({ '.npmrc': '# ignore-scripts=true\nsave-exact=true\n' }).some((m) => m.includes('ignore-scripts=true'))).toBe(true);
  });
  it('负向：只有 .npmrc 关闭脚本、pnpm-workspace.yaml 没有 ignoreScripts（pnpm 12 下无效）→ 失败', () => {
    expect(deps({ 'pnpm-workspace.yaml': 'packages:\n  - app\n  - shared\n' }).some((m) => m.includes('ignoreScripts: true'))).toBe(true);
    expect(deps({ 'pnpm-workspace.yaml': 'packages:\n  - app\n  - shared\nignoreScripts: false\n' }).some((m) => m.includes('ignoreScripts: true'))).toBe(true);
    expect(deps({ 'pnpm-workspace.yaml': 'packages:\n  - app\n  - shared\nignore-scripts: true\n' }).some((m) => m.includes('ignoreScripts: true'))).toBe(true);
  });
  it('负向：构建脚本白名单 / 安装期生命周期脚本被拒', () => {
    expect(deps({ 'pnpm-workspace.yaml': 'packages:\n  - app\n  - shared\nignoreScripts: true\nonlyBuiltDependencies:\n  - esbuild\n' }).some((m) => m.includes('onlyBuiltDependencies'))).toBe(true);
    expect(deps({ 'app/package.json': { name: '@x/app', version: '0.1.0', scripts: { postinstall: 'node x.js' } } }).some((m) => m.includes('scripts.postinstall'))).toBe(true);
  });
  it('负向：锁文件缺失 / 锁文件 specifier 浮动', () => {
    expect(deps({ 'pnpm-lock.yaml': null }).some((m) => m.includes('pnpm-lock.yaml 缺失'))).toBe(true);
    expect(deps({ 'pnpm-lock.yaml': "lockfileVersion: '9.0'\nimporters:\n  .:\n    devDependencies:\n      vue:\n        specifier: ^3.5.0\n        version: 3.5.46\n  app: {}\n" }).some((m) => m.includes('specifier 不合格'))).toBe(true);
  });
  it('负向：孤儿 package.json 与未登记成员', () => {
    expect(deps({ 'extra/package.json': { name: 'x', version: '1.0.0', dependencies: { y: '^1.0.0' } } }).some((m) => m.includes('extra/package.json') && m.includes('工作区之外'))).toBe(true);
    expect(deps({ 'pnpm-workspace.yaml': 'packages:\n  - app\n  - shared\n  - ghost\nignoreScripts: true\n' }).some((m) => m.includes('ghost'))).toBe(true);
  });
  it('负向：workspace 版本与成员版本不一致 / 指向非成员', () => {
    expect(deps({ 'app/package.json': { name: '@x/app', version: '0.1.0', dependencies: { '@x/shared': 'workspace:0.2.0' } } }).some((m) => m.includes('不一致'))).toBe(true);
    expect(deps({ 'app/package.json': { name: '@x/app', version: '0.1.0', dependencies: { '@x/none': 'workspace:0.1.0' } } }).some((m) => m.includes('不是工作区成员'))).toBe(true);
  });
});

// ---------------- i18n 缺键 ----------------
const cat = (zh, en, dir = 'shared/src/i18n/locales') => ({ [`${dir}/zh-CN.json`]: zh, [`${dir}/en.json`]: en });
const i18n = (files) => checkI18n(tree(files)).errors;

describe('i18n 缺键检查', () => {
  it('一致的词条通过', () => {
    expect(i18n(cat({ a: { b: '你好 {name}' }, c: '好' }, { a: { b: 'Hello {name}' }, c: 'ok' }))).toEqual([]);
  });
  it('真实仓库通过', () => {
    const r = checkI18n(FRONTEND_ROOT);
    expect(r.errors).toEqual([]);
    expect(r.stats.keys).toBeGreaterThan(20);
  });
  it('负向：en 缺键 / zh-CN 缺键（双向）', () => {
    const e = i18n(cat({ a: '一', b: '二' }, { a: 'one', c: 'three' }));
    expect(e.some((m) => m.includes('en 缺少键 b'))).toBe(true);
    expect(e.some((m) => m.includes('zh-CN 缺少键 c'))).toBe(true);
  });
  it('负向：缺整个语言文件 / 多出不支持的语言', () => {
    expect(i18n({ 'shared/src/i18n/locales/zh-CN.json': { a: '一' } }).some((m) => m.includes('缺少 en.json'))).toBe(true);
    expect(i18n({ ...cat({ a: '一' }, { a: 'one' }), 'shared/src/i18n/locales/fr.json': { a: 'un' } }).some((m) => m.includes('不支持的语言'))).toBe(true);
  });
  it('负向：占位符不一致 / 空值 / 非字符串叶子 / 非法键段 / 非法 JSON', () => {
    expect(i18n(cat({ a: '共 {n} 条' }, { a: 'items' })).some((m) => m.includes('占位符不一致'))).toBe(true);
    expect(i18n(cat({ a: '' }, { a: 'x' })).some((m) => m.includes('为空'))).toBe(true);
    expect(i18n(cat({ a: 1 }, { a: 'x' })).some((m) => m.includes('必须是字符串或对象'))).toBe(true);
    expect(i18n(cat({ 'a.b': 'x' }, { 'a.b': 'x' })).some((m) => m.includes('非法键段'))).toBe(true);
    expect(i18n({ 'shared/src/i18n/locales/zh-CN.json': '{bad', 'shared/src/i18n/locales/en.json': { a: 'x' } }).some((m) => m.includes('不是合法 JSON'))).toBe(true);
  });
  it("负向：未转义的 Vue I18n 特殊字符（@ | { }）；{'@'} 与 {name} 合法", () => {
    expect(i18n(cat({ a: '联系 a@b.c' }, { a: 'mail a@b.c' })).some((m) => m.includes('特殊字符'))).toBe(true);
    expect(i18n(cat({ a: '甲|乙' }, { a: 'a|b' })).some((m) => m.includes('特殊字符'))).toBe(true);
    expect(i18n(cat({ a: "邮箱 a{'@'}b.c {n}" }, { a: "mail a{'@'}b.c {n}" }))).toEqual([]);
  });
  it('负向：源码使用了不存在的键（模板与脚本、$t 与 t），动态键校验前缀', () => {
    const base = cat({ ok: { k: '好' }, kind: { x: 'x' } }, { ok: { k: 'ok' }, kind: { x: 'x' } });
    const e = i18n({
      ...base,
      'admin/src/A.vue': '<template><p>{{ $t("ok.k") }}{{ t("no.such") }}</p></template>\n<script setup lang="ts">\nconst t = (k: string) => k;\nt(`kind.${1}`); t(`nope.${1}`);\n</script>\n',
    });
    expect(e.some((m) => m.includes('"no.such"'))).toBe(true);
    expect(e.some((m) => m.includes('动态键前缀 "nope."'))).toBe(true);
    expect(e.some((m) => m.includes('"ok.k"') || m.includes('"kind.'))).toBe(false);
  });
  it('没有词条目录时 catalogs=0（脚本据此拒绝空转）', () => {
    expect(checkI18n(tree({ 'a.txt': 'x' })).stats.catalogs).toBe(0);
  });
});
