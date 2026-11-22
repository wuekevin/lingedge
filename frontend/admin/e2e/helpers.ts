import fs from 'node:fs';
import path from 'node:path';
import { createHmac } from 'node:crypto';
import { expect, test as base, type BrowserContext, type Page } from '@playwright/test';

export function env(name: string): string {
  const v = process.env[name];
  if (v === undefined || v === '') throw new Error(`missing environment variable ${name}: run the E2E through make e2e (the Go side starts the real environment)`);
  return v;
}
export const optEnv = (name: string): string => process.env[name] ?? '';
export const prodUrl = (): string => `http://127.0.0.1:${env('LE_E2E_PORT_PROD')}`;
export const fxUrl = (): string => `http://127.0.0.1:${env('LE_E2E_PORT_FX')}`;

// 每个测试用独立的来源 IP（经反代的 X-Forwarded-For；server 信任本机反代）：登录节流按 IP+账号计数，互不影响。
let ipSeq = 10 + Math.floor(Math.random() * 100);
const nextIp = (): string => `203.0.113.${(ipSeq++ % 240) + 10}`;

/** 带独立来源 IP 的 test：context 级额外请求头。 */
export const test = base.extend<{ context: BrowserContext }>({
  context: async ({ context }, use) => {
    await context.setExtraHTTPHeaders({ 'X-Forwarded-For': nextIp() });
    await use(context);
  },
});
export { expect };

// ---- 截图（目录由 Go 侧给出，不在仓库内）----
export async function shot(page: Page, name: string): Promise<void> {
  const dir = optEnv('LE_E2E_SHOT_DIR');
  if (!dir) return;
  fs.mkdirSync(dir, { recursive: true });
  // 不用 fullPage：整页截图会临时改视口尺寸，触发 resize，Arco 的浮层（下拉菜单、对话框）随之关闭，截到的就不是被审计的状态。
  await page.screenshot({ path: path.join(dir, `${name}.png`) });
}

// ---- 控制台与 CSP：console error / pageerror / CSP 违规都算失败。接口返回 4xx/5xx 时浏览器自己会打印「Failed to load resource」——
// 那是被测的预期错误响应（如错口令 401），不算页面脚本错误，予以忽略；CSP 违规与脚本异常不豁免。----
export function watchConsole(page: Page): { problems: string[] } {
  const problems: string[] = [];
  page.on('console', (m) => {
    const text = m.text();
    if (/^Failed to load resource: the server responded with a status of 4\d\d/.test(text)) return;
    if (m.type() === 'error' || /Content Security Policy|Refused to/i.test(text)) problems.push(`console.${m.type()}: ${text}`);
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

/** 等待浮层/抽屉的进出场动画真正结束：Vue Transition 在第一帧带 *-enter-from（此时 getAnimations() 还是空的），只看 getAnimations 会在浮层仍是 opacity 0 时就放行。 */
export async function settled(page: Page): Promise<void> {
  await page.waitForFunction(() => !document.querySelector('[class*="-enter-from"], [class*="-enter-active"], [class*="-leave-from"], [class*="-leave-active"]') && document.getAnimations().length === 0, undefined, { timeout: 10_000 });
}

export async function expectContrast(page: Page, label: string): Promise<void> {
  await settled(page);
  const { failures, checked } = await contrastFailures(page);
  expect(checked, `${label}: no text was audited (the audit ran empty)`).toBeGreaterThan(3);
  expect(failures, `${label}: text below 4.5:1 contrast`).toEqual([]);
}

export type Scheme = 'light' | 'dark';
export async function expectTheme(page: Page, scheme: Scheme): Promise<void> {
  await expect(page.locator('html')).toHaveAttribute('data-theme', scheme);
  const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const lum = (bg.match(/[\d.]+/g) ?? []).slice(0, 3).map(Number).reduce((x, y) => x + y, 0) / 3;
  if (scheme === 'dark') expect(lum, `dark theme body background ${bg}`).toBeLessThan(60);
  else expect(lum, `light theme body background ${bg}`).toBeGreaterThan(200);
}

// ---- 登录辅助 ----
/** 用 API 建立会话（不经 UI；UI 登录流程在 login.spec 单独覆盖）。恢复码是单次有效的，TOTP 码受 time-step 重放保护，所以管理员会话一律用恢复码建立。 */
export async function apiLogin(page: Page, base: string, email: string, secret: string, recoveryCode?: string): Promise<void> {
  const req = page.context().request;
  const r = await req.post(`${base}/api/v1/auth/login`, { data: { realm: 'admin', email, password: secret } });
  expect(r.status(), 'login').toBe(200);
  const body = (await r.json()) as { status: string; mfa_token?: string };
  if (body.status === 'totp_required') {
    expect(recoveryCode, 'admin has TOTP: a recovery code is required').toBeTruthy();
    const v = await req.post(`${base}/api/v1/auth/totp/verify`, { data: { mfa_token: body.mfa_token, code: recoveryCode } });
    expect(v.status(), 'recovery code verify').toBe(200);
  } else expect(body.status).toBe('ok');
}
export const recoveryCodes = (): string[] => env('LE_E2E_RECOVERY_CODES').split(',');

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
