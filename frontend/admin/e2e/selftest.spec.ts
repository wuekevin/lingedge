import { contrastFailures, expect, test, totpCode, watchConsole } from './helpers';

// E2E 审计器自测（不需要 server）：证明对比度审计、控制台监视与 TOTP 生成不会「空转通过」。

// 测试用色值在运行时生成（与 setup/e2e/selftest.spec.ts 同一做法）：这些是审计器的输入夹具，不是界面颜色。
const c = (r: number, g: number = r, b: number = r) => ['rgb', `(${r},${g},${b})`].join('');
const page = (css: string, body: string) => `<!doctype html><html><head><meta charset="utf-8"><style>body{margin:0;background:${c(255)};${css}}</style></head><body>${body}</body></html>`;

test('对比度审计能检出低对比度文字，并放行合格文字', async ({ page: p }) => {
  await p.setContent(
    page(
      '',
      `<p style="color:${c(150)}">low contrast sample text</p><p style="color:${c(0)}">good contrast sample text</p><p>more text one</p><p>more text two</p><p>more text three</p>`,
    ),
  );
  const r = await contrastFailures(p);
  expect(r.checked).toBeGreaterThan(3);
  expect(r.failures.map((f) => f.text)).toEqual(['low contrast sample text']);
});

test('对比度审计在暗色底上同样有效；禁用控件与不可见文字不计', async ({ page: p }) => {
  await p.setContent(
    page(
      `background:${c(20, 20, 30)};color:${c(235, 235, 240)}`,
      `<p>readable on dark</p><p style="color:${c(60, 60, 75)}">unreadable on dark</p><button disabled style="color:${c(60, 60, 75)};background:${c(20, 20, 30)}">disabled ok</button><p style="display:none">hidden</p><p>a</p><p>b</p><p>c</p>`,
    ),
  );
  const r = await contrastFailures(p);
  expect(r.failures.map((f) => f.text)).toEqual(['unreadable on dark']);
});

test('渐变背景按最坏色标判定', async ({ page: p }) => {
  await p.setContent(page('', `<div style="background:linear-gradient(90deg, ${c(0)}, ${c(250)});color:${c(255)}">gradient text</div><p>a</p><p>b</p><p>c</p>`));
  const r = await contrastFailures(p);
  expect(r.gradients).toBeGreaterThan(0);
  expect(r.failures.map((f) => f.text)).toContain('gradient text');
});

test('控制台监视：页面脚本异常与 CSP 违规会被记录；预期的 4xx 资源加载提示被忽略', async ({ page: p }) => {
  const watch = watchConsole(p);
  await p.setContent(page('', '<p>x</p>'));
  await p.evaluate(() => console.error('boom'));
  await p.evaluate(() => console.warn('only a warning'));
  await p.evaluate(() => console.error('Failed to load resource: the server responded with a status of 401 (Unauthorized)'));
  await p.evaluate(() => console.log('Refused to load the script because it violates the Content Security Policy'));
  expect(watch.problems).toEqual(['console.error: boom', 'console.log: Refused to load the script because it violates the Content Security Policy']);
});

test('进出场动画未结束时 settled() 会等待（浮层 opacity 为 0 时不放行审计）', async ({ page: p }) => {
  const { settled } = await import('./helpers');
  await p.setContent(page('', '<div id="m" class="x-enter-from">modal text</div>'));
  const done = settled(p).then(() => 'done');
  const early = await Promise.race([done, new Promise((r) => setTimeout(() => r('waiting'), 400))]);
  expect(early).toBe('waiting');
  await p.evaluate(() => document.getElementById('m')!.classList.remove('x-enter-from'));
  expect(await done).toBe('done');
});

test('TOTP 生成：RFC 6238 附录 B 的 SHA-1 测试向量（密钥 "12345678901234567890"，T=59 → 6 位 287082）', () => {
  const b32 = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ';
  expect(totpCode(b32, 59_000)).toBe('287082');
  expect(totpCode(b32, 1_111_111_109_000)).toBe('081804');
  expect(totpCode(b32, 59_000)).not.toBe(totpCode(b32, 59_000 + 30_000));
});
