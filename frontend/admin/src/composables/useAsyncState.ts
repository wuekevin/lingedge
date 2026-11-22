import { computed, ref, type Ref } from 'vue';
import type { LeErrorKind, LeStatus } from '@lingedge/shared';
import { errorKind, isApiError } from '../api/errors';

/**
 * 统一三态（FR-30）：loading / error / empty / ready。页面用它驱动 shared 的 LeStateBoundary：
 *   const s = useAsyncState(() => api.list(), (d) => d.items.length === 0);
 *   <LeStateBoundary :status="s.status.value" :error-kind="s.errorKind.value" :request-id="s.requestId.value" @retry="s.run" />
 * 重复触发时只认最后一次请求的结果（旧响应不覆盖新状态）。
 */
export function useAsyncState<T>(load: (signal: AbortSignal) => Promise<T>, isEmpty: (data: T) => boolean = () => false) {
  const status: Ref<LeStatus> = ref('loading');
  const data = ref<T | null>(null) as Ref<T | null>;
  const error = ref<unknown>(null);
  let seq = 0;
  let ctrl: AbortController | null = null;

  async function run(): Promise<void> {
    const mine = ++seq;
    ctrl?.abort();
    ctrl = new AbortController();
    status.value = 'loading';
    error.value = null;
    try {
      const d = await load(ctrl.signal);
      if (mine !== seq) return;
      data.value = d;
      status.value = isEmpty(d) ? 'empty' : 'ready';
    } catch (e) {
      if (mine !== seq || (e instanceof DOMException && e.name === 'AbortError')) return;
      error.value = e;
      status.value = 'error';
    }
  }
  function cancel(): void {
    seq++;
    ctrl?.abort();
  }
  const kind = computed<LeErrorKind>(() => errorKind(error.value));
  const requestId = computed(() => (isApiError(error.value) ? error.value.requestId : ''));
  return { status, data, error, errorKind: kind, requestId, run, cancel };
}
