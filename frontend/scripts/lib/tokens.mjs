// 由 tokens.json 派生三类产物：CSS 变量（tokens.css）、Tailwind preset、Arco 主题映射（arco-theme.css）。
// 纯函数：输入 tokens 对象，输出 { 文件名: 内容 }，便于测试与漂移检查。
import { contrastRatio, mix, parseHex } from './color.mjs';

const HEADER = '/* 自动生成，请勿手改。来源：frontend/shared/tokens/tokens.json；生成器：frontend/scripts/gen-tokens.mjs（make frontend-check 校验无漂移）。 */\n';

const THEMES = ['light', 'dark'];

/** 令牌名 -> CSS 变量名 */
const colorVar = (k) => `--le-color-${k}`;

function themeColorLines(colors) {
  return Object.entries(colors).map(([k, v]) => `  ${colorVar(k)}: ${v};`);
}

function nonColorLines(t) {
  const lines = [];
  lines.push(`  --le-font-sans: ${t.fontFamily.sans};`);
  lines.push(`  --le-font-mono: ${t.fontFamily.mono};`);
  for (const [name, s] of Object.entries(t.typography)) {
    lines.push(`  --le-text-${name}-size: ${s.size};`);
    lines.push(`  --le-text-${name}-weight: ${s.weight};`);
    lines.push(`  --le-text-${name}-line-height: ${s.lineHeight};`);
    if (s.letterSpacing) lines.push(`  --le-text-${name}-letter-spacing: ${s.letterSpacing};`);
    if (s.featureSettings) lines.push(`  --le-text-${name}-feature-settings: ${s.featureSettings};`);
  }
  for (const [k, v] of Object.entries(t.rounded)) lines.push(`  --le-radius-${k}: ${v};`);
  for (const [k, v] of Object.entries(t.spacing)) lines.push(`  --le-space-${k}: ${v};`);
  for (const [k, v] of Object.entries(t.layout)) lines.push(`  --le-layout-${k}: ${v};`);
  return lines;
}

export function renderTokensCss(t) {
  const darkSel = `:root[data-theme='dark'],\n[arco-theme='dark']`;
  const out = [HEADER];
  out.push(`:root,\n:root[data-theme='light'] {`);
  out.push(...themeColorLines(t.colors.light));
  out.push(`  --le-shadow-overlay: ${t.shadow.overlay.light};`);
  out.push(`  --le-gradient-brand: linear-gradient(135deg, var(--le-color-brand-primary) 0%, var(--le-color-brand-violet) 100%);`);
  out.push(`  color-scheme: light;`);
  out.push(...nonColorLines(t));
  out.push('}\n');
  out.push(`${darkSel} {`);
  out.push(...themeColorLines(t.colors.dark));
  out.push(`  --le-shadow-overlay: ${t.shadow.overlay.dark};`);
  // DESIGN.md「Colors」：暗色主题渐变端点降低 10% 不透明度，叠加在 bg-page 上，避免发光过强。
  out.push(
    `  --le-gradient-brand: linear-gradient(135deg, color-mix(in srgb, var(--le-color-brand-primary) 90%, transparent) 0%, color-mix(in srgb, var(--le-color-brand-violet) 90%, transparent) 100%);`,
  );
  out.push(`  color-scheme: dark;`);
  out.push('}\n');
  return out.join('\n');
}

// ---------- Tailwind preset ----------

/** 令牌名 -> var() 引用；preset 不含任何色值，主题由 data-theme 切换 CSS 变量实现。 */
const cv = (k) => `var(${colorVar(k)})`;

