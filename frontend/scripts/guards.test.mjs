import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { runGuards, stripComments } from './lib/guards.mjs';
import { FRONTEND_ROOT, OPENAPI_YAML } from './lib/paths.mjs';

const dirs = [];
afterEach(() => {
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});

/** 在临时目录里按 {相对路径: 内容} 建一棵 frontend 树并运行门禁。 */
function run(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'le-guards-'));
  dirs.push(root);
  for (const [rel, content] of Object.entries(files)) {
    const f = path.join(root, rel);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, content);
  }
  return runGuards({ root, openapiFile: OPENAPI_YAML });
}
const rules = (files) => [...new Set(run(files).violations.map((v) => v.rule))].sort();
const vue = (tpl, script = '', style = '') => `<template>${tpl}</template>\n<script setup lang="ts">\n${script}\n</script>\n<style scoped>\n${style}\n</style>\n`;

describe('全仓规则：no-v-html（负向用例必须失败）', () => {
  it.each([
    ['模板 v-html', { 'admin/src/A.vue': vue('<div v-html="x"></div>') }],
    ['模板 :innerHTML', { 'admin/src/A.vue': vue('<div :innerHTML="x"></div>') }],
    ['模板 :inner-html.prop', { 'console/src/A.vue': vue('<div :inner-html.prop="x"></div>') }],
    ['脚本 el.innerHTML 赋值', { 'admin/src/a.ts': 'export const f = (el: HTMLElement, x: string) => { el.innerHTML = x; };' }],
    ['渲染函数 h(…, {innerHTML})', { 'console/src/a.ts': "import { h } from 'vue'; export const r = (x: string) => h('div', { innerHTML: x });" }],
    ['.vue script 内的 insertAdjacentHTML', { 'admin/src/A.vue': vue('<i />', "const f = (e: HTMLElement) => e.insertAdjacentHTML('beforeend', 'x');") }],
    ['document.write', { 'website/a.ts': "document.write('x');" }],
  ])('%s', (_n, files) => {
    expect(rules(files)).toContain('no-v-html');
  });
  it('正向：普通插值与类名不误报', () => {
    expect(rules({ 'admin/src/A.vue': vue('<div class="html-wrapper" :title="t(\'a\')">{{ html }}</div>', "const html = 'x'; const t = (s: string) => s;") })).toEqual([]);
  });
});

describe('全仓规则：no-hardcoded-color', () => {
  it.each([
    ['css #rrggbb', { 'admin/src/a.css': '.a { color: #3B4FD8; }' }],
    ['css #rgb', { 'admin/src/a.css': '.a { background: #fff; }' }],
    ['css #rrggbbaa', { 'admin/src/a.css': '.a { color: #3B4FD8cc; }' }],
    ['vue style 内色值', { 'admin/src/A.vue': vue('<i />', '', '.a { border-color: #DCE1EC; }') }],
    ['内联 style 属性', { 'admin/src/A.vue': vue('<i style="color:#333"/>') }],
    ['ts 字符串内色值', { 'console/src/a.ts': "export const c = '#B42318';" }],
    ['rgb()', { 'admin/src/a.css': '.a { color: rgb(1, 2, 3); }' }],
    ['rgba()', { 'admin/src/a.css': '.a { background: rgba(0,0,0,.5); }' }],
    ['hsl()', { 'admin/src/a.scss': '.a { color: hsl(10 20% 30%); }' }],
    ['颜色名', { 'admin/src/a.css': '.a { color: red; }' }],
    ['border 简写里的颜色名', { 'admin/src/a.css': '.a { border: 1px solid black; }' }],
    ['website 目录同样检查', { 'website/app.html': '<div style="background:#000"></div>' }],
    ['shared 内也不允许（只有 shared/tokens 豁免）', { 'shared/src/a.css': '.a { color: #fff; }' }],
  ])('%s', (_n, files) => {
    expect(rules(files)).toContain('no-hardcoded-color');
  });
  it.each([
    ['令牌变量', { 'admin/src/a.css': '.a { color: var(--le-color-text-primary); border: 1px solid var(--le-color-border-default); }' }],
    ['Arco 三元组变量引用', { 'admin/src/a.css': '.a { color: rgb(var(--primary-6)); }' }],
    ['锚点 href="#fade"', { 'admin/src/A.vue': vue('<a href="#fade">{{ x }}</a>', 'const x = 1;') }],
    ['注释里的色值', { 'admin/src/a.css': '/* 品牌色 #3B4FD8 */ .a { color: var(--le-color-brand-primary); }' }],
    ['transparent / currentColor / inherit', { 'admin/src/a.css': '.a { color: currentColor; background: transparent; border-color: inherit; }' }],
    ['shared/tokens 是唯一合法位置', { 'shared/tokens/x.css': ':root { --a: #3B4FD8; }', 'shared/src/ok.ts': 'export const a = 1;' }],
    ['生成类型豁免', { 'shared/src/api-types/openapi.gen.ts': "export type A = '#fff';" }],
    ['标识符里的十六进制样子的词（class 名/变量名）', { 'admin/src/a.ts': 'export const deadbeef = 1; export const x = "a#bad";' }],
  ])('正向：%s 不误报', (_n, files) => {
    expect(rules(files)).toEqual([]);
  });
  it('stripComments 保留行号', () => {
    const out = stripComments('a\n/* x\ny */\nb', '.css');
    expect(out.split('\n')).toHaveLength(4);
  });
});

