import { flushPromises } from '@vue/test-utils';
import { afterEach, describe, expect, it } from 'vitest';
import EnvStep from './EnvStep.vue';
import InstalledView from './InstalledView.vue';
import TokenGate from './TokenGate.vue';
import zh from '../locales/zh-CN.json';
import { apiError, fakeClient, status } from '../testing/fakeClient';
import { click, mountStep, setValue, tid } from '../testing/mount';

afterEach(() => {
  document.body.replaceChildren();
});

describe('令牌门禁', () => {
  it('空令牌：本地提示，不发请求', async () => {
    const m = await mountStep(TokenGate, { enter: false, props: { urlTokenIgnored: false } });
    await click(m.wrapper, 'gate-submit');
    expect(m.wrapper.text()).toContain(zh.gate.empty);
    expect(m.client.status).not.toHaveBeenCalled();
  });

  it('输入框是不回显的口令类控件；错 token 显示稳定错误码、Request ID 与处置提示，并清空输入', async () => {
    const client = fakeClient({
      status: async () => {
        throw apiError(401, 'setup.token_invalid', 'rid-401');
      },
    });
    const m = await mountStep(TokenGate, { client, enter: false, props: { urlTokenIgnored: false } });
    const input = m.wrapper.find('[data-testid="f-setup_token"]');
    expect(input.attributes('type')).toBe('password');
    expect(input.attributes('autocomplete')).toBe('off');
    await setValue(m.wrapper, 'setup_token', 'wrong-token');
    await click(m.wrapper, 'gate-submit');
    expect(tid(m.wrapper, 'error-code').text()).toBe('setup.token_invalid');
    expect(m.wrapper.find('[data-le-request-id]').text()).toBe('rid-401');
    expect(tid(m.wrapper, 'error-hint').text()).toBe(zh.errors.hint.setup.token_invalid);
    expect((input.element as HTMLInputElement).value).toBe('');
    expect(client.token).toBe(''); // 失败后 token 不留在内存
    expect(m.wizard.state.phase).toBe('gate');
  });

  it('429 限流：展示 Retry-After 秒数', async () => {
    const client = fakeClient({
      status: async () => {
        throw apiError(429, 'rate_limit.exceeded', 'rid-429', [], 30);
      },
    });
    const m = await mountStep(TokenGate, { client, enter: false, props: { urlTokenIgnored: false } });
    await setValue(m.wrapper, 'setup_token', 'x');
    await click(m.wrapper, 'gate-submit');
    expect(m.wrapper.text()).toContain('30');
    expect(tid(m.wrapper, 'error-code').text()).toBe('rate_limit.exceeded');
  });

  it('正确 token：进入向导；token 不写 sessionStorage/localStorage/URL', async () => {
    const m = await mountStep(TokenGate, { client: fakeClient({ status: async () => status(['environment']) }), enter: false, props: { urlTokenIgnored: false } });
    await setValue(m.wrapper, 'setup_token', 'good-token-xyz');
    await click(m.wrapper, 'gate-submit');
    expect(m.wizard.state.phase).toBe('wizard');
    expect(m.client.token).toBe('good-token-xyz');
    expect(JSON.stringify(Object.entries(window.sessionStorage))).not.toContain('good-token-xyz');
    expect(JSON.stringify(Object.entries(window.localStorage))).not.toContain('good-token-xyz');
    expect(window.location.href).not.toContain('good-token-xyz');
    expect(document.cookie).not.toContain('good-token-xyz');
  });

  it('URL 里的 token 被忽略时给出说明', async () => {
    const m = await mountStep(TokenGate, { enter: false, props: { urlTokenIgnored: true } });
    expect(tid(m.wrapper, 'url-token-ignored').text()).toBe(zh.gate.urlTokenIgnored);
  });

  it('已安装页：展示 410 错误码与说明', async () => {
    const m = await mountStep(InstalledView, { enter: false });
    m.wizard.guard(apiError(410, 'setup.already_installed', 'rid-410'));
    await flushPromises();
    expect(m.wrapper.text()).toContain(zh.installed.title);
    expect(tid(m.wrapper, 'error-code').text()).toBe('setup.already_installed');
  });
});

describe('环境检查：三态', () => {
  it('loading → ready：逐项显示严重级别；无阻断时标记完成', async () => {
    let release: (v: unknown) => void = () => {};
    const gate = new Promise((r) => (release = r));
    const client = fakeClient({
      checks: async () => {
        await gate;
        return {
          items: [
            { key: 'server.version', ok: true, severity: 'info', message: 'lingedge-server v1' },
            { key: 'listen.loopback', ok: true, severity: 'warn', message: 'non loopback' },
          ],
        };
      },
    });
    const m = await mountStep(EnvStep, { client });
    expect(m.wrapper.find('[data-le-state="loading"]').exists()).toBe(true);
    release(null);
    await flushPromises();
    expect(m.wrapper.find('[data-le-state="loading"]').exists()).toBe(false);
    expect(m.wrapper.findAll('[data-check]')).toHaveLength(2);
    expect(m.wrapper.text()).toContain(zh.env.item.server.version);
    expect(m.wrapper.find('[data-check="listen.loopback"]').attributes('data-severity')).toBe('warn');
    expect(m.wizard.state.done.environment).toBe(true);
    expect(m.wrapper.text()).toContain(zh.env.passedWithWarnings);
  });

  it('阻断项：显示阻断提示，不标记完成', async () => {
    const m = await mountStep(EnvStep, { client: fakeClient({ checks: async () => ({ items: [{ key: 'master_key.readable', ok: false, severity: 'error', message: 'no key' }] }) }) });
    expect(tid(m.wrapper, 'env-blocking').exists()).toBe(true);
    expect(m.wizard.state.done.environment).toBe(false);
  });

  it('error：展示错误码与 Request ID，重试后恢复', async () => {
    let n = 0;
    const client = fakeClient({
      checks: async () => {
        if (n++ === 0) throw apiError(500, 'internal.error', 'rid-500');
        return { items: [{ key: 'server.version', ok: true, severity: 'info', message: 'm' }] };
      },
    });
    const m = await mountStep(EnvStep, { client });
    expect(tid(m.wrapper, 'error-code').text()).toBe('internal.error');
    expect(m.wrapper.find('[data-le-request-id]').text()).toBe('rid-500');
    await m.wrapper.find('[data-le-retry]').trigger('click');
    await flushPromises();
    expect(m.wrapper.find('[data-check]').exists()).toBe(true);
  });

  it('empty：服务端没有返回检查项 → 空态与重新检查，且不算完成', async () => {
    const m = await mountStep(EnvStep, { client: fakeClient({ checks: async () => ({ items: [] }) }) });
    expect(m.wrapper.find('[data-le-state="empty"]').exists()).toBe(true);
    expect(m.wizard.state.done.environment).toBe(false);
    expect(tid(m.wrapper, 'env-rerun').exists()).toBe(true);
  });

  it('步骤请求遇到 410：转入已安装，不展示为普通错误', async () => {
    const m = await mountStep(EnvStep, {
      client: fakeClient({
        checks: async () => {
          throw apiError(410, 'setup.already_installed');
        },
      }),
    });
    expect(m.wizard.state.phase).toBe('installed');
  });
});
