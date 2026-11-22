import { checkSetupCodes } from './lib/setup-codes.mjs';
import { FRONTEND_ROOT } from './lib/paths.mjs';

const { errors, stats } = checkSetupCodes(FRONTEND_ROOT);
if (stats.codes === 0 || stats.checks === 0) {
  console.error('向导消息对账：没有推导出任何错误码/检查项（拒绝空转通过）');
  process.exit(1);
}
if (errors.length > 0) {
  for (const e of errors) console.error(e);
  console.error(`向导消息对账失败：${errors.length} 个问题`);
  process.exit(1);
}
console.log(`向导消息对账通过：${stats.codes} 个错误码、${stats.checks} 个环境检查项、${stats.ids} 个句式词条（zh-CN 与 en 均齐全）`);
