import { computed, ref, type Ref } from 'vue';
import { useAsyncState } from './useAsyncState';

/**
 * 游标分页列表（审计/登录日志）：游标栈支持「上一页」，只认最后一次请求（useAsyncState）；筛选变化回到第一页。
 * fetch(cursor, limit, signal) 由页面提供，返回 { items, next_cursor }。
 */
export function useCursorList<T>(fetch: (cursor: string | undefined, limit: number, signal: AbortSignal) => Promise<{ items: T[]; next_cursor: string | null }>) {
  const limit = ref(20);
  const stack: Ref<(string | undefined)[]> = ref([undefined]); // stack[i] = 第 i+1 页的起始游标
  const index = ref(0);
  const state = useAsyncState((signal) => fetch(stack.value[index.value], limit.value, signal), (d) => d.items.length === 0);
  const next = computed(() => state.data.value?.next_cursor ?? null);

  async function reload(): Promise<void> {
    stack.value = [undefined];
    index.value = 0;
    await state.run();
  }
  async function goNext(): Promise<void> {
    const c = next.value;
    if (!c) return;
    stack.value = [...stack.value.slice(0, index.value + 1), c];
    index.value += 1;
    await state.run();
  }
  async function goPrev(): Promise<void> {
    if (index.value === 0) return;
    index.value -= 1;
    await state.run();
  }
  async function setLimit(n: number): Promise<void> {
    limit.value = n;
    await reload();
  }
  return { ...state, limit, pageNo: computed(() => index.value + 1), hasPrev: computed(() => index.value > 0), hasNext: computed(() => next.value !== null), reload, goNext, goPrev, setLimit };
}
