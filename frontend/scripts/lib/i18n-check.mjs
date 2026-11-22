// i18n 缺键检查（NFR-29、ADR-014 §7）：
//   1) 每个 locales/ 目录必须同时有 zh-CN.json 与 en.json，键集完全一致、占位符一致、无空值、值里没有 Vue I18n 的特殊字符未转义；
//   2) 源码里 t('键') / $t('键') 的静态键必须存在于词条（动态键 t(`前缀.${x}`) 校验前缀存在）。
import fs from 'node:fs';
import path from 'node:path';

const SKIP_DIRS = new Set(['node_modules', 'dist', '.pnpm-store', '.git', 'coverage', '.nuxt', '.output']);
const REQUIRED = ['zh-CN', 'en'];
const SRC_EXT = new Set(['.vue', '.ts', '.tsx', '.js', '.jsx', '.mjs']);

function walk(root) {
  const out = [];
  const rec = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      if (SKIP_DIRS.has(e.name)) continue;
      const p = path.join(dir, e.name);
      if (e.isDirectory()) rec(p);
      else out.push(path.relative(root, p).split(path.sep).join('/'));
    }
  };
  rec(root);
  return out.sort();
}

/** 压平成 key->string；非字符串叶子/数组/空键段记入 problems。 */
export function flatten(tree, problems, file, prefix = '') {
  const out = {};
  if (tree === null || typeof tree !== 'object' || Array.isArray(tree)) {
    problems.push(`${file}：词条根必须是对象`);
    return out;
  }
  for (const [k, v] of Object.entries(tree)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (k === '' || k.includes('.')) problems.push(`${file}：非法键段 "${k}"（不能为空或含 "."）`);
    if (typeof v === 'string') out[key] = v;
    else if (v && typeof v === 'object' && !Array.isArray(v)) Object.assign(out, flatten(v, problems, file, key));
    else problems.push(`${file}：${key} 的值必须是字符串或对象`);
  }
  return out;
}

const placeholders = (s) => [...s.matchAll(/\{([A-Za-z_]\w*)\}/g)].map((m) => m[1]).sort().join(',');

/** Vue I18n 消息语法里 @ | { } $ 是特殊字符；字面量必须写成 {'@'} 形式。这里只允许“{name}”这一种花括号用法。 */
function specialCharProblem(msg) {
  const stripped = msg.replace(/\{[A-Za-z_]\w*\}/g, '').replace(/\{'[^']*'\}/g, '');
  const m = /[@|{}$]/.exec(stripped);
  return m ? m[0] : null;
}

export function checkI18n(root) {
  const errors = [];
  const files = walk(root);
  const groups = new Map(); // dir -> {locale: file}
  for (const f of files) {
    const m = /^(.*\/)?locales\/([^/]+)\.json$/.exec(f);
    if (!m) continue;
    const dir = m[1] ?? '';
    if (!groups.has(dir)) groups.set(dir, {});
    groups.get(dir)[m[2]] = f;
  }
  const allKeys = new Set();
  const stats = { catalogs: groups.size, keys: 0, usedKeys: 0, dynamicSkipped: 0 };

  for (const [dir, locales] of groups) {
    for (const l of Object.keys(locales)) if (!REQUIRED.includes(l)) errors.push(`${locales[l]}：不支持的语言文件（只允许 ${REQUIRED.join(' / ')}）`);
    const flat = {};
    for (const l of REQUIRED) {
      const f = locales[l];
      if (!f) {
        errors.push(`${dir}locales/：缺少 ${l}.json`);
        continue;
      }
      let parsed;
      try {
        parsed = JSON.parse(fs.readFileSync(path.join(root, f), 'utf8'));
      } catch (e) {
        errors.push(`${f}：不是合法 JSON（${e.message}）`);
        continue;
      }
      const problems = [];
      flat[l] = flatten(parsed, problems, f);
      errors.push(...problems);
      for (const [k, v] of Object.entries(flat[l])) {
        if (v.trim() === '') errors.push(`${f}：${k} 为空`);
        const bad = specialCharProblem(v);
        if (bad) errors.push(`${f}：${k} 含未转义的 Vue I18n 特殊字符 "${bad}"（字面量请写成 {'${bad}'}）`);
      }
    }
    if (flat['zh-CN'] && flat.en) {
      for (const k of Object.keys(flat['zh-CN'])) {
        if (!(k in flat.en)) errors.push(`${dir}locales/：en 缺少键 ${k}`);
        else if (placeholders(flat['zh-CN'][k]) !== placeholders(flat.en[k])) {
          errors.push(`${dir}locales/：${k} 的占位符不一致（zh-CN {${placeholders(flat['zh-CN'][k])}} vs en {${placeholders(flat.en[k])}}）`);
        }
      }
      for (const k of Object.keys(flat.en)) if (!(k in flat['zh-CN'])) errors.push(`${dir}locales/：zh-CN 缺少键 ${k}`);
      for (const k of Object.keys(flat['zh-CN'])) allKeys.add(k);
    }
  }
  stats.keys = allKeys.size;

  // 源码使用的键
  const used = new Set();
  const re = /(?:\$t|\bt|\bte)\(\s*(['"`])((?:(?!\1)[^\\\n])*)\1/g;
  for (const f of files) {
    if (!SRC_EXT.has(path.extname(f)) || f.startsWith('scripts/') || /\.(test|spec)\.[cm]?[jt]sx?$/.test(f) || f.endsWith('.gen.ts')) continue;
    const src = fs.readFileSync(path.join(root, f), 'utf8');
    for (const m of src.matchAll(re)) {
      const key = m[2];
      const line = src.slice(0, m.index).split('\n').length;
      if (m[1] === '`' && key.includes('${')) {
        const prefix = key.slice(0, key.indexOf('${'));
        if (prefix === '') {
          stats.dynamicSkipped++;
          continue;
        }
        if (![...allKeys].some((k) => k.startsWith(prefix))) errors.push(`${f}:${line}：动态键前缀 "${prefix}" 在任何词条中都不存在`);
        used.add(prefix);
        continue;
      }
      used.add(key);
      if (!allKeys.has(key)) errors.push(`${f}:${line}：键 "${key}" 不存在于任何 zh-CN 词条`);
    }
  }
  stats.usedKeys = used.size;
  return { errors, stats };
}
