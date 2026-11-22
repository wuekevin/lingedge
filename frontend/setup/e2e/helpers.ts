import { createHmac } from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { expect, type Page } from '@playwright/test';

export function env(name: string): string {
  const v = process.env[name];
  if (v === undefined || v === '') throw new Error(`missing environment variable ${name}: run the E2E through make e2e (Go side browser_test.go starts the real environment)`);
  return v;
}
export const optEnv = (name: string): string => process.env[name] ?? '';

// ---- 结果输出：把页面里「只展示一次」的机密交给 Go 侧做泄漏扫描（日志/审计/状态/凭据文件里不得出现）----
export function writeOut(patch: Record<string, unknown>): void {
  const f = env('LE_E2E_OUT');
  const cur: Record<string, unknown> = fs.existsSync(f) ? (JSON.parse(fs.readFileSync(f, 'utf8')) as Record<string, unknown>) : {};
  fs.writeFileSync(f, JSON.stringify({ ...cur, ...patch }, null, 2));
}

// ---- 截图（目录由 Go 侧给出，不在仓库内）----
export async function shot(page: Page, name: string): Promise<void> {
  const dir = optEnv('LE_E2E_SHOT_DIR');
  if (!dir) return;
  fs.mkdirSync(dir, { recursive: true });
  await page.screenshot({ path: path.join(dir, `${name}.png`), fullPage: true });
}

