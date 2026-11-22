<script setup lang="ts">
import { computed, reactive, ref } from 'vue';
import { LeStateBoundary } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import ResultNote from '../components/ResultNote.vue';
import SCheckbox from '../components/SCheckbox.vue';
import SetupError from '../components/SetupError.vue';
import SInput from '../components/SInput.vue';
import StepShell from '../components/StepShell.vue';
import type { SetupRedisConfig } from '../api/types';
import { STEP_ORDER } from '../api/types';
import { useStepRequest } from '../state/request';
import { useWizard } from '../state/wizard';
import { parseHostPort } from '../validation';

const wizard = useWizard();
const { t } = useI18n();
const req = useStepRequest(wizard);

const EXAMPLE_ADDR = '127.0.0.1:6379';
const form = reactive({ address: EXAMPLE_ADDR, password: '', db: '0', tls: false });
const errors = reactive<Record<string, string>>({});
const serverMessage = ref<string | undefined>(undefined);
const resumed = computed(() => wizard.state.done.redis && req.status.value === 'empty');

function validate(): boolean {
  for (const k of Object.keys(errors)) delete errors[k];
  if (!parseHostPort(form.address)) errors.address = t('redis.err.addressFormat');
  const d = Number(form.db);
  if (!Number.isInteger(d) || d < 0 || d > 15) errors.db = t('redis.err.dbRange');
  return Object.keys(errors).length === 0;
}

async function submit() {
  if (!validate()) {
    setTimeout(() => document.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus(), 0);
    return;
  }
  const body: SetupRedisConfig = { address: form.address.trim(), db: Number(form.db), tls: form.tls };
  if (form.password !== '') body.password = form.password;
  const r = await req.run(() => wizard.client.redis(body));
  if (!r.ok) {
    for (const d of req.error.value?.details ?? []) if (d.field && !errors[d.field]) errors[d.field] = d.reason;
    return;
  }
  serverMessage.value = r.value.message;
  form.password = '';
  wizard.markDone('redis');
}
</script>

<template>
  <StepShell testid="step-redis" :title="t('redis.title')" :description="t('redis.description')" :step="3" :total="STEP_ORDER.length">
    <p v-if="resumed" class="notice notice--info" role="status">{{ t('db.resumeHint') }}</p>
    <form class="form" novalidate autocomplete="off" @submit.prevent="submit">
      <fieldset class="form__group" :disabled="req.status.value === 'loading'">
        <SInput v-model="form.address" name="address" :label="t('redis.field.address')" :hint="t('redis.field.addressHint')" required :error="errors.address" />
        <SInput v-model="form.password" name="redis_password" type="password" autocomplete="new-password" :label="t('redis.field.pw')" :hint="t('redis.field.pwHint')" :error="errors.password" />
        <SInput v-model="form.db" name="db" type="number" inputmode="numeric" :label="t('redis.field.db')" :error="errors.db" />
        <SCheckbox v-model="form.tls" name="redis_tls" :label="t('redis.field.tls')" />
      </fieldset>
      <div class="actions">
        <button type="submit" class="btn btn--primary" data-testid="redis-submit" :disabled="req.status.value === 'loading'">{{ t('redis.submit') }}</button>
      </div>
    </form>
    <LeStateBoundary :status="req.status.value" @retry="submit">
      <template #error>
        <SetupError v-if="req.error.value" :error="req.error.value" @retry="submit" />
      </template>
      <template #empty>
        <p class="result result--empty" data-testid="redis-empty">{{ t('redis.resultEmpty') }}</p>
      </template>
      <ResultNote :message="t('redis.ok')" :server-message="serverMessage" testid="redis-ok" />
    </LeStateBoundary>
  </StepShell>
</template>
