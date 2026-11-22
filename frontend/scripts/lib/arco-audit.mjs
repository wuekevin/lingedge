// Arco 实心控件的文字对比度审计（Story 1.37b，处置 1.36 发现 7）。
//
// Arco 2.59 的实心控件（主按钮、状态按钮、日历/日期选中格、徽标、步骤图标、复选框勾……）把文字色写死为 #fff（或 var(--color-white)），
// 底色取调色板变量 rgb(var(--<色>-N))。暗色主题下调色板 6 级是明亮的 #7C93FF 等，白字对比度只有 2–3.9:1。
// 本审计解析 Arco 的真实 CSS：找出全部「白字 + 调色板底色」的规则，按令牌推导出的两个主题的调色板逐状态计算对比度；
// 并要求 tokens.json 的 arco.solidTextOnDark.selectors 恰好覆盖「暗色下白字不合格」的规则（既不漏也无死条目）——Arco 升级引入新规则会使其失败。
import { arcoScale } from './tokens.mjs';
import { contrastRatio, mix } from './color.mjs';

/** 解析 CSS 里的简单规则（Arco 的 dist/arco.css 没有嵌套规则块之外的结构；@media 内的规则同样被捕获）。 */
export function parseRules(css) {
  const rules = [];
  for (const m of css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selectors = m[1].split(',').map((s) => s.replace(/\s+/g, ' ').trim()).filter(Boolean);
    const decl = {};
    for (const d of m[2].split(';')) {
      const i = d.indexOf(':');
      if (i > 0) decl[d.slice(0, i).trim()] = d.slice(i + 1).trim();
    }
    rules.push({ selectors, decl });
  }
  return rules;
}

const WHITE = /^(#fff|#ffffff|var\(--color-white\))$/i;

/** 白字 + 调色板底色 的规则 → { selectors, palette, level, white } */
export function whiteOnPaletteRules(css) {
  const out = [];
  for (const r of parseRules(css)) {
    const color = r.decl.color;
    if (!color || !WHITE.test(color)) continue;
    const bg = r.decl['background-color'] ?? r.decl.background ?? '';
    let m = /^rgb\(var\(--(primary|success|warning|danger|link)-(\d+)\)\)$/.exec(bg);
    let palette;
    let level;
    if (m) {
      palette = m[1];
      level = Number(m[2]);
    } else if ((m = /^var\(--color-(primary|success|warning|danger|link)-light-(\d+)\)$/.exec(bg))) {
      palette = m[1];
      level = Number(m[2]); // Arco：--color-<色>-light-N = rgb(var(--<色>-N))
    } else continue;
    out.push({ selectors: r.selectors, palette, level, white: color.toLowerCase().startsWith('var(') ? 'var' : 'hex' });
  }
  return out;
}

/** 主题的调色板（与 arco-theme.css 同一函数推导）。 */
export function paletteHex(tokens, theme) {
  const c = tokens.colors[theme];
  const towards = theme === 'light' ? '#000000' : '#FFFFFF';
  const sc = (base, hover, ground) => arcoScale(base, hover, ground, towards, c['on-brand']);
  return {
    primary: sc(c['brand-primary'], c['brand-primary-hover'], c['bg-surface']),
    success: sc(c.success, undefined, c['success-bg']),
    warning: sc(c.warning, undefined, c['warning-bg']),
    danger: sc(c.danger, undefined, c['danger-bg']),
    link: sc(c.info, undefined, c['info-bg']),
  };
}

/** 在底色上叠加半透明白（Arco 暗色 --color-white 为 rgba(255,255,255,.9)）。 */
const blendWhite = (bg, a) => mix(bg, '#FFFFFF', a);

export function auditArcoSolidText(tokens, css) {
  const errors = [];
  const rules = whiteOnPaletteRules(css);
  const cfg = tokens.arco?.solidTextOnDark;
  if (!cfg) return { errors: ['tokens.json 缺少 arco.solidTextOnDark'], rows: [], stats: { rules: rules.length } };
  const listed = new Set(cfg.selectors);
  const onBrand = tokens.colors.dark[cfg.token];
  const pal = { light: paletteHex(tokens, 'light'), dark: paletteHex(tokens, 'dark') };
  const min = tokens.contrast.minText;
  const rows = [];
  const needed = new Set();
  for (const r of rules) {
    for (const sel of r.selectors) {
      const disabled = /disabled/.test(sel); // 禁用控件按 WCAG 1.4.3 不要求对比度，仍按真实颜色记录
      const exempt = disabled ? '禁用态（WCAG 1.4.3 不要求）' : undefined;
      for (const theme of ['light', 'dark']) {
        const bg = pal[theme][r.palette][r.level - 1];
        const fgWhite = theme === 'dark' && r.white === 'var' ? blendWhite(bg, 0.9) : '#FFFFFF';
        const useInk = theme === 'dark' && listed.has(sel);
        const fg = useInk ? onBrand : fgWhite;
        const ratio = contrastRatio(fg, bg);
        rows.push({ sel, theme, bg, fg: useInk ? cfg.token : 'white', ratio, exempt });
        if (theme === 'dark' && !disabled && contrastRatio(fgWhite, bg) < min) needed.add(sel);
        if (ratio < min && !exempt) errors.push(`[${theme}] ${sel}：${useInk ? cfg.token : '白字'} on ${bg} = ${ratio.toFixed(2)}:1 < ${min}:1`);
      }
    }
  }
  const all = new Set(rules.flatMap((r) => r.selectors));
  for (const s of needed) if (!listed.has(s)) errors.push(`暗色下白字不合格的 Arco 规则未被覆盖：${s}（加入 tokens.json arco.solidTextOnDark.selectors）`);
  for (const s of listed) {
    if (!all.has(s)) errors.push(`arco.solidTextOnDark.selectors 里的 ${s} 在 Arco CSS 中不存在（死条目；Arco 升级后需同步）`);
    else if (!needed.has(s)) errors.push(`arco.solidTextOnDark.selectors 里的 ${s} 暗色下白字本就合格，不需要覆盖（或属禁用态）`);
  }
  return { errors, rows, stats: { rules: rules.length, selectors: all.size, overridden: listed.size } };
}
