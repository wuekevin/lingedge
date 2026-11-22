// UX-27 门禁：全局界面不提供任何「执行命令」入口。规则与负向自测见 lib/no-command-entry.mjs、no-command-entry.test.mjs。
import { checkNoCommandEntry } from './lib/no-command-entry.mjs';
import { FRONTEND_ROOT } from './lib/paths.mjs';

const { violations, scanned } = checkNoCommandEntry(FRONTEND_ROOT);
if (scanned === 0) {
  console.error('check-no-command-entry：没有扫描到任何词条/页面注册表/依赖声明（目录结构异常，拒绝空转通过）');
  process.exit(1);
}
if (violations.length > 0) {
  for (const v of violations) console.error(`${v.file}:${v.line}  [${v.rule}] ${v.message}`);
  console.error(`check-no-command-entry 失败：${violations.length} 处违规`);
  process.exit(1);
}
console.log(`check-no-command-entry 通过（${scanned} 个词条/注册表/依赖声明，无「执行命令」入口）`);
