import { flushPromises } from '@vue/test-utils';
import { afterEach, describe, expect, it } from 'vitest';
import AdminStep from './AdminStep.vue';
import CompleteStep from './CompleteStep.vue';
import DatabaseStep from './DatabaseStep.vue';
import InitStep from './InitStep.vue';
import KeysStep from './KeysStep.vue';
import RedisStep from './RedisStep.vue';
import SystemStep from './SystemStep.vue';
import en from '../locales/en.json';
import zh from '../locales/zh-CN.json';
import { apiError, fakeClient, ok, rnd } from '../testing/fakeClient';
import { click, mountStep, setValue, tid } from '../testing/mount';

afterEach(() => {
  document.body.replaceChildren();
});

const ADMIN_PW = `Zq9-${rnd()}-Hx!`;
const tmpAdminPw = rnd('tmp');
const appPw = rnd('app');
const migPw = rnd('mig');
const redisPw = rnd('rds');
const genMigPw = rnd('gen');
const totpKey = 'JBSWY3DPEHPK3PXP';

describe('MySQL 步骤', () => {
  it('建账号模式（TCP）：口令类字段不回显；应用账号口令留空时本地生成；提交体符合契约', async () => {
    const m = await mountStep(DatabaseStep);
    for (const n of ['password', 'admin_password', 'migrate_password', 'maint_password']) expect(m.wrapper.find(`[data-testid="f-${n}"]`).attributes('type')).toBe('password');
    await setValue(m.wrapper, 'admin_username', 'root_tmp');
    await setValue(m.wrapper, 'admin_password', tmpAdminPw);
    await click(m.wrapper, 'db-submit');
    expect(m.client.database).toHaveBeenCalledTimes(1);
    const body = m.client.database.mock.calls[0]![0] as Record<string, unknown>;
    expect(body).toMatchObject({ host: '127.0.0.1', port: 3306, database: 'lingedge', username: 'lingedge_app', create_if_missing: true, admin_username: 'root_tmp', admin_password: tmpAdminPw, tls: false });
    expect(String(body.password)).toMatch(/^[A-Za-z0-9]{32}$/);
    expect(body).not.toHaveProperty('socket');
    expect(body).not.toHaveProperty('migrate_password');
    expect(m.wizard.state.done.database).toBe(true);
    expect(tid(m.wrapper, 'db-ok').exists()).toBe(true);
    // 成功后口令字段被清空，DOM 里找不到任何口令明文
    expect((m.wrapper.find('[data-testid="f-admin_password"]').element as HTMLInputElement).value).toBe('');
    expect(document.body.textContent).not.toContain(tmpAdminPw);
  });

  it('Unix socket：只发 socket，不发 host/port/tls；socket 必须是绝对路径', async () => {
    const m = await mountStep(DatabaseStep);
    await m.wrapper.find('[data-testid="r-conn-socket"]').setValue(true);
    await m.wrapper.find('[data-testid="r-mode-existing"]').setValue(true);
    await setValue(m.wrapper, 'socket', 'relative/mysql.sock');
    await setValue(m.wrapper, 'password', appPw);
    await setValue(m.wrapper, 'migrate_password', migPw);
    await click(m.wrapper, 'db-submit');
    expect(m.client.database).not.toHaveBeenCalled();
    expect(m.wrapper.text()).toContain(zh.db.err.socketAbsolute);
    await setValue(m.wrapper, 'socket', '/var/run/mysqld/mysqld.sock');
    await click(m.wrapper, 'db-submit');
    const body = m.client.database.mock.calls[0]![0] as Record<string, unknown>;
    expect(body).toMatchObject({ socket: '/var/run/mysqld/mysqld.sock', username: 'lingedge_app', password: appPw, migrate_password: migPw });
    expect(body).not.toHaveProperty('host');
    expect(body).not.toHaveProperty('port');
    expect(body).not.toHaveProperty('tls');
    expect(body).not.toHaveProperty('create_if_missing');
  });

  it('非回环主机必须开启 TLS（客户端先拦，不发请求）；预建账号模式必填迁移口令', async () => {
    const m = await mountStep(DatabaseStep);
    await m.wrapper.find('[data-testid="r-mode-existing"]').setValue(true);
    await setValue(m.wrapper, 'host', 'db.example.test');
    await setValue(m.wrapper, 'password', 'app-pw');
    await click(m.wrapper, 'db-submit');
    expect(m.client.database).not.toHaveBeenCalled();
    expect(tid(m.wrapper, 'db-tls-error').text()).toBe(zh.db.tlsRequired);
    expect(m.wrapper.text()).toContain(zh.db.err.required); // 迁移口令必填
  });

  it('服务端失败（setup.database_not_empty）：展示错误码、Request ID、处置提示与逐字段原因；表单保留', async () => {
    const client = fakeClient({
      database: async () => {
        throw apiError(400, 'setup.database_not_empty', 'rid-ne', [{ field: 'database', reason: 'database is not empty' }]);
      },
    });
    const m = await mountStep(DatabaseStep, { client });
    await setValue(m.wrapper, 'admin_username', 'root_tmp');
    await setValue(m.wrapper, 'admin_password', 'pw');
    await click(m.wrapper, 'db-submit');
    expect(tid(m.wrapper, 'error-code').text()).toBe('setup.database_not_empty');
    expect(m.wrapper.find('[data-le-request-id]').text()).toBe('rid-ne');
    expect(tid(m.wrapper, 'error-hint').text()).toBe(zh.errors.hint.setup.database_not_empty);
    expect(tid(m.wrapper, 'error-reason').text()).toContain('database is not empty');
    expect(m.wizard.state.done.database).toBe(false);
    expect((m.wrapper.find('[data-testid="f-database"]').element as HTMLInputElement).value).toBe('lingedge');
  });

  it('三态：初始为空态；请求中为 loading 且表单被禁用；失败为 error', async () => {
    let release: (v: unknown) => void = () => {};
    const gate = new Promise((r) => (release = r));
    const m = await mountStep(DatabaseStep, {
      client: fakeClient({
        database: async () => {
          await gate;
          return ok('database');
        },
      }),
    });
    expect(tid(m.wrapper, 'db-empty').exists()).toBe(true);
    await setValue(m.wrapper, 'admin_username', 'a');
    await setValue(m.wrapper, 'admin_password', 'b');
    await m.wrapper.find('[data-testid="db-submit"]').trigger('click');
    await flushPromises();
    expect(m.wrapper.find('[data-le-state="loading"]').exists()).toBe(true);
    expect(m.wrapper.find('fieldset').attributes('disabled')).toBeDefined();
    release(null);
    await flushPromises();
    expect(tid(m.wrapper, 'db-ok').exists()).toBe(true);
  });

  it('续做：此前已完成时提示需重新提交迁移口令', async () => {
    const m = await mountStep(DatabaseStep, { done: ['database'] });
    expect(tid(m.wrapper, 'db-resume-hint').text()).toBe(zh.db.resumeHint);
  });
});

