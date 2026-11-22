// Arco 实心控件文字对比度审计（Story 1.37b）：对照 Arco 真实 CSS，在 light/dark 两个主题逐状态计算，失败即退出码 1。
import fs from 'node:fs';
import { auditArcoSolidText } from './lib/arco-audit.mjs';
import { ARCO_CSS, TOKENS_JSON } from './lib/paths.mjs';

if (!fs.existsSync(ARCO_CSS)) {
  console.error(`Arco 对比度审计：找不到 ${ARCO_CSS}（先 pnpm install；拒绝空转通过）`);
  process.exit(1);
}
const tokens = JSON.parse(fs.readFileSync(TOKENS_JSON, 'utf8'));
const { errors, rows, stats } = auditArcoSolidText(tokens, fs.readFileSync(ARCO_CSS, 'utf8'));
if (stats.rules === 0) {
  console.error('Arco 对比度审计：没有解析到任何「白字+调色板底色」规则（Arco CSS 结构变化？拒绝空转通过）');
  process.exit(1);
}
const lows = rows.filter((r) => !r.exempt).map((r) => r.ratio);
console.log(`Arco 实心控件对比度：${stats.rules} 条规则、${stats.selectors} 个选择器、${stats.overridden} 个暗色覆盖；最低 ${Math.min(...lows).toFixed(2)}:1（禁用态不计）`);
if (errors.length > 0) {
  for (const e of errors) console.error(e);
  console.error(`Arco 实心控件对比度审计失败：${errors.length} 个问题`);
  process.exit(1);
}
console.log('Arco 实心控件对比度审计通过');
