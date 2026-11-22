import { mount } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createLeI18n } from '../i18n';
import { errorKindFromStatus, ERROR_KINDS } from '../state/error-kind';
import LeEmpty from './LeEmpty.vue';
import LeError from './LeError.vue';
import LeLoading from './LeLoading.vue';
import LeStateBoundary from './LeStateBoundary.vue';

const withI18n = (locale = 'en') => ({ global: { plugins: [createLeI18n({ locale })] } });

describe('LeLoading', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('骨架 + 可访问性属性，初始不显示“仍在加载”', () => {
    const w = mount(LeLoading, { ...withI18n(), props: { lines: 4 } });
    expect(w.attributes('role')).toBe('status');
    expect(w.attributes('aria-busy')).toBe('true');
    expect(w.findAll('.le-loading__line')).toHaveLength(4);
    expect(w.text()).toContain('Loading…');
    expect(w.find('[data-le-slow]').exists()).toBe(false);
  });

  it('超过 slowAfterMs 追加“仍在加载…”与取消/重试，并发出事件', async () => {
    const w = mount(LeLoading, { ...withI18n('zh-CN'), props: { slowAfterMs: 10000 } });
    await vi.advanceTimersByTimeAsync(9999);
    expect(w.find('[data-le-slow]').exists()).toBe(false);
    await vi.advanceTimersByTimeAsync(1);
    expect(w.find('[data-le-slow]').exists()).toBe(true);
    expect(w.text()).toContain('仍在加载…');
    const [cancel, retry] = w.findAll('button');
    await cancel!.trigger('click');
    await retry!.trigger('click');
    expect(w.emitted('cancel')).toHaveLength(1);
    expect(w.emitted('retry')).toHaveLength(1);
  });

  it('卸载后清除计时器', () => {
    const w = mount(LeLoading, withI18n());
    w.unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe('LeError', () => {
  it('区块级错误卡：role=alert、原因、重试、Request ID', async () => {
    const w = mount(LeError, { ...withI18n('zh-CN'), props: { kind: 'server', subject: '站点列表', requestId: 'r_8f3a1c' } });
    expect(w.attributes('role')).toBe('alert');
    expect(w.text()).toContain('无法加载站点列表');
    expect(w.text()).toContain('主控暂时不可用');
    expect(w.find('[data-le-request-id]').text()).toBe('r_8f3a1c');
    await w.find('[data-le-retry]').trigger('click');
    expect(w.emitted('retry')).toHaveLength(1);
  });

  it.each(ERROR_KINDS)('每种类别 %s 都有非空文案（zh-CN 与 en）', (kind) => {
    for (const locale of ['zh-CN', 'en']) {
      const w = mount(LeError, { ...withI18n(locale), props: { kind } });
      const reason = w.find('.le-error__reason').text();
      expect(reason.length).toBeGreaterThan(5);
      expect(reason).not.toContain('state.error.kind'); // 未回退成键名
    }
  });

  it('403/404/400 默认不给重试；可显式覆盖', () => {
    for (const kind of ['forbidden', 'notFound', 'validation'] as const) {
      expect(mount(LeError, { ...withI18n(), props: { kind } }).find('[data-le-retry]').exists()).toBe(false);
    }
    expect(mount(LeError, { ...withI18n(), props: { kind: 'forbidden', retryable: true } }).find('[data-le-retry]').exists()).toBe(true);
    expect(mount(LeError, { ...withI18n(), props: { kind: 'server', retryable: false } }).find('[data-le-retry]').exists()).toBe(false);
  });

  it('复制 Request ID：成功与失败两种结果', async () => {
    const writeText = vi.fn().mockResolvedValueOnce(undefined).mockRejectedValueOnce(new Error('denied'));
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const w = mount(LeError, { ...withI18n('en'), props: { requestId: 'r_x' } });
    const btn = w.findAll('button').find((b) => b.text() === 'Copy')!;
    await btn.trigger('click');
    await Promise.resolve();
    expect(writeText).toHaveBeenCalledWith('r_x');
    expect(w.text()).toContain('Copied');
    await btn.trigger('click');
    await Promise.resolve();
    await w.vm.$nextTick();
    expect(w.text()).toContain('Copy failed');
  });

  it('HTTP 状态码到类别的映射', () => {
    const cases: Array<[number | null | undefined, string]> = [
      [null, 'network'],
      [0, 'network'],
      [400, 'validation'],
      [403, 'forbidden'],
      [404, 'notFound'],
      [409, 'conflict'],
      [412, 'preconditionFailed'],
      [429, 'rateLimited'],
      [500, 'server'],
      [503, 'server'],
      [418, 'unknown'],
    ];
    for (const [status, want] of cases) expect(errorKindFromStatus(status)).toBe(want);
  });

  it('服务端 detail 以文本插值呈现，不被当作 HTML', () => {
    const w = mount(LeError, { ...withI18n(), props: { detail: '<img src=x onerror=alert(1)>' } });
    expect(w.find('img').exists()).toBe(false);
    expect(w.find('.le-error__detail').text()).toBe('<img src=x onerror=alert(1)>');
  });
});

describe('LeEmpty', () => {
  it('尚无数据：说明 + action 插槽，不含任何数字示例', () => {
    const w = mount(LeEmpty, { ...withI18n('zh-CN'), slots: { action: '<button data-test-action>新建站点</button>' } });
    expect(w.text()).toContain('尚无数据');
    expect(w.find('[data-test-action]').exists()).toBe(true);
    expect(w.find('[data-le-clear-filters]').exists()).toBe(false);
    expect(w.text()).not.toMatch(/\d/);
  });

  it('筛选无结果：提供清除筛选并发事件', async () => {
    const w = mount(LeEmpty, { ...withI18n('en'), props: { reason: 'no-results' } });
    expect(w.text()).toContain('No results match the filters');
    await w.find('[data-le-clear-filters]').trigger('click');
    expect(w.emitted('clear-filters')).toHaveLength(1);
  });

  it('可覆盖标题/说明', () => {
    const w = mount(LeEmpty, { ...withI18n(), props: { title: 'T', description: 'D' } });
    expect(w.text()).toContain('T');
    expect(w.text()).toContain('D');
  });
});

describe('LeStateBoundary', () => {
  const slots = { default: '<p data-ready>ready</p>' };
  it.each([
    ['loading', 'loading'],
    ['error', 'error'],
    ['empty', 'empty'],
  ] as const)('status=%s 渲染对应状态且不渲染内容', (status, marker) => {
    const w = mount(LeStateBoundary, { ...withI18n(), props: { status }, slots });
    expect(w.find(`[data-le-state="${marker}"]`).exists()).toBe(true);
    expect(w.find('[data-ready]').exists()).toBe(false);
  });
  it('status=ready 只渲染内容', () => {
    const w = mount(LeStateBoundary, { ...withI18n(), props: { status: 'ready' }, slots });
    expect(w.find('[data-ready]').exists()).toBe(true);
    expect(w.find('[data-le-state]').exists()).toBe(false);
  });
  it('失败态透传类别/Request ID，并转发 retry', async () => {
    const w = mount(LeStateBoundary, { ...withI18n(), props: { status: 'error', errorKind: 'network', requestId: 'r_1' } });
    expect(w.find('[data-le-error-kind="network"]').exists()).toBe(true);
    expect(w.find('[data-le-request-id]').text()).toBe('r_1');
    await w.find('[data-le-retry]').trigger('click');
    expect(w.emitted('retry')).toHaveLength(1);
  });
});
