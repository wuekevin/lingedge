<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';
import { errorText } from '../api/messages';
import { isApiError } from '../api/errors';
import { readLocalePref } from '../i18n';
import { redirectTarget } from '../router/guard';
import { ROUTE } from '../router/routes';
import { useFormRules } from '../composables/useFormRules';
import { useLocale } from '../composables/useLocale';
import { useSessionStore, type LoginOutcome } from '../stores/session';
import { useTabsStore } from '../stores/tabs';
import { localStore } from '../theme/theme';
import { resolveLocale } from '@lingedge/shared';
import ShellPrefs from '../layout/ShellPrefs.vue';

const { t, te } = useI18n();
const route = useRoute();
const router = useRouter();
const session = useSessionStore();
const tabs = useTabsStore();
const { setLocale } = useLocale();
const rules = useFormRules();

type Step = 'credentials' | 'totp';
const step = computed<Step>(() => (session.mfa ? 'totp' : 'credentials'));
const form = reactive({ email: '', pw: '' });
const mfaForm = reactive({ code: '' });
const useRecovery = ref(false);
const busy = ref(false);
const error = ref<unknown>(null);
const mfaExpired = ref(false);

const formRules = computed(() => ({
  email: [rules.required(t('login.email')), rules.email(t('login.email'))],
  pw: [rules.required(t('login.pw'))],
}));
const mfaRules = computed(() => ({ code: [rules.required(useRecovery.value ? t('login.totp.recoveryCode') : t('login.totp.code'))] }));

const errorMsg = computed(() => (error.value ? errorText(t, te, error.value) : ''));
const requestId = computed(() => (isApiError(error.value) ? error.value.requestId : ''));

// 第二步挑战 5 分钟过期：到点提示并回到第一步（服务端同样会拒绝）。
let timer: ReturnType<typeof setTimeout> | undefined;
watch(
  () => session.mfa,
  (m) => {
    if (timer) clearTimeout(timer);
    mfaExpired.value = false;
    if (m)
      timer = setTimeout(
        () => {
          session.cancelMfa();
          mfaExpired.value = true;
          mfaForm.code = '';
        },
        Math.max(0, m.expiresAt - Date.now()),
      );
  },
  { immediate: true },
);
onBeforeUnmount(() => timer && clearTimeout(timer));

async function afterLogin(outcome: LoginOutcome): Promise<void> {
  tabs.closeAll(); // 新身份不继承任何旧标签（防跨身份残留视图）
  form.pw = '';
  mfaForm.code = '';
  if (outcome === 'must_change') {
    await router.replace({ name: ROUTE.changePw });
    return;
  }
  if (!readLocalePref(localStore()) && session.user) setLocale(resolveLocale(session.user.locale));
  await session.resolveActingTenant();
  await router.replace(redirectTarget(route) ?? { name: ROUTE.home });
}

async function submitCredentials(): Promise<void> {
  if (busy.value) return;
  busy.value = true;
  error.value = null;
  mfaExpired.value = false;
  try {
    const outcome = await session.login(form.email.trim(), form.pw);
    if (outcome === 'totp_required') {
      useRecovery.value = false;
      form.pw = '';
      return;
    }
    await afterLogin(outcome);
  } catch (e) {
    error.value = e;
  } finally {
    busy.value = false;
  }
}

async function submitCode(): Promise<void> {
  if (busy.value) return;
  busy.value = true;
  error.value = null;
  try {
    const outcome = await session.verifyTotp(mfaForm.code);
    await afterLogin(outcome);
  } catch (e) {
    error.value = e;
    // 挑战已用尽/失效（401 unauthenticated）：回到第一步重新登录
    if (isApiError(e) && e.status === 401 && e.code === 'auth.unauthenticated') session.cancelMfa();
  } finally {
    busy.value = false;
  }
}

function backToCredentials(): void {
  session.cancelMfa();
  mfaForm.code = '';
  error.value = null;
}
function toggleRecovery(): void {
  useRecovery.value = !useRecovery.value;
  mfaForm.code = '';
  error.value = null;
}
</script>