export function buildTailwindPreset(t) {
  const colorKeys = Object.keys(t.colors.light);
  const colors = Object.fromEntries(colorKeys.map((k) => [`le-${k}`, cv(k)]));
  const fontSize = Object.fromEntries(
    Object.keys(t.typography).map((n) => [
      `le-${n}`,
      [`var(--le-text-${n}-size)`, { lineHeight: `var(--le-text-${n}-line-height)`, fontWeight: `var(--le-text-${n}-weight)` }],
    ]),
  );
  const borderRadius = Object.fromEntries(Object.keys(t.rounded).map((k) => [`le-${k}`, `var(--le-radius-${k})`]));
  const spacing = Object.fromEntries(Object.keys(t.spacing).map((k) => [`le-${k}`, `var(--le-space-${k})`]));
  return {
    theme: {
      extend: {
        colors,
        fontFamily: { 'le-sans': ['var(--le-font-sans)'], 'le-mono': ['var(--le-font-mono)'] },
        fontSize,
        borderRadius,
        spacing,
        boxShadow: { 'le-overlay': 'var(--le-shadow-overlay)' },
        backgroundImage: { 'le-brand': 'var(--le-gradient-brand)' },
        screens: Object.fromEntries(Object.entries(t.screens).filter(([k]) => k !== 'xs')),
        maxWidth: { 'le-admin': 'var(--le-layout-content-max-admin)', 'le-website': 'var(--le-layout-content-max-website)' },
      },
    },
  };
}

export function renderTailwindPreset(t) {
  const preset = buildTailwindPreset(t);
  const js = `${HEADER}// Tailwind 3.x preset（官网 Nuxt/Tailwind 使用；Admin/Console 不使用 Tailwind 做主结构，DESIGN.md）。\n// 颜色全部是 CSS 变量引用，亮/暗由 data-theme 切换，不在组件内写分支。\nexport default ${JSON.stringify(preset, null, 2)};\n`;
  const dts = `${HEADER}declare const preset: { theme: { extend: Record<string, unknown> } };\nexport default preset;\n`;
  return { js, dts };
}

// ---------- Arco 主题映射 ----------

/** Arco 的色板变量是 "r,g,b" 三元组（用法 rgb(var(--primary-6))）。 */
const triplet = (hex) => parseHex(hex).join(',');

/**
 * 生成 Arco 10 级色阶（确定性，提议值）：
 * 6 级 = 基色；7 级 = 悬停色（缺省为向 towards 混 15%）；1–5 级向“底色”渐变（1 最浅）；8–10 级继续向黑（亮）/白（暗）加深。
 * 5 级是 Arco 实心控件的悬停底色：其上的文字（solidText，主题的 on-brand）对比度必须 ≥ 4.5:1，
 * 因此 5 级的底色占比取 [0.2, 0.15, 0.1, 0.05, 0] 中第一个满足对比度的值（Story 1.37b；亮色主题的悬停态在 0.2 时仅 3.9–4.2:1）。
 */
export const SOLID_TEXT_MIN = 4.5;
export function arcoScale(base, hover, ground, towards, solidText) {
  const tints = [0.9, 0.8, 0.6, 0.4, 0.2]; // 1..5：底色占比
  if (solidText) {
    const w5 = [0.2, 0.15, 0.1, 0.05, 0].find((w) => contrastRatio(solidText, mix(base, ground, w)) >= SOLID_TEXT_MIN);
    tints[4] = w5 ?? 0;
  }
  const seven = hover ?? mix(base, towards, 0.15);
  const deep = [0.2, 0.4, 0.6].map((w) => mix(seven, towards, w));
  return [...tints.map((w) => mix(base, ground, w)), base, seven, ...deep];
}

