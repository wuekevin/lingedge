<script setup lang="ts">
import { ref } from 'vue';
import { LeLoading } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import SetupError from '../components/SetupError.vue';
import SInput from '../components/SInput.vue';
import { toApiError } from '../state/request';
import { useWizard } from '../state/wizard';
import type { SetupApiError } from '../api/errors';

const props = defineProps<{ urlTokenIgnored: boolean }>();
const wizard = useWizard();
const { t } = useI18n();

const token = ref('');
const checking = ref(false);
const emptyError = ref(false);
const error = ref<SetupApiError | null>(null);

async function submit() {
  error.value = null;
  emptyError.value = token.value.trim() === '';
  if (emptyError.value) return;
  checking.value = true;
  const value = token.value.trim();
  try {
    await wizard.verifyToken(value);
  } catch (e) {
    error.value = toApiError(e);
  } finally {
    checking.value = false;
    // 无论成败都清空输入框：令牌只在 client 闭包的内存里（成功时），页面上不留明文。
    token.value = '';
  }
}
</script>

<template>
  <section class="step gate" data-testid="gate" aria-labelledby="gate-title">
    <h2 id="gate-title" class="step__title">{{ t('gate.title') }}</h2>
    <p class="step__desc">{{ t('gate.intro') }}</p>
    <p v-if="props.urlTokenIgnored" class="notice notice--warn" role="status" data-testid="url-token-ignored">{{ t('gate.urlTokenIgnored') }}</p>
    <form class="form" novalidate autocomplete="off" @submit.prevent="submit">
      <SInput v-model="token" name="setup_token" type="password" :label="t('gate.label')" :hint="t('gate.hint')" :error="emptyError ? t('gate.empty') : undefined" hide-marker required :disabled="checking" autocomplete="off" />
      <p class="field__hint">{{ t('gate.memoryOnly') }}</p>
      <div class="actions">
        <button type="submit" class="btn btn--primary" data-testid="gate-submit" :disabled="checking">{{ t('gate.submit') }}</button>
      </div>
    </form>
    <div v-if="checking" class="panel" aria-live="polite">
      <LeLoading :lines="1" />
      <p class="field__hint">{{ t('gate.checking') }}</p>
    </div>
    <SetupError v-else-if="error" :error="error" @retry="submit" />
  </section>
</template>
