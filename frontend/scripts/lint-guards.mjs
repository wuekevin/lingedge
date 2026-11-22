// 前端架构门禁：全仓禁 v-html / 硬编码色值 / 硬编码文案；shared 禁路由表/菜单/权限/会话/API 客户端。
import { runGuards } from './lib/guards.mjs';
import { FRONTEND_ROOT, OPENAPI_YAML } from './lib/paths.mjs';

const { violations, scanned } = runGuards({ root: FRONTEND_ROOT, openapiFile: OPENAPI_YAML });
if (scanned === 0) {
  console.error('lint-guards：没有扫描到任何文件（目录结构异常，拒绝空转通过）');
  process.exit(1);
}
if (violations.length > 0) {
  for (const v of violations) console.error(`${v.file}:${v.line}  [${v.rule}] ${v.message}`);
  console.error(`lint-guards 失败：${violations.length} 处违规（已扫描 ${scanned} 个文件）`);
  process.exit(1);
}
console.log(`lint-guards 通过（已扫描 ${scanned} 个文件）`);