function themeArcoBlock(t, theme) {
  const c = t.colors[theme];
  const towards = theme === 'light' ? '#000000' : '#FFFFFF';
  const lines = [];
  const scale = (name, base, hover, ground) => {
    arcoScale(base, hover, ground, towards, c['on-brand']).forEach((hex, i) => lines.push(`  --${name}-${i + 1}: ${triplet(hex)};`));
  };
  scale('primary', c['brand-primary'], c['brand-primary-hover'], c['bg-surface']);
  scale('success', c.success, undefined, c['success-bg']);
  scale('warning', c.warning, undefined, c['warning-bg']);
  scale('danger', c.danger, undefined, c['danger-bg']);
  scale('link', c.info, undefined, c['info-bg']);
  const v = (k) => `var(${colorVar(k)})`;
  lines.push(
    `  --color-bg-1: ${v('bg-surface')};`,
    `  --color-bg-2: ${v('bg-surface')};`,
    `  --color-bg-3: ${v('bg-surface-raised')};`,
    `  --color-bg-4: ${v('bg-surface-raised')};`,
    `  --color-bg-5: ${v('bg-surface-raised')};`,
    `  --color-bg-popup: ${v('bg-surface-raised')};`,
    `  --color-text-1: ${v('text-primary')};`,
    `  --color-text-2: ${v('text-secondary')};`,
    `  --color-text-3: ${v('text-secondary')};`,
    `  --color-text-4: ${v('text-disabled')};`,
    `  --color-fill-1: ${v('bg-sunken')};`,
    `  --color-fill-2: ${v('bg-sunken')};`,
    `  --color-fill-3: ${v('border-default')};`,
    `  --color-fill-4: ${v('border-strong')};`,
    `  --color-border: ${v('border-default')};`,
    `  --color-border-1: ${v('border-default')};`,
    `  --color-border-2: ${v('border-default')};`,
    `  --color-border-3: ${v('border-strong')};`,
    `  --color-border-4: ${v('border-strong')};`,
  );
  return lines;
}

/**
 * 暗色主题下 Arco 实心控件的文字色窄覆盖（Story 1.37b）：Arco 把这些规则的文字色写死为 #fff，而暗色调色板 6 级是明亮色。
 * 只写 color（不用 !important、不改结构）；选择器 = 作用域前缀 + Arco 原选择器，特异性恒高于 Arco 自身（多一个属性选择器与 body 类型选择器）。
 * 清单来自 tokens.json arco.solidTextOnDark，由 scripts/lib/arco-audit.mjs 对照 Arco 真实 CSS 校验。
 */
export function renderSolidTextOverrides(t) {
  const cfg = t.arco?.solidTextOnDark;
  if (!cfg || cfg.selectors.length === 0) return [];
  const scopes = ["body[arco-theme='dark']", ":root[data-theme='dark'] body"];
  const sels = cfg.selectors.flatMap((sel) => scopes.map((sc) => `${sc} ${sel}`));
  return [
    '/* 暗色主题：Arco 实心控件文字色写死为 #fff（对明亮的暗色调色板仅 1.8–3.9:1），窄覆盖为 on-brand 令牌（仅 color；清单与理由见 tokens.json arco）。 */',
    `${sels.join(',\n')} {`,
    `  color: var(${colorVar(cfg.token)});`,
    '}\n',
  ];
}

export function renderArcoTheme(t) {
  const out = [HEADER, '/* Arco Design Vue 主题层映射：只覆盖 CSS 变量，不改 Arco 源码、不用 !important（DESIGN.md「Arco 定制边界」）。引入顺序：先 Arco 自带样式，后本文件。 */\n'];
  out.push('body {');
  out.push(...themeArcoBlock(t, 'light'));
  out.push(`  --border-radius-small: ${t.rounded.sm};`);
  out.push(`  --border-radius-medium: ${t.rounded.md};`);
  out.push(`  --border-radius-large: ${t.rounded.lg};`);
  out.push(`  --border-radius-circle: ${t.rounded.full};`);
  out.push('}\n');
  // 与 Arco 自带 body[arco-theme='dark'] 同特异性，靠后加载取胜；data-theme 写在 html 上时用后代选择器取胜。
  out.push(`body[arco-theme='dark'],\n:root[data-theme='dark'] body {`);
  out.push(...themeArcoBlock(t, 'dark'));
  out.push('}\n');
  out.push(...renderSolidTextOverrides(t));
  return out.join('\n');
}

/** 全部生成物：{ 相对 TOKENS_OUT 的文件名: 内容 } */
export function renderAll(t) {
  const { js, dts } = renderTailwindPreset(t);
  return {
    'tokens.css': renderTokensCss(t),
    'arco-theme.css': renderArcoTheme(t),
    'tailwind-preset.js': js,
    'tailwind-preset.d.ts': dts,
  };
}

export { THEMES };
