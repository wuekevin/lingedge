// 用法：node scripts/check-isolation.mjs [--console <dir>] [--admin <dir>] [--openapi <file>]
// 缺省检查 frontend/dist/console 与 frontend/dist/admin。任一违规 → 退出码 1；目录缺失 → 退出码 2（不空转通过）。
import fs from 'node:fs';
import path from 'node:path';
import { checkIsolation } from './lib/isolation.mjs';
import { loadOpenapi, platformManifest } from './lib/openapi.mjs';
import { FRONTEND_ROOT, OPENAPI_YAML } from './lib/paths.mjs';

const arg = (name, dflt) => {
  const i = process.argv.indexOf(name);
  return i >= 0 ? path.resolve(process.argv[i + 1]) : dflt;
};
const consoleDir = arg('--console', path.join(FRONTEND_ROOT, 'dist/console'));
const adminDir = arg('--admin', path.join(FRONTEND_ROOT, 'dist/admin'));
const openapi = arg('--openapi', OPENAPI_YAML);

const manifest = platformManifest(loadOpenapi(openapi));
const adminPrefixes = JSON.parse(fs.readFileSync(path.join(FRONTEND_ROOT, 'isolation/admin-route-prefixes.json'), 'utf8')).prefixes;
console.log(`隔离清单（由 openapi.yaml 自动生成）：${manifest.platformOperationCount} 个 platform operation → ${manifest.platformOnly.length} 个精确路径模板；Admin 路由前缀 ${adminPrefixes.join(', ')}`);
if (manifest.shared.length > 0) {
  console.log(`  注意：${manifest.shared.length} 个路径形状同时承载 platform 与非 platform operation，字符串层面无法区分，不进入 grep 清单（授权靠服务端）：${manifest.shared.map((s) => s.shape).join(', ')}`);
}

let failed = false;
try {
  for (const [role, dir] of [['console', consoleDir], ['admin', adminDir]]) {
    const r = checkIsolation({ distDir: dir, role, manifest, adminPrefixes });
    if (r.violations.length > 0) {
      failed = true;
      for (const v of r.violations) console.error(`${role} 产物 ${v.file}:${v.line}：含 ${v.what}，命中 ${JSON.stringify(v.match)}`);
    }
    console.log(`${role} 产物隔离检查${r.violations.length > 0 ? '失败' : '通过'}：扫描 ${r.scannedFiles} 个文件${role === 'console' ? `，${r.templates} 个路径模板` : ''}`);
  }
} catch (e) {
  console.error(String(e.message ?? e));
  process.exit(2);
}
process.exit(failed ? 1 : 0);
