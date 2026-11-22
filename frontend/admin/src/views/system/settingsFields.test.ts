import { describe, expect, it } from 'vitest';
import en from '../../locales/en.json';
import zhCN from '../../locales/zh-CN.json';
import { changedFields, fieldSeg, fromDraft, toDraft, validateField, type FieldDef } from './settingsFields';

// 设置键 → 词条键的映射（SettingsForm 用动态键 settings.<页>.field.<键>，i18n 门禁只能校验前缀，这里逐个对账两种语言）。
const PAGES: Record<string, string[]> = {
  basic: ['general.base_url', 'general.admin_url', 'general.console_url', 'general.docs_url', 'general.status_url', 'general.timezone', 'general.locales', 'general.registration_open'],
  brand: ['name', 'name_en', 'primary_color', 'support_email'],
  login: ['login.totp_required_for_admin', 'login.throttle_window_s', 'login.throttle_account_max', 'login.throttle_ip_max', 'auth.session_idle_ttl_s', 'auth.session_absolute_ttl_s', 'auth.reauth_window_s', 'auth.mfa_window_s'],
};
const field = (f: Partial<FieldDef> & Pick<FieldDef, 'key' | 'kind'>): FieldDef => f;

describe('设置字段', () => {
  it.each(Object.entries(PAGES))('%s：每个字段在 zh-CN 与 en 都有标签词条', (page, keys) => {
    for (const tree of [zhCN, en] as Record<string, unknown>[]) {
      const fields = ((tree.settings as Record<string, Record<string, Record<string, string>>>)[page] as Record<string, Record<string, string>>).field as Record<string, string>;
      for (const k of keys) expect(fields[fieldSeg(k)], `${page}:${k}`).toBeTruthy();
    }
  });

  it('整数：范围、整数、必填', () => {
    const f = field({ key: 'x', kind: 'int', min: 1, max: 10 });
    expect(validateField(f, '5')).toBeNull();
    expect(validateField(f, '0')?.key).toBe('form.rule.range');
    expect(validateField(f, '11')?.key).toBe('form.rule.range');
    expect(validateField(f, '1.5')?.key).toBe('form.rule.integer');
    expect(validateField(f, '1e3')?.key).toBe('form.rule.integer');
    expect(validateField(f, ' ')?.key).toBe('form.rule.required');
    expect(fromDraft(f, ' 7 ')).toBe(7);
    expect(fromDraft(f, 'x')).toBeUndefined();
  });
  it('网址只接受 http/https；颜色 #RRGGBB；邮箱；时区；长度', () => {
    expect(validateField(field({ key: 'u', kind: 'url' }), 'https://a.example.test/x')).toBeNull();
    expect(validateField(field({ key: 'u', kind: 'url' }), 'javascript:alert(1)')?.key).toBe('form.rule.url');
    expect(validateField(field({ key: 'u', kind: 'url' }), 'a.example.test')?.key).toBe('form.rule.url');
    expect(validateField(field({ key: 'c', kind: 'color' }), ['#', 'a1B2c3'].join(''))).toBeNull();
    expect(validateField(field({ key: 'c', kind: 'color' }), 'blue')?.key).toBe('form.rule.color');
    expect(validateField(field({ key: 'e', kind: 'email' }), 'a@b.test')).toBeNull();
    expect(validateField(field({ key: 'e', kind: 'email' }), 'a@b')?.key).toBe('form.rule.email');
    expect(validateField(field({ key: 't', kind: 'timezone' }), 'Asia/Shanghai')).toBeNull();
    expect(validateField(field({ key: 't', kind: 'timezone' }), 'Mars/Olympus')?.key).toBe('form.rule.timezone');
    expect(validateField(field({ key: 'n', kind: 'text', required: true, maxLen: 3 }), 'abcd')?.key).toBe('form.rule.maxLength');
    expect(validateField(field({ key: 'n', kind: 'text', required: true }), '  ')?.key).toBe('form.rule.required');
    expect(validateField(field({ key: 'l', kind: 'locales' }), [])?.key).toBe('form.rule.locales');
  });
  it('可空网址：空 = null（恢复默认）；提交值与原值比较，null 与空串等价，只列出真正变化的字段', () => {
    const url = field({ key: 'general.docs_url', kind: 'url', nullable: true });
    const name = field({ key: 'name', kind: 'text', required: true });
    expect(fromDraft(url, '')).toBeNull();
    expect(toDraft(url, null)).toBe('');
    const original = { 'general.docs_url': toDraft(url, null), name: toDraft(name, 'A') };
    expect(changedFields([url, name], { 'general.docs_url': '  ', name: 'A' }, original)).toEqual([]);
    expect(changedFields([url, name], { 'general.docs_url': 'https://x.example.test', name: 'B' }, original).map((f) => f.key)).toEqual(['general.docs_url', 'name']);
  });
  it('语言列表按固定顺序提交', () => {
    const f = field({ key: 'general.locales', kind: 'locales' });
    expect(fromDraft(f, ['en', 'zh-CN'])).toEqual(['zh-CN', 'en']);
  });
});
