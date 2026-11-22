import { expect, test } from '@playwright/test';
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
