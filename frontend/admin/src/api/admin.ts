import type { ApiClient } from './client';
import type {
  AdminUser,
  AdminUserCreate,
  AdminUserList,
  AdminUserUpdate,
  ApiToken,
  ApiTokenCreate,
  ApiTokenCreated,
  ApiTokenList,
  AuditLogCursorList,
  BrandSettings,
  LoginLogCursorList,
  PermissionList,
  Role,
  RoleCreate,
  RoleList,
  RunModeBody,
  SettingsMap,
  TemporaryPassword,
  VersionInfo,
} from './types';

// 1.39 的业务端点（系统设置 / 用户与角色 / API Token / 审计）。端点写完整路径（构建隔离检查按路径模板匹配）；类型全部来自 OpenAPI 生成物。
// 再认证（L4）由 client 统一处理：窗口外 403 auth.reauth_required → 验证框 → 自动重试，这里的方法不需要知道。

export interface PageQuery {
  page?: number;
  page_size?: number;
  sort?: string;
}
export interface CursorQuery {
  cursor?: string;
  limit?: number;
}
export interface AuditFilter extends CursorQuery {
  action?: string;
  resource_type?: string;
  resource_uid?: string;
  actor_uid?: string;
  result?: string;
  request_id?: string;
  from?: string;
  to?: string;
}
export interface LoginLogFilter extends CursorQuery {
  event?: string;
  identifier?: string;
  ip?: string;
  from?: string;
  to?: string;
}

const clean = <T extends object>(q: T): Record<string, string | number> => {
  const out: Record<string, string | number> = {};
  for (const [k, v] of Object.entries(q)) if (v !== undefined && v !== '') out[k] = v as string | number;
  return out;
};

/** 新的幂等键（创建类请求；同一次用户操作的重试复用同一个键）。 */
export const newIdempotencyKey = (): string => globalThis.crypto.randomUUID();

export function createAdminApi(c: ApiClient) {
  return {
    // ---- 用户 ----
    listUsers: (q: PageQuery, signal?: AbortSignal) => c.get<AdminUserList>('/admin/users', { query: clean(q), ...(signal ? { signal } : {}) }),
    createUser: (body: AdminUserCreate, idempotencyKey: string) => c.post<AdminUser>('/admin/users', { body, idempotencyKey }),
    updateUser: (uid: string, body: AdminUserUpdate) => c.patch<AdminUser>(`/admin/users/${encodeURIComponent(uid)}`, { body }),
    deleteUser: (uid: string) => c.delete<void>(`/admin/users/${encodeURIComponent(uid)}`),
    setUserRoles: (uid: string, roleCodes: string[]) => c.put<AdminUser>(`/admin/users/${encodeURIComponent(uid)}/roles`, { body: { role_codes: roleCodes } }),
    unlockUser: (uid: string) => c.post<void>(`/admin/users/${encodeURIComponent(uid)}/unlock`),
    /** newSecret 省略 = 由服务端生成并经响应回显一次。 */
    resetUserSecret: (uid: string, newSecret?: string) =>
      c.post<TemporaryPassword | undefined>(`/admin/users/${encodeURIComponent(uid)}/reset-password`, { body: newSecret ? { new_password: newSecret } : {} }),
    revokeUserSessions: (uid: string) => c.delete<void>(`/admin/users/${encodeURIComponent(uid)}/sessions`),
    // ---- 角色与权限 ----
    listRoles: (q: PageQuery, signal?: AbortSignal) => c.get<RoleList>('/admin/roles', { query: clean(q), ...(signal ? { signal } : {}) }),
    createRole: (body: RoleCreate, idempotencyKey: string) => c.post<Role>('/admin/roles', { body, idempotencyKey }),
    updateRole: (uid: string, body: { name?: string; description?: string }) => c.patch<Role>(`/admin/roles/${encodeURIComponent(uid)}`, { body }),
    deleteRole: (uid: string) => c.delete<void>(`/admin/roles/${encodeURIComponent(uid)}`),
    setRolePermissions: (uid: string, permissions: string[]) => c.put<Role>(`/admin/roles/${encodeURIComponent(uid)}/permissions`, { body: { permissions } }),
    listPermissions: (q: PageQuery, signal?: AbortSignal) => c.get<PermissionList>('/admin/permissions', { query: clean(q), ...(signal ? { signal } : {}) }),
    // ---- API Token（个人）----
    listApiTokens: (q: PageQuery, signal?: AbortSignal) => c.get<ApiTokenList>('/auth/api-tokens', { query: clean(q), ...(signal ? { signal } : {}) }),
    createApiToken: (body: ApiTokenCreate, idempotencyKey: string) => c.post<ApiTokenCreated>('/auth/api-tokens', { body, idempotencyKey }),
    rotateApiToken: (uid: string) => c.post<ApiTokenCreated>(`/auth/api-tokens/${encodeURIComponent(uid)}/rotate`),
    revokeApiToken: (uid: string) => c.delete<void>(`/auth/api-tokens/${encodeURIComponent(uid)}`),
    // ---- 日志 ----
    listAuditLogs: (q: AuditFilter, signal?: AbortSignal) => c.get<AuditLogCursorList>('/audit-logs', { query: clean(q), ...(signal ? { signal } : {}) }),
    listLoginLogs: (q: LoginLogFilter, signal?: AbortSignal) => c.get<LoginLogCursorList>('/admin/login-logs', { query: clean(q), ...(signal ? { signal } : {}) }),
    // ---- 系统设置 ----
    getSettings: (signal?: AbortSignal) => c.get<SettingsMap>('/admin/settings', signal ? { signal } : {}),
    patchSettings: (values: Record<string, unknown>) => c.patch<SettingsMap>('/admin/settings', { body: { values } }),
    getBrand: (signal?: AbortSignal) => c.get<BrandSettings>('/admin/settings/brand', signal ? { signal } : {}),
    putBrand: (body: BrandSettings) => c.put<BrandSettings>('/admin/settings/brand', { body }),
    getRunMode: (signal?: AbortSignal) => c.get<RunModeBody>('/admin/system/run-mode', signal ? { signal } : {}),
    putRunMode: (runMode: RunModeBody['run_mode']) => c.put<RunModeBody>('/admin/system/run-mode', { body: { run_mode: runMode } }),
    getVersion: (signal?: AbortSignal) => c.get<VersionInfo>('/admin/system/version', signal ? { signal } : {}),
  };
}
export type AdminApi = ReturnType<typeof createAdminApi>;
export type { ApiToken };
