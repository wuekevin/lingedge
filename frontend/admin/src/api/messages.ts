import { errorKind, isApiError } from './errors';

type T = (key: string, named?: Record<string, unknown>) => string;
type Te = (key: string) => boolean;

/** 错误 → 用户可读文案：优先按服务端稳定错误码映射（errors.code.<code>），否则按状态码类别给通用说明；未知错误码不显示服务端原文。 */
export function errorText(t: T, te: Te, e: unknown): string {
  if (isApiError(e)) {
    // 词条键里的 password 一律写成 pw（G-6：键名不得以 password 等疑似口令字面量的词出现），与向导同一约定
    const key = `errors.code.${e.code.replaceAll('password', 'pw')}`;
    let s = te(key) ? t(key) : t(`state.error.kind.${errorKind(e)}`);
    if (e.retryAfterSeconds && e.status === 429) s += ` ${t('errors.retryAfter', { seconds: e.retryAfterSeconds })}`;
    return s;
  }
  return t('state.error.kind.unknown');
}
