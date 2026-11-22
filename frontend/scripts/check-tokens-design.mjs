// tokens.json 与 docs/ux/DESIGN.md frontmatter 一致性校验。
import fs from 'node:fs';
import { diffTokensAgainstDesign, loadDesign } from './lib/design-md.mjs';
import { DESIGN_MD, TOKENS_JSON } from './lib/paths.mjs';

const tokens = JSON.parse(fs.readFileSync(TOKENS_JSON, 'utf8'));
const diffs = diffTokensAgainstDesign(tokens, loadDesign(DESIGN_MD));
if (diffs.length > 0) {
  console.error('tokens.json 与 DESIGN.md 不一致（改动设计令牌须两处同步，UD-14）：');
  for (const d of diffs) console.error('  ' + d);
  process.exit(1);
}
console.log('tokens.json 与 DESIGN.md frontmatter 一致');
