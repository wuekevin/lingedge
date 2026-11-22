// 由 docs/api/openapi.yaml 生成 TypeScript 类型（openapi-typescript，版本固定在 package.json）。
import crypto from 'node:crypto';
import fs from 'node:fs';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import openapiTS, { astToString } from 'openapi-typescript';

const require = createRequire(import.meta.url);

export async function renderApiTypes(openapiFile) {
  const raw = fs.readFileSync(openapiFile);
  const sha = crypto.createHash('sha256').update(raw).digest('hex');
  const version = require('openapi-typescript/package.json').version;
  const ast = await openapiTS(pathToFileURL(openapiFile), { alphabetize: true, exportType: true });
  const header = [
    '/* eslint-disable */',
    '// 自动生成，请勿手改。来源：docs/api/openapi.yaml；生成器：frontend/scripts/gen-api-types.mjs（openapi-typescript ' + version + '）。',
    '// 仅含类型，不含任何运行时代码、API 客户端或端点调用（ADR-014 §2）。每个应用只 import 自己需要的 operation。',
    '// make contract-check 重新生成并比对：openapi.yaml 变动而本文件未同步即失败（漂移）。',
    `// openapi.yaml sha256: ${sha}`,
    '',
    '',
  ].join('\n');
  return header + astToString(ast);
}