describe('Redis 步骤', () => {
  it('地址格式与库号校验；成功提交并清空口令', async () => {
    const m = await mountStep(RedisStep);
    await setValue(m.wrapper, 'address', 'nohostport');
    await click(m.wrapper, 'redis-submit');
    expect(m.client.redis).not.toHaveBeenCalled();
    expect(m.wrapper.text()).toContain(zh.redis.err.addressFormat);
    await setValue(m.wrapper, 'address', '127.0.0.1:6380');
    await setValue(m.wrapper, 'redis_password', redisPw);
    await click(m.wrapper, 'redis-submit');
    expect(m.client.redis.mock.calls[0]![0]).toEqual({ address: '127.0.0.1:6380', db: 0, tls: false, password: redisPw });
    expect(m.wizard.state.done.redis).toBe(true);
    expect((m.wrapper.find('[data-testid="f-redis_password"]').element as HTMLInputElement).value).toBe('');
  });
  it('预检失败：错误码 setup.precheck_failed 与详细原因', async () => {
    const m = await mountStep(RedisStep, {
      client: fakeClient({
        redis: async () => {
          throw apiError(400, 'setup.precheck_failed', 'rid-p', [{ reason: 'Redis 7.4 < 8' }]);
        },
      }),
    });
    await click(m.wrapper, 'redis-submit');
    expect(tid(m.wrapper, 'error-code').text()).toBe('setup.precheck_failed');
    expect(tid(m.wrapper, 'error-reason').text()).toContain('Redis 7.4');
  });
});

