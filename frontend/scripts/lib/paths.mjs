import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const FRONTEND_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
export const REPO_ROOT = path.resolve(FRONTEND_ROOT, '..');
export const TOKENS_JSON = path.join(FRONTEND_ROOT, 'shared/tokens/tokens.json');
export const TOKENS_OUT = path.join(FRONTEND_ROOT, 'shared/tokens/generated');
export const DESIGN_MD = path.join(REPO_ROOT, 'docs/ux/DESIGN.md');
export const OPENAPI_YAML = path.join(REPO_ROOT, 'docs/api/openapi.yaml');
export const TYPES_OUT = path.join(FRONTEND_ROOT, 'shared/src/api-types/openapi.gen.ts');

/** 把 CI/测试里可能出现的相对路径解析到当前工作目录。 */
export function abs(p) {
  return path.resolve(p);
}
