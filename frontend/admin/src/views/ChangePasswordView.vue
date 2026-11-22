<script setup lang="ts">
import { computed, reactive, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRouter } from 'vue-router';
import { isApiError } from '../api/errors';
import { errorText } from '../api/messages';
import { useFormRules } from '../composables/useFormRules';
import { ROUTE } from '../router/routes';
import { useSessionStore } from '../stores/session';
import { useTabsStore } from '../stores/tabs';

// 服务端要求先改口令的账号（must_change_password）只能到这里：除 /auth/me、登出、改口令外的接口一律拒绝（authenticator.mustChangeAllowed）。
const { t, te } = useI18n();
const router = useRouter();
const session = useSessionStore();
const tabs = useTabsStore();
const rules = useFormRules();

const form = reactive({ current: '', next: '', confirm: '' });
const busy = ref(false);
const error = ref<unknown>(null);
const formRules = computed(() => ({
  current: [rules.required(t('changePw.current'))],
  next: [rules.required(t('changePw.next')), rules.minLength(t('changePw.next'), 12), rules.maxLength(t('changePw.next'), 128)],
  confirm: [rules.required(t('changePw.confirm')), rules.sameAs(t('changePw.confirm'), () => form.next)],
}));
const errorMsg = computed(() => (error.value ? errorText(t, te, error.value) : ''));
const requestId = computed(() => (isApiError(error.value) ? error.value.requestId : ''));

async function submit(): Promise<void> {
  if (busy.value) return;
  busy.value = true;
  error.value = null;
  try {
    await session.changePassword(form.current, form.next); // 成功后服务端吊销了该用户的全部会话（含当前），store 已清空本地状态
    tabs.closeAll();
    form.current = form.next = form.confirm = '';
    await router.replace({ name: ROUTE.login });
  } catch (e) {
    error.value = e;
  } finally {
    busy.value = false;
  }
}
async function logout(): Promise<void> {
  await session.logout();
  tabs.closeAll();
  await router.replace({ name: ROUTE.login });
}
</script>

<template>
  <main class="le-cp" data-testid="change-password-page">
    <section class="le-cp__card" aria-labelledby="cp-title">
      <h1 id="cp-title" class="le-cp__title">{{ t('changePw.title') }}</h1>
      <p class="le-cp__hint">{{ t('changePw.hint') }}</p>
      <a-form :model="form" :rules="formRules" layout="vertical" auto-label-width @submit-success="submit">
        <a-form-item field="current" :label="t('changePw.current')" validate-trigger="blur">
          <a-input-password v-model="form.current" :input-attrs="{ autocomplete: 'current-password', 'data-testid': 'cp-current' }" />
        </a-form-item>
        <a-form-item field="next" :label="t('changePw.next')" validate-trigger="blur">
          <a-input-password v-model="form.next" :input-attrs="{ autocomplete: 'new-password', 'data-testid': 'cp-next' }" />
        </a-form-item>
        <a-form-item field="confirm" :label="t('changePw.confirm')" validate-trigger="blur">
          <a-input-password v-model="form.confirm" :input-attrs="{ autocomplete: 'new-password', 'data-testid': 'cp-confirm' }" />
        </a-form-item>
        <div v-if="error" class="le-cp__error" role="alert" data-testid="cp-error">
          <span>{{ errorMsg }}</span>
          <span v-if="requestId">{{ t('errors.requestId') }}: <code>{{ requestId }}</code></span>
        </div>
        <a-button type="primary" html-type="submit" long :loading="busy" data-testid="cp-submit">{{ t('changePw.submit') }}</a-button>
        <button type="button" class="le-cp__logout" @click="logout">{{ t('changePw.logout') }}</button>
      </a-form>
    </section>
  </main>
</template>

<style scoped>
.le-cp {
  min-height: 100vh;
  display: grid;
  place-items: center;
  padding: var(--le-space-6) var(--le-space-4);
  background: var(--le-color-bg-page);
}
.le-cp__card {
  width: min(440px, 100%);
  padding: var(--le-space-8) var(--le-space-6);
  background: var(--le-color-bg-surface);
  border: 1px solid var(--le-color-border-default);
  border-radius: var(--le-radius-lg);
}
.le-cp__title {
  margin: 0 0 var(--le-space-2);
  font-size: var(--le-text-heading-3-size);
  color: var(--le-color-text-primary);
}
.le-cp__hint {
  margin: 0 0 var(--le-space-4);
  color: var(--le-color-text-secondary);
}
.le-cp__error {
  display: grid;
  gap: var(--le-space-1);
  margin-bottom: var(--le-space-4);
  padding: var(--le-space-3);
  border-radius: var(--le-radius-md);
  background: var(--le-color-danger-bg);
  color: var(--le-color-danger);
}
.le-cp__logout {
  margin-top: var(--le-space-3);
  min-height: 44px;
  border: 0;
  background: none;
  color: var(--le-color-brand-primary);
  font: inherit;
  text-decoration: underline;
  cursor: pointer;
}
</style>
