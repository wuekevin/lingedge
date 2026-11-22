<script setup lang="ts">
import { computed, ref } from 'vue';
import { LeStateBoundary } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import { useServerText } from '../api/useServerText';
import CopyButton from '../components/CopyButton.vue';
import MasterKeyNotice from '../components/MasterKeyNotice.vue';
import SCheckbox from '../components/SCheckbox.vue';
import SetupError from '../components/SetupError.vue';
import SInput from '../components/SInput.vue';
import StepShell from '../components/StepShell.vue';
import { STEP_ORDER } from '../api/types';
import { useStepRequest } from '../state/request';
import { useWizard } from '../state/wizard';
import { isValidBaseUrl, normalizeBaseUrl } from '../validation';

const wizard = useWizard();
const { t } = useI18n();
const req = useStepRequest(wizard);

const COMMAND = 'sudo lingedge-server migrate credential init';
const masterAck = ref(false);
const migratePassword = ref('');
const migrateAck = ref(false);
const manualUrl = ref('');
const finished = computed(() => wizard.state.finished);
const serverMessage = ref('');
const st = useServerText();
const serverNote = computed(() => st.step('complete', serverMessage.value));

/** Admin URL：基础配置里填写的访问地址；刷新后内存已丢失时让安装者再确认一次（只用于展示链接）。 */
const adminUrl = computed(() => {
  if (wizard.state.baseUrl) return wizard.state.baseUrl;
  return isValidBaseUrl(manualUrl.value) ? normalizeBaseUrl(manualUrl.value) : '';
});

async function complete() {
  const r = await req.run(() => wizard.client.complete());
  if (!r.ok) return;
  migratePassword.value = r.value.one_time_secrets?.migrate_password ?? '';
  serverMessage.value = r.value.message ?? '';
  wizard.finish();
}
</script>

<template>
  <StepShell testid="step-complete" :title="finished ? t('complete.doneTitle') : t('complete.title')" :description="finished ? t('complete.doneBody') : t('complete.description')" :step="8" :total="STEP_ORDER.length">
    <MasterKeyNotice>
      <SCheckbox v-if="!finished" v-model="masterAck" name="master_key_ack" :label="t('masterKey.ack')" />
    </MasterKeyNotice>

    <template v-if="!finished">
      <div class="actions">
        <button type="button" class="btn btn--primary" data-testid="complete-submit" :disabled="!masterAck || req.status.value === 'loading'" @click="complete">{{ t('complete.submit') }}</button>
      </div>
      <LeStateBoundary :status="req.status.value" @retry="complete">
        <template #error>
          <SetupError v-if="req.error.value" :error="req.error.value" @retry="complete" />
        </template>
        <template #empty>
          <span />
        </template>
        <span />
      </LeStateBoundary>
    </template>

    <template v-else>
      <section class="panel" data-testid="complete-done" aria-labelledby="admin-url-title">
        <h3 id="admin-url-title" class="step__subtitle">{{ t('complete.adminUrl') }}</h3>
        <template v-if="adminUrl">
          <p>
            <a class="link" :href="adminUrl" rel="noopener noreferrer" data-testid="admin-url">{{ adminUrl }}</a>
          </p>
          <p>
            <a class="btn btn--primary" :href="adminUrl" rel="noopener noreferrer" data-testid="admin-url-open">{{ t('complete.adminUrlOpen') }}</a>
          </p>
        </template>
        <template v-else>
          <p>{{ t('complete.adminUrlMissing') }}</p>
          <SInput v-model="manualUrl" name="admin_url_manual" type="url" inputmode="url" :label="t('complete.adminUrl')" hide-marker />
        </template>
        <p>{{ t('complete.adminLogin') }}</p>
        <p v-if="serverNote" class="result__server"><span class="result__server-label">{{ t('app.serverNote') }}:</span> <span :lang="serverNote?.lang" :data-localized="serverNote?.localized" data-testid="server-message">{{ serverNote?.text }}</span></p>
      </section>

      <section v-if="migratePassword" class="notice notice--warn" data-testid="migrate-secret" aria-labelledby="migrate-title">
        <h3 id="migrate-title" class="notice__title">{{ t('complete.migrate.title') }}</h3>
        <p>{{ t('complete.migrate.body') }}</p>
        <p>
          <code class="secret" data-testid="migrate-password">{{ migratePassword }}</code>
          <CopyButton :value="migratePassword" testid="migrate-copy" />
        </p>
        <p class="field__hint">{{ t('complete.migrate.command') }}:</p>
        <p>
          <code class="secret" data-testid="migrate-command">{{ COMMAND }}</code>
          <CopyButton :value="COMMAND" />
        </p>
        <SCheckbox v-model="migrateAck" name="migrate_ack" :label="t('complete.migrate.ack')" />
      </section>

      <section class="panel" aria-labelledby="next-title">
        <h3 id="next-title" class="step__subtitle">{{ t('complete.next.title') }}</h3>
        <ul class="plain-list">
          <li>{{ t('complete.next.tempAdmin') }}</li>
          <li>{{ t('complete.next.credential') }}</li>
          <li>{{ t('complete.next.backup') }}</li>
          <li>{{ t('complete.next.node') }}</li>
        </ul>
      </section>
    </template>
  </StepShell>
</template>
