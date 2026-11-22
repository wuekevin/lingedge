// 由 shared/tokens/tokens.json 生成 CSS 变量 / Tailwind preset / Arco 主题映射。
// 用法：node scripts/gen-tokens.mjs         写出生成物
//       node scripts/gen-tokens.mjs --check  与已入库生成物比对，漂移即失败
import fs from 'node:fs';
import path from 'node:path';
import { renderAll } from './lib/tokens.mjs';
import { TOKENS_JSON, TOKENS_OUT } from './lib/paths.mjs';

const check = process.argv.includes('--check');
const tokens = JSON.parse(fs.readFileSync(TOKENS_JSON, 'utf8'));
const files = renderAll(tokens);

let drift = 0;
for (const [name, content] of Object.entries(files)) {
  const file = path.join(TOKENS_OUT, name);
  if (check) {
    const cur = fs.existsSync(file) ? fs.readFileSync(file, 'utf8') : null;
    if (cur !== content) {
      console.error(`漂移：${path.relative(process.cwd(), file)} 与 tokens.json 生成结果不一致（运行 pnpm run gen:tokens 并提交）`);
      drift++;
    }
  } else {
    fs.mkdirSync(TOKENS_OUT, { recursive: true });
    fs.writeFileSync(file, content);
    console.log(`写出 ${path.relative(process.cwd(), file)}`);
  }
}
if (drift > 0) process.exit(1);
if (check) console.log(`tokens 生成物无漂移（${Object.keys(files).length} 个文件）`);
