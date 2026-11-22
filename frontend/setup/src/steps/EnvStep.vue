<script setup lang="ts">
import { computed, onMounted, shallowRef } from 'vue';
import { LeEmpty, LeStateBoundary } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import { useServerText } from '../api/useServerText';
import SetupError from '../components/SetupError.vue';
import StepShell from '../components/StepShell.vue';
import type { SetupCheckItem } from '../api/types';
import { STEP_ORDER } from '../api/types';
import { useStepRequest } from '../state/request';
import { useWizard } from '../state/wizard';

const wizard = useWizard();
const { t, te } = useI18n();
const st = useServerText();
const req = useStepRequest(wizard, 'loading');
const items = shallowRef<SetupCheckItem[]>([]);

const blocking = computed(() => items.value.some((i) => !i.ok && i.severity === 'error'));
const warned = computed(() => items.value.some((i) => i.severity === 'warn' || (!i.ok && i.severity !== 'error')));

/** 检查项的稳定键（点分，如 data_dir.writable）→ 界面标签；未知键退回服务端给的文案。 */
function labelOf(it: SetupCheckItem): string {
  const key = `env.item.${it.key === 'server.env' ? 'server_env' : it.key}`;
  return te(key) ? t(key) : it.message;
}
function sevOf(it: SetupCheckItem): 'info' | 'warn' | 'error' {
  if (!it.ok) return it.severity === 'warn' ? 'warn' : 'error';
  return it.severity === 'warn' ? 'warn' : 'info';
}

async function run() {
  const r = await req.run(() => wizard.client.checks());
  if (!r.ok) {
    wizard.markDone('environment', false);
    return;
  }
  items.value = r.value.items;
  const ok = !r.value.items.some((i) => !i.ok && i.severity === 'error');
  wizard.markDone('environment', ok && r.value.items.length > 0);
  if (r.value.items.length === 0) req.status.value = 'empty';
}
onMounted(() => {
  void run();
});
</script>

<template>
  <StepShell testid="step-environment" :title="t('env.title')" :description="t('env.description')" :step="1" :total="STEP_ORDER.length">
    <LeStateBoundary :status="req.status.value" @retry="run">
      <template #error>
        <SetupError v-if="req.error.value" :error="req.error.value" @retry="run" />
      </template>
      <template #empty>
        <LeEmpty reason="no-data">
          <template #action>
            <p>{{ t('env.emptyTitle') }}. {{ t('env.emptyBody') }}</p>
            <button type="button" class="btn btn--primary" data-testid="env-rerun" @click="run">{{ t('env.rerun') }}</button>
          </template>
        </LeEmpty>
      </template>
      <ul class="checks" data-testid="env-items">
        <li v-for="it in items" :key="it.key" class="check-item" :data-check="it.key" :data-severity="sevOf(it)">
          <span class="badge" :class="`badge--${sevOf(it)}`" data-testid="env-severity">{{ t(`env.severity.${sevOf(it)}`) }}</span>
          <div class="check-item__text">
            <strong>{{ labelOf(it) }}</strong>
            <span class="check-item__msg" :lang="st.check(it).lang" :data-localized="st.check(it).localized" data-testid="check-msg">{{ st.check(it).text }}</span>
          </div>
        </li>
      </ul>
      <p v-if="blocking" class="notice notice--danger" role="alert" data-testid="env-blocking">{{ t('env.blocking') }}</p>
      <p v-else-if="warned" class="notice notice--warn" role="status">{{ t('env.passedWithWarnings') }}</p>
      <p v-else class="notice notice--ok" role="status" data-testid="env-passed">{{ t('env.passed') }}</p>
      <div class="actions">
        <button type="button" class="btn" data-testid="env-rerun" @click="run">{{ t('env.rerun') }}</button>
      </div>
    </LeStateBoundary>
  </StepShell>
</template>
