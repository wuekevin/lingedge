// 设计令牌文字配对对比度 ≥ 4.5:1 校验（非文本图形 ≥ 3:1；已知例外设下限）。失败即退出码 1。
import fs from 'node:fs';
import { evaluateContrast } from './lib/contrast.mjs';
import { TOKENS_JSON } from './lib/paths.mjs';

const file = process.argv[2] ?? TOKENS_JSON;
const tokens = JSON.parse(fs.readFileSync(file, 'utf8'));
const { rows, failures } = evaluateContrast(tokens);
const exceptions = rows.filter((r) => r.exception);
console.log(`对比度校验：${rows.length} 个配对（light + dark）`);
for (const r of exceptions) console.log(`  已知例外 [${r.theme}] ${r.fg} on ${r.bg}：${r.ratio.toFixed(2)}:1（下限 ${r.min}）— ${r.exception}`);
if (failures.length > 0) {
  console.error('对比度校验失败：');
  for (const f of failures) console.error('  ' + f);
  process.exit(1);
}
console.log('对比度校验通过');