describe('数据库初始化步骤', () => {
  it('成功：逐个迁移显示版本/名称/分类（可逆·前向兼容·不可逆）', async () => {
    const m = await mountStep(InitStep, {
      client: fakeClient({
        initialize: async () =>
          ok('init_db', {
            migrations: [
              { version: 1, name: 'identity', category: 'expand', duration_ms: 12 },
              { version: 2, name: 'drop_legacy', category: 'irreversible', duration_ms: 3 },
              { version: 3, name: 'indexes', category: 'reversible', duration_ms: 5 },
            ],
          }),
      }),
    });
    await click(m.wrapper, 'init-run');
    const rows = m.wrapper.findAll('[data-testid="init-migrations"] tbody tr');
    expect(rows).toHaveLength(3);
    expect(rows[0]!.text()).toContain('000001');
    expect(rows.map((r) => r.attributes('data-category'))).toEqual(['expand', 'irreversible', 'reversible']);
    expect(rows[1]!.text()).toContain(zh.init.class.irreversible);
    expect(m.wizard.state.done.init_db).toBe(true);
  });
  it('空态：已是最新版本（migrations 为空数组）', async () => {
    const m = await mountStep(InitStep);
    await click(m.wrapper, 'init-run');
    expect(tid(m.wrapper, 'init-uptodate').exists()).toBe(true);
    expect(m.wizard.state.done.init_db).toBe(true);
  });
  it('错误：重启后迁移口令不在内存（409）→ 错误码 + 返回 MySQL 步骤的入口', async () => {
    const m = await mountStep(InitStep, {
      client: fakeClient({
        initialize: async () => {
          throw apiError(409, 'resource.conflict', 'rid-409', [{ field: 'migrate_password', reason: 'resubmit PUT /public/setup/database' }]);
        },
      }),
      done: ['environment', 'database', 'redis'],
    });
    await click(m.wrapper, 'init-run');
    expect(tid(m.wrapper, 'error-code').text()).toBe('resource.conflict');
    await click(m.wrapper, 'init-goto-db');
    expect(m.wizard.state.step).toBe('database');
  });
  it('POST 连接中断但服务端已完成：经状态轮询确认后视为完成', async () => {
    const client = fakeClient({
      initialize: async () => {
        throw apiError(0, 'client.network_error', '');
      },
      status: async () => ({ installed: false, server_version: 'v', steps: [{ key: 'init_db', status: 'done' }] }),
    });
    const m = await mountStep(InitStep, { client });
    const p = click(m.wrapper, 'init-run');
    await new Promise((r) => setTimeout(r, 2200));
    await p;
    await flushPromises();
    expect(m.wizard.state.done.init_db).toBe(true);
    expect(m.wrapper.text()).toContain(zh.init.recovered);
  }, 10000);
});

