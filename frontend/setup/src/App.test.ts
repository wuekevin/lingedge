import { flushPromises } from '@vue/test-utils';
import { afterEach, describe, expect, it } from 'vitest';
import App from './App.vue';
import en from './locales/en.json';
import zh from './locales/zh-CN.json';
import { fakeClient, status } from './testing/fakeClient';
import { click, mountStep, setValue, tid } from './testing/mount';

afterEach(() => {
  document.body.replaceChildren();
});

describe('应用外壳', () => {
  it('门禁 → 8 步导航：步骤名中英文、当前步骤 aria-current、未完成步骤不可点', async () => {
    const client = fakeClient({ status: async () => status(['environment']), checks: async () => ({ items: [{ key: 'server.version', ok: true, severity: 'info', message: 'm' }] }) });
    const m = await mountStep(App, { client, enter: false });
    expect(tid(m.wrapper, 'gate').exists()).toBe(true);
    expect(tid(m.wrapper, 'stepper').exists()).toBe(false); // 未验证 token 前不展示任何向导内容
    await setValue(m.wrapper, 'setup_token', 'tok');
    await click(m.wrapper, 'gate-submit');
    const nav = tid(m.wrapper, 'stepper');
    expect(nav.findAll('li')).toHaveLength(8);
    expect(nav.text()).toContain(zh.steps.init_db);
    expect(tid(m.wrapper, 'nav-database').attributes('aria-current')).toBe('step'); // 续做：第一个未完成步骤
    expect(tid(m.wrapper, 'nav-admin').attributes('disabled')).toBeDefined(); // 不可跳过
    expect(tid(m.wrapper, 'nav-environment').attributes('disabled')).toBeUndefined(); // 已完成可回看
  });

  it('语言切换：中文 ↔ English，<html lang> 与文档标题同步', async () => {
    const m = await mountStep(App, { enter: false });
    expect(m.wrapper.text()).toContain(zh.gate.title);
    await m.wrapper.find('[data-testid="lang-select"]').setValue('en');
    expect(m.wrapper.text()).toContain(en.gate.title);
    expect(document.documentElement.getAttribute('lang')).toBe('en');
    expect(document.title).toBe(en.app.title);
  });

  it('键盘与读屏：有跳转链接；切换步骤后焦点移到新步骤标题', async () => {
    const client = fakeClient({ status: async () => status(['environment']) });
    const m = await mountStep(App, { client, enter: false });
    expect(m.wrapper.find('a.skip').attributes('href')).toBe('#main');
    await setValue(m.wrapper, 'setup_token', 'tok');
    await click(m.wrapper, 'gate-submit');
    await flushPromises();
    await new Promise((r) => setTimeout(r, 0));
    expect(document.activeElement?.id).toBe('step-database-title');
    // 所有表单控件都有关联的 label
    for (const input of m.wrapper.findAll('input')) {
      const id = input.attributes('id');
      expect(id, '控件缺 id').toBeTruthy();
      expect(m.wrapper.find(`label[for="${id}"]`).exists(), `控件 ${id} 缺 label`).toBe(true);
    }
  });

  it('已安装（410）：任何时刻转入关闭页，stepper 消失', async () => {
    const m = await mountStep(App, { client: fakeClient({ status: async () => status([], true) }), enter: false });
    await setValue(m.wrapper, 'setup_token', 'tok');
    await click(m.wrapper, 'gate-submit');
    expect(tid(m.wrapper, 'installed').exists()).toBe(true);
    expect(tid(m.wrapper, 'stepper').exists()).toBe(false);
  });
});
