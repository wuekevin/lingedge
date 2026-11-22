<script setup lang="ts">
import { computed, onBeforeUnmount, ref, shallowRef } from 'vue';
import { LeEmpty, LeLoading, LeStateBoundary } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import ResultNote from '../components/ResultNote.vue';
import SetupError from '../components/SetupError.vue';
import StepShell from '../components/StepShell.vue';
import { CODE_NETWORK } from '../api/errors';
import type { SetupMigration } from '../api/types';
import { STEP_ORDER } from '../api/types';
import { useStepRequest } from '../state/request';
import { useWizard } from '../state/wizard';

const wizard = useWizard();
const { t } = useI18n();
const req = useStepRequest(wizard);
const migrations = shallowRef<SetupMigration[]>([]);
const note = ref('');
const upToDate = ref(false);
const recovered = ref(false);

const needsDbStep = computed(() => req.error.value?.details.some((d) => d.field === 'migrate_password') ?? false);
let polling = true;
onBeforeUnmount(() => {
  polling = false;
});

/**
 * 迁移可能很久：若 POST 的连接在中途断开（代理超时/隧道抖动），服务端仍在继续执行。
 * 此时轮询 GET status（OpenAPI 约定的耗时步骤进度通道），直到 init_db 不再处于 running。
 */
async function recoverByPolling(): Promise<'done' | 'pending'> {
  const deadline = Date.now() + 10 * 60 * 1000;
  while (polling && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 2000));
    try {
      const st = await wizard.client.status();
      const s = st.steps?.find((x) => x.key === 'init_db');
      if (s?.status === 'done') return 'done';
      if (s && s.status !== 'running') return 'pending';
    } catch (e) {
      if (wizard.guard(e)) return 'pending';
    }
  }
  return 'pending';
}

async function run() {
  recovered.value = false;
  const r = await req.run(async () => {
    try {
      return await wizard.client.initialize();
    } catch (e) {
      if ((e as { code?: string }).code === CODE_NETWORK && (await recoverByPolling()) === 'done') return null;
      throw e;
    }
  });
  if (!r.ok) return;
  if (r.value === null) {
    recovered.value = true;
    migrations.value = [];
    upToDate.value = false;
    note.value = t('init.recovered');
  } else {
    migrations.value = r.value.migrations ?? [];
    upToDate.value = migrations.value.length === 0;
    note.value = t('init.ok');
  }
  wizard.markDone('init_db');
  if (upToDate.value) req.status.value = 'empty';
}
const alreadyDone = computed(() => wizard.state.done.init_db && req.status.value === 'empty' && !upToDate.value && !recovered.value);
</script>

<template>
  <StepShell testid="step-init" :title="t('init.title')" :description="t('init.description')" :step="4" :total="STEP_ORDER.length">
    <div class="actions">
      <button type="button" class="btn btn--primary" data-testid="init-run" :disabled="req.status.value === 'loading'" @click="run">{{ wizard.state.done.init_db ? t('init.rerun') : t('init.run') }}</button>
    </div>
    <p v-if="alreadyDone" class="notice notice--info" role="status" data-testid="init-already">{{ t('init.alreadyDone') }}</p>
    <LeStateBoundary :status="req.status.value" @retry="run">
      <template #loading>
        <div>
          <LeLoading :lines="2" @retry="run" />
          <p class="field__hint" role="status" data-testid="init-running">{{ t('init.running') }}</p>
        </div>
      </template>
      <template #error>
        <div>
          <SetupError v-if="req.error.value" :error="req.error.value" @retry="run" />
          <div v-if="needsDbStep" class="actions">
            <button type="button" class="btn" data-testid="init-goto-db" @click="wizard.go('database')">{{ t('init.needDbStep') }}</button>
          </div>
        </div>
      </template>
      <template #empty>
        <LeEmpty v-if="upToDate" reason="no-data" data-testid="init-uptodate">
          <template #action>
            <p>{{ t('init.upToDateTitle') }}. {{ t('init.upToDateBody') }}</p>
          </template>
        </LeEmpty>
        <span v-else />
      </template>
      <div>
        <ResultNote :message="note" step="init_db" testid="init-ok" />
        <table v-if="migrations.length > 0" class="table" data-testid="init-migrations">
          <caption class="table__caption">{{ t('init.table.caption') }}</caption>
          <thead>
            <tr>
              <th scope="col">{{ t('init.table.version') }}</th>
              <th scope="col">{{ t('init.table.name') }}</th>
              <th scope="col">{{ t('init.table.class') }}</th>
              <th scope="col">{{ t('init.table.duration') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in migrations" :key="m.version" :data-category="m.category">
              <td class="table__num">{{ String(m.version).padStart(6, '0') }}</td>
              <td><code>{{ m.name }}</code></td>
              <td>
                <span class="badge" :class="m.category === 'irreversible' ? 'badge--warn' : 'badge--info'">{{ t(`init.class.${m.category}`) }}</span>
              </td>
              <td class="table__num">{{ t('init.table.durationMs', { n: m.duration_ms }) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </LeStateBoundary>
  </StepShell>
</template>
