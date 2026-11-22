// 管理员口令强度提示（只给安装者即时反馈；服务端 ValidateSetupPassword 才是权威，二者规则一致但服务端还拒绝常见弱口令片段）。
// 规则镜像服务端：12–128 字符；不含邮箱本地部分（≥4 字符）；至少三类字符（大写/小写/数字/符号），长度 ≥20 的口令短语至少两类；≥5 个不同字符。

export const MIN_LENGTH = 12;
export const MAX_LENGTH = 128;
export const PASSPHRASE_LENGTH = 20;

export type RuleKey = 'length' | 'classes' | 'distinct' | 'noEmail';

export interface PasswordAssessment {
  /** 逐条规则是否满足（供界面逐项展示，不只靠颜色）。 */
  rules: Record<RuleKey, boolean>;
  /** 0（空）… 4（强）：仅用于强度条与文字档位。 */
  score: 0 | 1 | 2 | 3 | 4;
  /** 满足全部硬性规则（服务端仍可能因常见弱口令片段拒绝）。 */
  acceptable: boolean;
}

function charClasses(pw: string): number {
  let lower = false;
  let upper = false;
  let digit = false;
  let symbol = false;
  for (const ch of pw) {
    if (/\p{Ll}/u.test(ch)) lower = true;
    else if (/\p{Lu}/u.test(ch)) upper = true;
    else if (/\p{Nd}/u.test(ch)) digit = true;
    else symbol = true;
  }
  return [lower, upper, digit, symbol].filter(Boolean).length;
}

export function assessPassword(pw: string, email: string): PasswordAssessment {
  const chars = [...pw];
  const len = chars.length;
  const classes = charClasses(pw);
  const local = email.toLowerCase().split('@')[0] ?? '';
  const rules: Record<RuleKey, boolean> = {
    length: len >= MIN_LENGTH && len <= MAX_LENGTH,
    classes: len >= PASSPHRASE_LENGTH ? classes >= 2 : classes >= 3,
    distinct: new Set(chars).size >= 5,
    noEmail: !(email.includes('@') && local.length >= 4 && pw.toLowerCase().includes(local)),
  };
  const acceptable = Object.values(rules).every(Boolean);
  let score: PasswordAssessment['score'] = 0;
  if (len > 0) {
    score = 1;
    if (acceptable) score = 2;
    if (acceptable && (len >= 16 || classes === 4)) score = 3;
    if (acceptable && len >= 16 && classes >= 3) score = 4;
  }
  return { rules, score, acceptable };
}
