import fs from 'node:fs';
import { describe, expect, it } from 'vitest';
import { auditArcoSolidText, whiteOnPaletteRules } from './lib/arco-audit.mjs';
import { arcoScale, renderArcoTheme } from './lib/tokens.mjs';
import { contrastRatio } from './lib/color.mjs';
import { ARCO_CSS, TOKENS_JSON } from './lib/paths.mjs';

const tokens = () => JSON.parse(fs.readFileSync(TOKENS_JSON, 'utf8'));
const arco = () => fs.readFileSync(ARCO_CSS, 'utf8');

describe('Arco 实心控件对比度审计（Story 1.37b）', () => {
  it('真实 Arco CSS + 真实令牌：通过，且覆盖了暗色下的主按钮', () => {
    const { errors, stats } = auditArcoSolidText(tokens(), arco());
    expect(errors).toEqual([]);
    expect(stats.rules).toBeGreaterThan(20);
    expect(tokens().arco.solidTextOnDark.selectors).toContain('.arco-btn-primary');
  });

  it('生成的 arco-theme.css 对暗色两种作用域都输出覆盖，且只写 color、无 !important', () => {
    const css = renderArcoTheme(tokens());
    expect(css).toContain("body[arco-theme='dark'] .arco-btn-primary,");
    expect(css).toContain(":root[data-theme='dark'] body .arco-btn-primary");
    const block = css.slice(css.indexOf('/* 暗色主题：Arco 实心控件'));
    expect(block).toContain('color: var(--le-color-on-brand);');
    expect(block).not.toContain('!important');
    expect(block.match(/\{[^}]*\}/g)).toHaveLength(1);
  });

  it('白字 + 调色板底色的规则被识别（含 var(--color-white) 与禁用态）', () => {
    const r = whiteOnPaletteRules('.a{color:#fff;background-color:rgb(var(--primary-6))} .b,.c:hover{color:var(--color-white);background-color:var(--color-danger-light-3)} .d{color:#fff;background:rgba(0,0,0,.5)}');
    expect(r.map((x) => [x.selectors.join('|'), x.palette, x.level])).toEqual([
      ['.a', 'primary', 6],
      ['.b|.c:hover', 'danger', 3],
    ]);
  });

  it('负向：从覆盖清单删掉一个暗色下不合格的选择器 → 失败', () => {
    const t = tokens();
    t.arco.solidTextOnDark.selectors = t.arco.solidTextOnDark.selectors.filter((s) => s !== '.arco-btn-primary');
    expect(auditArcoSolidText(t, arco()).errors.some((e) => e.includes('.arco-btn-primary') && e.includes('未被覆盖'))).toBe(true);
  });

  it('负向：清单里有 Arco 中不存在的选择器（死条目）→ 失败', () => {
    const t = tokens();
    t.arco.solidTextOnDark.selectors.push('.arco-btn-ghost-nope');
    expect(auditArcoSolidText(t, arco()).errors.some((e) => e.includes('.arco-btn-ghost-nope') && e.includes('不存在'))).toBe(true);
  });

  it('负向：Arco 升级新增一条白字+调色板底色规则而清单没跟上 → 失败', () => {
    const css = arco() + '\n.arco-new-solid{color:#fff;background-color:rgb(var(--primary-6))}\n';
    expect(auditArcoSolidText(tokens(), css).errors.some((e) => e.includes('.arco-new-solid'))).toBe(true);
  });

  it('负向：覆盖色选了对比度不足的令牌 → 失败', () => {
    const t = tokens();
    t.arco.solidTextOnDark.token = 'text-primary'; // 浅色字放在明亮底上
    expect(auditArcoSolidText(t, arco()).errors.some((e) => e.includes('text-primary'))).toBe(true);
  });

  it('负向：亮色主题的悬停底色过浅（白字不足）→ 色阶函数会收紧 5 级', () => {
    // 不收紧时 #3B4FD8 向白混 20% 得 4.2:1；arcoScale 带 solidText 时取 ≥4.5 的占比。
    const loose = arcoScale('#3B4FD8', '#2F42C4', '#FFFFFF', '#000000');
    const tight = arcoScale('#3B4FD8', '#2F42C4', '#FFFFFF', '#000000', '#FFFFFF');
    expect(contrastRatio('#FFFFFF', loose[4])).toBeLessThan(4.5);
    expect(contrastRatio('#FFFFFF', tight[4])).toBeGreaterThanOrEqual(4.5);
  });
});
