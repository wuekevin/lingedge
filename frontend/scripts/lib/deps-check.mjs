// 依赖锁定检查（Story 1.36 AC1、dev-environment §4）：
//   精确版本（禁止 ^ ~ * latest x 区间 标签 git/http/file/link）、packageManager 固定、生命周期脚本关闭（pnpm-workspace.yaml 的 ignoreScripts 与 .npmrc）、
//   pnpm-lock.yaml 入库且其 specifier 同样精确、工作区成员与 pnpm-workspace.yaml 一致、不得登记构建脚本白名单。
import fs from 'node:fs';
import path from 'node:path';
import YAML from 'yaml';

const SEMVER = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;
const DEP_FIELDS = ['dependencies', 'devDependencies', 'optionalDependencies', 'peerDependencies'];
const LIFECYCLE = ['preinstall', 'install', 'postinstall', 'prepare', 'prepublish'];
const BUILD_ALLOWLIST_KEYS = ['onlyBuiltDependencies', 'dangerouslyAllowAllBuilds', 'trustedDependencies', 'allowBuilds', 'neverBuiltDependencies', 'onlyBuiltDependenciesFile'];

/** @returns {string|null} 不合格原因；null 表示合格 */
export function specProblem(spec) {
  if (typeof spec !== 'string' || spec === '') return '空版本';
  let s = spec;
  if (s.startsWith('workspace:')) s = s.slice('workspace:'.length);
  else if (s.startsWith('npm:')) {
    const at = s.lastIndexOf('@');
    s = at > 3 ? s.slice(at + 1) : s;
  }
  if (SEMVER.test(s)) return null;
  if (s === 'latest' || s === 'next') return `浮动标签 ${spec}`;
  if (s.includes('*') || /\bx\b/i.test(s)) return `通配版本 ${spec}`;
  if (/^[\^~<>=]/.test(s) || /\|\||\s-\s|\s/.test(s)) return `版本区间 ${spec}`;
  if (/^(git|github|http|https|file|link|portal|patch):/.test(s) || s.includes('/')) return `非 registry 来源 ${spec}`;
  return `不是精确版本 ${spec}`;
}