describe('全仓规则：no-hardcoded-copy（文案走 i18n 键）', () => {
  it.each([
    ['模板英文文本', { 'admin/src/A.vue': vue('<p>Hello world</p>') }],
    ['模板中文文本', { 'admin/src/A.vue': vue('<button>保存</button>') }],
    ['静态 placeholder', { 'admin/src/A.vue': vue('<input placeholder="Search" />') }],
    ['静态 title', { 'console/src/A.vue': vue('<span title="提示">{{ x }}</span>', 'const x = 1;') }],
    ['静态 aria-label', { 'admin/src/A.vue': vue('<button aria-label="Close">{{ x }}</button>', 'const x = 1;') }],
    ['插值里的中文字面量', { 'admin/src/A.vue': vue("<p>{{ '已保存' }}</p>") }],
    ['属性表达式里的中文字面量', { 'admin/src/A.vue': vue('<i :title="\'提示\'" />') }],
    ['脚本里的中文字面量', { 'admin/src/a.ts': "export const msg = '保存成功';" }],
    ['.vue 脚本里的中文字面量', { 'admin/src/A.vue': vue('<i />', "const m = '失败';") }],
    ['模板字符串里的中文', { 'admin/src/a.ts': 'export const m = (n: number) => `共 ${n} 条`;' }],
  ])('%s', (_n, files) => {
    expect(rules(files)).toContain('no-hardcoded-copy');
  });
  it.each([
    ['t() 键', { 'admin/src/A.vue': vue("<p>{{ t('a.b') }}</p><input :placeholder=\"t('c')\" />", 'const t = (k: string) => k;') }],
    ['纯符号/数字文本', { 'admin/src/A.vue': vue('<span>·</span><span>/</span><span>1</span>') }],
    ['非文案属性（type/class/data-*）', { 'admin/src/A.vue': vue('<button type="button" class="primary" data-state="loading">{{ x }}</button>', 'const x = 1;') }],
    ['测试文件可断言中文', { 'shared/src/a.test.ts': "import { expect } from 'vitest'; expect('保存').toBe('保存');" }],
    ['中文注释', { 'admin/src/a.ts': '// 这是注释\nexport const a = 1; /* 另一个注释 */' }],
    ['locales JSON 不扫描', { 'shared/src/i18n/locales/zh-CN.json': '{"a":"保存"}' }],
  ])('正向：%s 不误报', (_n, files) => {
    expect(rules(files)).toEqual([]);
  });
});

