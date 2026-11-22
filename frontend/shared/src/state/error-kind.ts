export type LeErrorKind =
  | 'network'
  | 'server'
  | 'forbidden'
  | 'notFound'
  | 'conflict'
  | 'preconditionFailed'
  | 'validation'
  | 'rateLimited'
  | 'unknown';

export const ERROR_KINDS: readonly LeErrorKind[] = [
  'network',
  'server',
  'forbidden',
  'notFound',
  'conflict',
  'preconditionFailed',
  'validation',
  'rateLimited',
  'unknown',
];

/** HTTP 状态码 → 失败态分类（EXPERIENCE.md「全局三态」：不同原因给出不同文案）。无状态码视为网络中断。 */
export function errorKindFromStatus(status: number | null | undefined): LeErrorKind {
  if (status === null || status === undefined || status === 0) return 'network';
  if (status === 400) return 'validation';
  if (status === 403) return 'forbidden';
  if (status === 404) return 'notFound';
  if (status === 409) return 'conflict';
  if (status === 412) return 'preconditionFailed';
  if (status === 429) return 'rateLimited';
  if (status >= 500) return 'server';
  return 'unknown';
}

/** 这些类别重试没有意义（权限/不存在/校验），默认不展示重试按钮。 */
export function isRetryableKind(kind: LeErrorKind): boolean {
  return !(kind === 'forbidden' || kind === 'notFound' || kind === 'validation');
}