// ---- 控制台与 CSP：任何 console error / pageerror / CSP 违规都算失败 ----
export function watchConsole(page: Page): { problems: string[] } {
  const problems: string[] = [];
  page.on('console', (m) => {
    if (m.type() === 'error' || /Content Security Policy|Refused to/i.test(m.text())) problems.push(`console.${m.type()}: ${m.text()}`);
  });
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`));
  return { problems };
}

// ---- 对比度（WCAG 2.x）：遍历页面上可见文字，逐个取有效前景/背景色。
// 渐变背景（品牌侧栏）：解析计算后的渐变色标（含 color-mix 产生的半透明色标，先叠到页面底色上），文字必须对每个色标都 ≥ 阈值（最坏情形）；图片背景无法判定，跳过。----
export interface ContrastFailure {
  text: string;
  ratio: number;
  fg: string;
  bg: string;
}
export async function contrastFailures(page: Page, min = 4.5): Promise<{ failures: ContrastFailure[]; checked: number; gradients: number }> {
  return page.evaluate((limit) => {
    const nums = (s: string) => (s.match(/[\d.]+/g) ?? []).map(Number);
    const lin = (c: number) => {
      const x = c / 255;
      return x <= 0.03928 ? x / 12.92 : Math.pow((x + 0.055) / 1.055, 2.4);
    };
    const lum = (c: number[]) => 0.2126 * lin(c[0] ?? 0) + 0.7152 * lin(c[1] ?? 0) + 0.0722 * lin(c[2] ?? 0);
    const over = (top: number[], below: number[]): number[] => {
      const a = top[3] ?? 1;
      return [0, 1, 2].map((i) => (top[i] ?? 0) * a + (below[i] ?? 0) * (1 - a));
    };
    /** 颜色字面量（rgb()/rgba()/color(srgb …)）→ [r,g,b,a]（0..255） */
    const colorsIn = (s: string): number[][] => {
      const out: number[][] = [];
      for (const m of s.matchAll(/(rgba?|color)\(([^)]*)\)/g)) {
        const v = nums(m[2] ?? '');
        if (m[1] === 'color') {
          // color(srgb r g b / a)：分量为 0..1
          if (!/srgb/.test(m[2] ?? '') || v.length < 3) continue;
          out.push([(v[0] ?? 0) * 255, (v[1] ?? 0) * 255, (v[2] ?? 0) * 255, v[3] ?? 1]);
        } else if (v.length >= 3) out.push([v[0] ?? 0, v[1] ?? 0, v[2] ?? 0, v[3] ?? 1]);
      }
      return out;
    };
    const pageBg = (() => {
      const c = nums(getComputedStyle(document.body).backgroundColor);
      return c.length >= 3 && (c[3] ?? 1) > 0.99 ? c : [255, 255, 255];
    })();
    let gradients = 0;
    /** 有效底色：纯色 → 一个；线性渐变 → 每个色标一个（叠在页面底色上）；其他图片 → null。 */
    const effectiveBgs = (el: Element): number[][] | null => {
      let cur: Element | null = el;
      while (cur) {
        const cs = getComputedStyle(cur);
        if (cs.backgroundImage !== 'none') {
          if (!/^linear-gradient/.test(cs.backgroundImage)) return null;
          const stops = colorsIn(cs.backgroundImage);
          if (stops.length < 2) return null;
          gradients++;
          return stops.map((c) => over(c, pageBg));
        }
        const c = nums(cs.backgroundColor);
        if (c.length >= 3 && (c[3] ?? 1) > 0.99) return [c];
        cur = cur.parentElement;
      }
      return [[255, 255, 255]];
    };
    const failures: { text: string; ratio: number; fg: string; bg: string }[] = [];
    let checked = 0;
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const t = (n.textContent ?? '').trim();
      const el = n.parentElement;
      if (!t || !el) continue;
      const cs = getComputedStyle(el);
      if (cs.visibility === 'hidden' || cs.display === 'none') continue;
      const r = el.getBoundingClientRect();
      if (r.width < 2 || r.height < 2 || el.closest('.le-sr-only')) continue;
      if (el.closest(':disabled') || (el as HTMLButtonElement).disabled) continue; // 禁用控件不在 WCAG 对比度要求内
      const bgs = effectiveBgs(el);
      if (!bgs) continue;
      const fg = nums(cs.color);
      let worst = Infinity;
      let worstBg: number[] = bgs[0] ?? [255, 255, 255];
      for (const bg of bgs) {
        const L1 = lum(over(fg, bg));
        const L2 = lum(bg);
        const ratio = (Math.max(L1, L2) + 0.05) / (Math.min(L1, L2) + 0.05);
        if (ratio < worst) {
          worst = ratio;
          worstBg = bg;
        }
      }
      checked++;
      if (worst < limit) failures.push({ text: t.slice(0, 40), ratio: Math.round(worst * 100) / 100, fg: cs.color, bg: `${worstBg.map((v) => Math.round(v)).join(',')}` });
    }
    return { failures, checked, gradients };
  }, min);
}

export async function expectContrast(page: Page, label: string): Promise<void> {
  const { failures, checked } = await contrastFailures(page);
  expect(checked, `${label}: no text was audited (the audit ran empty)`).toBeGreaterThan(3);
  expect(failures, `${label}: text below 4.5:1 contrast`).toEqual([]);
  // 每个对比度检查点同时核对：主题跟随系统偏好、服务端消息全部本地化（Story 1.37b）。
  await expectTheme(page);
  await expectNoUnlocalized(page, label);
}

// ---- 配色方案：E2E 以 Playwright 的 colorScheme 模拟系统偏好（LE_E2E_SCHEME=light|dark）；向导必须跟随，并在 <html data-theme> 上体现 ----
export const scheme = (): 'light' | 'dark' => (optEnv('LE_E2E_SCHEME') === 'dark' ? 'dark' : 'light');
export async function expectTheme(page: Page): Promise<void> {
  await expect(page.locator('html')).toHaveAttribute('data-theme', scheme());
  const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  // 页面底色必须真的随主题变化（暗色底的亮度远低于亮色底）
  const lum = (bg.match(/[\d.]+/g) ?? []).slice(0, 3).map(Number).reduce((a, b) => a + b, 0) / 3;
  if (scheme() === 'dark') expect(lum, `dark theme body background ${bg}`).toBeLessThan(60);
  else expect(lum, `light theme body background ${bg}`).toBeGreaterThan(200);
}

// ---- 服务端消息本地化（Story 1.37b）：真实 server 返回的全部错误码/检查项/回执都必须命中前端词条，没有任何一处回落到服务端原文 ----
export async function expectNoUnlocalized(page: Page, label: string): Promise<void> {
  const raw = await page.locator('[data-localized="false"]').allInnerTexts();
  expect(raw, `${label}: server text that fell back to the raw server message (add a phrase pattern)`).toEqual([]);
  if (optEnv('LE_E2E_LOCALE') === 'en') {
    // 英文界面：服务端消息区域（检查项、回执、错误说明与原因）不得出现中文
    const texts = await page.locator('[data-testid="check-msg"], [data-testid="server-message"], [data-testid="error-message"], [data-testid="error-reason"]').allInnerTexts();
    expect(texts.filter((t) => /[\u3400-\u9fff]/.test(t)), `${label}: Chinese text in the English UI`).toEqual([]);
  }
}

// ---- token 只在内存：页面存储、cookie、URL 里都不得出现 ----
export async function expectTokenNotPersisted(page: Page, token: string): Promise<void> {
  const dump = await page.evaluate(() => JSON.stringify({ s: { ...sessionStorage }, l: { ...localStorage }, c: document.cookie, u: location.href }));
  expect(dump).not.toContain(token);
  const cookies = await page.context().cookies();
  expect(JSON.stringify(cookies)).not.toContain(token);
}

// ---- TOTP（RFC 6238，SHA-1，30 秒，6 位）----
function base32Decode(s: string): Buffer {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const ch of s.replace(/=+$/, '').toUpperCase()) {
    const i = alphabet.indexOf(ch);
    if (i < 0) throw new Error('invalid base32 character');
    bits += i.toString(2).padStart(5, '0');
  }
  const bytes: number[] = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(Number.parseInt(bits.slice(i, i + 8), 2));
  return Buffer.from(bytes);
}
export function totpCode(secret: string, atMs = Date.now()): string {
  const counter = Math.floor(atMs / 1000 / 30);
  const msg = Buffer.alloc(8);
  msg.writeBigUInt64BE(BigInt(counter));
  const h = createHmac('sha1', base32Decode(secret)).update(msg).digest();
  const off = (h[h.length - 1] ?? 0) & 0x0f;
  const bin = (((h[off] ?? 0) & 0x7f) << 24) | (((h[off + 1] ?? 0) & 0xff) << 16) | (((h[off + 2] ?? 0) & 0xff) << 8) | ((h[off + 3] ?? 0) & 0xff);
  return String(bin % 1_000_000).padStart(6, '0');
}
