import type { ApiClient } from './client';
import type { LoginResponse, Me, ReauthResponse, Tenant } from './types';

// /auth/* 与 /tenants 的薄封装；端点写完整路径（构建隔离检查按路径模板匹配）。
export function createAuthApi(c: ApiClient) {
  return {
    /** 登录第一步；realm 固定 admin（租户账号会得到统一的 401 auth.invalid_credentials）。 */
    login(email: string, secret: string, signal?: AbortSignal): Promise<LoginResponse> {
      return c.post<LoginResponse>('/auth/login', { auth: false, body: { realm: 'admin', email, password: secret }, ...(signal ? { signal } : {}) });
    },
    /** 登录第二步：6 位 TOTP 或一次性恢复码。 */
    verifyTotp(mfaToken: string, code: string, signal?: AbortSignal): Promise<LoginResponse> {
      return c.post<LoginResponse>('/auth/totp/verify', { auth: false, body: { mfa_token: mfaToken, code }, ...(signal ? { signal } : {}) });
    },
    me(signal?: AbortSignal): Promise<Me> {
      return c.get<Me>('/auth/me', { ...(signal ? { signal } : {}) });
    },
    logout(): Promise<void> {
      return c.post<void>('/auth/logout', { auth: false });
    },
    changePassword(current: string, next: string): Promise<void> {
      return c.post<void>('/auth/password/change', { auth: false, body: { current_password: current, new_password: next } });
    },
    /** 再认证（L4 step-up）：口令（已启用 TOTP 另加 6 位验证码）由服务端校验，成功后会话进入再认证窗口。auth:false —— 口令错误的 401 不应被当作会话过期。 */
    reauth(secret: string, totpCode?: string): Promise<ReauthResponse> {
      return c.post<ReauthResponse>('/auth/reauth', { auth: false, body: { password: secret, ...(totpCode ? { totp_code: totpCode } : {}) } });
    },
    /** 默认租户（D-13）：用于代管横幅与 X-Tenant-ID。需要 tenant:read；调用方按权限决定是否调用。 */
    listTenants(): Promise<{ items: Tenant[] }> {
      return c.get<{ items: Tenant[] }>('/tenants', { query: { page: 1, page_size: 200 } });
    },
  };
}
export type AuthApi = ReturnType<typeof createAuthApi>;
