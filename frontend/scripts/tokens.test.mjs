import fs from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import postcss from 'postcss';
import tailwind from 'tailwindcss';
import { describe, expect, it } from 'vitest';
import { contrastRatio, mix } from './lib/color.mjs';
import { evaluateContrast } from './lib/contrast.mjs';
import { diffTokensAgainstDesign, loadDesign } from './lib/design-md.mjs';
import { DESIGN_MD, FRONTEND_ROOT, TOKENS_JSON, TOKENS_OUT } from './lib/paths.mjs';
import { buildTailwindPreset, renderAll } from './lib/tokens.mjs';

const require = createRequire(import.meta.url);
const tokens = () => JSON.parse(fs.readFileSync(TOKENS_JSON, 'utf8'));
const clone = (o) => JSON.parse(JSON.stringify(o));

function walk(dir, out = []) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    if (e.name === 'node_modules' || e.name === 'generated') continue;
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else out.push(p);
  }
  return out;
}

describe('颜色工具', () => {
  it('对比度与 WCAG 参考值一致', () => {
    expect(contrastRatio('#000000', '#FFFFFF')).toBeCloseTo(21, 5);
    expect(contrastRatio('#FFFFFF', '#FFFFFF')).toBeCloseTo(1, 5);
    // DESIGN.md 声明：白字在 brand-primary 上 6.37
    expect(contrastRatio('#FFFFFF', '#3B4FD8')).toBeCloseTo(6.37, 2);
  });
  it('混色端点与中点', () => {
    expect(mix('#000000', '#FFFFFF', 0)).toBe('#000000');
    expect(mix('#000000', '#FFFFFF', 1)).toBe('#FFFFFF');
    expect(mix('#000000', '#FFFFFF', 0.5)).toBe('#808080');
  });
  it('非法色值抛错', () => {
    expect(() => contrastRatio('red', '#FFF')).toThrow();
  });
});

describe('对比度脚本（文字配对 ≥ 4.5:1）', () => {
  it('当前令牌全部通过', () => {
    const { failures, rows } = evaluateContrast(tokens());
    expect(failures).toEqual([]);
    expect(rows.length).toBeGreaterThan(60);
    expect(rows.filter((r) => r.kind === 'text' && !r.exception).every((r) => r.ratio >= 4.5)).toBe(true);
  });
  it('负向：把 text-secondary 调浅即失败（light 与 dark 都会被检出）', () => {
    const t = clone(tokens());
    t.colors.light['text-secondary'] = '#B7C0D4';
    t.colors.dark['text-secondary'] = '#3A4A7A';
    const { failures } = evaluateContrast(t);
    expect(failures.some((f) => f.startsWith('[light] text-secondary on bg-surface'))).toBe(true);
    expect(failures.some((f) => f.startsWith('[dark] text-secondary on bg-page'))).toBe(true);
  });
  it('负向：已知例外不能继续恶化（低于登记下限即失败）', () => {
    const t = clone(tokens());
    t.colors.light['text-disabled'] = '#C0C7D4';
    const { failures } = evaluateContrast(t);
    expect(failures.some((f) => f.includes('text-disabled') && f.includes('已知例外的下限'))).toBe(true);
  });
  it('负向：引用不存在的令牌 / 死例外配置', () => {
    const t = clone(tokens());
    t.contrast.text.push(['no-such-token', 'bg-page']);
    t.contrast.exceptions.push({ fg: 'info', bg: 'bg-sunken', themes: ['light'], min: 2, reason: 'x' });
    const { failures } = evaluateContrast(t);
    expect(failures.some((f) => f.includes('未知令牌 no-such-token'))).toBe(true);
    expect(failures.some((f) => f.includes('死配置'))).toBe(true);
  });
  it('图表序列（非文本 3:1）：把 chart-2 换成近白色即失败', () => {
    const t = clone(tokens());
    t.colors.dark['chart-2'] = '#0D152B';
    expect(evaluateContrast(t).failures.some((f) => f.startsWith('[dark] chart-2'))).toBe(true);
  });
});

describe('令牌结构与 DESIGN.md 一致性', () => {
  it('light / dark 键集一致（组件内无主题分支的前提）', () => {
    const t = tokens();
    expect(Object.keys(t.colors.dark).sort()).toEqual(Object.keys(t.colors.light).sort());
  });
  it('与 docs/ux/DESIGN.md frontmatter 无差异', () => {
    expect(diffTokensAgainstDesign(tokens(), loadDesign(DESIGN_MD))).toEqual([]);
  });
  it('负向：改一个色值 / 删一个键 / 改字号 都被检出', () => {
    const design = loadDesign(DESIGN_MD);
    const t = clone(tokens());
    t.colors.light['brand-primary'] = '#000000';
    delete t.colors.dark['info-bg'];
    t.typography.body.size = '15px';
    const diffs = diffTokensAgainstDesign(t, design);
    expect(diffs.some((d) => d.startsWith('colors.light.brand-primary'))).toBe(true);
    expect(diffs.some((d) => d.includes('colors.dark.info-bg'))).toBe(true);
    expect(diffs.some((d) => d.startsWith('typography.body.size'))).toBe(true);
  });
});

