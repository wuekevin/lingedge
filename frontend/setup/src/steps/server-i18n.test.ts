import { afterEach, describe, expect, it } from 'vitest';
import EnvStep from './EnvStep.vue';
import KeysStep from './KeysStep.vue';
import TokenGate from './TokenGate.vue';
import SetupError from '../components/SetupError.vue';
import { SetupApiError } from '../api/errors';
import en from '../locales/en.json';
import zh from '../locales/zh-CN.json';
import { apiError, fakeClient, ok } from '../testing/fakeClient';
import { click, mountStep, setValue, tid } from '../testing/mount';

afterEach(() => {
  document.body.replaceChildren();
});

const CJK = /[㐀-鿿]/;
const checks = {
  items: [
    { key: 'system.time', ok: true, severity: 'info', message: '系统时间 2026-11-22T01:02:03Z（UTC）' },
    { key: 'data_dir.writable', ok: false, severity: 'error', message: '数据目录不可写（/var/lib/lingedge）：请确认目录存在且属主为运行用户' },
    { key: 'disk.space', ok: true, severity: 'info', message: '磁盘剩余 12 GB' }, // 前端不认识的检查项
  ],
};

describe('向导服务端消息 i18n（Story 1.37b）', () => {
  it('英文界面：已知检查项显示英文并保留动态值；未知检查项回落服务端原文并标注 zh-CN', async () => {
    const m = await mountStep(EnvStep, { locale: 'en', client: fakeClient({ checks: async () => checks }) });
    const msgs = m.wrapper.findAll('[data-testid="check-msg"]');
    expect(msgs[0]!.text()).toBe('System time 2026-11-22T01:02:03Z (UTC)');
    expect(msgs[1]!.text()).toContain('/var/lib/lingedge');
    expect(CJK.test(msgs[0]!.text() + msgs[1]!.text())).toBe(false);
    expect(msgs[0]!.attributes('data-localized')).toBe('true');
    expect(msgs[2]!.text()).toBe('磁盘剩余 12 GB');
    expect(msgs[2]!.attributes('lang')).toBe('zh-CN');
    expect(msgs[2]!.attributes('data-localized')).toBe('false');
  });

  it('中文界面：同一数据显示中文词条', async () => {
    const m = await mountStep(EnvStep, { locale: 'zh-CN', client: fakeClient({ checks: async () => checks }) });
    expect(m.wrapper.findAll('[data-testid="check-msg"]')[0]!.text()).toBe('系统时间 2026-11-22T01:02:03Z（UTC）');
  });

  it('成功回执：英文界面显示英文（含 CA 指纹），不再出现中文', async () => {
    const msg = '内部身份与密钥已初始化（私有 CA、配置签名密钥、Server gRPC 叶证书、deploy.lineage）；CA 指纹 AB:CD:EF。请把主密钥 master.key 与数据库备份分开保管（丢失主密钥将无法解密任何私钥，F-7）';
    const m = await mountStep(KeysStep, { locale: 'en', client: fakeClient({ keys: async () => ok('keys', { message: msg }) }) });
    await click(m.wrapper, 'keys-run');
    const note = tid(m.wrapper, 'server-message');
    expect(note.text()).toContain('CA fingerprint AB:CD:EF');
    expect(CJK.test(note.text())).toBe(false);
    expect(note.attributes('data-localized')).toBe('true');
  });

  it('成功回执：句式对不上时回落原文并标注语言', async () => {
    const m = await mountStep(KeysStep, { locale: 'en', client: fakeClient({ keys: async () => ok('keys', { message: '一条前端不认识的新回执' }) }) });
    await click(m.wrapper, 'keys-run');
    const note = tid(m.wrapper, 'server-message');
    expect(note.text()).toBe('一条前端不认识的新回执');
    expect(note.attributes('lang')).toBe('zh-CN');
  });

  it('已知错误码：英文界面显示词条化说明 + 错误码 + Request ID', async () => {
    const client = fakeClient({
      status: async () => {
        throw apiError(401, 'setup.token_invalid', 'rid-9');
      },
    });
    const m = await mountStep(TokenGate, { client, locale: 'en', enter: false, props: { urlTokenIgnored: false } });
    await setValue(m.wrapper, 'setup_token', 'x');
    await click(m.wrapper, 'gate-submit');
    expect(tid(m.wrapper, 'error-message').text()).toBe(en.errors.code.setup.token_invalid);
    expect(tid(m.wrapper, 'error-message').attributes('data-localized')).toBe('true');
    expect(tid(m.wrapper, 'error-code').text()).toBe('setup.token_invalid');
    expect(m.wrapper.find('[data-le-request-id]').text()).toBe('rid-9');
  });

  it('未知错误码：回落到服务端 message，仍显示错误码与 Request ID', async () => {
    const e = new SetupApiError({ status: 409, code: 'setup.future_code', message: 'A brand new server message', requestId: 'rid-new' });
    const m = await mountStep(SetupError, { props: { error: e }, locale: 'zh-CN', enter: false });
    expect(tid(m.wrapper, 'error-message').text()).toBe('A brand new server message');
    expect(tid(m.wrapper, 'error-message').attributes('data-localized')).toBe('false');
    expect(tid(m.wrapper, 'error-message').attributes('lang')).toBe('en');
    expect(tid(m.wrapper, 'error-code').text()).toBe('setup.future_code');
    expect(m.wrapper.find('[data-le-request-id]').text()).toBe('rid-new');
  });

  it('详细原因：已知句式随界面语言；未知原文（驱动错误）原样保留', async () => {
    const e = new SetupApiError({
      status: 400,
      code: 'setup.precheck_failed',
      message: 'database precheck failed',
      requestId: 'rid-p',
      details: [{ reason: 'database "lingedge" already contains 3 table(s); the wizard never overwrites an existing database (FR-6). Use an empty dedicated database' }, { reason: 'dial tcp 127.0.0.1:3306: connect: connection refused' }],
    });
    const m = await mountStep(SetupError, { props: { error: e }, locale: 'zh-CN', enter: false });
    const rows = m.wrapper.findAll('[data-testid="error-reason"] span:last-child');
    expect(rows[0]!.text()).toContain('已含 3 张表');
    expect(rows[1]!.text()).toBe('dial tcp 127.0.0.1:3306: connect: connection refused');
    expect(rows[1]!.attributes('data-localized')).toBe('false');
  });

  it('词条自检：本测试引用的键存在于两种语言', () => {
    expect(zh.errors.code.setup.token_invalid).not.toBe('');
    expect(en.serverMsg.check.system_time_ok).toContain('{time}');
  });
});
