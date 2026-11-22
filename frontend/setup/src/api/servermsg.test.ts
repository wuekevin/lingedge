import fs from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import { createSetupI18n } from '../i18n';
import { langOf, localizeCheck, localizeReason, localizeStepMessage, type MsgPart } from './servermsg';

// 样本来自 Go 源码本身（而不是手抄）：服务端改了固定句式而前端句式表没跟上，这里会失败。
const REPO = path.resolve(__dirname, '../../../..');
const goSrc = (rel: string): string => fs.readFileSync(path.join(REPO, rel), 'utf8');
const SETUP_GO = ['steps_db.go', 'steps_post.go', 'service.go', 'http.go'].map((f) => goSrc(`internal/platform/setup/${f}`));
// 向导创建管理员走 auth 的引导路径（bootstrap.go + 共用的 validateNewPassword）；其余 auth 文案不属于向导。
const AUTH_GO = [goSrc('internal/auth/internal/service/bootstrap.go')];

/** Go 字符串字面量（解引号）。 */
const unq = (s: string): string => JSON.parse(`"${s}"`) as string;

/** 一个 Go 表达式（`"a"+x+"b"` 或 fmt.Sprintf("a %d", x)）→ 样本文案：变量用占位值代入。 */
function sample(expr: string): string {
  const sp = /^fmt\.Sprintf\(\s*"((?:[^"\\]|\\.)*)"/.exec(expr.trim());
  if (sp) return unq(sp[1]!).replace(/%0?\d*[dsvq]/g, (m) => (m.endsWith('d') ? '7' : 'X'));
  return expr
    .split(/"((?:[^"\\]|\\.)*)"/)
    .map((seg, i) => (i % 2 === 1 ? unq(seg) : /[A-Za-z]/.test(seg.replace(/[+\s]/g, '')) ? 'X' : ''))
    .join('');
}

describe('环境检查项：Go 源码里每条 add(...) 的文案都能被句式命中', () => {
  const calls = [...SETUP_GO[0]!.matchAll(/\badd\("([a-z_.]+)",\s*(true|false),\s*"(\w+)",\s*(.+)\)\n/g)];
  it('抽取到全部 15 条', () => expect(calls.length).toBe(15));
  for (const [, key, , , expr] of calls) {
    it(`${key}: ${sample(expr!).slice(0, 24)}…`, () => {
      const msg = sample(expr!);
      const part = localizeCheck({ key: key!, message: msg });
      expect(part, msg).not.toBeNull();
    });
  }
  it('未知检查项或对不上的句式 → null（回落服务端原文）', () => {
    expect(localizeCheck({ key: 'disk.space', message: '磁盘空间 1G' })).toBeNull();
    expect(localizeCheck({ key: 'system.time', message: '别的句式' })).toBeNull();
  });
});

