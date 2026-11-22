<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { isApiError } from '../api/errors';
import { errorText } from '../api/messages';
import { useReauthStore } from '../stores/reauth';
import { useSessionStore } from '../stores/session';

// L4 再认证验证框（IMP-12）：由 API 客户端在服务端返回 403 auth.reauth_required 时触发；验证通过后原请求自动重试。
// 口令（已启用 TOTP 另加 6 位验证码）经 POST /auth/reauth 由服务端校验；本组件不做任何判定。
const { t, te } = useI18n();
const store = useReauthStore();
const session = useSessionStore();
const totpEnabled = computed(() => session.me?.user.totp_enabled === true);
const form = reactive({ pw: '', code: '' });
const localError = ref('');
const errorMsg = computed(() => (store.error ? errorText(t, te, store.error) : ''));
const requestId = computed(() => (isApiError(store.error) ? store.error.requestId : ''));
const open = computed({
  get: () => store.open,
  set: (v: boolean) => {
    if (!v && store.open) store.cancel();
  },
});
watch(
  () => store.open,
  (v) => {
    if (v) {
      form.pw = '';
      form.code = '';
      localError.value = '';
    }
  },
);

async function submit(): Promise<void> {
  localError.value = '';
  if (!form.pw) {
    localError.value = t('form.rule.required', { field: t('reauth.pw') });
    return;
  }
  const code = form.code.trim();
  if (totpEnabled.value && !/^[0-9]{6}$/.test(code)) {
    localError.value = t('reauth.codeInvalid');
    return;
  }
  await store.submit(form.pw, totpEnabled.value ? code : undefined);
  if (!store.open) {
    form.pw = '';
    form.code = '';
  }
}
</script>

<template>
  <a-modal v-model:visible="open" unmount-on-close :footer="false" :mask-closable="false" :title="t('reauth.title')" modal-class="le-reauth">
    <form class="le-dialog" data-testid="reauth-dialog" @submit.prevent="submit">
      <p>{{ t('reauth.description') }}</p>
      <div class="le-dialog__typed">
        <label class="le-dialog__label" for="le-reauth-secret">{{ t('reauth.pw') }}</label>
        <a-input-password v-model="form.pw" :input-attrs="{ id: 'le-reauth-secret', 'data-testid': 'reauth-secret', autocomplete: 'current-password' }" />
      </div>
      <div v-if="totpEnabled" class="le-dialog__typed">
        <label class="le-dialog__label" for="le-reauth-code">{{ t('reauth.code') }}</label>
        <a-input v-model="form.code" :max-length="6" :input-attrs="{ id: 'le-reauth-code', 'data-testid': 'reauth-code', inputmode: 'numeric', autocomplete: 'one-time-code' }" />
      </div>
      <div v-if="localError || store.error" class="le-dialog__error" role="alert" data-testid="reauth-error">
        <span>{{ localError || errorMsg }}</span>
        <span v-if="!localError && requestId">{{ t('errors.requestId') }}: <code>{{ requestId }}</code></span>
      </div>
      <div class="le-dialog__actions">
        <a-button data-testid="reauth-cancel" @click="store.cancel()">{{ t('common.cancel') }}</a-button>
        <a-button type="primary" html-type="submit" :loading="store.busy" data-testid="reauth-submit">{{ t('reauth.submit') }}</a-button>
      </div>
      <p class="le-dialog__audit" data-testid="reauth-audit">{{ t('danger.audit') }}</p>
    </form>
  </a-modal>
</template>
