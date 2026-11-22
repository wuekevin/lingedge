import { ref } from 'vue';

/** 写操作的统一状态：busy / error；run 返回结果，失败返回 undefined 且错误留在 error（由页面按错误码显示，不吞掉）。 */
export function useAction() {
  const busy = ref(false);
  const error = ref<unknown>(null);
  async function run<T>(fn: () => Promise<T>): Promise<{ ok: true; value: T } | { ok: false }> {
    if (busy.value) return { ok: false };
    busy.value = true;
    error.value = null;
    try {
      return { ok: true, value: await fn() };
    } catch (e) {
      error.value = e;
      return { ok: false };
    } finally {
      busy.value = false;
    }
  }
  return { busy, error, run, reset: () => (error.value = null) };
}
