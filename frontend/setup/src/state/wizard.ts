import { computed, inject, reactive, type InjectionKey } from 'vue';
import { isAlreadyInstalled, type SetupApiError } from '../api/errors';
import type { SetupClient } from '../api/client';
import { STEP_ORDER, type SetupStatus, type SetupStepKey } from '../api/types';

export type Phase = 'gate' | 'wizard' | 'installed';

/** 非机密的草稿（访问地址）可以在同一标签页内跨刷新保留；setup token 与任何口令绝不进入存储。 */
const BASE_URL_KEY = 'le.setup.baseUrl';

export interface WizardOptions {
  /** 测试注入；缺省取 window.sessionStorage（不可用时退化为内存）。 */
  storage?: Pick<Storage, 'getItem' | 'setItem' | 'removeItem'> | null;
}

export function createWizard(client: SetupClient, options: WizardOptions = {}) {
  const storage = options.storage === undefined ? safeSessionStorage() : options.storage;
  const state = reactive({
    phase: 'gate' as Phase,
    step: 'environment' as SetupStepKey,
    done: Object.fromEntries(STEP_ORDER.map((k) => [k, false])) as Record<SetupStepKey, boolean>,
    serverVersion: '',
    baseUrl: storage?.getItem(BASE_URL_KEY) ?? '',
    /** complete 成功后为 true：向导不再发任何请求。 */
    finished: false,
    /** 最近一次导致进入“已安装”的错误（展示错误码与 Request ID）。 */
    installedError: null as SetupApiError | null,
  });

  const stepIndex = computed(() => STEP_ORDER.indexOf(state.step));
  /** 可到达的最远步骤：第一个未完成的步骤（不允许跳过未完成的步骤）。 */
  const frontier = computed(() => {
    const i = STEP_ORDER.findIndex((k) => k !== 'complete' && !state.done[k]);
    return i === -1 ? STEP_ORDER.length - 1 : i;
  });

  function applyStatus(st: SetupStatus): void {
    state.serverVersion = st.server_version ?? '';
    if (st.installed) {
      state.phase = 'installed';
      return;
    }
    for (const k of STEP_ORDER) state.done[k] = false;
    for (const s of st.steps ?? []) state.done[s.key] = s.status === 'done';
    state.phase = 'wizard';
    // 中断续做：回到第一个未完成的步骤；环境检查总是先重新跑一遍（服务端重启后该进度在内存里已丢失）。
    state.step = STEP_ORDER[frontier.value] ?? 'environment';
  }

  /** 校验 token 并载入进度。失败时清除 token（只在内存里存在过）并把错误抛给调用方展示。 */
  async function verifyToken(token: string): Promise<void> {
    client.setToken(token);
    try {
      applyStatus(await client.status());
    } catch (e) {
      client.setToken('');
      if (isAlreadyInstalled(e)) enterInstalled(e as SetupApiError);
      throw e;
    }
  }

  /** 步骤请求失败时先过这里：410 = 向导已关闭，统一转入“已安装”页并停止后续请求。返回 true 表示已处理。 */
  function guard(e: unknown): boolean {
    if (isAlreadyInstalled(e)) {
      enterInstalled(e as SetupApiError);
      return true;
    }
    return false;
  }

  function enterInstalled(e: SetupApiError): void {
    client.setToken('');
    state.installedError = e;
    state.phase = 'installed';
  }

  function markDone(step: SetupStepKey, done = true): void {
    state.done[step] = done;
    // 回头重做某一步会使其后依赖的步骤在服务端重新校验；本地只标记当前步骤，其余以服务端状态为准（进入步骤时刷新）。
  }

  function go(step: SetupStepKey): void {
    const to = STEP_ORDER.indexOf(step);
    if (to <= frontier.value || state.done[step]) state.step = step;
  }
  function next(): void {
    const i = stepIndex.value;
    const n = STEP_ORDER[i + 1];
    if (n) state.step = n;
  }
  function back(): void {
    const p = STEP_ORDER[stepIndex.value - 1];
    if (p) state.step = p;
  }

  function setBaseUrl(url: string): void {
    state.baseUrl = url;
    try {
      if (url) storage?.setItem(BASE_URL_KEY, url);
      else storage?.removeItem(BASE_URL_KEY);
    } catch {
      /* 存储不可用：仅内存 */
    }
  }

  /** complete 成功：token 立即清除，向导不再发请求（服务端此后也只会返回 410）。 */
  function finish(): void {
    client.setToken('');
    state.finished = true;
    state.done.complete = true;
  }

  return { state, stepIndex, frontier, client, verifyToken, applyStatus, guard, markDone, go, next, back, setBaseUrl, finish };
}

export type Wizard = ReturnType<typeof createWizard>;
export const WIZARD_KEY: InjectionKey<Wizard> = Symbol('le-setup-wizard');

export function useWizard(): Wizard {
  const w = inject(WIZARD_KEY);
  if (!w) throw new Error('wizard not provided');
  return w;
}

function safeSessionStorage(): Storage | null {
  try {
    return typeof window !== 'undefined' ? window.sessionStorage : null;
  } catch {
    return null;
  }
}
