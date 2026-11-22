import { flushPromises, mount } from '@vue/test-utils';
import type { Component } from 'vue';
import { createSetupI18n } from '../i18n';
import { createWizard, WIZARD_KEY } from '../state/wizard';
import { fakeClient, type FakeClient } from './fakeClient';

export interface Mounted {
  wrapper: ReturnType<typeof mount>;
  wizard: ReturnType<typeof createWizard>;
  client: FakeClient;
}

/** 以真实的向导状态机 + 真实 i18n 挂载一个组件；只有网络层（SetupClient）是替身。 */
export async function mountStep(component: Component, opts: { client?: FakeClient; locale?: string; props?: Record<string, unknown>; done?: string[]; enter?: boolean } = {}): Promise<Mounted> {
  const client = opts.client ?? fakeClient();
  const store = new Map<string, string>();
  const wizard = createWizard(client as never, { storage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => void store.set(k, v), removeItem: (k) => void store.delete(k) } });
  if (opts.enter !== false) {
    (client.setToken as unknown as (t: string) => void)('t');
    wizard.state.phase = 'wizard';
  }
  for (const k of opts.done ?? []) wizard.state.done[k as keyof typeof wizard.state.done] = true;
  const wrapper = mount(component, { props: opts.props ?? {}, attachTo: document.body, global: { plugins: [createSetupI18n(opts.locale ?? 'zh-CN')], provide: { [WIZARD_KEY as symbol]: wizard } } });
  await flushPromises();
  return { wrapper, wizard, client };
}

export const tid = (w: Mounted['wrapper'], id: string) => w.find(`[data-testid="${id}"]`);
export async function setValue(w: Mounted['wrapper'], name: string, value: string) {
  await w.find(`[data-testid="f-${name}"]`).setValue(value);
}
export async function click(w: Mounted['wrapper'], id: string) {
  await w.find(`[data-testid="${id}"]`).trigger('click');
  await flushPromises();
}
export async function submit(w: Mounted['wrapper'], formTestId: string) {
  await w.find(`[data-testid="${formTestId}"]`).trigger('click');
  await flushPromises();
}
