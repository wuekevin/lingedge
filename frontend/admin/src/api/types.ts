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
