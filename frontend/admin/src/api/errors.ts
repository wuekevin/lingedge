import { errorKindFromStatus, type LeErrorKind } from '@lingedge/shared';

/** 客户端自己产生的错误码（不是服务端契约的一部分）。 */
export const CODE_NETWORK = 'client.network';
export const CODE_BAD_RESPONSE = 'client.bad_response';

export interface ApiErrorDetail {
  field?: string;
  reason: string;
  code?: string;
}

/** 统一错误：状态码 + 服务端稳定错误码 + Request ID。文案由界面按错误码本地化，不直接展示服务端 message。 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId: string;
  readonly details: ApiErrorDetail[];
  readonly retryAfterSeconds: number | null;

  constructor(init: { status: number; code: string; message?: string; requestId?: string; details?: ApiErrorDetail[]; retryAfterSeconds?: number | null }) {
    super(init.message ?? init.code);
    this.name = 'ApiError';
    this.status = init.status;
    this.code = init.code;
    this.requestId = init.requestId ?? '';
    this.details = init.details ?? [];
    this.retryAfterSeconds = init.retryAfterSeconds ?? null;
  }
}

export const isApiError = (e: unknown): e is ApiError => e instanceof ApiError;

/** 任意异常 → 三态组件的失败分类。 */
export function errorKind(e: unknown): LeErrorKind {
  return isApiError(e) ? errorKindFromStatus(e.status) : 'unknown';
}
