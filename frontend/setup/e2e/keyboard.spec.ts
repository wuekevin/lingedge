import { expect, test, type Page } from '@playwright/test';
import { env, shot } from './helpers';

// 键盘可操作（AC2）：只用 Tab / Shift+Tab / Enter / Space 走完「令牌 → 环境检查 → MySQL 表单」，焦点可见、顺序符合视觉顺序、焦点随步骤切换。
const active = (page: Page) => page.evaluate(() => (document.activeElement as HTMLElement | null)?.getAttribute('data-testid') ?? document.activeElement?.id ?? document.activeElement?.tagName ?? '');

async function tabUntil(page: Page, target: string, max = 40): Promise<string[]> {
  const seen: string[] = [];
  for (let i = 0; i < max; i++) {
    await page.keyboard.press('Tab');
    const a = await active(page);
    seen.push(a);
    if (a === target) return seen;
  }
  throw new Error(`Tab ${max} 次仍未到达 ${target}；经过：${seen.join(' → ')}`);
}

test('仅用键盘完成令牌验证并进入表单；焦点可见且随步骤移动', async ({ page }) => {
  await page.goto('/setup');
  await expect(page.getByTestId('gate')).toBeVisible();
  // 第一个 Tab 停在“跳到主要内容”链接；它获得焦点时可见
  await page.keyboard.press('Tab');
  expect(await page.evaluate(() => document.activeElement?.className)).toContain('skip');
  expect((await page.locator('a.skip').boundingBox())?.y ?? -1).toBeGreaterThanOrEqual(0);
  // 到达令牌输入框；焦点环可见（outline 不是 none）
  await tabUntil(page, 'f-setup_token');
  const outline = await page.evaluate(() => {
    const cs = getComputedStyle(document.activeElement as Element);
    return { style: cs.outlineStyle, width: Number.parseFloat(cs.outlineWidth) };
  });
  expect(outline.style).not.toBe('none');
  expect(outline.width).toBeGreaterThanOrEqual(2);
  await page.keyboard.type(env('LE_E2E_TOKEN'));
  await shot(page, 'keyboard-00-gate-focus');
  await page.keyboard.press('Enter'); // 回车提交表单
  await expect(page.getByTestId('stepper')).toBeVisible();
  // 焦点移到新步骤的标题
  await expect.poll(() => active(page)).toBe('step-environment-title');
  await expect(page.getByTestId('env-items')).toBeVisible();
  // Tab → 到“下一步”，Enter 进入 MySQL 步骤
  await tabUntil(page, 'nav-next');
  await page.keyboard.press('Enter');
  await expect.poll(() => active(page)).toBe('step-database-title');
  // 表单控件的 Tab 顺序与视觉顺序一致，并且全部可达
  const seen = await tabUntil(page, 'db-submit', 60);
  const order = ['r-mode-create', 'r-conn-tcp', 'f-host', 'f-port', 'f-database', 'f-password', 'c-tls', 'f-admin_username', 'f-admin_password'];
  let cursor = -1;
  for (const id of order) {
    const idx = seen.indexOf(id);
    expect(idx, `Tab 序列里应包含 ${id}；实际：${seen.join(' → ')}`).toBeGreaterThan(cursor);
    cursor = idx;
  }
  await shot(page, 'keyboard-01-db-focus');
  // 空表单回车：本地校验出错并把焦点交给第一个无效字段，不发请求
  await page.keyboard.press('Enter');
  await expect(page.getByTestId('f-admin_username')).toBeFocused();
  await expect(page.locator('[aria-invalid="true"]').first()).toBeVisible();
  // 复选框用空格切换
  await page.getByTestId('c-tls').focus();
  await page.keyboard.press('Space');
  await expect(page.getByTestId('c-tls')).toBeChecked();
  // Shift+Tab 回到上一个控件
  await page.keyboard.press('Shift+Tab');
  await expect.poll(() => active(page)).toBe('f-password');
});
