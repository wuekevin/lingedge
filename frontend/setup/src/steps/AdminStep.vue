<script setup lang="ts">
import { computed, reactive, ref, useId } from 'vue';
import { LeStateBoundary } from '@lingedge/shared';
import { resolveLocale, type LeLocale } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import { useServerText } from '../api/useServerText';
import CopyButton from '../components/CopyButton.vue';
import QrCode from '../components/QrCode.vue';
import ResultNote from '../components/ResultNote.vue';
import SCheckbox from '../components/SCheckbox.vue';
import SetupError from '../components/SetupError.vue';
import SInput from '../components/SInput.vue';
import SRadioGroup from '../components/SRadioGroup.vue';
import StepShell from '../components/StepShell.vue';
import StrengthMeter from '../components/StrengthMeter.vue';
import type { SetupAdminCreate } from '../api/types';
import { STEP_ORDER } from '../api/types';
import { assessPassword } from '../password';
import { useStepRequest } from '../state/request';
import { useWizard } from '../state/wizard';
import { isValidEmail } from '../validation';

const wizard = useWizard();
const { t, locale } = useI18n();
const st = useServerText();
const req = useStepRequest(wizard);
const meterId = useId();

type Phase = 'form' | 'totp' | 'recovery' | 'done';
const phase = ref<Phase>('form');
const form = reactive({ email: '', displayName: '', password: '', confirm: '', locale: resolveLocale(locale.value) as LeLocale, enableTotp: false, totpCode: '' });
const errors = reactive<Record<string, string>>({});
const totp = reactive({ secret: '', uri: '' });
const recoveryCodes = ref<string[]>([]);
const recoveryAck = ref(false);
const serverMessage = ref<string | undefined>(undefined);

const assessment = computed(() => assessPassword(form.password, form.email));
const alreadyDone = computed(() => wizard.state.done.admin && phase.value === 'form' && req.status.value === 'empty');

function validateForm(): boolean {
  for (const k of Object.keys(errors)) delete errors[k];
  if (!isValidEmail(form.email)) errors.email = t('admin.err.email');
  if (form.displayName.trim() === '') errors.display_name = t('admin.err.name');
  if (!assessment.value.acceptable) errors.password = t('admin.err.weak');
  if (form.confirm !== form.password) errors.confirm = t('admin.err.mismatch');
  return Object.keys(errors).length === 0;
}

function body(withCode: boolean): SetupAdminCreate {
  const b: SetupAdminCreate = { email: form.email.trim(), display_name: form.displayName.trim(), password: form.password, locale: form.locale };
  if (form.enableTotp) {
    b.enable_totp = true;
    if (withCode) b.totp_code = form.totpCode.trim();
  }
  return b;
}

/** 口令与验证码用完即清（含确认输入）；服务端永不回显。 */
function wipePasswords() {
  form.password = '';
  form.confirm = '';
}

function mapFieldErrors() {
  for (const d of req.error.value?.details ?? []) if (d.field && !errors[d.field]) errors[d.field] = st.reason(d.reason).text;
}

async function submit() {
  if (!validateForm()) {
    setTimeout(() => document.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus(), 0);
    return;
  }
  const r = await req.run(() => wizard.client.admin(body(false)));
  if (!r.ok) {
    mapFieldErrors();
    return;
  }
  serverMessage.value = r.value.message;
  if (form.enableTotp) {
    const s = r.value.one_time_secrets;
    totp.secret = s?.totp_secret ?? '';
    totp.uri = s?.totp_otpauth_uri ?? '';
    phase.value = 'totp';
    req.status.value = 'empty';
    return;
  }
  wipePasswords();
  wizard.markDone('admin');
  phase.value = 'done';
}

async function confirmTotp() {
  delete errors.totp_code;
  if (!/^[0-9]{6}$/.test(form.totpCode.trim())) {
    errors.totp_code = t('admin.totp.codeFormat');
    return;
  }
  // 管理员步骤幂等：再次提交同一口令（仍在内存中）并附验证码。
  const r = await req.run(() => wizard.client.admin(body(true)));
  if (!r.ok) {
    mapFieldErrors();
    return;
  }
  recoveryCodes.value = r.value.one_time_secrets?.recovery_codes ?? [];
  serverMessage.value = r.value.message;
  totp.secret = '';
  totp.uri = '';
  form.totpCode = '';
  wipePasswords();
  phase.value = 'recovery';
  wizard.markDone('admin');
  req.status.value = 'empty';
}

const recoveryText = computed(() => recoveryCodes.value.join('\n'));
function finishRecovery() {
  recoveryCodes.value = [];
  recoveryAck.value = false;
  phase.value = 'done';
}
</script>

