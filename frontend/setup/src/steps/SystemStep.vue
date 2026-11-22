<script setup lang="ts">
import { computed, reactive, ref } from 'vue';
import { LeStateBoundary, resolveLocale, type LeLocale } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import ResultNote from '../components/ResultNote.vue';
import SetupError from '../components/SetupError.vue';
import SInput from '../components/SInput.vue';
import SRadioGroup from '../components/SRadioGroup.vue';
import StepShell from '../components/StepShell.vue';
import type { SetupSystemConfig } from '../api/types';
import { STEP_ORDER } from '../api/types';
import { useStepRequest } from '../state/request';
import { useWizard } from '../state/wizard';
import { browserTimeZone, isValidBaseUrl, isValidEmail, isValidTimeZone, listTimeZones, normalizeBaseUrl } from '../validation';

const wizard = useWizard();
const { t, locale } = useI18n();
const req = useStepRequest(wizard);

const EXAMPLE_URL = 'https://admin.example.com';
const TZ_LIST_ID = 'le-tz-list';
const timeZones = listTimeZones();
const form = reactive({
  runMode: 'self_hosted' as 'self_hosted' | 'commercial',
  brandName: '',
  brandNameEn: '',
  supportEmail: '',
  baseUrl: wizard.state.baseUrl,
  locale: resolveLocale(locale.value) as LeLocale,
  timezone: browserTimeZone(),
});
const errors = reactive<Record<string, string>>({});
const serverMessage = ref<string | undefined>(undefined);
const resumed = computed(() => wizard.state.done.system && req.status.value === 'empty');

function validate(): boolean {
  for (const k of Object.keys(errors)) delete errors[k];
  if (!isValidBaseUrl(form.baseUrl)) errors.base_url = t('system.err.baseUrl');
  if (!isValidTimeZone(form.timezone)) errors.timezone = t('system.err.timezone');
  if (form.supportEmail.trim() !== '' && !isValidEmail(form.supportEmail)) errors.support_email = t('system.err.email');
  return Object.keys(errors).length === 0;
}

async function submit() {
  if (!validate()) {
    setTimeout(() => document.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus(), 0);
    return;
  }
  const baseUrl = normalizeBaseUrl(form.baseUrl);
  const brand: NonNullable<SetupSystemConfig['brand']> = {};
  if (form.brandName.trim() !== '') brand.name = form.brandName.trim();
  if (form.brandNameEn.trim() !== '') brand.name_en = form.brandNameEn.trim();
  if (form.supportEmail.trim() !== '') brand.support_email = form.supportEmail.trim();
  const body: SetupSystemConfig = { run_mode: form.runMode, base_url: baseUrl, default_locale: form.locale, timezone: form.timezone.trim() };
  if (Object.keys(brand).length > 0) body.brand = brand;
  const r = await req.run(() => wizard.client.system(body));
  if (!r.ok) {
    for (const d of req.error.value?.details ?? []) if (d.field && !errors[d.field]) errors[d.field] = d.reason;
    return;
  }
  serverMessage.value = r.value.message;
  wizard.setBaseUrl(baseUrl);
  wizard.markDone('system');
}
</script>

<template>
  <StepShell testid="step-system" :title="t('system.title')" :description="t('system.description')" :step="6" :total="STEP_ORDER.length">
    <p v-if="resumed" class="notice notice--info" role="status">{{ t('db.resumeHint') }}</p>
    <form class="form" novalidate autocomplete="off" @submit.prevent="submit">
      <fieldset class="form__group" :disabled="req.status.value === 'loading'">
        <SInput v-model="form.brandName" name="brand_name" :label="t('system.field.brandName')" :error="errors['brand.name']" />
        <SInput v-model="form.brandNameEn" name="brand_name_en" :label="t('system.field.brandNameEn')" :error="errors['brand.name_en']" />
        <SInput v-model="form.supportEmail" name="support_email" type="email" inputmode="email" :label="t('system.field.supportEmail')" :error="errors.support_email" />
        <SInput v-model="form.baseUrl" name="base_url" type="url" inputmode="url" :label="t('system.field.baseUrl')" :hint="t('system.field.baseUrlHint')" :placeholder="EXAMPLE_URL" required :error="errors.base_url" />
        <SRadioGroup v-model="form.runMode" name="run_mode" :legend="t('system.field.runMode')" :options="[{ value: 'self_hosted', label: t('system.field.selfHosted'), hint: t('system.field.runModeHint') }, { value: 'commercial', label: t('system.field.commercial') }]" />
        <SRadioGroup v-model="form.locale" name="default_locale" :legend="t('system.field.locale')" :options="[{ value: 'zh-CN', label: t('lang.zh-CN') }, { value: 'en', label: t('lang.en') }]" />
        <SInput v-model="form.timezone" name="timezone" :list="TZ_LIST_ID" :label="t('system.field.timezone')" :hint="t('system.field.timezoneHint')" required :error="errors.timezone" />
        <datalist :id="TZ_LIST_ID">
          <option v-for="z in timeZones" :key="z" :value="z" />
        </datalist>
      </fieldset>
      <div class="actions">
        <button type="submit" class="btn btn--primary" data-testid="system-submit" :disabled="req.status.value === 'loading'">{{ t('system.submit') }}</button>
      </div>
    </form>
    <LeStateBoundary :status="req.status.value" @retry="submit">
      <template #error>
        <SetupError v-if="req.error.value" :error="req.error.value" @retry="submit" />
      </template>
      <template #empty>
        <span />
      </template>
      <ResultNote :message="t('system.done')" :server-message="serverMessage" testid="system-ok" />
    </LeStateBoundary>
  </StepShell>
</template>
