// 全仓前端门禁（Story 1.36 AC3，ADR-014 §2/§7，DESIGN.md「令牌单一来源」）：
//   全仓：no-v-html / no-hardcoded-color / no-hardcoded-copy
//   shared：shared-no-forbidden-import / shared-no-route-or-menu / shared-no-permission / shared-no-session / shared-no-api-client
// 实现基于 TypeScript 编译器 AST 与 @vue/compiler-sfc 模板 AST（不是整文件 grep），并有负向自测（guards.test.mjs）。
import fs from 'node:fs';
import path from 'node:path';
import ts from 'typescript';
import { parse as parseSfc } from 'vue/compiler-sfc';
import { allPathTemplates, allPermissions, loadOpenapi } from './openapi.mjs';

const SCAN_EXT = new Set(['.vue', '.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs', '.css', '.scss', '.less', '.html']);
const SCRIPT_EXT = new Set(['.ts', '.tsx', '.js', '.jsx', '.mjs', '.cjs']);
const SKIP_DIRS = new Set(['node_modules', 'dist', '.pnpm-store', '.git', 'coverage', '.nuxt', '.output']);
// 工具链自身（scripts/）不是产品代码；shared/tokens 是色值的唯一合法位置；生成类型不含逻辑。
const SKIP_PREFIXES = ['scripts/', 'shared/tokens/', 'shared/src/api-types/'];
const isGenerated = (rel) => rel.endsWith('.gen.ts') || rel.endsWith('.d.ts');
const isTest = (rel) => /\.(test|spec)\.[cm]?[jt]sx?$/.test(rel) || rel.split('/').includes('__tests__');

/** 复制：能被视为“文案”的静态属性名（模板里写死的值必须走 i18n 键）。 */
const COPY_ATTRS = new Set([
  'placeholder', 'title', 'alt', 'aria-label', 'aria-placeholder', 'aria-description', 'aria-roledescription',
  'label', 'tooltip', 'description', 'header', 'subtitle', 'confirm-text', 'ok-text', 'cancel-text', 'empty-text',
  'loading-text', 'okText', 'cancelText',
]);

const FORBIDDEN_IMPORT = /^(vue-router|pinia|axios|ky|ofetch|openapi-fetch|swr|got|node-fetch|undici|superagent|@tanstack\/[\w-]+|@lingedge\/(admin|console|website))(\/|$)/;
const ROUTE_IDENT = /^(createRouter|createWebHistory|createWebHashHistory|createMemoryHistory|RouteRecordRaw|RouteLocationRaw|useRouter|useRoute|RouterLink|RouterView|onBeforeRouteLeave|onBeforeRouteUpdate)$/;
const ROUTE_OR_MENU_DECL = /^(routes?|menus?|menuItems?|menuConfig|menuList|navItems?|navigation|sidebar\w*|routeTable|routeMap|\w+Routes|\w+Menus?|\w+MenuItems?)$/i;
const PERMISSION_IDENT = /^(hasPermissions?|hasPerm|checkPermission|usePermissions?|permissions?|userPermissions|rolePermissions|requirePermission|isAdmin|isPlatformAdmin|isSuperAdmin|canAccess|canView|canEdit)$/i;
const PERMISSION_LITERAL = /^[a-z][a-z0-9_]*:(read|write|manage|create|update|delete|publish|impersonate|\*)$/;
const SESSION_IDENT = /^(localStorage|sessionStorage|useSession|useAuth|login|logout|signIn|signOut|csrfToken|useUserStore|useAuthStore|sessionStore|currentUser|getToken|accessToken|refreshToken)$/;
const SESSION_LITERAL = /(le_(admin|console)_sid|x-csrf-token|^authorization$|^bearer\b)/i;
const API_CLIENT_IDENT = /^(axios|XMLHttpRequest|fetch|WebSocket|EventSource|createClient|ofetch|\$fetch|useFetch|useRequest|apiClient|httpClient|createApiClient|baseURL)$/;

const NAMED_COLORS = 'white|black|red|green|blue|gray|grey|orange|yellow|purple|pink|cyan|magenta|silver|navy|teal|maroon|olive|lime|aqua|fuchsia|brown|gold';
const COLOR_PROPS = 'color|background(?:-color)?|border(?:-(?:top|right|bottom|left))?(?:-color)?|outline(?:-color)?|fill|stroke|box-shadow|text-shadow|caret-color|accent-color';

