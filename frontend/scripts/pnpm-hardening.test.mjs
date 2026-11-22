// 用真实的 pnpm install 验证“生命周期脚本被关闭”确实生效（不是只检查配置文本）。
// 背景：pnpm 12.18.0 实测——.npmrc 的 ignore-scripts 不再被读取；有效开关是 pnpm-workspace.yaml 的 ignoreScripts。
import { spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

const dirs = [];
afterEach(() => {
  for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});

/** 建一个依赖本地包的 app：依赖和 app 自身都有 postinstall，执行即写探针文件。 */
function fixture(workspaceYaml, npmrc) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'le-pnpm-'));
  dirs.push(root);
  const write = (rel, c) => {
    const f = path.join(root, rel);
    fs.mkdirSync(path.dirname(f), { recursive: true });
    fs.writeFileSync(f, c);
  };
  const probe = (name) => `node -e "require('fs').writeFileSync(process.env.${name},'ran')"`;
  write('dep/package.json', JSON.stringify({ name: 'dep', version: '1.0.0', scripts: { postinstall: probe('PROBE_DEP') } }));
  write('app/package.json', JSON.stringify({ name: 'app', version: '1.0.0', private: true, dependencies: { dep: 'file:../dep' }, scripts: { postinstall: probe('PROBE_APP') } }));
  if (workspaceYaml !== null) write('app/pnpm-workspace.yaml', workspaceYaml);
  if (npmrc !== null) write('app/.npmrc', npmrc);
  return root;
}
function install(root) {
  const env = { ...process.env, PROBE_DEP: path.join(root, 'probe-dep'), PROBE_APP: path.join(root, 'probe-app'), CI: 'true' };
  const r = spawnSync('pnpm', ['install'], { cwd: path.join(root, 'app'), env, encoding: 'utf8' });
  return { status: r.status, out: (r.stdout ?? '') + (r.stderr ?? ''), depRan: fs.existsSync(env.PROBE_DEP), appRan: fs.existsSync(env.PROBE_APP) };
}

describe('pnpm 真实行为：生命周期脚本', () => {
  it('pnpm-workspace.yaml 的 ignoreScripts: true → 依赖与工作区自身的脚本都不执行，安装成功', () => {
    const r = install(fixture('ignoreScripts: true\n', null));
    expect(r.status).toBe(0);
    expect(r.depRan).toBe(false);
    expect(r.appRan).toBe(false);
  });
  it('只有 .npmrc 的 ignore-scripts=true（旧写法）→ pnpm 12 下工作区自身脚本仍会执行（这就是必须配置 ignoreScripts 的原因）', () => {
    const r = install(fixture(null, 'ignore-scripts=true\n'));
    expect(r.appRan).toBe(true);
    expect(r.depRan).toBe(false); // 依赖脚本默认被 pnpm 拦截，并以 ERR_PNPM_IGNORED_BUILDS 失败
    expect(r.status).not.toBe(0);
  });
  it('未配置任何关闭项：依赖脚本被 pnpm 默认拦截（需审批），但仍不是我们要的“全关”', () => {
    const r = install(fixture(null, null));
    expect(r.depRan).toBe(false);
    expect(r.out).toContain('ERR_PNPM_IGNORED_BUILDS');
  });
  it('真实 frontend/ 工作区：存在 ignoreScripts: true', () => {
    const ws = fs.readFileSync(new URL('../pnpm-workspace.yaml', import.meta.url), 'utf8');
    expect(ws).toMatch(/^ignoreScripts:\s*true\s*$/m);
  });
});
