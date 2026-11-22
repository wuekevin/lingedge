// 生产构建不得含 mock 入口（NFR-1、ADR-023、dev-environment §6.4）。约定：
//   · mock 模块文件位于 mocks/ 目录或命名为 *.mock.ts，且其入口函数执行时输出含 LE_MOCK_ENTRY 的字符串（如 console.warn('LE_MOCK_ENTRY active')）；
//     标记必须是被执行代码里的字符串字面量——只导出一个常量会被摇树优化掉，标记随之消失（e2e-build.test.mjs 已实测）；
//   · 只能经动态 import() 且受 import.meta.env.MODE === 'mock' 之类的编译期条件保护，禁止静态 import；
//   · 生产模式的 .env 不得打开 mock 开关。
// 本检查从三个角度兜底：产物（文件名 + 内容标记 + sourcemap sources）、源码（静态 import mock）、环境文件。
import fs from 'node:fs';
import path from 'node:path';

const TEXT_EXT = new Set(['.js', '.mjs', '.cjs', '.css', '.html', '.json', '.map', '.txt']);
export const MOCK_MARKERS = ['LE_MOCK_ENTRY', 'mockServiceWorker', 'msw/browser', 'setupWorker(', '__LE_MOCK__', 'VITE_ENABLE_MOCK', 'VITE_USE_MOCK'];
const MOCK_FILE = /(^|\/)(mockServiceWorker\.js|mocks?(\/|$)|[^/]*\.mock\.[cm]?[jt]s$|mock[^/]*\.(js|json)$)/i;

function* allFiles(dir) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) yield* allFiles(p);
    else yield p;
  }
}

/** 产物检查。 */
export function checkDistForMock(distDir) {
  if (!fs.existsSync(distDir)) throw new Error(`产物目录不存在：${distDir}（先构建；拒绝空转通过）`);
  const violations = [];
  let scanned = 0;
  for (const f of allFiles(distDir)) {
    const rel = path.relative(distDir, f).split(path.sep).join('/');
    scanned++;
    if (MOCK_FILE.test(rel)) violations.push(`产物含 mock 文件：${rel}`);
    if (!TEXT_EXT.has(path.extname(f))) continue;
    const text = fs.readFileSync(f, 'utf8');
    for (const m of MOCK_MARKERS) if (text.includes(m)) violations.push(`产物 ${rel} 含 mock 标记 ${m}`);
    if (f.endsWith('.map')) {
      try {
        for (const s of JSON.parse(text).sources ?? []) if (/(\/mocks?\/|\.mock\.)/i.test(s)) violations.push(`sourcemap ${rel} 引用了 mock 源文件 ${s}`);
      } catch {
        violations.push(`sourcemap ${rel} 无法解析`);
      }
    }
  }
  if (scanned === 0) throw new Error(`产物目录为空：${distDir}`);
  return { violations, scanned };
}

/** 源码与环境文件检查：appDirs 是各应用目录（admin/console/website），不含 node_modules/dist。 */
export function checkSourcesForMock(appDirs) {
  const violations = [];
  const staticImport = /(?:^|\n)\s*(?:import|export)\b[^;]*?\bfrom\s*['"]([^'"]*)['"]|(?:^|\n)\s*import\s*['"]([^'"]*)['"]/g;
  for (const dir of appDirs) {
    if (!fs.existsSync(dir)) continue;
    const walk = (d) => {
      for (const e of fs.readdirSync(d, { withFileTypes: true })) {
        if (e.name === 'node_modules' || e.name === 'dist') continue;
        const p = path.join(d, e.name);
        if (e.isDirectory()) walk(p);
        else {
          const rel = path.relative(dir, p).split(path.sep).join('/');
          if (/^\.env(\..+)?$/.test(e.name)) {
            const prodLike = !/\.(development|test|mock)(\.|$)/.test(e.name);
            const on = fs.readFileSync(p, 'utf8').split('\n').filter((l) => /^\s*VITE_[A-Z_]*MOCK[A-Z_]*\s*=\s*(true|1|yes|on)\s*$/i.test(l));
            if (prodLike && on.length > 0) violations.push(`${path.basename(dir)}/${rel}：生产相关环境文件打开了 mock 开关（${on[0].trim()}）`);
          } else if (/\.(vue|ts|tsx|js|jsx|mjs)$/.test(e.name) && !/\.(test|spec)\./.test(e.name) && !/(^|\/)(mocks?)\//.test(rel) && !/\.mock\./.test(e.name)) {
            const src = fs.readFileSync(p, 'utf8');
            for (const m of src.matchAll(staticImport)) {
              const spec = m[1] ?? m[2] ?? '';
              if (/(\/mocks?(\/|$)|\.mock(\.|$)|^msw(\/|$))/i.test(spec)) violations.push(`${path.basename(dir)}/${rel}：静态 import 了 mock 模块 ${spec}（只允许受编译期条件保护的动态 import()）`);
            }
          }
        }
      }
    };
    walk(dir);
  }
  return { violations };
}