describe('shared 边界规则（ADR-014 §2）：负向用例必须失败', () => {
  const cases = [
    // 路由表 / 菜单
    ['shared-no-forbidden-import', 'import { createRouter } from "vue-router"; export const a = createRouter;'],
    ['shared-no-route-or-menu', "import type { RouteRecordRaw } from 'vue-router'; export type R = RouteRecordRaw;"],
    ['shared-no-route-or-menu', "export const routes = [{ path: '/x' }];"],
    ['shared-no-route-or-menu', 'export const adminMenuItems = [1, 2];'],
    ['shared-no-route-or-menu', 'const sidebarConfig = {}; export default sidebarConfig;'],
    ['shared-no-route-or-menu', 'export function useRouter() { return 1; }'],
    // 权限常量与判定
    ['shared-no-permission', "export const P = 'user:read';"],
    ['shared-no-permission', "export const Q = 'site:publish';"],
    // 不符合通用 resource:action 形态、只能靠 openapi 的 x-permissions 清单识别的权限串
    ['shared-no-permission', "export const P3 = 'cache:purge';"],
    ['shared-no-permission', "export const P4 = 'tenant:member:manage';"],
    ['shared-no-permission', 'export function hasPermission(p: string) { return p.length > 0; }'],
    ['shared-no-permission', 'export const isAdmin = true;'],
    // 会话
    ['shared-no-session', 'export const s = () => document.cookie;'],
    ['shared-no-session', "export const k = 'le_admin_sid';"],
    ['shared-no-session', "export const h = 'X-CSRF-Token';"],
    ['shared-no-session', "export const t = () => localStorage.getItem('a');"],
    ['shared-no-session', 'export async function login() { return 1; }'],
    ['shared-no-forbidden-import', "import { defineStore } from 'pinia'; export const useX = defineStore('x', {});"],
    // API 客户端 / 端点
    ['shared-no-forbidden-import', "import axios from 'axios'; export default axios;"],
    ['shared-no-api-client', "export const c = axios.create({ baseURL: '/api/v1' });"],
    ['shared-no-api-client', "export const get = () => fetch('/x');"],
    ['shared-no-api-client', "export const p = '/auth/login';"],
    ['shared-no-api-client', "export const p = '/api/v1/clusters';"],
    ['shared-no-api-client', 'export const x = new XMLHttpRequest();'],
    ['shared-no-forbidden-import', "import { x } from '@lingedge/admin'; export default x;"],
  ];
  it.each(cases)('%s：%s', (rule, code) => {
    expect(rules({ 'shared/src/bad.ts': code })).toContain(rule);
  });

  it.each([
    ['shared-no-route-or-menu', '<router-link to="x">{{ t("a") }}</router-link>', 'const t = (k: string) => k;'],
    ['shared-no-permission', '<button v-permission="x">{{ t("a") }}</button>', 'const t = (k: string) => k; const x = 1;'],
    ['shared-no-route-or-menu', '<RouterView />', ''],
  ])('模板：%s', (rule, tpl, script) => {
    expect(rules({ 'shared/src/B.vue': vue(tpl, script) })).toContain(rule);
  });

  it('同样的代码放在 admin/console 里是合法的（边界只约束 shared）', () => {
    const code = [
      'import { createRouter, createWebHistory } from "vue-router";',
      'import axios from "axios";',
      "export const routes = [{ path: '/x', component: {} }];",
      "export const P = 'user:read';",
      "export const api = axios.create({ baseURL: '/api/v1' });",
      "export const sid = 'le_admin_sid';",
      'export const router = createRouter({ history: createWebHistory(), routes });',
      'export const get = () => fetch("/auth/me");',
    ].join('\n');
    expect(rules({ 'admin/src/router.ts': code, 'console/src/router.ts': code })).toEqual([]);
  });

  it('正向：典型的 shared 代码（组件/格式化/i18n）不触发', () => {
    expect(
      rules({
        'shared/src/ok.ts': "import { computed } from 'vue';\nexport const f = (n: number) => new Intl.NumberFormat('en').format(n);\nexport const g = computed(() => 1);\nexport const requestId = 'r_1';",
        'shared/src/Ok.vue': vue('<div role="status" :data-x="x">{{ t("a") }}</div>', "const x = 1; const t = (k: string) => k;"),
      }),
    ).toEqual([]);
  });
});

describe('门禁自身的健壮性', () => {
  it('空目录 scanned=0（lint-guards 脚本据此拒绝空转通过）', () => {
    expect(run({}).scanned).toBe(0);
  });
  it('违规带文件名与行号', () => {
    const v = run({ 'admin/src/a.css': '.a {}\n.b {\n  color: #fff;\n}' }).violations[0];
    expect(v).toMatchObject({ rule: 'no-hardcoded-color', file: 'admin/src/a.css', line: 3 });
  });
  it('SFC 里的违规行号是文件行号（不是块内行号）', () => {
    const v = run({ 'shared/src/B.vue': '<template><i /></template>\n<script setup lang="ts">\nconst a = 1;\nexport const routes = 1;\n</script>\n' }).violations;
    expect(v.find((x) => x.rule === 'shared-no-route-or-menu')?.line).toBe(4);
  });
  it('真实仓库当前通过全部门禁', () => {
    const { violations, scanned } = runGuards({ root: FRONTEND_ROOT, openapiFile: OPENAPI_YAML });
    expect(violations).toEqual([]);
    expect(scanned).toBeGreaterThan(10);
  });
});