/** 去掉注释但保留换行（行号不变）。 */
export function stripComments(src, ext) {
  let out = src.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ' '));
  if (ext === '.vue' || ext === '.html') out = out.replace(/<!--[\s\S]*?-->/g, (m) => m.replace(/[^\n]/g, ' '));
  if (SCRIPT_EXT.has(ext) || ext === '.vue') out = out.replace(/(^|[^:'"`\\])\/\/[^\n]*/g, (m, p1) => p1 + ' '.repeat(m.length - p1.length));
  return out;
}

const lineOf = (text, index) => text.slice(0, index).split('\n').length;

// ---------------- 颜色 ----------------
function colorViolations(rel, ext, src) {
  const text = stripComments(src, ext);
  const found = [];
  const add = (index, msg) => found.push({ rule: 'no-hardcoded-color', file: rel, line: lineOf(text, index), message: msg });
  for (const m of text.matchAll(/(?<![\w&/.-])#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})(?![\w-])/g)) {
    const before = text.slice(Math.max(0, m.index - 40), m.index);
    if (/(href|to|id|for|xlink:href|aria-[\w-]+)\s*=\s*["']$/.test(before) || /url\(\s*["']?$/.test(before)) continue; // 锚点/片段引用
    add(m.index, `硬编码色值 ${m[0]}：色值只允许出现在 shared/tokens（用 var(--le-color-*)）`);
  }
  for (const m of text.matchAll(/\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch)\(\s*(?!var\()/gi)) {
    add(m.index, `硬编码颜色函数 ${m[1]}(…)：改用令牌（rgb(var(--primary-6)) 这类 Arco 变量引用除外）`);
  }
  const named = new RegExp(`(?:^|[\\s;{"'])(${COLOR_PROPS})\\s*:\\s*([^;}"'\\n]*)`, 'gi');
  for (const m of text.matchAll(named)) {
    const hit = new RegExp(`(?:^|[\\s,(])(${NAMED_COLORS})(?=[\\s,;)]|$)`, 'i').exec(m[2]);
    if (hit && !/var\(/.test(hit[0])) add(m.index, `硬编码颜色名 ${hit[1]}（${m[1]}）`);
  }
  return found;
}

// ---------------- TS AST ----------------
function scriptKind(ext, lang) {
  if (lang === 'tsx' || ext === '.tsx') return ts.ScriptKind.TSX;
  if (lang === 'ts' || ext === '.ts') return ts.ScriptKind.TS;
  if (ext === '.jsx') return ts.ScriptKind.JSX;
  return ts.ScriptKind.JS;
}

/** 收集一段脚本里的标识符/字面量/导入/声明名；line 加上 offset（.vue 里 script 块的起始行）。 */
function analyzeScript(code, ext, lang, lineOffset) {
  const sf = ts.createSourceFile('x', code, ts.ScriptTarget.Latest, true, scriptKind(ext, lang));
  const facts = { idents: [], strings: [], imports: [], declNames: [], props: [] };
  const line = (n) => sf.getLineAndCharacterOfPosition(n.getStart(sf)).line + 1 + lineOffset;
  const visit = (n) => {
    if (ts.isIdentifier(n)) facts.idents.push({ name: n.text, line: line(n) });
    if (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n)) facts.strings.push({ text: n.text, line: line(n) });
    if (ts.isTemplateHead(n) || ts.isTemplateMiddle(n) || ts.isTemplateTail(n)) facts.strings.push({ text: n.text, line: line(n) });
    if (ts.isImportDeclaration(n) && ts.isStringLiteral(n.moduleSpecifier)) facts.imports.push({ spec: n.moduleSpecifier.text, line: line(n) });
    if (ts.isExportDeclaration(n) && n.moduleSpecifier && ts.isStringLiteral(n.moduleSpecifier)) facts.imports.push({ spec: n.moduleSpecifier.text, line: line(n) });
    if (ts.isCallExpression(n) && n.arguments.length > 0 && ts.isStringLiteralLike(n.arguments[0])) {
      const callee = n.expression;
      if (ts.isIdentifier(callee) && callee.text === 'require') facts.imports.push({ spec: n.arguments[0].text, line: line(n) });
      if (callee.kind === ts.SyntaxKind.ImportKeyword) facts.imports.push({ spec: n.arguments[0].text, line: line(n) });
    }
    if ((ts.isVariableDeclaration(n) || ts.isFunctionDeclaration(n) || ts.isClassDeclaration(n) || ts.isEnumDeclaration(n)) && n.name && ts.isIdentifier(n.name)) {
      facts.declNames.push({ name: n.name.text, line: line(n) });
    }
    if (ts.isPropertyAccessExpression(n)) facts.props.push({ obj: n.expression.getText(sf), name: n.name.text, line: line(n) });
    ts.forEachChild(n, visit);
  };
  visit(sf);
  return facts;
}

function scriptRules(rel, facts, opts, isShared) {
  const found = [];
  const add = (rule, line, message) => found.push({ rule, file: rel, line, message });
  for (const i of facts.idents) {
    if (/^(innerHTML|outerHTML|insertAdjacentHTML|dangerouslySetInnerHTML)$/.test(i.name)) add('no-v-html', i.line, `禁止 ${i.name}（等同 v-html，XSS 风险）`);
  }
  for (const p of facts.props) {
    if (p.name === 'write' && p.obj === 'document') add('no-v-html', p.line, '禁止 document.write');
  }
  if (isShared) {
    for (const i of facts.imports) {
      if (FORBIDDEN_IMPORT.test(i.spec)) add('shared-no-forbidden-import', i.line, `shared 不得依赖 ${i.spec}（路由/状态/HTTP 客户端/应用包属于各应用自己，ADR-014 §2）`);
    }
    const seen = new Set();
    const once = (rule, key, line, msg) => {
      const k = `${rule}:${key}:${line}`;
      if (!seen.has(k)) {
        seen.add(k);
        add(rule, line, msg);
      }
    };
    for (const i of facts.idents) {
      if (ROUTE_IDENT.test(i.name)) once('shared-no-route-or-menu', i.name, i.line, `shared 不得使用路由 API：${i.name}`);
      if (PERMISSION_IDENT.test(i.name)) once('shared-no-permission', i.name, i.line, `shared 不得含权限判定/常量：${i.name}`);
      if (SESSION_IDENT.test(i.name)) once('shared-no-session', i.name, i.line, `shared 不得含会话/登录逻辑：${i.name}`);
      if (API_CLIENT_IDENT.test(i.name)) once('shared-no-api-client', i.name, i.line, `shared 不得出现 API 客户端/网络调用：${i.name}`);
    }
    for (const d of facts.declNames) {
      if (ROUTE_OR_MENU_DECL.test(d.name)) once('shared-no-route-or-menu', d.name, d.line, `shared 不得定义路由表/菜单：${d.name}`);
    }
    for (const p of facts.props) {
      if (p.obj === 'document' && p.name === 'cookie') once('shared-no-session', 'cookie', p.line, 'shared 不得读写 document.cookie（会话属于各应用）');
    }
    for (const s of facts.strings) {
      if (opts.permissions.has(s.text) || PERMISSION_LITERAL.test(s.text)) once('shared-no-permission', s.text, s.line, `shared 不得含权限串常量：'${s.text}'`);
      if (SESSION_LITERAL.test(s.text)) once('shared-no-session', s.text, s.line, `shared 不得含会话/认证常量：'${s.text}'`);
      if (opts.pathSet.has(s.text.replace(/^\/api\/v1/, ''))) once('shared-no-api-client', s.text, s.line, `shared 不得引用具体业务端点：'${s.text}'`);
    }
  }
  return found;
}

function copyScriptViolations(rel, facts) {
  return facts.strings
    .filter((s) => /\p{Script=Han}/u.test(s.text))
    .map((s) => ({ rule: 'no-hardcoded-copy', file: rel, line: s.line, message: `脚本里硬编码中文文案 '${s.text.slice(0, 20)}'：走 i18n 键` }));
}

// ---------------- Vue 模板 ----------------
const hasLetter = (s) => /[\p{L}]/u.test(s);

function walkTemplate(node, visit) {
  visit(node);
  for (const c of node.children ?? []) if (typeof c === 'object') walkTemplate(c, visit);
}

function templateRules(rel, ast, lineBase, isShared, opts) {
  const found = [];
  const add = (rule, line, message) => found.push({ rule, file: rel, line: line + lineBase, message });
  const test = isTest(rel);
  walkTemplate(ast, (n) => {
    if (n.type === 1) {
      if (isShared && /^(router-link|router-view|RouterLink|RouterView)$/.test(n.tag)) add('shared-no-route-or-menu', n.loc.start.line, `shared 不得使用 <${n.tag}>`);
      for (const p of n.props) {
        if (p.type === 7) {
          if (p.name === 'html') add('no-v-html', p.loc.start.line, '禁止 v-html（XSS 风险）');
          const argName = p.arg && p.arg.type === 4 ? p.arg.content : '';
          if (p.name === 'bind' && /^(innerHTML|inner-html|outerHTML)$/.test(argName)) add('no-v-html', p.loc.start.line, `禁止 :${argName}（等同 v-html）`);
          if (isShared && /^(permission|perm|auth|role|access)$/.test(p.name)) add('shared-no-permission', p.loc.start.line, `shared 不得含权限指令 v-${p.name}`);
          if (p.exp && !test && /['"`][^'"`]*\p{Script=Han}/u.test(p.exp.content)) add('no-hardcoded-copy', p.loc.start.line, `模板表达式里硬编码中文文案：${p.exp.content.slice(0, 30)}`);
        } else if (p.type === 6) {
          if (!test && p.value && COPY_ATTRS.has(p.name) && hasLetter(p.value.content)) {
            add('no-hardcoded-copy', p.loc.start.line, `静态属性 ${p.name}="${p.value.content}" 是硬编码文案：改用 :${p.name}="t('…')"`);
          }
        }
      }
    } else if (n.type === 2) {
      if (!test && hasLetter(n.content)) add('no-hardcoded-copy', n.loc.start.line, `模板里硬编码文案 "${n.content.trim().slice(0, 30)}"：走 i18n 键`);
    } else if (n.type === 5) {
      if (!test && n.content && /['"`][^'"`]*\p{Script=Han}/u.test(n.content.content ?? '')) add('no-hardcoded-copy', n.loc.start.line, `插值里硬编码中文文案：${n.content.content.slice(0, 30)}`);
    }
  });
  void opts;
  return found;
}

// ---------------- 驱动 ----------------
/** 递归列出待扫描文件（相对 root 的 posix 路径）。 */
export function listFiles(root) {
  const out = [];
  const walk = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      if (SKIP_DIRS.has(e.name)) continue;
      const p = path.join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (SCAN_EXT.has(path.extname(e.name))) out.push(path.relative(root, p).split(path.sep).join('/'));
    }
  };
  walk(root);
  return out.sort();
}

export function scanFile(root, rel, opts) {
  const ext = path.extname(rel);
  const src = fs.readFileSync(path.join(root, rel), 'utf8');
  const isShared = rel.startsWith('shared/');
  const found = [...colorViolations(rel, ext, src)];
  if (ext === '.vue') {
    const { descriptor, errors } = parseSfc(src, { filename: rel });
    if (errors.length > 0) found.push({ rule: 'parse-error', file: rel, line: 1, message: `SFC 解析失败：${errors[0].message}` });
    if (descriptor.template?.ast) found.push(...templateRules(rel, descriptor.template.ast, 0, isShared, opts));
    for (const block of [descriptor.script, descriptor.scriptSetup].filter(Boolean)) {
      const facts = analyzeScript(block.content, '.vue', block.lang, block.loc.start.line - 1);
      found.push(...scriptRules(rel, facts, opts, isShared));
      if (!isTest(rel)) found.push(...copyScriptViolations(rel, facts));
    }
  } else if (SCRIPT_EXT.has(ext)) {
    const facts = analyzeScript(src, ext, undefined, 0);
    found.push(...scriptRules(rel, facts, opts, isShared));
    if (!isTest(rel)) found.push(...copyScriptViolations(rel, facts));
  }
  return found;
}

/**
 * @param {{ root: string, openapiFile?: string }} args root = frontend 根目录（含 admin/console/website/shared）
 * @returns {{ violations: Array<{rule:string,file:string,line:number,message:string}>, scanned: number }}
 */
export function runGuards({ root, openapiFile }) {
  let permissions = new Set();
  let pathSet = new Set();
  if (openapiFile) {
    const doc = loadOpenapi(openapiFile);
    permissions = allPermissions(doc);
    pathSet = new Set(allPathTemplates(doc));
  }
  const opts = { permissions, pathSet };
  const violations = [];
  let scanned = 0;
  for (const rel of listFiles(root)) {
    if (SKIP_PREFIXES.some((p) => rel.startsWith(p)) || isGenerated(rel)) continue;
    scanned++;
    violations.push(...scanFile(root, rel, opts));
  }
  return { violations, scanned };
}