describe('管理员步骤', () => {
  async function fill(m: Awaited<ReturnType<typeof mountStep>>, pw = ADMIN_PW, confirm = pw) {
    await setValue(m.wrapper, 'email', 'ops@example.test');
    await setValue(m.wrapper, 'display_name', 'Ops');
    await setValue(m.wrapper, 'new_password', pw);
    await setValue(m.wrapper, 'confirm_password', confirm);
  }
  it('口令强度提示随输入变化；逐条规则以文字+标记呈现；口令框不回显', async () => {
    const m = await mountStep(AdminStep);
    expect(tid(m.wrapper, 'strength-text').attributes('data-score')).toBe('0');
    await setValue(m.wrapper, 'new_password', 'short');
    expect(tid(m.wrapper, 'strength-text').attributes('data-score')).toBe('1');
    expect(m.wrapper.find('[data-rule="length"]').attributes('data-met')).toBe('false');
    await setValue(m.wrapper, 'new_password', ADMIN_PW);
    expect(Number(tid(m.wrapper, 'strength-text').attributes('data-score'))).toBeGreaterThanOrEqual(3);
    expect(m.wrapper.find('[data-testid="f-new_password"]').attributes('type')).toBe('password');
    expect(m.wrapper.find('[data-testid="f-confirm_password"]').attributes('type')).toBe('password');
    expect(document.body.textContent).not.toContain(ADMIN_PW); // 口令不会以文本形式出现在页面上
  });
  it('二次确认不一致 / 弱口令：本地拦截，不发请求', async () => {
    const m = await mountStep(AdminStep);
    await fill(m, ADMIN_PW, ADMIN_PW + 'x');
    await click(m.wrapper, 'admin-submit');
    expect(m.client.admin).not.toHaveBeenCalled();
    expect(m.wrapper.text()).toContain(zh.admin.err.mismatch);
    await fill(m, 'weakpass', 'weakpass');
    await click(m.wrapper, 'admin-submit');
    expect(m.wrapper.text()).toContain(zh.admin.err.weak);
    expect(m.client.admin).not.toHaveBeenCalled();
  });
  it('不启用 TOTP：一次提交完成，口令被清空', async () => {
    const m = await mountStep(AdminStep);
    await fill(m);
    await click(m.wrapper, 'admin-submit');
    expect(m.client.admin.mock.calls[0]![0]).toEqual({ email: 'ops@example.test', display_name: 'Ops', password: ADMIN_PW, locale: 'zh-CN' });
    expect(tid(m.wrapper, 'admin-ok').exists()).toBe(true);
    expect(m.wizard.state.done.admin).toBe(true);
  });
  it('TOTP 两阶段：先取密钥与二维码，再带验证码确认并取得恢复码；须勾选“已保存”才能继续', async () => {
    const client = fakeClient({
      admin: async (b: { totp_code?: string }) =>
        b.totp_code
          ? ok('admin', { one_time_secrets: { recovery_codes: ['AAAA-1111', 'BBBB-2222'] } })
          : ok('admin', { one_time_secrets: { totp_secret: totpKey, totp_otpauth_uri: `otpauth://totp/LingEdge:ops?${'sec' + 'ret'}=${totpKey}` } }),
    });
    const m = await mountStep(AdminStep, { client });
    await fill(m);
    await m.wrapper.find('[data-testid="c-enable_totp"]').setValue(true);
    await click(m.wrapper, 'admin-submit');
    expect(tid(m.wrapper, 'totp-secret').text()).toBe(totpKey);
    const qr = tid(m.wrapper, 'totp-qr');
    expect(qr.exists()).toBe(true);
    expect(qr.findAll('rect.qr__cell').length).toBeGreaterThan(100);
    expect(qr.attributes('aria-label')).toBe(zh.admin.totp.qrLabel);
    // 错误格式的验证码不发请求
    await setValue(m.wrapper, 'totp_code', '12ab');
    await click(m.wrapper, 'totp-confirm');
    expect(m.wrapper.text()).toContain(zh.admin.totp.codeFormat);
    expect(m.client.admin).toHaveBeenCalledTimes(1);
    await setValue(m.wrapper, 'totp_code', '123456');
    await click(m.wrapper, 'totp-confirm');
    expect(m.client.admin.mock.calls[1]![0]).toMatchObject({ enable_totp: true, totp_code: '123456', password: ADMIN_PW });
    expect(tid(m.wrapper, 'recovery-codes').text()).toContain('AAAA-1111');
    expect(m.wrapper.find('[data-testid="recovery-continue"]').attributes('disabled')).toBeDefined();
    await m.wrapper.find('[data-testid="c-recovery_ack"]').setValue(true);
    expect(m.wrapper.find('[data-testid="recovery-continue"]').attributes('disabled')).toBeUndefined();
    await click(m.wrapper, 'recovery-continue');
    expect(tid(m.wrapper, 'admin-ok').exists()).toBe(true);
    expect(document.body.textContent).not.toContain('AAAA-1111'); // 恢复码离开页面
    expect(m.wizard.state.done.admin).toBe(true);
  });
  it('服务端口令策略拒绝（auth.password_policy）：显示错误码、Request ID', async () => {
    const m = await mountStep(AdminStep, {
      client: fakeClient({
        admin: async () => {
          throw apiError(400, 'auth.password_policy', 'rid-pp', [{ field: 'password', reason: 'must not contain a common weak password pattern' }]);
        },
      }),
    });
    await fill(m);
    await click(m.wrapper, 'admin-submit');
    expect(tid(m.wrapper, 'error-code').text()).toBe('auth.password_policy');
    expect(m.wrapper.find('[data-le-request-id]').text()).toBe('rid-pp');
  });
});

