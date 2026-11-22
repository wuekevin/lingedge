// UX-27 / FR-35：全局界面不提供任何「执行命令」入口（终端、Shell、运行脚本、远程执行）。
// 升级、备份恢复等只读展示 + CLI 指引，由 lingedge 命令行执行，网页不提供「一键执行任意命令」（ia.md §1.15）。
// 静态检查三处：
//   1) 词条（admin/console 的 locales/*.json）的文案里不得出现「执行命令/终端/运行脚本」类入口措辞；
//   2) 页面注册表（admin/src/registry/pages.ts）登记的 key / path 不得是 exec/terminal/shell/command/script/ssh 这类入口；
//   3) 应用依赖里不得引入终端模拟/SSH 类库。
// 单元自测（no-command-entry.test.mjs）用临时目录里的负向样本证明这三处都能检出。
import fs from 'node:fs';
import path from 'node:path';

/** 入口措辞（中英文）。只匹配「执行/运行 + 命令/脚本」与终端类名词，不误伤「命令行指引」「CLI」的说明性文字。 */
export const FORBIDDEN_COPY = [
  /(执行|运行|远程执行|在线执行)\s*(任意|远程|系统|自定义|shell\s*)?\s*(命令|脚本|指令)/i,
  /(网页|在线|Web|远程)\s*(终端|Shell|SSH)/i,
  /(命令行?终端|终端模拟|终端窗口|打开终端|进入终端)/,
  /\b(run|execute|exec)\s+((an?|any|arbitrary|remote|custom)\s+)*(shell\s+)?(command|script|shell)s?\b/i,
  /\b(web|remote|browser|online)\s+(terminal|shell|ssh)\b/i,
  /\b(open|launch)\s+(a\s+)?(terminal|shell|console\s+window)\b/i,
];
/** 页面 key/path 里的入口段（按 / . - 切分后精确匹配）。 */
export const FORBIDDEN_SEGMENTS = new Set(['exec', 'execute', 'terminal', 'shell', 'command', 'commands', 'cmd', 'script', 'scripts', 'ssh', 'webshell', 'console-run', 'run-command']);
/** 终端模拟 / SSH 类依赖。 */
export const FORBIDDEN_DEPS = /^(xterm(-.*)?|@xterm\/.*|ssh2|node-pty|term\.js|hterm|webssh.*|ttyd.*)$/i;

const APPS = ['admin', 'console'];

function leaves(tree, prefix = '') {
  const out = [];
  for (const [k, v] of Object.entries(tree)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (typeof v === 'string') out.push([key, v]);
    else if (v && typeof v === 'object') out.push(...leaves(v, key));
  }
  return out;
}
const lineOf = (text, index) => text.slice(0, index).split('\n').length;

export function checkNoCommandEntry(root) {
  const violations = [];
  let scanned = 0;
  for (const app of APPS) {
    const base = path.join(root, app);
    if (!fs.existsSync(base)) continue;
    // 1) 词条
    for (const lang of ['zh-CN', 'en']) {
      const f = path.join(base, 'src/locales', `${lang}.json`);
      if (!fs.existsSync(f)) continue;
      scanned++;
      const rel = path.relative(root, f).split(path.sep).join('/');
      for (const [key, value] of leaves(JSON.parse(fs.readFileSync(f, 'utf8')))) {
        const hit = FORBIDDEN_COPY.find((re) => re.test(value));
        if (hit) violations.push({ file: rel, line: 1, rule: 'no-command-entry', message: `词条 ${key} 含「执行命令」类入口措辞：${value.slice(0, 40)}` });
      }
    }
    // 2) 页面注册表
    const reg = path.join(base, 'src/registry/pages.ts');
    if (fs.existsSync(reg)) {
      scanned++;
      const text = fs.readFileSync(reg, 'utf8');
      const rel = path.relative(root, reg).split(path.sep).join('/');
      for (const m of text.matchAll(/\b(key|path)\s*:\s*'([^']*)'/g)) {
        const bad = m[2].toLowerCase().split(/[/.\-_]+/).filter(Boolean).find((seg) => FORBIDDEN_SEGMENTS.has(seg)) ?? (FORBIDDEN_SEGMENTS.has(m[2].toLowerCase()) ? m[2] : null);
        if (bad) violations.push({ file: rel, line: lineOf(text, m.index), rule: 'no-command-entry', message: `页面 ${m[1]} '${m[2]}' 是「执行命令」类入口（${bad}）` });
      }
    }
    // 3) 依赖
    const pkg = path.join(base, 'package.json');
    if (fs.existsSync(pkg)) {
      scanned++;
      const j = JSON.parse(fs.readFileSync(pkg, 'utf8'));
      for (const name of Object.keys({ ...(j.dependencies ?? {}), ...(j.devDependencies ?? {}) })) {
        if (FORBIDDEN_DEPS.test(name)) violations.push({ file: `${app}/package.json`, line: 1, rule: 'no-command-entry', message: `依赖 ${name} 是终端/SSH 类库（UX-27：界面不提供执行命令入口）` });
      }
    }
  }
  return { violations, scanned };
}
