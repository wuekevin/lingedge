/** `<input type="datetime-local">` 的值（本地时间，如 2026-11-23T10:30）→ RFC 3339 UTC；空 → undefined；无效 → null。 */
export function localToIso(v: string): string | undefined | null {
  if (!v) return undefined;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d.toISOString();
}

/** 起止时间校验：任一无效 → 'invalid'；起 > 止 → 'order'；否则 null。 */
export function rangeProblem(from: string, to: string): 'invalid' | 'order' | null {
  const a = localToIso(from);
  const b = localToIso(to);
  if (a === null || b === null) return 'invalid';
  if (a && b && a > b) return 'order';
  return null;
}
