<script setup lang="ts">
import { computed, reactive, ref } from 'vue';
import { LeStateBoundary } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import ResultNote from '../components/ResultNote.vue';
import SCheckbox from '../components/SCheckbox.vue';
import SetupError from '../components/SetupError.vue';
import SInput from '../components/SInput.vue';
import SRadioGroup from '../components/SRadioGroup.vue';
import StepShell from '../components/StepShell.vue';
import type { SetupDbConfig } from '../api/types';
import { STEP_ORDER } from '../api/types';
import { useStepRequest } from '../state/request';
import { useWizard } from '../state/wizard';
import { DEFAULT_MIGRATE_USER, FIXED_APP_USER, FIXED_MAINT_USER, isLoopbackHost, isValidDbName, randomPassword } from '../validation';

const wizard = useWizard();
const { t } = useI18n();
const req = useStepRequest(wizard);

// 技术性示例值（非用户文案）。
const EXAMPLE_SOCKET = '/var/run/mysqld/mysqld.sock';
const form = reactive({
  mode: 'create' as 'create' | 'existing',
  conn: 'tcp' as 'tcp' | 'socket',
  host: '127.0.0.1',
  port: '3306',
  socket: '',
  database: 'lingedge',
  password: '',
  tls: false,
  adminUser: '',
  adminPassword: '',
  migrateUser: DEFAULT_MIGRATE_USER,
  migratePassword: '',
  maintPassword: '',
});
const errors = reactive<Record<string, string>>({});
const message = ref('');
const serverMessage = ref<string | undefined>(undefined);
const resumed = computed(() => wizard.state.done.database && req.status.value === 'empty');
const create = computed(() => form.mode === 'create');
// 折叠区里的字段出现错误时自动展开，避免错误藏在看不见的地方。
const advancedError = computed(() => !!(errors.migrate_username || errors.maint_password || (create.value && errors.migrate_password)));

function validate(): boolean {
  for (const k of Object.keys(errors)) delete errors[k];
  const need = t('db.err.required');
  if (form.conn === 'tcp') {
    if (form.host.trim() === '') errors.host = t('db.err.hostRequired');
    const p = Number(form.port);
    if (!Number.isInteger(p) || p < 1 || p > 65535) errors.port = t('db.err.portRange');
    if (!form.tls && form.host.trim() !== '' && !isLoopbackHost(form.host)) errors.tls = t('db.tlsRequired');
  } else if (!form.socket.trim().startsWith('/')) {
    errors.socket = t('db.err.socketAbsolute');
  }
  if (!isValidDbName(form.database)) errors.database = t('db.err.databaseName');
  if (create.value) {
    if (form.adminUser.trim() === '') errors.admin_username = need;
    if (form.adminPassword === '') errors.admin_password = need;
  } else {
    if (form.password === '') errors.password = need;
    if (form.migratePassword === '') errors.migrate_password = need;
  }
  return Object.keys(errors).length === 0;
}

function buildBody(): SetupDbConfig {
  const body: SetupDbConfig = {
    database: form.database.trim(),
    username: FIXED_APP_USER,
    // 建账号模式：口令留空则本地生成随机口令（只加密存于运行期凭据文件，无需人记）。
    password: create.value && form.password === '' ? randomPassword() : form.password,
  };
  if (form.conn === 'tcp') {
    body.host = form.host.trim();
    body.port = Number(form.port);
    body.tls = form.tls;
  } else {
    body.socket = form.socket.trim();
  }
  if (create.value) {
    body.create_if_missing = true;
    body.admin_username = form.adminUser.trim();
    body.admin_password = form.adminPassword;
  }
  if (form.migrateUser.trim() !== '' && form.migrateUser.trim() !== DEFAULT_MIGRATE_USER) body.migrate_username = form.migrateUser.trim();
  if (form.migratePassword !== '') body.migrate_password = form.migratePassword;
  if (form.maintPassword !== '') {
    body.maint_username = FIXED_MAINT_USER;
    body.maint_password = form.maintPassword;
  }
  return body;
}

function clearSecrets() {
  form.password = '';
  form.adminPassword = '';
  form.migratePassword = '';
  form.maintPassword = '';
}

function focusFirstInvalid() {
  setTimeout(() => document.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus(), 0);
}

async function submit() {
  if (!validate()) {
    focusFirstInvalid();
    return;
  }
  const body = buildBody();
  const r = await req.run(() => wizard.client.database(body));
  // 服务端校验失败的逐字段原因映射回字段（口令不回显、不保留到下一次渲染之外）。
  if (!r.ok) {
    const e = req.error.value;
    for (const d of e?.details ?? []) if (d.field && !errors[d.field]) errors[d.field] = d.reason;
    return;
  }
  message.value = t('db.ok');
  serverMessage.value = r.value.message;
  clearSecrets();
  wizard.markDone('database');
}
</script>

