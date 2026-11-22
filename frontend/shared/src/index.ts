// @lingedge/shared：只放无权限语义的内容（ADR-014 §1/§2）。
// 禁止出现：路由表、菜单、权限常量与判定、会话/登录逻辑、API 客户端实例（scripts/lint-guards.mjs 强制）。
export { default as LeLoading } from './components/LeLoading.vue';
export { default as LeError } from './components/LeError.vue';
export { default as LeEmpty } from './components/LeEmpty.vue';
export { default as LeStateBoundary } from './components/LeStateBoundary.vue';
export { ERROR_KINDS, errorKindFromStatus, isRetryableKind, type LeErrorKind } from './state/error-kind';
export type { LeStatus } from './state/status';
export * from './format';
export * from './i18n';
export { applyTheme, type LeTheme } from './theme';
