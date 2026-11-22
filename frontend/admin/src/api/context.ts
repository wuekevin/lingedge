import { inject, type InjectionKey } from 'vue';
import type { AdminApi } from './admin';

export const ADMIN_API_KEY: InjectionKey<AdminApi> = Symbol('le-admin-api');

/** 页面取业务 API（由 createAdminApp 提供；测试只替换其底层 fetch）。 */
export function useAdminApi(): AdminApi {
  const api = inject(ADMIN_API_KEY);
  if (!api) throw new Error('admin API is not provided (createAdminApp)');
  return api;
}
