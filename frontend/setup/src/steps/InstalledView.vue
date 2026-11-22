<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import SetupError from '../components/SetupError.vue';
import { useWizard } from '../state/wizard';

const wizard = useWizard();
const { t } = useI18n();
</script>

<template>
  <section class="step" data-testid="installed" aria-labelledby="installed-title">
    <h2 id="installed-title" class="step__title" tabindex="-1">{{ t('installed.title') }}</h2>
    <p class="step__desc">{{ t('installed.body') }}</p>
    <SetupError v-if="wizard.state.installedError" :error="wizard.state.installedError" />
  </section>
</template>
