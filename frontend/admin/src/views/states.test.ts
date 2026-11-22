import { LeStateBoundary } from '@lingedge/shared';
import { flushPromises, mount } from '@vue/test-utils';
import { defineComponent, h } from 'vue';
import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/errors';
import { useAsyncState } from '../composables/useAsyncState';
import { createAdminI18n } from '../i18n';
import BootFailure from './BootFailure.vue';

const i18n = createAdminI18n('zh-CN');

// 页面按统一方式使用三态：useAsyncState 驱动 shared 的 LeStateBoundary。
function pageWith(load: () => Promise<number[]>) {
  return defineComponent({
    setup() {
      const s = useAsyncState(load, (d) => d.length === 0);
      void s.run();
      return () =>
        h(LeStateBoundary, { status: s.status.value, errorKind: s.errorKind.value, requestId: s.requestId.value, onRetry: s.run }, { default: () => h('p', { 'data-testid': 'ready' }, String(s.data.value?.length)) });
    },
  });
}

describe('统一三态（页面接入方式）', () => {
  it('加载中 → 就绪', async () => {
    const w = mount(pageWith(() => Promise.resolve([1, 2])), { global: { plugins: [i18n] } });
    expect(w.find('[data-le-state="loading"]').exists()).toBe(true);
    await flushPromises();
    expect(w.find('[data-testid="ready"]').text()).toBe('2');
  });
  it('空态：无数据，不含示例数字', async () => {
    const w = mount(pageWith(() => Promise.resolve([])), { global: { plugins: [i18n] } });
    await flushPromises();
    expect(w.find('[data-le-state="empty"]').exists()).toBe(true);
    expect(w.text()).not.toMatch(/\d/);
  });
  it('失败态：按原因给不同文案、展示 Request ID；点击重试回到加载并成功', async () => {
    let n = 0;
    const w = mount(
      pageWith(() => (++n === 1 ? Promise.reject(new ApiError({ status: 503, code: 'internal.error', requestId: 'RID-1' })) : Promise.resolve([1]))),
      { global: { plugins: [i18n] } },
    );
    await flushPromises();
    const err = w.find('[data-le-state="error"]');
    expect(err.attributes('data-le-error-kind')).toBe('server');
    expect(err.text()).toContain('RID-1');
    await w.find('.le-error__actions button').trigger('click');
    await flushPromises();
    expect(w.find('[data-testid="ready"]').exists()).toBe(true);
  });
  it('403 失败态不提供重试', async () => {
    const w = mount(pageWith(() => Promise.reject(new ApiError({ status: 403, code: 'auth.forbidden' }))), { global: { plugins: [i18n] } });
    await flushPromises();
    expect(w.find('[data-le-error-kind="forbidden"]').exists()).toBe(true);
    expect(w.find('.le-error__actions button').exists()).toBe(false);
  });
});

describe('启动失败页', () => {
  it('读取身份时网络中断/5xx 显示失败态与重试，而不是当作已退出', () => {
    const w = mount(BootFailure, { props: { error: new ApiError({ status: 0, code: 'client.network' }) }, global: { plugins: [i18n] } });
    expect(w.find('[data-le-error-kind="network"]').exists()).toBe(true);
    expect(w.find('.le-error__actions button').exists()).toBe(true);
  });
});
