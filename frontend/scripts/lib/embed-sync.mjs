// 安装向导产物 → go:embed 目录的同步与漂移检查（Story 1.37）。
// 选择“产物入库 + 漂移/缺失检查”而不是“只由 make 生成”：见 frontend/README「安装向导的嵌入」。
import fs from 'node:fs';
import path from 'node:path';

/** 相对 root 的全部普通文件（posix 路径，升序）；遇到符号链接直接报错（嵌入目录不得含链接）。 */
export function listTree(root) {
  const out = [];
  const walk = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name);
      if (e.isSymbolicLink()) throw new Error(`不允许符号链接：${p}`);
      if (e.isDirectory()) walk(p);
      else out.push(path.relative(root, p).split(path.sep).join('/'));
    }
  };
  walk(root);
  return out.sort();
}

function requireDist(src) {
  if (!fs.existsSync(src) || !fs.statSync(src).isDirectory()) throw new Error(`构建产物目录不存在：${src}（先运行 pnpm --filter @lingedge/setup run build；拒绝空转通过）`);
  const files = listTree(src);
  if (!files.includes('index.html') || !files.some((f) => f.startsWith('assets/') && f.endsWith('.js'))) throw new Error(`构建产物不完整（缺 index.html 或 assets/*.js）：${src}`);
  return files;
}

/** 比较产物与嵌入目录：返回缺失（嵌入目录没有）、多余（嵌入目录多出）、内容不同的相对路径。 */
export function diffTrees(src, dst) {
  const srcFiles = requireDist(src);
  const dstFiles = fs.existsSync(dst) ? listTree(dst) : [];
  const srcSet = new Set(srcFiles);
  const dstSet = new Set(dstFiles);
  const missing = srcFiles.filter((f) => !dstSet.has(f));
  const extra = dstFiles.filter((f) => !srcSet.has(f));
  const changed = srcFiles.filter((f) => dstSet.has(f) && !fs.readFileSync(path.join(src, f)).equals(fs.readFileSync(path.join(dst, f))));
  return { missing, extra, changed, ok: missing.length + extra.length + changed.length === 0, files: srcFiles.length };
}

/** 用产物整体替换嵌入目录（先清空再复制，保证没有残留的旧哈希文件）。 */
export function syncTrees(src, dst) {
  const files = requireDist(src);
  fs.rmSync(dst, { recursive: true, force: true });
  for (const f of files) {
    const to = path.join(dst, f);
    fs.mkdirSync(path.dirname(to), { recursive: true });
    fs.copyFileSync(path.join(src, f), to);
  }
  return files;
}
