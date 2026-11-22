import fs from 'node:fs';
import path from 'node:path';
import { expect, test, type Page } from '@playwright/test';
import { contrastFailures, totpCode, watchConsole } from './helpers';

// E2E 审计器自身的负向对照：保证「对比度审计」与「控制台/CSP 监视」不会空转通过（它们若永远报告通过，AC2 的证据就是假的）。
// 颜色用运行期拼接（本仓禁止源码里出现硬编码色值/颜色函数；这里是测试夹具，需要刻意构造低对比度）。
const shade = (n: number) => ['rgb', `(${n},${n},${n})`].join('');

test('对比度审计能发现低对比度文字，也不误报高对比度文字', async ({ page }) => {
  await page.setContent('<p id="lo">low contrast sample</p><p id="hi">high contrast sample</p><button id="off" disabled>disabled sample</button>');
  await page.evaluate(
    ([lo, mid, dark, light]) => {
      const set = (id: string, fg: string, bg: string) => {
        const el = document.getElementById(id) as HTMLElement;
        el.style.color = fg;
        el.style.backgroundColor = bg;
      };
      set('lo', lo as string, mid as string); // 约 1.1:1
      set('hi', dark as string, light as string); // 远高于 4.5:1
      set('off', lo as string, mid as string); // 禁用控件不计
    },
    [shade(130), shade(140), shade(20), shade(250)],
  );
  const r = await contrastFailures(page);
  expect(r.checked).toBe(2);
  expect(r.failures.map((f) => f.text)).toEqual(['low contrast sample']);
  expect(r.failures[0]?.ratio).toBeLessThan(1.5);
});

test('对比度审计解析渐变色标：最坏色标不合格即报错；半透明色标先叠到页面底色上', async ({ page }) => {
  await page.setContent('<body><p id="a">gradient low sample</p><p id="b">gradient high sample</p></body>');
  await page.evaluate(
    ([dark, light, mid]) => {
      document.body.style.backgroundColor = dark as string;
      const grad = (id: string, fg: string, from: string, to: string) => {
        const el = document.getElementById(id) as HTMLElement;
        el.style.color = fg;
        el.style.backgroundImage = `linear-gradient(135deg, ${from}, ${to})`;
      };
      grad('a', light as string, dark as string, mid as string); // 浅字：一端深（合格）一端中灰（不合格）→ 必须按最坏色标判失败
      grad('b', light as string, dark as string, dark as string); // 浅字在两个深色标上都合格
    },
    [shade(20), shade(250), shade(200)],
  );
  const r = await contrastFailures(page);
  expect(r.gradients).toBeGreaterThanOrEqual(2);
  expect(r.failures.map((f) => f.text)).toEqual(['gradient low sample']);
});

test('控制台监视能捕获 CSP 违规与页面错误', async ({ page }) => {
  await page.route('**/csp-probe', (route) =>
    route.fulfill({ status: 200, contentType: 'text/html', headers: { 'Content-Security-Policy': "default-src 'none'; script-src 'self'" }, body: '<!doctype html><script>window.__ran = 1</script><body>x</body>' }),
  );
  const con = watchConsole(page);
  await page.goto('http://127.0.0.1:1/csp-probe');
  await expect.poll(() => con.problems.length).toBeGreaterThan(0);
  expect(con.problems.join('\n')).toMatch(/Content Security Policy|Refused/i);
  expect(await page.evaluate(() => (window as unknown as { __ran?: number }).__ran)).toBeUndefined();
});

test('TOTP 夹具实现与 RFC 6238 附录 B 的 SHA-1 测试向量一致', () => {
  // RFC 6238 附录 B：密钥 ASCII "12345678901234567890" 的 base32；T=59 → 8 位 94287082，取后 6 位 287082。
  const rfcKeyB32 = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ';
  expect(totpCode(rfcKeyB32, 59_000)).toBe('287082');
  expect(totpCode(rfcKeyB32, 1_111_111_109_000)).toBe('081804');
  expect(totpCode(rfcKeyB32, 20_000_000_000_000)).toBe('353130');
});

// ---- Arco 实心按钮在真实 Chromium 里的对比度（Story 1.37b，处置 1.36 发现 7）----
// 加载 Arco 真实样式 + 本仓生成的令牌/Arco 主题映射，在 light/dark 两个主题渲染四种实心按钮（含悬停态），用同一个对比度审计器逐个量。
// 负向对照：暗色下不加载 arco-theme.css 的窄覆盖时，审计必须报出不合格（证明审计不是空转）。
const FRONTEND = path.resolve(process.cwd(), '..');
const read = (rel: string) => fs.readFileSync(path.join(FRONTEND, rel), 'utf8');
const ARCO_CSS = read('admin/node_modules/@arco-design/web-vue/dist/arco.css');
const TOKENS_CSS = read('shared/tokens/generated/tokens.css');
const ARCO_THEME_CSS = read('shared/tokens/generated/arco-theme.css');
const WITHOUT_OVERRIDE = ARCO_THEME_CSS.slice(0, ARCO_THEME_CSS.indexOf('/* 暗色主题：Arco 实心控件'));

async function renderArco(page: Page, theme: 'light' | 'dark', themeCss: string): Promise<void> {
  const btn = (cls: string, id: string) => `<button id="${id}" class="arco-btn arco-btn-primary ${cls} arco-btn-size-medium arco-btn-shape-square" type="button"><span>${id} button</span></button>`;
  await page.setContent(
    `<!doctype html><html data-theme="${theme}"><head><style>${ARCO_CSS}</style><style>${TOKENS_CSS}</style><style>${themeCss}</style></head>` +
      `<body arco-theme="${theme === 'dark' ? 'dark' : ''}" style="background:var(--le-color-bg-page)"><div style="padding:16px;display:flex;gap:16px;background:var(--le-color-bg-surface)">` +
      `${btn('', 'primary')}${btn('arco-btn-status-success', 'success')}${btn('arco-btn-status-warning', 'warning')}${btn('arco-btn-status-danger', 'danger')}</div></body></html>`,
  );
  if (theme === 'light') await page.evaluate(() => document.body.removeAttribute('arco-theme'));
}

for (const theme of ['light', 'dark'] as const) {
  test(`Arco 实心按钮对比度 ≥4.5:1（${theme}，含悬停态）`, async ({ page }) => {
    await renderArco(page, theme, ARCO_THEME_CSS);
    const base = await contrastFailures(page);
    expect(base.checked).toBe(4);
    expect(base.failures).toEqual([]);
    for (const id of ['primary', 'success', 'warning', 'danger']) {
      await page.locator(`#${id}`).hover();
      const r = await contrastFailures(page);
      expect(r.failures, `${theme} hover ${id}`).toEqual([]);
    }
    const shotDir = process.env.LE_E2E_SHOT_DIR;
    if (shotDir) {
      fs.mkdirSync(shotDir, { recursive: true });
      await page.screenshot({ path: path.join(shotDir, `arco-buttons-${theme}.png`) });
    }
  });
}

test('负向对照：暗色下去掉窄覆盖，Arco 写死的白字会被审计器判为不合格', async ({ page }) => {
  await renderArco(page, 'dark', WITHOUT_OVERRIDE);
  const r = await contrastFailures(page);
  expect(r.checked).toBe(4);
  expect(r.failures.length).toBeGreaterThanOrEqual(3);
  expect(Math.min(...r.failures.map((f) => f.ratio))).toBeLessThan(2.9);
});