describe('生成物', () => {
  it('入库生成物与 tokens.json 逐字节一致（无漂移）', () => {
    for (const [name, content] of Object.entries(renderAll(tokens()))) {
      expect(fs.readFileSync(path.join(TOKENS_OUT, name), 'utf8'), name).toBe(content);
    }
  });
  it('tokens.css：亮/暗两套完整，暗色由 data-theme / arco-theme 双开关触发', () => {
    const css = fs.readFileSync(path.join(TOKENS_OUT, 'tokens.css'), 'utf8');
    const t = tokens();
    for (const k of Object.keys(t.colors.light)) {
      expect(css.match(new RegExp(`--le-color-${k}:`, 'g'))?.length, k).toBe(2);
    }
    expect(css).toContain(":root[data-theme='dark']");
    expect(css).toContain("[arco-theme='dark']");
  });
  it('shared 源码引用的每个 var(--le-*) 都在 tokens.css 中定义（防拼写错误）', () => {
    const css = fs.readFileSync(path.join(TOKENS_OUT, 'tokens.css'), 'utf8');
    const defined = new Set([...css.matchAll(/(--le-[a-z0-9-]+):/g)].map((m) => m[1]));
    const used = new Set();
    const roots = ['shared/src', 'admin/src', 'console/src'].map((r) => path.join(FRONTEND_ROOT, r));
    for (const root of roots.filter((r) => fs.existsSync(r))) {
      for (const f of walk(root).filter((p) => /\.(vue|css|ts)$/.test(p) && !p.endsWith('.gen.ts') && !p.endsWith('.test.ts'))) {
        for (const m of fs.readFileSync(f, 'utf8').matchAll(/var\((--le-[a-z0-9-]+)/g)) used.add(m[1]);
      }
    }
    expect(used.size).toBeGreaterThan(10);
    expect([...used].filter((v) => !defined.has(v))).toEqual([]);
  });
  it('Tailwind preset：不含任何色值，颜色全是 CSS 变量引用', () => {
    const js = fs.readFileSync(path.join(TOKENS_OUT, 'tailwind-preset.js'), 'utf8');
    expect(js).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(js).not.toMatch(/rgba?\(/);
  });
  it('Tailwind preset 经真实 tailwindcss 编译：令牌类生成对应 CSS 变量引用', async () => {
    const preset = buildTailwindPreset(tokens());
    const out = await postcss([
      tailwind({
        presets: [preset],
        content: [{ raw: '<div class="bg-le-bg-surface text-le-text-primary rounded-le-md p-le-4 shadow-le-overlay font-le-mono text-le-body bg-le-brand md:max-w-le-website">' }],
        corePlugins: { preflight: false },
      }),
    ]).process('@tailwind utilities;', { from: undefined });
    const css = out.css;
    expect(css).toContain('var(--le-color-bg-surface)');
    expect(css).toContain('var(--le-color-text-primary)');
    expect(css).toContain('var(--le-radius-md)');
    expect(css).toContain('var(--le-space-4)');
    expect(css).toContain('var(--le-shadow-overlay)');
    expect(css).toContain('var(--le-font-mono)');
    expect(css).toContain('var(--le-text-body-size)');
    expect(css).toContain('var(--le-gradient-brand)');
    expect(css).toContain('var(--le-layout-content-max-website)');
  });
});

describe('Arco 主题映射', () => {
  const arcoCss = fs.readFileSync(require.resolve('@arco-design/web-vue/dist/arco.css', { paths: [path.join(FRONTEND_ROOT, 'admin')] }), 'utf8');
  const theme = fs.readFileSync(path.join(TOKENS_OUT, 'arco-theme.css'), 'utf8');

  it('我们覆盖的每个变量都是 Arco 2.59 自身定义过的变量（兼容性证据，防止拼错名/Arco 改名）', () => {
    const arcoDefined = new Set([...arcoCss.matchAll(/(--[a-z0-9-]+):/g)].map((m) => m[1]));
    const ours = [...theme.matchAll(/^\s+(--[a-z0-9-]+):/gm)].map((m) => m[1]);
    expect(ours.length).toBeGreaterThan(60);
    expect(ours.filter((v) => !arcoDefined.has(v))).toEqual([]);
  });
  it('色阶是 Arco 的 "r,g,b" 三元组形式（Arco 以 rgb(var(--primary-6)) 取用），6 级等于品牌主色', () => {
    const light = theme.split("body[arco-theme='dark']")[0];
    expect(light).toMatch(/--primary-6: 59,79,216;/); // #3B4FD8
    expect(light).toMatch(/--primary-7: 47,66,196;/); // brand-primary-hover #2F42C4
    expect(arcoCss).toMatch(/rgb\(var\(--primary-6\)\)/);
    const dark = theme.split("body[arco-theme='dark']")[1];
    expect(dark).toMatch(/--primary-6: 124,147,255;/); // #7C93FF
  });
  it('暗色选择器与 Arco 的 body[arco-theme=dark] 同特异性，且不含 !important', () => {
    expect(arcoCss).toContain("body[arco-theme='dark']");
    expect(theme).toContain("body[arco-theme='dark']");
    expect(theme.replace(/\/\*[\s\S]*?\*\//g, '')).not.toContain('!important');
  });
});
