// 安装向导「服务端消息 i18n」对账（Story 1.37b）：
//   从 Go 源码与 OpenAPI 自动推导向导可能返回的 ① 错误码 ② 环境检查项 key ③ 步骤成功回执，
//   逐一要求前端词条（zh-CN 与 en）与 servermsg.ts 的句式表覆盖；缺任何一项即失败（不放宽）。
import fs from 'node:fs';
import path from 'node:path';
import YAML from 'yaml';
import { flatten } from './i18n-check.mjs';

const read = (root, rel) => fs.readFileSync(path.join(root, rel), 'utf8');

/** codes.go：Go 标识符 → 错误码。 */
export function parseCodeTable(src) {
  const out = new Map();
  for (const m of src.matchAll(/^\s*([A-Z][A-Za-z0-9]*)\s+Code = "([a-z0-9_.]+)"/gm)) out.set(m[1], m[2]);
  return out;
}

/** apierr 的便捷构造函数隐含的错误码（见 apierr.go）。 */
const HELPER_CODES = { Validation: 'validation.failed', NotFound: 'resource.not_found', Conflict: 'resource.conflict', PreconditionFailed: 'resource.precondition_failed', KeyReuse: 'idempotency.key_reuse', Internal: 'internal.error' };

/** 一段 Go 源码里引用到的 apierr 错误码。 */
export function codesUsedIn(src, table) {
  const out = new Set();
  for (const m of src.matchAll(/\bapierr\.([A-Z][A-Za-z0-9]*)/g)) {
    if (table.has(m[1])) out.add(table.get(m[1]));
    else if (HELPER_CODES[m[1]]) out.add(HELPER_CODES[m[1]]);
  }
  return out;
}

function goFiles(dir) {
  return fs.readdirSync(dir).filter((f) => f.endsWith('.go') && !f.endsWith('_test.go')).map((f) => path.join(dir, f));
}

/** OpenAPI 中 /public/setup/* 各操作声明的响应 → 其示例里的错误码。 */
export function openapiSetupCodes(yamlText) {
  const doc = YAML.parse(yamlText);
  const out = new Set();
  const refs = new Set();
  for (const [p, item] of Object.entries(doc.paths ?? {})) {
    if (!p.startsWith('/public/setup/')) continue;
    for (const op of Object.values(item)) {
      for (const [status, r] of Object.entries(op?.responses ?? {})) {
        if (status.startsWith('2')) continue;
        if (r?.$ref) refs.add(r.$ref.split('/').pop());
      }
    }
  }
  for (const name of refs) {
    const code = doc.components?.responses?.[name]?.content?.['application/json']?.example?.error?.code;
    if (typeof code === 'string') out.add(code);
  }
  return out;
}

/** steps_db.go 的环境检查项：`add("key", ...` */
export function checkKeys(src) {
  return [...new Set([...src.matchAll(/\badd\("([a-z0-9_.]+)"/g)].map((m) => m[1]))];
}

/** 产生成功回执的步骤：`s.result(StepX,` → types.go 的步骤键。 */
export function resultSteps(setupSrcs, typesSrc) {
  const names = new Map([...typesSrc.matchAll(/^\s*(Step[A-Za-z]+)\s*=\s*"([a-z_]+)"/gm)].map((m) => [m[1], m[2]]));
  const out = new Set();
  for (const src of setupSrcs) for (const m of src.matchAll(/\.result\((Step[A-Za-z]+),\s*([^)\n]*)/g)) if (names.has(m[1]) && m[2].trim() !== '""') out.add(names.get(m[1]));
  return [...out];
}

const codeKey = (c) => c.replace('password', 'pw');

export function checkSetupCodes(root) {
  const errors = [];
  const repo = path.resolve(root, '..');
  const table = parseCodeTable(read(repo, 'internal/platform/apierr/codes.go'));

  // ① 错误码：setup 包 + 向导调用的 auth 引导 + OpenAPI 声明 + 前端自有（client.*）
  const codes = new Set(['internal.error']);
  for (const f of goFiles(path.join(repo, 'internal/platform/setup'))) for (const c of codesUsedIn(fs.readFileSync(f, 'utf8'), table)) codes.add(c);
  for (const c of codesUsedIn(read(repo, 'internal/auth/internal/service/bootstrap.go'), table)) codes.add(c);
  for (const c of openapiSetupCodes(read(repo, 'docs/api/openapi.yaml'))) codes.add(c);
  for (const c of table.values()) if (c.startsWith('setup.')) codes.add(c); // 新增 setup.* 码即使还没被引用也必须有词条
  const errorsTs = read(root, 'setup/src/api/errors.ts');
  for (const m of errorsTs.matchAll(/export const CODE_[A-Z_]+ = '([a-z_.]+)'/g)) if (m[1].startsWith('client.')) codes.add(m[1]);
  for (const c of codes) if (![...table.values()].includes(c) && !c.startsWith('client.')) errors.push(`错误码 ${c} 不在 apierr/codes.go（推导出错）`);

  // 词条
  const flat = {};
  for (const l of ['zh-CN', 'en']) {
    const problems = [];
    flat[l] = flatten(JSON.parse(read(root, `setup/src/locales/${l}.json`)), problems, `${l}.json`);
    errors.push(...problems);
  }
  const need = (key, why) => {
    for (const l of ['zh-CN', 'en']) if (!(key in flat[l])) errors.push(`${l}.json 缺少键 ${key}（${why}）`);
  };
  for (const c of [...codes].sort()) need(`errors.code.${codeKey(c)}`, `向导可能返回的错误码 ${c}`);

  // ② 检查项 + ③ 步骤回执：句式表（servermsg.ts）必须覆盖
  const msgTs = read(root, 'setup/src/api/servermsg.ts');
  const dbSrc = read(repo, 'internal/platform/setup/steps_db.go');
  for (const k of checkKeys(dbSrc)) {
    if (!new RegExp(`key: '${k.replace(/\./g, '\\.')}'`).test(msgTs)) errors.push(`servermsg.ts 缺少环境检查项 ${k} 的句式`);
    const label = `env.item.${k === 'server.env' ? 'server_env' : k}`;
    need(label, `环境检查项 ${k} 的标签`);
  }
  const setupSrcs = goFiles(path.join(repo, 'internal/platform/setup')).map((f) => fs.readFileSync(f, 'utf8'));
  for (const s of resultSteps(setupSrcs, read(repo, 'internal/platform/setup/types.go'))) {
    if (!msgTs.includes(`case '${s}'`)) errors.push(`servermsg.ts 缺少步骤 ${s} 成功回执的句式`);
  }
  // 句式表引用的词条 id 必须存在
  const ids = new Set([...msgTs.matchAll(/\bid: '([a-z_.A-Z]+)'/g)].map((m) => m[1]));
  for (const m of msgTs.matchAll(/\bone\([^,]+,\s*'([a-z_.A-Z]+)'\)/g)) ids.add(m[1]);
  for (const id of ids) need(`serverMsg.${id}`, 'servermsg.ts 引用');
  return { errors, stats: { codes: codes.size, checks: checkKeys(dbSrc).length, ids: ids.size } };
}
