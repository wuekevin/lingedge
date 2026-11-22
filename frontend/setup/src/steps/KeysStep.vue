<script setup lang="ts">
import { computed, ref } from 'vue';
import { LeStateBoundary } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import MasterKeyNotice from '../components/MasterKeyNotice.vue';
import ResultNote from '../components/ResultNote.vue';
import SetupError from '../components/SetupError.vue';
import StepShell from '../components/StepShell.vue';
import { STEP_ORDER } from '../api/types';
import { useStepRequest } from '../state/request';
import { useWizard } from '../state/wizard';

const wizard = useWizard();
const { t } = useI18n();
const req = useStepRequest(wizard);
const serverMessage = ref<string | undefined>(undefined);
const resumed = computed(() => wizard.state.done.keys && req.status.value === 'empty');

async function run() {
  const r = await req.run(() => wizard.client.keys());
  if (!r.ok) return;
  serverMessage.value = r.value.message;
  wizard.markDone('keys');
}
</script>

<template>
  <StepShell testid="step-keys" :title="t('keys.title')" :description="t('keys.description')" :step="7" :total="STEP_ORDER.length">
    <MasterKeyNotice />
    <div class="actions">
      <button type="button" class="btn btn--primary" data-testid="keys-run" :disabled="req.status.value === 'loading'" @click="run">{{ resumed ? t('keys.rerun') : t('keys.run') }}</button>
    </div>
    <LeStateBoundary :status="req.status.value" @retry="run">
      <template #error>
        <SetupError v-if="req.error.value" :error="req.error.value" @retry="run" />
      </template>
      <template #empty>
        <span />
      </template>
      <ResultNote :message="t('keys.ok')" :server-message="serverMessage" step="keys" testid="keys-ok" />
    </LeStateBoundary>
  </StepShell>
</template>