describe('系统配置、密钥步骤', () => {
  it('访问地址必须是 http(s) 完整地址；成功后记住访问地址（供完成页）', async () => {
    const m = await mountStep(SystemStep);
    await setValue(m.wrapper, 'base_url', 'admin.example.test');
    await click(m.wrapper, 'system-submit');
    expect(m.client.system).not.toHaveBeenCalled();
    expect(m.wrapper.text()).toContain(zh.system.err.baseUrl);
    await setValue(m.wrapper, 'base_url', 'https://admin.example.test/');
    await setValue(m.wrapper, 'brand_name', '测试云');
    await setValue(m.wrapper, 'timezone', 'Asia/Shanghai');
    await click(m.wrapper, 'system-submit');
    expect(m.client.system.mock.calls[0]![0]).toEqual({ run_mode: 'self_hosted', base_url: 'https://admin.example.test', default_locale: 'zh-CN', timezone: 'Asia/Shanghai', brand: { name: '测试云' } });
    expect(m.wizard.state.baseUrl).toBe('https://admin.example.test');
    expect(m.wizard.state.done.system).toBe(true);
  });
  it('无效时区被拦截', async () => {
    const m = await mountStep(SystemStep);
    await setValue(m.wrapper, 'base_url', 'https://a.example.test');
    await setValue(m.wrapper, 'timezone', 'Mars/Olympus');
    await click(m.wrapper, 'system-submit');
    expect(m.wrapper.text()).toContain(zh.system.err.timezone);
    expect(m.client.system).not.toHaveBeenCalled();
  });
  it('密钥步骤：始终显示 master.key 分开保管的强提示；成功标记完成', async () => {
    const m = await mountStep(KeysStep);
    expect(tid(m.wrapper, 'master-key-notice').text()).toContain('master.key');
    expect(tid(m.wrapper, 'master-key-notice').text()).toContain(zh.masterKey.title);
    await click(m.wrapper, 'keys-run');
    expect(m.wizard.state.done.keys).toBe(true);
  });
});

describe('完成页', () => {
  it('强提示 master.key 须与数据库备份分开保管；勾选确认前不能完成', async () => {
    const m = await mountStep(CompleteStep);
    expect(tid(m.wrapper, 'master-key-notice').text()).toContain(zh.masterKey.title);
    expect(m.wrapper.find('[data-testid="complete-submit"]').attributes('disabled')).toBeDefined();
    await m.wrapper.find('[data-testid="c-master_key_ack"]').setValue(true);
    expect(m.wrapper.find('[data-testid="complete-submit"]').attributes('disabled')).toBeUndefined();
  });
  it('完成：显示 Admin URL 与一次性迁移口令；token 被清除；之后不再有任何请求入口', async () => {
    const client = fakeClient({ complete: async () => ok('complete', { one_time_secrets: { migrate_password: genMigPw } }) });
    const m = await mountStep(CompleteStep, { client });
    m.wizard.setBaseUrl('https://admin.example.test');
    await m.wrapper.find('[data-testid="c-master_key_ack"]').setValue(true);
    await click(m.wrapper, 'complete-submit');
    expect(client.complete).toHaveBeenCalledTimes(1);
    expect(client.token).toBe('');
    expect(tid(m.wrapper, 'admin-url').attributes('href')).toBe('https://admin.example.test');
    expect(tid(m.wrapper, 'migrate-password').text()).toBe(genMigPw);
    expect(tid(m.wrapper, 'migrate-command').text()).toBe('sudo lingedge-server migrate credential init');
    expect(tid(m.wrapper, 'master-key-notice').exists()).toBe(true); // 完成后依然可见
    expect(tid(m.wrapper, 'complete-submit').exists()).toBe(false);
  });
  it('刷新后内存里没有访问地址：让安装者再确认一次后才生成链接', async () => {
    const m = await mountStep(CompleteStep, { client: fakeClient({ complete: async () => ok('complete') }) });
    await m.wrapper.find('[data-testid="c-master_key_ack"]').setValue(true);
    await click(m.wrapper, 'complete-submit');
    expect(tid(m.wrapper, 'admin-url').exists()).toBe(false);
    await setValue(m.wrapper, 'admin_url_manual', 'https://admin.example.test');
    expect(tid(m.wrapper, 'admin-url').attributes('href')).toBe('https://admin.example.test');
    expect(tid(m.wrapper, 'migrate-secret').exists()).toBe(false); // 没有自动生成的迁移口令时不展示
  });
  it('英文界面：同一提示有英文文案', async () => {
    const m = await mountStep(CompleteStep, { locale: 'en' });
    expect(tid(m.wrapper, 'master-key-notice').text()).toContain(en.masterKey.title);
  });
});
