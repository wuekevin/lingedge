import { computed } from 'vue';
import { useSessionStore } from '../stores/session';

/** 按钮级权限（后端仍是最终裁决）：当前身份是否具备全部给定权限点。 */
export function useCan() {
  const session = useSessionStore();
  const perms = computed(() => new Set(session.me?.permissions ?? []));
  return (...need: string[]): boolean => need.every((p) => perms.value.has(p));
}
