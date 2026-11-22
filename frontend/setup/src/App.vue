<script setup lang="ts">
import { computed, watchEffect } from 'vue';
import { resolveLocale, type LeLocale } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import AdminStep from './steps/AdminStep.vue';
import CompleteStep from './steps/CompleteStep.vue';
import DatabaseStep from './steps/DatabaseStep.vue';
import EnvStep from './steps/EnvStep.vue';
import InitStep from './steps/InitStep.vue';
import InstalledView from './steps/InstalledView.vue';
import KeysStep from './steps/KeysStep.vue';
import RedisStep from './steps/RedisStep.vue';
import SystemStep from './steps/SystemStep.vue';
import TokenGate from './steps/TokenGate.vue';
import { persistLocale } from './i18n';
import { STEP_ORDER, type SetupStepKey } from './api/types';
import { useWizard } from './state/wizard';

defineProps<{ urlTokenIgnored?: boolean }>();
const wizard = useWizard();
const { t, locale } = useI18n();

const COMPONENTS: Record<SetupStepKey, unknown> = {
  environment: EnvStep,
  database: DatabaseStep,
  redis: RedisStep,
  init_db: InitStep,
  admin: AdminStep,
  system: SystemStep,
  keys: KeysStep,
  complete: CompleteStep,
};
const current = computed(() => COMPONENTS[wizard.state.step]);
const LOCALE_OPTIONS: LeLocale[] = ['zh-CN', 'en'];

watchEffect(() => {
  document.title = t('app.title');
  document.documentElement.setAttribute('lang', locale.value);
});

function onLocale(e: Event) {
  const l = resolveLocale((e.target as HTMLSelectElement).value);
  locale.value = l;
  try {
    persistLocale(window.sessionStorage, l);
  } catch {
    /* 存储不可用 */
  }
}

const lastIndex = STEP_ORDER.length - 1;
const canBack = computed(() => wizard.state.phase === 'wizard' && wizard.stepIndex.value > 0 && !wizard.state.finished);
const canNext = computed(() => wizard.state.phase === 'wizard' && wizard.stepIndex.value < lastIndex && wizard.state.done[wizard.state.step]);

function statusOf(k: SetupStepKey): 'done' | 'current' | 'pending' {
  if (k === wizard.state.step) return 'current';
  return wizard.state.done[k] ? 'done' : 'pending';
}
function reachable(k: SetupStepKey): boolean {
  return !wizard.state.finished && (wizard.state.done[k] || STEP_ORDER.indexOf(k) <= wizard.frontier.value);
}
</script>

<template>
  <a class="skip" href="#main">{{ t('app.skip') }}</a>
  <div class="layout">
    <aside class="side">
      <div class="side__brand">
        <p class="side__name">{{ t('app.brand') }}</p>
        <p class="side__tagline">{{ t('app.tagline') }}</p>
      </div>
      <nav v-if="wizard.state.phase === 'wizard'" :aria-label="t('app.stepsLabel')" data-testid="stepper">
        <ol class="stepper">
          <li v-for="(k, i) in STEP_ORDER" :key="k" class="stepper__item" :data-step="k" :data-status="statusOf(k)">
            <button type="button" class="stepper__btn" :aria-current="statusOf(k) === 'current' ? 'step' : undefined" :disabled="!reachable(k) || statusOf(k) === 'current'" :data-testid="`nav-${k}`" @click="wizard.go(k)">
              <span class="stepper__mark" aria-hidden="true">{{ statusOf(k) === 'done' ? '✓' : i + 1 }}</span>
              <span class="stepper__label">{{ t(`steps.${k}`) }}</span>
              <span class="le-sr-only">{{ t(`nav.${statusOf(k)}`) }}</span>
            </button>
          </li>
        </ol>
      </nav>
    </aside>

    <div class="content">
      <header class="topbar">
        <label class="topbar__lang">
          <span>{{ t('app.language') }}</span>
          <select class="select" data-testid="lang-select" :value="locale" @change="onLocale">
            <option v-for="l in LOCALE_OPTIONS" :key="l" :value="l">{{ t(`lang.${l}`) }}</option>
          </select>
        </label>
      </header>
      <main id="main" class="main" tabindex="-1">
        <TokenGate v-if="wizard.state.phase === 'gate'" :url-token-ignored="!!urlTokenIgnored" />
        <InstalledView v-else-if="wizard.state.phase === 'installed'" />
        <component :is="current" v-else :key="wizard.state.step" />
      </main>
      <footer v-if="wizard.state.phase === 'wizard'" class="pager">
        <button type="button" class="btn" data-testid="nav-back" :disabled="!canBack" @click="wizard.back()">{{ t('nav.back') }}</button>
        <button type="button" class="btn btn--primary" data-testid="nav-next" :disabled="!canNext" @click="wizard.next()">{{ t('nav.next') }}</button>
      </footer>
    </div>
  </div>
</template>