export function checkDeps(root) {
  const errors = [];
  const rel = (p) => path.relative(root, p) || '.';
  const read = (p) => fs.readFileSync(p, 'utf8');

  // 根 package.json
  const rootPkgFile = path.join(root, 'package.json');
  if (!fs.existsSync(rootPkgFile)) return { errors: ['缺少 package.json'], stats: { packages: 0, specs: 0 } };
  const rootPkg = JSON.parse(read(rootPkgFile));
  if (!/^pnpm@\d+\.\d+\.\d+$/.test(rootPkg.packageManager ?? '')) errors.push(`package.json：packageManager 必须固定为 pnpm@<精确版本>，当前 ${rootPkg.packageManager ?? '（缺失）'}`);

  // .npmrc
  const npmrcFile = path.join(root, '.npmrc');
  if (!fs.existsSync(npmrcFile)) errors.push('.npmrc 缺失（必须 ignore-scripts=true）');
  else {
    const lines = read(npmrcFile).split('\n').map((l) => l.trim()).filter((l) => l && !l.startsWith('#') && !l.startsWith(';'));
    const kv = Object.fromEntries(lines.map((l) => l.split('=').map((x) => x.trim())).map(([k, ...v]) => [k, v.join('=')]));
    if (kv['ignore-scripts'] !== 'true') errors.push('.npmrc：必须设置 ignore-scripts=true（关闭未经审核的生命周期脚本）');
    if (kv['save-exact'] !== 'true') errors.push('.npmrc：必须设置 save-exact=true');
  }

  // 工作区
  const wsFile = path.join(root, 'pnpm-workspace.yaml');
  let ws = {};
  if (!fs.existsSync(wsFile)) errors.push('pnpm-workspace.yaml 缺失');
  else ws = YAML.parse(read(wsFile)) ?? {};
  // pnpm 12 不读取 .npmrc 里的 ignore-scripts（实测），有效开关在 pnpm-workspace.yaml。
  if (fs.existsSync(wsFile) && ws.ignoreScripts !== true) errors.push('pnpm-workspace.yaml：必须设置 ignoreScripts: true（pnpm 12 不读取 .npmrc 的 ignore-scripts，这里才是有效开关）');
  for (const k of BUILD_ALLOWLIST_KEYS) {
    if (k in ws) errors.push(`pnpm-workspace.yaml：不得配置 ${k}（构建脚本白名单需要先经依赖审核并同步修改本检查）`);
    if (rootPkg.pnpm && k in rootPkg.pnpm) errors.push(`package.json：pnpm.${k} 不允许（同上）`);
  }
  const members = ws.packages ?? [];
  const pkgFiles = [rootPkgFile];
  const versions = new Map([[rootPkg.name, rootPkg.version]]);
  for (const m of members) {
    const f = path.join(root, m, 'package.json');
    if (!fs.existsSync(f)) errors.push(`pnpm-workspace.yaml 登记了 ${m}，但缺少 ${m}/package.json`);
    else pkgFiles.push(f);
  }
  const pkgs = pkgFiles.map((f) => [f, JSON.parse(read(f))]);
  for (const [, p] of pkgs) versions.set(p.name, p.version);

  // 孤儿 package.json
  const orphan = [];
  const walk = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      if (e.name === 'node_modules' || e.name === 'dist' || e.name === '.git') continue;
      const p = path.join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name === 'package.json' && !pkgFiles.includes(p)) orphan.push(rel(p));
    }
  };
  walk(root);
  for (const o of orphan) errors.push(`${o}：存在于工作区之外（未登记到 pnpm-workspace.yaml），其依赖不受锁定检查`);

  let specs = 0;
  const checkSpec = (where, name, spec) => {
    specs++;
    const prob = specProblem(spec);
    if (prob) errors.push(`${where}：${name} 的版本不合格——${prob}`);
    if (typeof spec === 'string' && spec.startsWith('workspace:')) {
      const want = versions.get(name);
      if (want === undefined) errors.push(`${where}：${name} 不是工作区成员`);
      else if (spec.slice('workspace:'.length) !== want) errors.push(`${where}：${name} 的 workspace 版本 ${spec} 与成员版本 ${want} 不一致`);
    }
  };

  for (const [f, p] of pkgs) {
    for (const field of DEP_FIELDS) for (const [n, v] of Object.entries(p[field] ?? {})) checkSpec(`${rel(f)}(${field})`, n, v);
    for (const [n, v] of Object.entries(p.overrides ?? {})) checkSpec(`${rel(f)}(overrides)`, n, v);
    for (const [n, v] of Object.entries(p.pnpm?.overrides ?? {})) checkSpec(`${rel(f)}(pnpm.overrides)`, n, v);
    for (const s of LIFECYCLE) if (p.scripts?.[s]) errors.push(`${rel(f)}：scripts.${s} 会在安装时被执行/被静默忽略，禁止使用`);
  }
  for (const [n, v] of Object.entries(ws.overrides ?? {})) checkSpec('pnpm-workspace.yaml(overrides)', n, v);
  for (const [n, v] of Object.entries(ws.catalog ?? {})) checkSpec('pnpm-workspace.yaml(catalog)', n, v);
  for (const [cat, entries] of Object.entries(ws.catalogs ?? {})) for (const [n, v] of Object.entries(entries ?? {})) checkSpec(`pnpm-workspace.yaml(catalogs.${cat})`, n, v);

  // 锁文件
  const lockFile = path.join(root, 'pnpm-lock.yaml');
  if (!fs.existsSync(lockFile)) errors.push('pnpm-lock.yaml 缺失（必须入库，CI 使用 --frozen-lockfile）');
  else {
    // pnpm 12 的锁文件是多文档 YAML（第一份记录 packageManager 的环境锁，随后是工作区主锁）。
    const docs = YAML.parseAllDocuments(read(lockFile)).map((d) => d.toJS());
    if (docs.some((d) => !d?.lockfileVersion)) errors.push('pnpm-lock.yaml：存在缺少 lockfileVersion 的文档');
    const importers = Object.assign({}, ...docs.map((d) => d?.importers ?? {}));
    if (!Object.keys(importers).some((k) => k !== '.') && members.length > 0) errors.push('pnpm-lock.yaml：没有任何工作区成员的 importers 记录');
    for (const [imp, body] of Object.entries(importers)) {
      for (const field of ['dependencies', 'devDependencies', 'optionalDependencies']) {
        for (const [n, d] of Object.entries(body?.[field] ?? {})) {
          const prob = specProblem(d.specifier);
          if (prob) errors.push(`pnpm-lock.yaml：importers.${imp}.${field}.${n} specifier 不合格——${prob}`);
        }
      }
    }
  }
  return { errors, stats: { packages: pkgs.length, specs } };
}
