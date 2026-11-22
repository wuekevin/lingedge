import { defineStore } from 'pinia';
import { ref } from 'vue';
import type { AuthApi } from '../api/auth';

// L4 再认证（step-up，IMP-12）：窗口外的敏感操作被服务端拒绝（403 auth.reauth_required）时，API 客户端调用 request() 弹出验证框；
// 用户提交口令（已启用 TOTP 另加 6 位验证码）→ POST /auth/reauth（服务端校验，前端不做任何判定）→ 成功后客户端自动重试原请求。
// 口令/验证码只在组件的输入框里短暂存在，不写入 store、不写任何浏览器存储。

let authApi: AuthApi | null = null;
export function bindReauthApi(api: AuthApi): void {
  authApi = api;
}

export const useReauthStore = defineStore('reauth', () => {
  const open = ref(false);
  const busy = ref(false);
  const error = ref<unknown>(null);
  // 挂起的请求与其 resolver 属于 store 实例（不是模块全局）：每个应用实例互不影响。
  let pending: Promise<boolean> | null = null;
  let settle: ((ok: boolean) => void) | null = null;

  function finish(ok: boolean): void {
    open.value = false;
    error.value = null;
    const s = settle;
    pending = null;
    settle = null;
    s?.(ok);
  }
  /** 并发的多个再认证需求共用同一个验证框。 */
  function request(): Promise<boolean> {
    if (pending) return pending;
    open.value = true;
    error.value = null;
    pending = new Promise<boolean>((resolve) => {
      settle = resolve;
    });
    return pending;
  }
  async function submit(secret: string, totpCode?: string): Promise<void> {
    if (!authApi) throw new Error('reauth: auth API is not bound');
    busy.value = true;
    error.value = null;
    try {
      await authApi.reauth(secret, totpCode);
      finish(true);
    } catch (e) {
      error.value = e;
    } finally {
      busy.value = false;
    }
  }
  const cancel = (): void => finish(false);
  return { open, busy, error, request, submit, cancel, finish };
});
