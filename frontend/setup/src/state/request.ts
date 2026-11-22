import { ref, shallowRef } from 'vue';
import type { LeStatus } from '@lingedge/shared';
import { CODE_NETWORK, SetupApiError } from '../api/errors';
import type { Wizard } from './wizard';

/** 把任何异常规整为 SetupApiError（保证界面总能展示稳定错误码）。 */
export function toApiError(e: unknown): SetupApiError {
  if (e instanceof SetupApiError) return e;
  return new SetupApiError({ status: 0, code: CODE_NETWORK, message: e instanceof Error ? e.message : 'unknown error' });
}

/**
 * 一个步骤的“请求结果区”三态：loading（请求进行中）/ error（失败，带错误码与 Request ID）/ empty（尚未提交或无结果）/ ready（成功）。
 * 410 统一交给向导转入“已安装”页，不在本步骤展示为普通错误。
 */
export function useStepRequest(wizard: Wizard, initial: LeStatus = 'empty') {
  const status = ref<LeStatus>(initial);
  const error = shallowRef<SetupApiError | null>(null);

  async function run<T>(fn: () => Promise<T>): Promise<{ ok: true; value: T } | { ok: false }> {
    status.value = 'loading';
    error.value = null;
    try {
      const value = await fn();
      status.value = 'ready';
      return { ok: true, value };
    } catch (e) {
      if (wizard.guard(e)) return { ok: false };
      error.value = toApiError(e);
      status.value = 'error';
      return { ok: false };
    }
  }
  return { status, error, run };
}
