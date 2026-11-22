// 用法：node scripts/check-mock.mjs [--dist <dir>]... 缺省检查 frontend/dist/{console,admin} 与各应用源码/环境文件。
import path from 'node:path';
import { checkDistForMock, checkSourcesForMock } from './lib/mock-check.mjs';
import { FRONTEND_ROOT } from './lib/paths.mjs';

const dists = [];
for (let i = 0; i < process.argv.length; i++) if (process.argv[i] === '--dist') dists.push(path.resolve(process.argv[++i]));
if (dists.length === 0) dists.push(path.join(FRONTEND_ROOT, 'dist/console'), path.join(FRONTEND_ROOT, 'dist/admin'));

const violations = [];
try {
  for (const d of dists) {
    const r = checkDistForMock(d);
    violations.push(...r.violations.map((v) => `[${path.basename(d)}] ${v}`));
    console.log(`mock 检查 ${path.relative(FRONTEND_ROOT, d)}：扫描 ${r.scanned} 个文件`);
  }
} catch (e) {
  console.error(String(e.message ?? e));
  process.exit(2);
}
violations.push(...checkSourcesForMock(['admin', 'console', 'website'].map((a) => path.join(FRONTEND_ROOT, a))).violations);
if (violations.length > 0) {
  for (const v of violations) console.error(v);
  console.error(`mock 检查失败：${violations.length} 处`);
  process.exit(1);
}
console.log('mock 检查通过：生产产物与源码/环境文件不含 mock 入口');
