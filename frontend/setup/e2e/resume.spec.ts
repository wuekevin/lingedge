import { expect, test } from '@playwright/test';
import { env, optEnv, shot } from './helpers';
import { fillDbForm, passGate, stepAdmin, stepComplete, stepDb, stepEnv, stepInit, stepKeys, stepRedis, stepSystem } from './flow';

// 中断续做（FR-11、ADR-026 §6）：阶段 A 做到一半，Go 侧 SIGKILL 并重启 server（进程内存全部丢失）；阶段 B 刷新后重新输入 token，从已持久化的进度继续。
const phase = optEnv('LE_E2E_PHASE');
const scenario = optEnv('LE_E2E_SCENARIO') || 'resume';

test('阶段 A（early）：只完成环境检查 / MySQL / Redis', async ({ page }) => {
  test.skip(phase !== 'a-early');
  await passGate(page, `${scenario}-a`);
  await stepEnv(page, `${scenario}-a`);
  await stepDb(page, `${scenario}-a`, 'tcp');
  await stepRedis(page, `${scenario}-a`);
  await expect(page.getByTestId('step-init')).toBeVisible();
});

test('阶段 B（early）：续做 → 数据库初始化要求重新提供迁移凭据（409）→ 回到 MySQL 步骤重新提交 → 完成全部', async ({ page }) => {
  test.skip(phase !== 'b-early');
  await passGate(page, `${scenario}-b`);
  // 进度由持久状态重建：MySQL 与 Redis 已完成；环境检查（内存态）需重新运行，所以落在第 1 步
  await expect(page.getByTestId('nav-database')).toBeEnabled();
  await expect(page.getByTestId('nav-redis')).toBeEnabled();
  await expect(page.getByTestId('step-environment')).toBeVisible();
  await expect(page.getByTestId('env-items')).toBeVisible();
  await shot(page, `${scenario}-b-01-resumed-env`);
  await page.getByTestId('nav-next').click();
  await expect(page.getByTestId('step-database')).toBeVisible();
  await expect(page.getByTestId('db-resume-hint')).toBeVisible();
  await shot(page, `${scenario}-b-02-db-resume-hint`);
  await page.getByTestId('nav-next').click(); // redis（已完成）
  await expect(page.getByTestId('step-redis')).toBeVisible();
  await page.getByTestId('nav-next').click();
  await expect(page.getByTestId('step-init')).toBeVisible();
  await page.getByTestId('init-run').click();
  await expect(page.getByTestId('error-code')).toHaveText('resource.conflict');
  await expect(page.locator('[data-le-request-id]')).toBeVisible();
  await shot(page, `${scenario}-b-03-init-409`);
  await page.getByTestId('init-goto-db').click();
  await expect(page.getByTestId('step-database')).toBeVisible();
  await fillDbForm(page, { dbName: env('LE_E2E_DB_NAME') }); // 临时管理员 ALTER USER 重置迁移账号口令
  await expect(page.getByTestId('db-ok')).toBeVisible();
  await page.getByTestId('nav-next').click();
  await page.getByTestId('nav-next').click(); // redis 已完成
  await stepInit(page, `${scenario}-b`);
  await stepAdmin(page, `${scenario}-b`, { totp: false });
  await stepSystem(page, `${scenario}-b`);
  await stepKeys(page, `${scenario}-b`);
  await stepComplete(page, `${scenario}-b`);
});

test('阶段 A（late）：完成到「创建管理员」', async ({ page }) => {
  test.skip(phase !== 'a-late');
  await passGate(page, `${scenario}-a`);
  await stepEnv(page, `${scenario}-a`);
  await stepDb(page, `${scenario}-a`, 'tcp');
  await stepRedis(page, `${scenario}-a`);
  await stepInit(page, `${scenario}-a`);
  await stepAdmin(page, `${scenario}-a`, { totp: false });
  await expect(page.getByTestId('step-system')).toBeVisible();
});

test('阶段 B（late）：重启后从「基础配置」继续并完成（token 不保留，需重新输入）', async ({ page }) => {
  test.skip(phase !== 'b-late');
  await passGate(page, `${scenario}-b`);
  for (const k of ['database', 'redis', 'init_db', 'admin']) await expect(page.getByTestId(`nav-${k}`)).toBeEnabled(); // 已完成的步骤可回看
  await expect(page.getByTestId('nav-system')).toBeEnabled();
  await expect(page.getByTestId('nav-keys')).toBeDisabled(); // 未完成的后续步骤不可跳过
  // 环境检查先行（内存态），随后依次 Next 到「基础配置」
  await stepEnv(page, `${scenario}-b`);
  for (const id of ['step-database', 'step-redis', 'step-init', 'step-admin']) {
    await expect(page.getByTestId(id)).toBeVisible();
    await page.getByTestId('nav-next').click();
  }
  await shot(page, `${scenario}-b-01-resumed-system`);
  await stepSystem(page, `${scenario}-b`);
  await stepKeys(page, `${scenario}-b`);
  await stepComplete(page, `${scenario}-b`);
});