<template>
  <StepShell testid="step-database" :title="t('db.title')" :description="t('db.description')" :step="2" :total="STEP_ORDER.length">
    <p v-if="resumed" class="notice notice--info" role="status" data-testid="db-resume-hint">{{ t('db.resumeHint') }}</p>
    <form class="form" novalidate autocomplete="off" @submit.prevent="submit">
      <fieldset class="form__group" :disabled="req.status.value === 'loading'">
        <SRadioGroup v-model="form.mode" name="mode" :legend="t('db.mode.label')" :options="[{ value: 'create', label: t('db.mode.create'), hint: t('db.mode.createHint') }, { value: 'existing', label: t('db.mode.existing'), hint: t('db.mode.existingHint') }]" />
        <SRadioGroup v-model="form.conn" name="conn" :legend="t('db.conn.label')" :options="[{ value: 'tcp', label: t('db.conn.tcp') }, { value: 'socket', label: t('db.conn.socket') }]" />
        <div v-if="form.conn === 'tcp'" class="grid">
          <SInput v-model="form.host" name="host" :label="t('db.field.host')" required :error="errors.host" />
          <SInput v-model="form.port" name="port" type="number" inputmode="numeric" :label="t('db.field.port')" required :error="errors.port" />
        </div>
        <SInput v-else v-model="form.socket" name="socket" :label="t('db.field.socket')" :hint="t('db.field.socketHint')" :placeholder="EXAMPLE_SOCKET" required :error="errors.socket" />
        <SInput v-model="form.database" name="database" :label="t('db.field.database')" required :error="errors.database" />
        <p class="fixed" data-testid="db-fixed-accounts">{{ t('db.field.fixedAccounts') }} <code>{{ FIXED_APP_USER }}</code>, <code>{{ FIXED_MAINT_USER }}</code></p>
        <SInput v-model="form.password" name="password" type="password" autocomplete="new-password" :label="t('db.field.pw')" :hint="create ? t('db.field.pwCreateHint') : t('db.field.pwExistingHint')" :required="!create" :error="errors.password" />
        <SCheckbox v-if="form.conn === 'tcp'" v-model="form.tls" name="tls" :label="t('db.field.tls')" :hint="t('db.field.tlsHint')" />
        <p v-if="errors.tls" class="field__error" role="alert" data-testid="db-tls-error">{{ errors.tls }}</p>
        <template v-if="create">
          <SInput v-model="form.adminUser" name="admin_username" :label="t('db.field.adminUsername')" required :error="errors.admin_username" />
          <SInput v-model="form.adminPassword" name="admin_password" type="password" autocomplete="new-password" :label="t('db.field.adminPw')" required :error="errors.admin_password" />
        </template>
        <SInput v-if="!create" v-model="form.migratePassword" name="migrate_password" type="password" autocomplete="new-password" :label="t('db.field.migratePw')" :hint="t('db.field.migratePwExistingHint')" required :error="errors.migrate_password" />
        <details class="advanced" :open="advancedError || undefined">
          <summary>{{ t('db.advanced') }}</summary>
          <SInput v-model="form.migrateUser" name="migrate_username" :label="t('db.field.migrateUsername')" :error="errors.migrate_username" />
          <SInput v-if="create" v-model="form.migratePassword" name="migrate_password" type="password" autocomplete="new-password" :label="t('db.field.migratePw')" :hint="t('db.field.migratePwCreateHint')" :error="errors.migrate_password" />
          <SInput v-model="form.maintPassword" name="maint_password" type="password" autocomplete="new-password" :label="t('db.field.maintPw')" :hint="t('db.field.maintPwHint')" :error="errors.maint_password" />
        </details>
      </fieldset>
      <div class="actions">
        <button type="submit" class="btn btn--primary" data-testid="db-submit" :disabled="req.status.value === 'loading'">{{ t('db.submit') }}</button>
      </div>
    </form>
    <LeStateBoundary :status="req.status.value" @retry="submit">
      <template #error>
        <SetupError v-if="req.error.value" :error="req.error.value" @retry="submit" />
      </template>
      <template #empty>
        <p class="result result--empty" data-testid="db-empty">{{ t('db.resultEmpty') }}</p>
      </template>
      <ResultNote :message="message" :server-message="serverMessage" testid="db-ok" />
    </LeStateBoundary>
  </StepShell>
</template>