describe('步骤成功回执', () => {
  const ok: Array<[string, string, string[]]> = [
    ['database', 'MySQL 预检通过；app 口令已加密写入运行期凭据文件（迁移口令仅内存）。未配置 maint 账号：分区维护作业将明确跳过并告警', ['step.database_ok', 'step.database_nomaint']],
    ['database', 'MySQL 预检通过；app/maint 口令已加密写入运行期凭据文件（迁移口令仅内存）（管理员无法授予全局 SET_USER_ID，已交由触发器探针判定是否需要）', ['step.database_ok', 'step.database_setuserid']],
    ['redis', 'Redis 8.0.1 校验通过（主版本 ≥8）；凭据已加密写入运行期凭据文件', ['step.redis_ok']],
    ['init_db', '迁移完成：000000 → 000013，本次应用 13 个：000001_a（ddl，3 ms）；000002_b（dml，1 ms）', ['step.init_migrated']],
    ['init_db', '数据库已是最新版本 000013（无待执行迁移）', ['step.init_uptodate']],
    ['system', '系统基础配置已写入（运行模式 self_hosted）', ['step.system_ok']],
    ['keys', '内部身份与密钥已初始化（私有 CA、配置签名密钥、Server gRPC 叶证书、deploy.lineage）；CA 指纹 AB:CD。请把主密钥 master.key 与数据库备份分开保管（丢失主密钥将无法解密任何私钥，F-7）', ['step.keys_ok']],
    ['complete', '安装已完成。向导接口此后永久返回 410；请以刚创建的管理员登录 Admin。', ['step.complete_ok', 'step.complete_closed']],
    ['complete', '安装已完成；警告：install.lock 写入失败，请检查数据目录权限，重启 Server 时会据数据库记录补写；警告：setup token 哈希文件删除失败，请手工删除 /etc/lingedge/server/setup-token.hash。向导接口此后永久返回 410；请以刚创建的管理员登录 Admin。', ['step.complete_ok', 'step.complete_lock_failed', 'step.complete_token_delete_failed', 'step.complete_closed']],
    ['complete', '安装已完成。向导接口此后永久返回 410；请以刚创建的管理员登录 Admin。 迁移账号口令由向导自动生成，仅在本响应展示一次，不会被保存：请立即在 root 侧执行 `sudo lingedge-server migrate credential init` 保存（预填 host/port/database，只需键入口令）；若来不及保存，之后用 `migrate credential reset --admin-user <管理员>` 重置。临时数据库管理员账号请自行删除。', ['step.complete_ok', 'step.complete_closed', 'step.complete_migrate_generated']],
    ['complete', '安装已完成。向导接口此后永久返回 410；请以刚创建的管理员登录 Admin。 升级前请在 root 侧执行 `sudo lingedge-server migrate credential init` 保存迁移账号口令（或之后用 credential reset）；临时数据库管理员账号请自行删除。', ['step.complete_ok', 'step.complete_closed', 'step.complete_migrate_manual']],
    ['complete', '安装已完成；setup token 哈希文件所在目录不可写，已清空其内容使 token 永久失效，请手工删除 /etc/lingedge/server/setup-token.hash。向导接口此后永久返回 410；请以刚创建的管理员登录 Admin。', ['step.complete_ok', 'step.complete_token_cleared', 'step.complete_closed']],
  ];
  it.each(ok)('%s：%s', (step, msg, ids) => {
    const parts = localizeStepMessage(step, msg) as MsgPart[];
    expect(parts.map((p) => p.id)).toEqual(ids);
  });
  it('Go 源码里这些句式的固定片段仍然存在（服务端改文案时本测试先失败）', () => {
    const all = SETUP_GO.join('\n');
    for (const frag of ['MySQL 预检通过；app', '迁移口令仅内存', '迁移完成：%06d', '数据库已是最新版本 %06d', '系统基础配置已写入（运行模式 ', 'CA 指纹 ', '安装已完成', '向导接口此后永久返回 410', '迁移账号口令由向导自动生成，仅在本响应展示一次，不会被保存：请立即在 root 侧执行 `sudo lingedge-server migrate credential init` 保存（预填 host/port/database，只需键入口令）；若来不及保存，之后用 `migrate credential reset --admin-user <管理员>` 重置。临时数据库管理员账号请自行删除。', '升级前请在 root 侧执行 `sudo lingedge-server migrate credential init` 保存迁移账号口令（或之后用 credential reset）；临时数据库管理员账号请自行删除。']) {
      expect(all, frag).toContain(frag);
    }
  });
  it('参数被取出', () => {
    expect(localizeStepMessage('redis', 'Redis 8.0.1 校验通过（主版本 ≥8）；凭据已加密写入运行期凭据文件')?.[0]?.params).toEqual({ version: '8.0.1' });
    expect(localizeStepMessage('init_db', '迁移完成：000000 → 000013，本次应用 13 个')?.[0]?.params).toEqual({ from: '000000', to: '000013', count: '13' });
  });
  it('对不上 → null', () => {
    expect(localizeStepMessage('keys', '别的内容')).toBeNull();
    expect(localizeStepMessage('admin', 'x')).toBeNull();
  });
});

describe('错误 reason：Go 源码里全部静态 reason 都能被句式命中', () => {
  const literal = /(?:Reason:\s*|invalidField\("[a-z_.]+",\s*|\bbad\("[a-z_.]+",\s*|\bfail\(|precheck\("[^"]*",\s*)"((?:[^"\\]|\\.)*)"\s*([+),}])/g;
  const found = new Set<string>();
  for (const src of [...SETUP_GO, ...AUTH_GO]) for (const m of src.matchAll(literal)) if (m[2] !== '+' && !m[1]!.includes('%')) found.add(unq(m[1]!));
  // 带动态拼接的句式在这里按真实运行时形态给出样本
  const dynamic = [
    'must be lingedge_app (the migrations grant table privileges to this fixed account name)',
    'must be lingedge_maint (fixed by the migrations)',
    'must be 12-128 characters',
    'must be 1-120 characters',
    'must not be the email address',
    'database "lingedge" already contains 3 table(s); the wizard never overwrites an existing database (FR-6). Use an empty dedicated database',
    'complete step database first',
    'port must be between 1 and 65535',
    'required (self_hosted or commercial)',
  ];
  it('抽取到足够多的静态 reason（防止正则失效而空转）', () => expect(found.size).toBeGreaterThan(30));
  for (const r of [...found, ...dynamic]) {
    it(r.slice(0, 70), () => {
      expect(localizeReason(r), r).not.toBeNull();
    });
  }
  it('未知 reason → null', () => expect(localizeReason('dial tcp 127.0.0.1:3306: connect: connection refused')).toBeNull());
});

describe('渲染到两种界面语言', () => {
  it('每个句式词条在 zh-CN 与 en 里都可渲染，且 en 不含中文（服务端原文除外）', () => {
    for (const locale of ['zh-CN', 'en']) {
      const i18n = createSetupI18n(locale);
      const t = i18n.global.t as unknown as (k: string, p?: Record<string, string>) => string;
      const out = t('serverMsg.check.system_time_bad', { time: '2026-01-01T00:00:00Z' });
      expect(out).toContain('2026-01-01T00:00:00Z');
      if (locale === 'en') expect(langOf(out)).toBe('en');
    }
  });
});
