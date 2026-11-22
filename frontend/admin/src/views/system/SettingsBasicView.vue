<script setup lang="ts">
import { Message } from '@arco-design/web-vue';
import { LeStateBoundary } from '@lingedge/shared';
import { computed, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useAdminApi } from '../../api/context';
import type { RunMode } from '../../api/types';
import DangerDialog from '../../components/DangerDialog.vue';
import { useAction } from '../../composables/useAction';
import { useAsyncState } from '../../composables/useAsyncState';
import { useCan } from '../../composables/useCan';
import { useSessionStore } from '../../stores/session';
import SettingsForm from './SettingsForm.vue';
import type { FieldDef } from './settingsFields';

// 基本设置（ia.md §1.17）：入口 URL、时区、语言、自助注册 + 运行模式。
// 运行模式切换是 L3 危险操作（键入目标模式 + 固定审计提示），同时在服务端是 L4（x-reauth）：窗口外会弹出再认证框并自动重试。
const { t } = useI18n();
const api = useAdminApi();
const session = useSessionStore();
const can = useCan();
const canWrite = computed(() => can('setting:write'));

const FIELDS: readonly FieldDef[] = [
  { key: 'general.base_url', kind: 'url', nullable: true, maxLen: 512 },
  { key: 'general.admin_url', kind: 'url', nullable: true, maxLen: 512 },
  { key: 'general.console_url', kind: 'url', nullable: true, maxLen: 512 },
  { key: 'general.docs_url', kind: 'url', nullable: true, maxLen: 512 },
  { key: 'general.status_url', kind: 'url', nullable: true, maxLen: 512 },
  { key: 'general.timezone', kind: 'timezone', required: true, maxLen: 48 },
  { key: 'general.locales', kind: 'locales' },
  { key: 'general.registration_open', kind: 'bool' },
];

// ---- 运行模式 ----
const mode = useAsyncState((signal) => api.getRunMode(signal));
onMounted(() => void mode.run());
const current = computed<RunMode | null>(() => mode.data.value?.run_mode ?? null);
const target = computed<RunMode>(() => (current.value === 'commercial' ? 'self_hosted' : 'commercial'));
const open = ref(false);
const sw = useAction();
function ask(): void {
  sw.reset();
  open.value = true;
}
async function confirm(): Promise<void> {
  const r = await sw.run(() => api.putRunMode(target.value));
  if (!r.ok) return;
  open.value = false;
  mode.data.value = r.value;
  Message.success(t('settings.basic.mode.switched', { mode: t(`runMode.${r.value.run_mode}`) }));
  await session.restore(); // 能力清单/运行模式以服务端 /auth/me 为准：菜单随之变化（FR-24、FR-31）
}
</script>

<template>
  <section class="le-page" data-testid="settings-basic-page">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.settingsBasic') }}</h1>
    </header>
    <div class="le-card" data-testid="run-mode-card">
      <h2 class="le-card__title">{{ t('settings.basic.mode.title') }}</h2>
      <LeStateBoundary :status="mode.status.value" :error-kind="mode.errorKind.value" :request-id="mode.requestId.value" @retry="mode.run">
        <p>
          {{ t('settings.basic.mode.current') }}
          <span class="le-chip le-chip--info" data-testid="run-mode-current" :data-mode="current ?? ''">{{ current ? t(`runMode.${current}`) : '' }}</span>
        </p>
        <p class="le-page__desc">{{ t('settings.basic.mode.desc') }}</p>
        <div v-if="canWrite" class="le-page__actions">
          <a-button status="danger" data-testid="run-mode-switch" @click="ask">{{ t('settings.basic.mode.switch', { mode: t(`runMode.${target}`) }) }}</a-button>
        </div>
      </LeStateBoundary>
    </div>
    <SettingsForm
      testid="settings-basic-form"
      i18n-prefix="settings.basic"
      :fields="FIELDS"
      :can-write="canWrite"
      :load="(signal) => api.getSettings(signal)"
      :save="(changed) => api.patchSettings(changed)"
    />
    <DangerDialog
      v-model:visible="open"
      level="L3"
      reauth
      :title="t('settings.basic.mode.dialogTitle', { mode: t(`runMode.${target}`) })"
      :impact="t(`settings.basic.mode.impact.${target}`)"
      :confirm-word="target"
      :ok-text="t('settings.basic.mode.confirm', { mode: t(`runMode.${target}`) })"
      :busy="sw.busy.value"
      :error="sw.error.value"
      @confirm="confirm"
    />
  </section>
</template>
