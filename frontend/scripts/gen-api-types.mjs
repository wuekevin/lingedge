// 用法：node scripts/gen-api-types.mjs          生成 shared/src/api-types/openapi.gen.ts
//       node scripts/gen-api-types.mjs --check  与已入库产物比对，漂移即失败（接入 make contract-check）
import fs from 'node:fs';
import path from 'node:path';
import { renderApiTypes } from './lib/api-types.mjs';
import { OPENAPI_YAML, TYPES_OUT } from './lib/paths.mjs';

const check = process.argv.includes('--check');
const content = await renderApiTypes(OPENAPI_YAML);
if (check) {
  const cur = fs.existsSync(TYPES_OUT) ? fs.readFileSync(TYPES_OUT, 'utf8') : null;
  if (cur !== content) {
    console.error('漂移：frontend/shared/src/api-types/openapi.gen.ts 与 docs/api/openapi.yaml 生成结果不一致（运行 pnpm -C frontend run gen:types 并提交）');
    process.exit(1);
  }
  console.log('OpenAPI 类型生成物无漂移');
} else {
  fs.mkdirSync(path.dirname(TYPES_OUT), { recursive: true });
  fs.writeFileSync(TYPES_OUT, content);
  console.log(`写出 ${path.relative(process.cwd(), TYPES_OUT)}（${content.length} 字节）`);
}
