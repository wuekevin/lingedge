import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { checkSetupCodes, checkKeys, codesUsedIn, parseCodeTable } from './lib/setup-codes.mjs';
import { FRONTEND_ROOT, REPO_ROOT } from './lib/paths.mjs';

const dirs = [];
afterEach(() => {
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});

const FILES = [
  'docs/api/openapi.yaml',
  'internal/platform/apierr/codes.go',
  'internal/auth/internal/service/bootstrap.go',
  'frontend/setup/src/api/errors.ts',
  'frontend/setup/src/api/servermsg.ts',
  'frontend/setup/src/locales/zh-CN.json',
  'frontend/setup/src/locales/en.json',
];
/** 把真实仓库里对账所需的文件复制到临时树，便于逐个制造缺失。 */
function clone() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'le-setupcodes-'));
  dirs.push(root);
  const put = (rel) => {
    const to = path.join(root, rel);
    fs.mkdirSync(path.dirname(to), { recursive: true });
    fs.copyFileSync(path.join(REPO_ROOT, rel), to);
  };
  for (const f of FILES) put(f);
  const setupDir = 'internal/platform/setup';
  for (const f of fs.readdirSync(path.join(REPO_ROOT, setupDir))) if (f.endsWith('.go') && !f.endsWith('_test.go')) put(`${setupDir}/${f}`);
  return { root, frontend: path.join(root, 'frontend') };
}
const edit = (file, fn) => fs.writeFileSync(file, fn(fs.readFileSync(file, 'utf8')));

describe('向导消息对账（check:setup-codes）', () => {
  it('真实仓库：通过，且推导出了预期的码/检查项', () => {
    const { errors, stats } = checkSetupCodes(FRONTEND_ROOT);
    expect(errors).toEqual([]);
    expect(stats.codes).toBeGreaterThanOrEqual(16);
    expect(stats.checks).toBe(8);
  });

  it('推导：setup 包用到的码包含便捷构造隐含的码', () => {
    const table = parseCodeTable('\tSetupModeActive Code = "setup.mode_active"\n\tInternalError Code = "internal.error"\n');
    const used = codesUsedIn('apierr.New(apierr.SetupModeActive, "x"); apierr.Validation(d); apierr.NotFound()', table);
    expect([...used].sort()).toEqual(['resource.not_found', 'setup.mode_active', 'validation.failed']);
    expect(checkKeys('add("a.b", true, "info", "x")\nadd("c", false, "warn", "y")')).toEqual(['a.b', 'c']);
  });

  it('负向：删除任一语言的错误码词条 → 失败', () => {
    const { root, frontend } = clone();
    edit(path.join(frontend, 'setup/src/locales/en.json'), (s) => {
      const j = JSON.parse(s);
      delete j.errors.code.setup.mode_active;
      return JSON.stringify(j);
    });
    const { errors } = checkSetupCodes(frontend);
    expect(errors.some((e) => e.includes('en.json') && e.includes('errors.code.setup.mode_active'))).toBe(true);
    expect(root).toBeTruthy();
  });

  it('负向：codes.go 新增 setup.* 错误码而前端没有词条 → 失败', () => {
    const { root, frontend } = clone();
    edit(path.join(root, 'internal/platform/apierr/codes.go'), (s) => s.replace('\t// InternalError 内部错误。', '\tSetupBrandNew Code = "setup.brand_new"\n\t// InternalError 内部错误。'));
    const { errors } = checkSetupCodes(frontend);
    expect(errors.some((e) => e.includes('errors.code.setup.brand_new'))).toBe(true);
  });

  it('负向：setup 包引用了某个新的业务错误码 → 失败', () => {
    const { root, frontend } = clone();
    fs.appendFileSync(path.join(root, 'internal/platform/setup/steps_db.go'), '\nvar _ = apierr.TenantQuotaExceeded\n');
    const { errors } = checkSetupCodes(frontend);
    expect(errors.some((e) => e.includes('errors.code.tenant.quota_exceeded'))).toBe(true);
  });

  it('负向：新增环境检查项而句式表/标签没有 → 失败', () => {
    const { root, frontend } = clone();
    fs.appendFileSync(path.join(root, 'internal/platform/setup/steps_db.go'), '\nfunc _x(add func(string, bool, string, string)) { add("disk.space", true, "info", "x") }\n');
    const { errors } = checkSetupCodes(frontend);
    expect(errors.some((e) => e.includes('servermsg.ts') && e.includes('disk.space'))).toBe(true);
    expect(errors.some((e) => e.includes('env.item.disk.space'))).toBe(true);
  });

  it('负向：句式表引用的词条 id 缺失 → 失败', () => {
    const { frontend } = clone();
    edit(path.join(frontend, 'setup/src/locales/zh-CN.json'), (s) => {
      const j = JSON.parse(s);
      delete j.serverMsg.step.keys_ok;
      return JSON.stringify(j);
    });
    const { errors } = checkSetupCodes(frontend);
    expect(errors.some((e) => e.includes('serverMsg.step.keys_ok'))).toBe(true);
  });
});
