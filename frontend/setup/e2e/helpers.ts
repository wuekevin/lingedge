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

// ---- 对比度（WCAG 2.x）：遍历页面上可见文字，逐个取有效前景/背景色；渐变背景（品牌侧栏）由令牌对比度脚本覆盖，这里跳过 ----
export interface ContrastFailure {
  text: string;
  ratio: number;
  fg: string;
  bg: string;
}
export async function contrastFailures(page: Page, min = 4.5): Promise<{ failures: ContrastFailure[]; checked: number }> {
  return page.evaluate((limit) => {
    const nums = (s: string) => (s.match(/[\d.]+/g) ?? []).map(Number);
    const lin = (c: number) => {
      const x = c / 255;
      return x <= 0.03928 ? x / 12.92 : Math.pow((x + 0.055) / 1.055, 2.4);
    };
    const lum = (c: number[]) => 0.2126 * lin(c[0] ?? 0) + 0.7152 * lin(c[1] ?? 0) + 0.0722 * lin(c[2] ?? 0);
    const effectiveBg = (el: Element): number[] | null => {
      let cur: Element | null = el;
      while (cur) {
        const cs = getComputedStyle(cur);
        if (cs.backgroundImage !== 'none') return null; // 渐变/图片背景：跳过
        const c = nums(cs.backgroundColor);
        if (c.length >= 3 && (c[3] ?? 1) > 0.99) return c;
        cur = cur.parentElement;
      }
      return [255, 255, 255];
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
      const bg = effectiveBg(el);
      if (!bg) continue;
      const fg = nums(cs.color);
      const L1 = lum(fg);
      const L2 = lum(bg);
      const ratio = (Math.max(L1, L2) + 0.05) / (Math.min(L1, L2) + 0.05);
      checked++;
      if (ratio < limit) failures.push({ text: t.slice(0, 40), ratio: Math.round(ratio * 100) / 100, fg: cs.color, bg: `${bg.join(',')}` });
    }
    return { failures, checked };
  }, min);
}

export async function expectContrast(page: Page, label: string): Promise<void> {
  const { failures, checked } = await contrastFailures(page);
  expect(checked, `${label}: no text was audited (the audit ran empty)`).toBeGreaterThan(3);
  expect(failures, `${label}: text below 4.5:1 contrast`).toEqual([]);
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
