import { errorKindFromStatus, type LeErrorKind } from '@lingedge/shared';

export interface ApiErrorDetail {
  field?: string;
  reason: string;
  code?: string;
}

/**
 * 向导 API 的失败：保留稳定错误码与 Request ID（运维友好原则，UX）。
 * status=0 表示没有收到任何 HTTP 响应（网络中断/连接被拒）。
 */
export class SetupApiError extends Error {
  readonly status: number;
  readonly code: string;
  /** 服务端原文 message（未知错误码时作为回落展示；已知码走本地词条）。 */
  readonly serverMessage: string;
  readonly requestId: string;
  readonly details: ApiErrorDetail[];
  readonly retryAfterSeconds: number | null;

  constructor(init: { status: number; code: string; message: string; requestId?: string; details?: ApiErrorDetail[]; retryAfterSeconds?: number | null }) {
    super(`${init.code}: ${init.message}`);
    this.name = 'SetupApiError';
    this.status = init.status;
    this.code = init.code;
    this.serverMessage = init.message;
    this.requestId = init.requestId ?? '';
    this.details = init.details ?? [];
    this.retryAfterSeconds = init.retryAfterSeconds ?? null;
  }

  get kind(): LeErrorKind {
    // 401（token 错误）在共享分类里没有专门类别：按“无权限”展示；410 由上层单独处理。
    if (this.status === 401) return 'forbidden';
    return errorKindFromStatus(this.status);
  }

  /** 服务端对该字段给出的说明（校验失败的逐字段映射）。 */
  fieldReason(field: string): string | undefined {
    return this.details.find((d) => d.field === field)?.reason;
  }
}

export const CODE_TOKEN_INVALID = 'setup.token_invalid';
export const CODE_ALREADY_INSTALLED = 'setup.already_installed';
export const CODE_NETWORK = 'client.network_error';
export const CODE_BAD_RESPONSE = 'client.bad_response';

export const isAlreadyInstalled = (e: unknown): boolean => e instanceof SetupApiError && (e.status === 410 || e.code === CODE_ALREADY_INSTALLED);
export const isTokenInvalid = (e: unknown): boolean => e instanceof SetupApiError && e.status === 401;
