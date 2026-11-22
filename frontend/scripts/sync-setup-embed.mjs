// 用法：node scripts/sync-setup-embed.mjs          把 frontend/dist/setup 同步到 internal/platform/setup/webdist（供 go:embed）
//       node scripts/sync-setup-embed.mjs --check  只比对：缺失/多余/内容不同即失败（退出码 1）；产物目录缺失 → 2（不空转通过）
import { diffTrees, syncTrees } from './lib/embed-sync.mjs';
import { SETUP_DIST, SETUP_EMBED } from './lib/paths.mjs';

const check = process.argv.includes('--check');
try {
  if (check) {
    const r = diffTrees(SETUP_DIST, SETUP_EMBED);
    if (!r.ok) {
      for (const f of r.missing) console.error(`嵌入目录缺少：${f}`);
      for (const f of r.extra) console.error(`嵌入目录多出：${f}`);
      for (const f of r.changed) console.error(`内容与新构建不同：${f}`);
      console.error('安装向导嵌入产物与源码不一致：运行 `make setup-web` 重新构建并提交 internal/platform/setup/webdist');
      process.exit(1);
    }
    console.log(`安装向导嵌入产物检查通过：${r.files} 个文件与新构建逐字节一致`);
  } else {
    const files = syncTrees(SETUP_DIST, SETUP_EMBED);
    console.log(`已同步 ${files.length} 个文件到 ${SETUP_EMBED}`);
  }
} catch (e) {
  console.error(String(e.message ?? e));
  process.exit(2);
}
