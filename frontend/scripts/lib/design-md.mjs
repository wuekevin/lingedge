// 读取 docs/ux/DESIGN.md 的 YAML frontmatter，并与 tokens.json 比对（防止“单一来源”与设计文档分叉）。
import fs from 'node:fs';
import YAML from 'yaml';

export function parseDesignFrontmatter(markdown) {
  const m = /^---\n([\s\S]*?)\n---\n/.exec(markdown);
  if (!m) throw new Error('DESIGN.md 缺少 YAML frontmatter');
  return YAML.parse(m[1]);
}

export function loadDesign(file) {
  return parseDesignFrontmatter(fs.readFileSync(file, 'utf8'));
}

/** @returns {string[]} 差异描述；空数组表示一致 */
export function diffTokensAgainstDesign(tokens, design) {
  const diffs = [];
  const eq = (path, a, b) => {
    if (String(a) !== String(b)) diffs.push(`${path}：tokens.json=${a} DESIGN.md=${b}`);
  };
  // 颜色：-dark 后缀属于 dark，其余属于 light。
  const light = {};
  const dark = {};
  for (const [k, v] of Object.entries(design.colors ?? {})) {
    if (k.endsWith('-dark')) dark[k.slice(0, -5)] = v;
    else light[k] = v;
  }
  for (const [theme, want] of [['light', light], ['dark', dark]]) {
    const have = tokens.colors[theme];
    for (const k of new Set([...Object.keys(want), ...Object.keys(have)])) {
      if (!(k in have)) diffs.push(`colors.${theme}.${k}：tokens.json 缺失`);
      else if (!(k in want)) diffs.push(`colors.${theme}.${k}：DESIGN.md 缺失`);
      else eq(`colors.${theme}.${k}`, have[k].toUpperCase(), want[k].toUpperCase());
    }
  }
  // 字体
  for (const [name, d] of Object.entries(design.typography ?? {})) {
    const t = tokens.typography[name];
    if (!t) {
      diffs.push(`typography.${name}：tokens.json 缺失`);
      continue;
    }
    const fam = tokens.fontFamily[t.family];
    eq(`typography.${name}.fontFamily`, fam, d.fontFamily);
    eq(`typography.${name}.size`, t.size, d.fontSize);
    eq(`typography.${name}.weight`, t.weight, d.fontWeight);
    eq(`typography.${name}.lineHeight`, t.lineHeight, d.lineHeight);
    if (d.letterSpacing !== undefined || t.letterSpacing !== undefined) eq(`typography.${name}.letterSpacing`, t.letterSpacing, d.letterSpacing);
    if (d.fontFeatureSettings !== undefined || t.featureSettings !== undefined) eq(`typography.${name}.featureSettings`, t.featureSettings, d.fontFeatureSettings);
  }
  for (const name of Object.keys(tokens.typography)) {
    if (!(name in (design.typography ?? {}))) diffs.push(`typography.${name}：DESIGN.md 缺失`);
  }
  // 圆角 / 间距 / 布局
  for (const [k, v] of Object.entries(design.rounded ?? {})) eq(`rounded.${k}`, tokens.rounded[k], v);
  for (const [k, v] of Object.entries(design.spacing ?? {})) {
    const target = /^\d+$/.test(k) ? tokens.spacing : tokens.layout;
    eq(`spacing.${k}`, target[k], v);
  }
  const designSpacingKeys = new Set(Object.keys(design.spacing ?? {}));
  for (const k of [...Object.keys(tokens.spacing), ...Object.keys(tokens.layout)]) {
    if (!designSpacingKeys.has(k)) diffs.push(`spacing.${k}：DESIGN.md 缺失`);
  }
  return diffs;
}
