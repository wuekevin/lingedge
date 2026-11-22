// 向导 API 的请求/响应类型：全部取自由 openapi.yaml 生成的 @lingedge/shared/api-types（只 import type，无运行时代码）。
import type { components } from '@lingedge/shared/api-types';

type S = components['schemas'];

export type SetupStatus = S['SetupStatus'];
export type SetupStepKey = NonNullable<SetupStatus['steps']>[number]['key'];
export type SetupStepState = NonNullable<SetupStatus['steps']>[number]['status'];
export type SetupCheckResult = S['SetupCheckResult'];
export type SetupCheckItem = SetupCheckResult['items'][number];
export type SetupDbConfig = S['SetupDbConfig'];
export type SetupRedisConfig = S['SetupRedisConfig'];
export type SetupAdminCreate = S['SetupAdminCreate'];
export type SetupSystemConfig = S['SetupSystemConfig'];
export type SetupStepResult = S['SetupStepResult'];
export type SetupMigration = NonNullable<SetupStepResult['migrations']>[number];

/** 向导的 8 个步骤（与 openapi SetupStatus.steps[].key 一一对应，顺序即流程）。 */
export const STEP_ORDER: readonly SetupStepKey[] = ['environment', 'database', 'redis', 'init_db', 'admin', 'system', 'keys', 'complete'];
