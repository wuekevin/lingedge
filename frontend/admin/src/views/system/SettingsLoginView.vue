<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useAdminApi } from '../../api/context';
import { useCan } from '../../composables/useCan';
import SettingsForm from './SettingsForm.vue';
import type { FieldDef } from './settingsFields';

// 登录设置（ia.md §1.17）：TOTP 策略、登录节流、会话时长与再认证窗口。这些键都是安全敏感键：服务端要求会话近期再认证（x-reauth 语义在服务内按键判定），
// 窗口外保存会弹出再认证框并自动重试。范围与后端 sysconfig 注册表一致；跨键约束（滑动过期 ≤ 绝对过期）前后端都校验。
const { t } = useI18n();
const api = useAdminApi();
const can = useCan();
const canWrite = computed(() => can('setting:write'));

const FIELDS: readonly FieldDef[] = [
  { key: 'login.totp_required_for_admin', kind: 'bool' },
  { key: 'login.throttle_window_s', kind: 'int', min: 1, max: 86400 },
  { key: 'login.throttle_account_max', kind: 'int', min: 1, max: 1_000_000 },
  { key: 'login.throttle_ip_max', kind: 'int', min: 1, max: 1_000_000 },
  { key: 'auth.session_idle_ttl_s', kind: 'int', min: 60, max: 30 * 86400 },
  { key: 'auth.session_absolute_ttl_s', kind: 'int', min: 300, max: 90 * 86400 },
  { key: 'auth.reauth_window_s', kind: 'int', min: 10, max: 86400 },
  { key: 'auth.mfa_window_s', kind: 'int', min: 30, max: 86400 },
];
const cross = (v: Record<string, unknown>): Record<string, string> => {
  const idle = v['auth.session_idle_ttl_s'];
  const abs = v['auth.session_absolute_ttl_s'];
  return typeof idle === 'number' && typeof abs === 'number' && idle > abs ? { 'auth.session_idle_ttl_s': 'settings.login.crossSession' } : {};
};
</script>

<template>
  <section class="le-page" data-testid="settings-login-page">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.settingsLogin') }}</h1>
    </header>
    <p class="le-page__desc">{{ t('settings.login.desc') }}</p>
    <SettingsForm
      testid="settings-login-form"
      i18n-prefix="settings.login"
      :fields="FIELDS"
      :can-write="canWrite"
      :cross="cross"
      :load="(signal) => api.getSettings(signal)"
      :save="(changed) => api.patchSettings(changed)"
    />
  </section>
</template>
