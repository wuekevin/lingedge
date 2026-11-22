// 依赖锁定检查：CI 禁止 ^ / latest / * 等浮动版本；packageManager 固定；.npmrc 关闭生命周期脚本；锁文件入库。
import { checkDeps } from './lib/deps-check.mjs';
import { FRONTEND_ROOT } from './lib/paths.mjs';

const { errors, stats } = checkDeps(FRONTEND_ROOT);
if (errors.length > 0) {
  for (const e of errors) console.error(e);
  console.error(`依赖锁定检查失败：${errors.length} 个问题`);
  process.exit(1);
}
console.log(`依赖锁定检查通过：${stats.packages} 个工作区包、${stats.specs} 条依赖声明全部为精确版本`);
