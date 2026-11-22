import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { renderApiTypes } from './lib/api-types.mjs';
import { FRONTEND_ROOT, OPENAPI_YAML, TYPES_OUT } from './lib/paths.mjs';

const dirs = [];
afterEach(() => {
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});

describe('OpenAPI → TypeScript 类型生成与漂移检测', () => {
  it('入库的生成物与 openapi.yaml 当前生成结果逐字节一致（contract-check 联动的核心断言）', async () => {
    expect(fs.readFileSync(TYPES_OUT, 'utf8')).toBe(await renderApiTypes(OPENAPI_YAML));
  });

  it('生成物只含类型：无 import/函数/类/客户端实例', () => {
    const body = fs.readFileSync(TYPES_OUT, 'utf8').split('\n').filter((l) => !l.startsWith('//') && !l.startsWith('/*'));
    const text = body.join('\n');
    expect(text).not.toMatch(/^\s*import\s/m);
    expect(text).not.toMatch(/\bfunction\b|\bclass\b|=>\s*\{|\bfetch\(|\bnew\s+\w+\(/);
    expect(text).toContain('export type paths');
    expect(text).toContain('export type components');
    expect(text).toMatch(/export (type|interface) operations\b/);
  });

  it('负向：openapi.yaml 新增一个 operation 而生成物未更新 → 生成结果不同（漂移被检出）', async () => {
    const src = fs.readFileSync(OPENAPI_YAML, 'utf8');
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'le-drift-'));
    dirs.push(dir);
    const mutated = path.join(dir, 'openapi.yaml');
    // 在 paths: 之后插入一个新路径；其余内容不变。
    expect(src).toMatch(/^paths:\n/m);
    fs.writeFileSync(mutated, src.replace(/^paths:\n/m, 'paths:\n  /le-drift-probe:\n    get:\n      operationId: leDriftProbe\n      responses:\n        "204":\n          description: probe\n'));
    const regenerated = await renderApiTypes(mutated);
    expect(regenerated).not.toBe(fs.readFileSync(TYPES_OUT, 'utf8'));
    expect(regenerated).toContain('/le-drift-probe');
  });

  it('CLI：--check 在无漂移时退出 0', () => {
    const r = spawnSync('node', ['scripts/gen-api-types.mjs', '--check'], { cwd: FRONTEND_ROOT, encoding: 'utf8' });
    expect(r.status, r.stderr).toBe(0);
  });

  it('CLI：生成物被改动（手改/过期）→ --check 退出 1 并点名文件', () => {
    const backup = fs.readFileSync(TYPES_OUT);
    try {
      fs.appendFileSync(TYPES_OUT, '\n// drift\n');
      const r = spawnSync('node', ['scripts/gen-api-types.mjs', '--check'], { cwd: FRONTEND_ROOT, encoding: 'utf8' });
      expect(r.status).toBe(1);
      expect(r.stderr).toContain('openapi.gen.ts');
    } finally {
      fs.writeFileSync(TYPES_OUT, backup);
    }
  });
});