<template>
  <main class="le-login" data-testid="login-page">
    <div class="le-login__prefs"><ShellPrefs /></div>
    <section class="le-login__card" :aria-labelledby="step === 'credentials' ? 'login-title' : 'totp-title'">
      <p class="le-login__brand">{{ t('topbar.brand') }}</p>

      <template v-if="step === 'credentials'">
        <h1 id="login-title" class="le-login__title">{{ t('login.title') }}</h1>
        <p class="le-login__hint">{{ t('login.subtitle') }}</p>
        <a-alert v-if="session.expired" type="warning" class="le-login__alert" data-testid="login-expired">{{ t('login.expired') }}</a-alert>
        <a-alert v-if="mfaExpired" type="warning" class="le-login__alert" data-testid="mfa-expired">{{ t('login.totp.expired') }}</a-alert>
        <a-form :model="form" :rules="formRules" layout="vertical" auto-label-width @submit-success="submitCredentials">
          <a-form-item field="email" :label="t('login.email')" validate-trigger="blur">
            <a-input v-model="form.email" :placeholder="t('login.emailPlaceholder')" allow-clear :input-attrs="{ autocomplete: 'username', 'data-testid': 'login-email', inputmode: 'email' }" />
          </a-form-item>
          <a-form-item field="pw" :label="t('login.pw')" validate-trigger="blur">
            <a-input-password v-model="form.pw" :input-attrs="{ autocomplete: 'current-password', 'data-testid': 'login-pw' }" />
          </a-form-item>
          <div v-if="error" class="le-login__error" role="alert" data-testid="login-error">
            <strong>{{ t('login.error.title') }}</strong>
            <span data-testid="login-error-text">{{ errorMsg }}</span>
            <span v-if="requestId" class="le-login__rid">{{ t('errors.requestId') }}: <code>{{ requestId }}</code></span>
          </div>
          <a-button type="primary" html-type="submit" long :loading="busy" data-testid="login-submit">{{ busy ? t('login.submitting') : t('login.submit') }}</a-button>
        </a-form>
      </template>

      <template v-else>
        <h1 id="totp-title" class="le-login__title">{{ t('login.totp.title') }}</h1>
        <p class="le-login__hint" data-testid="totp-hint">{{ useRecovery ? t('login.totp.recoveryHint') : t('login.totp.hint') }}</p>
        <a-form :model="mfaForm" :rules="mfaRules" layout="vertical" auto-label-width @submit-success="submitCode">
          <a-form-item field="code" :label="useRecovery ? t('login.totp.recoveryCode') : t('login.totp.code')" validate-trigger="blur">
            <a-input
              v-model="mfaForm.code"
              allow-clear
              :max-length="32"
              :input-attrs="{ autocomplete: 'one-time-code', 'data-testid': 'totp-code', inputmode: useRecovery ? 'text' : 'numeric', spellcheck: 'false' }"
            />
          </a-form-item>
          <div v-if="error" class="le-login__error" role="alert" data-testid="login-error">
            <strong>{{ t('login.error.title') }}</strong>
            <span data-testid="login-error-text">{{ errorMsg }}</span>
            <span v-if="requestId" class="le-login__rid">{{ t('errors.requestId') }}: <code>{{ requestId }}</code></span>
          </div>
          <a-button type="primary" html-type="submit" long :loading="busy" data-testid="totp-submit">{{ busy ? t('login.totp.verifying') : t('login.totp.verify') }}</a-button>
          <div class="le-login__links">
            <button type="button" class="le-link" data-testid="totp-toggle" @click="toggleRecovery">{{ useRecovery ? t('login.totp.useTotp') : t('login.totp.useRecovery') }}</button>
            <button type="button" class="le-link" data-testid="totp-back" @click="backToCredentials">{{ t('login.totp.back') }}</button>
          </div>
        </a-form>
      </template>
    </section>
  </main>
</template>

<style scoped>
.le-login {
  min-height: 100vh;
  display: grid;
  place-items: center;
  padding: var(--le-space-6) var(--le-space-4);
  background: var(--le-color-bg-page);
  position: relative;
}
.le-login__prefs {
  position: absolute;
  top: var(--le-space-3);
  right: var(--le-space-4);
}
.le-login__card {
  width: min(420px, 100%);
  padding: var(--le-space-8) var(--le-space-6);
  background: var(--le-color-bg-surface);
  border: 1px solid var(--le-color-border-default);
  border-radius: var(--le-radius-lg);
  box-shadow: var(--le-shadow-overlay);
}
.le-login__brand {
  margin: 0 0 var(--le-space-2);
  font-size: var(--le-text-label-size);
  font-weight: 600;
  color: var(--le-color-brand-primary);
  letter-spacing: 0.04em;
}
.le-login__title {
  margin: 0 0 var(--le-space-2);
  font-size: var(--le-text-heading-3-size);
  font-weight: var(--le-text-heading-3-weight);
  color: var(--le-color-text-primary);
}
.le-login__hint {
  margin: 0 0 var(--le-space-4);
  color: var(--le-color-text-secondary);
}
.le-login__alert {
  margin-bottom: var(--le-space-4);
}
.le-login__error {
  display: grid;
  gap: var(--le-space-1);
  margin-bottom: var(--le-space-4);
  padding: var(--le-space-3);
  border-radius: var(--le-radius-md);
  background: var(--le-color-danger-bg);
  color: var(--le-color-danger);
}
.le-login__rid {
  font-size: var(--le-text-caption-size);
}
.le-login__links {
  display: flex;
  justify-content: space-between;
  gap: var(--le-space-3);
  margin-top: var(--le-space-4);
}
.le-link {
  padding: var(--le-space-2) 0;
  border: 0;
  background: none;
  color: var(--le-color-brand-primary);
  font: inherit;
  cursor: pointer;
  text-decoration: underline;
  min-height: 44px;
}
</style>
