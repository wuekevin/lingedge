// 类型全部来自 OpenAPI 生成物（只 import type；禁止手写重复类型，ADR-014）。
import type { components } from '@lingedge/shared/api-types';

type S = components['schemas'];
export type Me = S['Me'];
export type LoginRequest = S['LoginRequest'];
export type LoginResponse = S['LoginResponse'];
export type TotpVerifyRequest = S['TotpVerifyRequest'];
export type PasswordChangeRequest = S['PasswordChangeRequest'];
export type UserSummary = S['UserSummary'];
export type TenantSummary = S['TenantSummary'];
export type Tenant = S['Tenant'];
export type RunMode = NonNullable<Me['features']>['run_mode'];
export type ReauthResponse = S['ReauthResponse'];
export type AdminUser = S['User'];
export type AdminUserCreate = S['UserCreate'];
export type AdminUserUpdate = S['UserUpdate'];
export type AdminUserList = S['UserList'];
export type Role = S['Role'];
export type RoleCreate = S['RoleCreate'];
export type RoleList = S['RoleList'];
export type Permission = S['Permission'];
export type PermissionList = S['PermissionList'];
export type ApiToken = S['ApiToken'];
export type ApiTokenCreate = S['ApiTokenCreate'];
export type ApiTokenCreated = S['ApiTokenCreated'];
export type ApiTokenList = S['ApiTokenList'];
export type BrandSettings = S['BrandSettings'];
export type VersionInfo = S['VersionInfo'];
export type AuditLog = S['AuditLog'];
export type AuditLogCursorList = S['AuditLogCursorList'];
export type LoginLog = S['LoginLog'];
export type LoginLogCursorList = S['LoginLogCursorList'];
export type TemporaryPassword = S['TemporaryPassword'];
/** 系统设置：点分键 → JSON 值（敏感键只回 {configured}）。 */
export type SettingsMap = Record<string, unknown>;
export type RunModeBody = S['RunMode'];