<template>
  <StepShell testid="step-admin" :title="t('admin.title')" :description="t('admin.description')" :step="5" :total="STEP_ORDER.length">
    <p v-if="alreadyDone" class="notice notice--info" role="status" data-testid="admin-already">{{ t('admin.doneBefore') }}</p>

    <form v-if="phase === 'form'" class="form" novalidate autocomplete="off" @submit.prevent="submit">
      <fieldset class="form__group" :disabled="req.status.value === 'loading'">
        <SInput v-model="form.email" name="email" type="email" inputmode="email" autocomplete="off" :label="t('admin.field.email')" required :error="errors.email" />
        <SInput v-model="form.displayName" name="display_name" :label="t('admin.field.displayName')" required :error="errors.display_name" />
        <SInput v-model="form.password" name="new_password" type="password" autocomplete="new-password" :label="t('admin.field.pw')" required :error="errors.password" />
        <StrengthMeter :id="meterId" :assessment="assessment" />
        <SInput v-model="form.confirm" name="confirm_password" type="password" autocomplete="new-password" :label="t('admin.field.confirm')" required :error="errors.confirm" />
        <SRadioGroup v-model="form.locale" name="admin_locale" :legend="t('admin.field.locale')" :options="[{ value: 'zh-CN', label: t('lang.zh-CN') }, { value: 'en', label: t('lang.en') }]" />
        <SCheckbox v-model="form.enableTotp" name="enable_totp" :label="t('admin.field.enableTotp')" />
      </fieldset>
      <div class="actions">
        <button type="submit" class="btn btn--primary" data-testid="admin-submit" :disabled="req.status.value === 'loading'">{{ t('admin.submit') }}</button>
      </div>
    </form>

    <section v-else-if="phase === 'totp'" class="totp" data-testid="admin-totp" aria-labelledby="totp-title">
      <h3 id="totp-title" class="step__subtitle">{{ t('admin.totp.title') }}</h3>
      <p>{{ t('admin.totp.intro') }}</p>
      <div class="totp__grid">
        <QrCode v-if="totp.uri" :value="totp.uri" :label="t('admin.totp.qrLabel')" />
        <dl class="totp__facts">
          <dt>{{ t('admin.totp.manualKey') }}</dt>
          <dd>
            <code class="secret" data-testid="totp-secret">{{ totp.secret }}</code>
            <CopyButton :value="totp.secret" testid="totp-secret-copy" />
          </dd>
          <dt>{{ t('admin.totp.uri') }}</dt>
          <dd>
            <code class="secret secret--wrap" data-testid="totp-uri">{{ totp.uri }}</code>
            <CopyButton :value="totp.uri" />
          </dd>
        </dl>
      </div>
      <form class="form" novalidate autocomplete="off" @submit.prevent="confirmTotp">
        <fieldset class="form__group" :disabled="req.status.value === 'loading'">
          <SInput v-model="form.totpCode" name="totp_code" inputmode="numeric" autocomplete="one-time-code" :maxlength="6" :label="t('admin.totp.code')" required :error="errors.totp_code" />
        </fieldset>
        <div class="actions">
          <button type="submit" class="btn btn--primary" data-testid="totp-confirm" :disabled="req.status.value === 'loading'">{{ t('admin.totp.confirm') }}</button>
        </div>
      </form>
    </section>

    <section v-else-if="phase === 'recovery'" class="recovery" data-testid="admin-recovery" aria-labelledby="recovery-title">
      <h3 id="recovery-title" class="step__subtitle">{{ t('admin.recovery.title') }}</h3>
      <p class="notice notice--warn" role="note">{{ t('admin.recovery.warning') }}</p>
      <ul class="codes" data-testid="recovery-codes">
        <li v-for="c in recoveryCodes" :key="c"><code>{{ c }}</code></li>
      </ul>
      <CopyButton :value="recoveryText" testid="recovery-copy" />
      <SCheckbox v-model="recoveryAck" name="recovery_ack" :label="t('admin.recovery.ack')" />
      <div class="actions">
        <button type="button" class="btn btn--primary" data-testid="recovery-continue" :disabled="!recoveryAck" @click="finishRecovery">{{ t('nav.next') }}</button>
      </div>
    </section>

    <ResultNote v-else-if="phase === 'done'" :message="t('admin.done')" :server-message="serverMessage" step="admin" testid="admin-ok" />

    <LeStateBoundary v-if="phase !== 'done' && phase !== 'recovery'" :status="req.status.value" @retry="phase === 'totp' ? confirmTotp() : submit()">
      <template #error>
        <SetupError v-if="req.error.value" :error="req.error.value" @retry="phase === 'totp' ? confirmTotp() : submit()" />
      </template>
      <template #empty>
        <span />
      </template>
      <span />
    </LeStateBoundary>
  </StepShell>
</template>
