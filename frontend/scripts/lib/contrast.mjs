// 文字配对对比度校验（Story 1.36 AC2）：tokens.json 的 contrast.text / contrast.graphic 在 light 与 dark 各算一遍。
import { contrastRatio } from './color.mjs';

/**
 * @param {any} tokens tokens.json
 * @returns {{ rows: Array<{theme:string, fg:string, bg:string, kind:string, ratio:number, min:number, ok:boolean, exception?:string}>, failures: string[] }}
 */
export function evaluateContrast(tokens) {
  const c = tokens.contrast;
  const rows = [];
  const failures = [];
  const exceptionFor = (fg, bg, theme) =>
    (c.exceptions ?? []).find((e) => e.fg === fg && e.bg === bg && e.themes.includes(theme));

  for (const theme of ['light', 'dark']) {
    const colors = tokens.colors[theme];
    const groups = [
      ['text', c.text, c.minText],
      ['graphic', c.graphic, c.minNonText],
    ];
    for (const [kind, pairs, defaultMin] of groups) {
      for (const [fg, bg] of pairs) {
        for (const name of [fg, bg]) {
          if (!colors[name]) {
            failures.push(`[${theme}] 未知令牌 ${name}（${fg} / ${bg}）`);
          }
        }
        if (!colors[fg] || !colors[bg]) continue;
        const ratio = contrastRatio(colors[fg], colors[bg]);
        const ex = exceptionFor(fg, bg, theme);
        const min = ex ? ex.min : defaultMin;
        const ok = ratio >= min;
        rows.push({ theme, fg, bg, kind, ratio, min, ok, exception: ex?.reason });
        if (!ok) failures.push(`[${theme}] ${fg} on ${bg}：${ratio.toFixed(2)}:1 < ${min}:1${ex ? '（已知例外的下限）' : ''}`);
      }
    }
  }
  // 例外条目必须真的被某个配对用到，否则是死配置（防止登记后无人校验）。
  for (const e of c.exceptions ?? []) {
    const used = rows.some((r) => r.fg === e.fg && r.bg === e.bg && r.exception);
    if (!used) failures.push(`例外 ${e.fg} on ${e.bg} 没有对应的校验配对（死配置）`);
  }
  return { rows, failures };
}
