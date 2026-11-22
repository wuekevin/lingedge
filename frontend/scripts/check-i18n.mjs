import { checkI18n } from './lib/i18n-check.mjs';
import { FRONTEND_ROOT } from './lib/paths.mjs';

const { errors, stats } = checkI18n(FRONTEND_ROOT);
if (stats.catalogs === 0) {
  console.error('i18n 检查：没有找到任何 locales/ 词条目录（拒绝空转通过）');
  process.exit(1);
}
if (errors.length > 0) {
  for (const e of errors) console.error(e);
  console.error(`i18n 检查失败：${errors.length} 个问题`);
  process.exit(1);
}
console.log(`i18n 检查通过：${stats.catalogs} 个词条目录、${stats.keys} 个键（zh-CN = en）、源码使用 ${stats.usedKeys} 个键${stats.dynamicSkipped ? `，${stats.dynamicSkipped} 处无静态前缀的动态键未校验` : ''}`);
