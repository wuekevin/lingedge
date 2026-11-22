<script setup lang="ts">
import { Message } from '@arco-design/web-vue';
import { LeStateBoundary, formatDateTime, formatUtc } from '@lingedge/shared';
import { computed, onMounted, reactive, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { newIdempotencyKey } from '../../api/admin';
import { useAdminApi } from '../../api/context';
import { isApiError } from '../../api/errors';
import { errorText } from '../../api/messages';
import type { AdminUser, Role } from '../../api/types';
import DangerDialog from '../../components/DangerDialog.vue';
import PagerBar from '../../components/PagerBar.vue';
import SecretDialog from '../../components/SecretDialog.vue';
import { useAction } from '../../composables/useAction';
import { useAsyncState } from '../../composables/useAsyncState';
import { useCan } from '../../composables/useCan';
import { useFormRules } from '../../composables/useFormRules';
import { useRefreshOnActivate } from '../../composables/useRefreshOnActivate';
import { useSessionStore } from '../../stores/session';

// 用户列表（ia.md §1.11；GET /admin/users 只返回平台用户 = 管理员）：创建、角色分配、启停、解锁、吊销会话、重置口令、删除。
// 「系统设置 → 管理员管理」是别名入口，共用本组件（UX-3）。
// 危险操作分级（UX-12）：停用/吊销会话/重置口令/删除 = L2+（影响范围 + 固定审计提示）；创建/提权管理员、重置口令、删除、改角色在服务端是 L4（x-reauth），
// 窗口外会话由 API 客户端弹出再认证框并自动重试。
const { t, te, locale } = useI18n();
const api = useAdminApi();
const session = useSessionStore();
const can = useCan();
const rules = useFormRules();

const page = ref(1);
const PAGE_SIZE = 20;
const s = useAsyncState(
  (signal) => api.listUsers({ page: page.value, page_size: PAGE_SIZE, sort: 'created_at' }, signal),
  (d) => d.items.length === 0,
);
onMounted(() => void s.run());
useRefreshOnActivate(() => void s.run());
function goto(p: number): void {
  page.value = p;
  void s.run();
}
const canWrite = computed(() => can('user:write'));
const canAssign = computed(() => can('user:write', 'role:write'));
const canListRoles = computed(() => can('role:read'));
const when = (v: string | null | undefined): string => formatDateTime(v, { locale: String(locale.value) });
const isSelf = (u: AdminUser): boolean => u.uid === session.user?.uid;
const statusChip = (st: string): string => (st === 'active' ? 'le-chip--ok' : st === 'locked' ? 'le-chip--warn' : 'le-chip--muted');
const toast = (key: string): void => void Message.success(t(key));

// ---- 角色目录（平台角色；创建用户与角色分配用）----
const roles = ref<Role[]>([]);
async function loadRoles(): Promise<void> {
  if (!canListRoles.value) return;
  try {
    roles.value = (await api.listRoles({ page_size: 200 })).items.filter((r) => r.scope === 'platform');
  } catch {
    roles.value = [];
  }
}

// ---- 创建 ----
const createOpen = ref(false);
const createForm = reactive({ email: '', display_name: '', locale: 'zh-CN', initial: '', role_codes: [] as string[] });
const create = useAction();
let createKey = '';
const createRules = computed(() => ({
  email: [rules.required(t('users.col.email')), rules.email(t('users.col.email')), rules.maxLength(t('users.col.email'), 255)],
  display_name: [rules.required(t('users.col.name')), rules.maxLength(t('users.col.name'), 128)],
  initial: [rules.minLength(t('users.create.initial'), 12), rules.maxLength(t('users.create.initial'), 128)],
}));
function openCreate(): void {
  Object.assign(createForm, { email: '', display_name: '', locale: String(locale.value) === 'en' ? 'en' : 'zh-CN', initial: '', role_codes: [] });
  create.reset();
  createKey = newIdempotencyKey();
  createOpen.value = true;
  void loadRoles();
}
const secretOpen = ref(false);
const secretText = ref('');
const secretTitleKey = ref('users.once.createdTitle');
function showSecret(titleKey: string, value: string): void {
  secretTitleKey.value = titleKey;
  secretText.value = value;
  secretOpen.value = true;
}
function dropSecret(): void {
  secretText.value = '';
}
async function submitCreate(): Promise<void> {
  const r = await create.run(() =>
    api.createUser(
      {
        email: createForm.email.trim(),
        display_name: createForm.display_name.trim(),
        user_type: 'platform',
        locale: createForm.locale as 'zh-CN' | 'en',
        ...(createForm.initial ? { initial_password: createForm.initial } : {}),
        ...(createForm.role_codes.length > 0 ? { role_codes: createForm.role_codes } : {}),
      },
      createKey,
    ),
  );
  if (!r.ok) return;
  createOpen.value = false;
  createForm.initial = '';
  void s.run();
  if (r.value.temporary_password) showSecret('users.once.createdTitle', r.value.temporary_password);
  else toast('users.toast.created');
}

// ---- 角色分配 ----
const rolesTarget = ref<AdminUser | null>(null);
const rolesOpen = computed({ get: () => rolesTarget.value !== null, set: (v: boolean) => (v ? undefined : (rolesTarget.value = null)) });
const rolesPick = ref<string[]>([]);
const assign = useAction();
function openRoles(u: AdminUser): void {
  rolesTarget.value = u;
  rolesPick.value = [...(u.role_codes ?? [])];
  assign.reset();
  void loadRoles();
}
async function submitRoles(): Promise<void> {
  const u = rolesTarget.value;
  if (!u) return;
  const r = await assign.run(() => api.setUserRoles(u.uid, rolesPick.value));
  if (!r.ok) return;
  rolesTarget.value = null;
  toast('users.toast.rolesSaved');
  void s.run();
  if (isSelf(u)) await session.restore(); // 自己的角色变了：权限与菜单以服务端 /auth/me 为准
}

// ---- L2+ 危险操作 ----
type Danger = { kind: 'disable' | 'revoke' | 'reset' | 'delete'; user: AdminUser };
const danger = ref<Danger | null>(null);
const dangerOpen = computed({ get: () => danger.value !== null, set: (v: boolean) => (v ? undefined : (danger.value = null)) });
const dangerAction = useAction();
const manualPw = ref('');
function ask(kind: Danger['kind'], user: AdminUser): void {
  danger.value = { kind, user };
  manualPw.value = '';
  dangerAction.reset();
}
const manualPwProblem = computed(() => (manualPw.value !== '' && (manualPw.value.length < 12 || manualPw.value.length > 128) ? t('form.rule.minLength', { field: t('users.reset.manual'), min: 12 }) : ''));
async function confirmDanger(): Promise<void> {
  const d = danger.value;
  if (!d) return;
  if (d.kind === 'reset' && manualPwProblem.value) return;
  const r = await dangerAction.run(async () => {
    if (d.kind === 'disable') await api.updateUser(d.user.uid, { status: 'disabled' });
    else if (d.kind === 'revoke') await api.revokeUserSessions(d.user.uid);
    else if (d.kind === 'delete') await api.deleteUser(d.user.uid);
    else return api.resetUserSecret(d.user.uid, manualPw.value || undefined);
    return undefined;
  });
  if (!r.ok) return;
  danger.value = null;
  void s.run();
  if (d.kind === 'reset') {
    if (r.value?.temporary_password) showSecret('users.once.resetTitle', r.value.temporary_password);
    else toast('users.toast.reset');
  } else toast(`users.toast.${d.kind}`);
}
const dangerTitle = computed(() => (danger.value ? t(`users.danger.${danger.value.kind}.title`, { name: danger.value.user.email }) : ''));
const dangerImpact = computed(() => (danger.value ? t(`users.danger.${danger.value.kind}.impact`, { name: danger.value.user.email }) : ''));
const dangerOk = computed(() => (danger.value ? t(`users.danger.${danger.value.kind}.ok`) : ''));

// ---- 直接操作（L1）----
const rowAction = useAction();
const rowError = computed(() => (rowAction.error.value ? errorText(t, te, rowAction.error.value) : ''));
const rowRequestId = computed(() => (isApiError(rowAction.error.value) ? rowAction.error.value.requestId : ''));
async function enable(u: AdminUser): Promise<void> {
  if ((await rowAction.run(() => api.updateUser(u.uid, { status: 'active' }))).ok) {
    toast('users.toast.enabled');
    void s.run();
  }
}
async function unlock(u: AdminUser): Promise<void> {
  if ((await rowAction.run(() => api.unlockUser(u.uid))).ok) {
    toast('users.toast.unlocked');
    void s.run();
  }
}
</script>

<template>
  <section class="le-page" data-testid="users-page">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.usersList') }}</h1>
      <div class="le-page__actions">
        <a-button v-if="canWrite" type="primary" data-testid="user-create" @click="openCreate">{{ t('users.create.open') }}</a-button>
      </div>
    </header>
    <p class="le-page__desc">{{ t('users.scope') }}</p>
    <div v-if="rowError" class="le-dialog__error" role="alert" data-testid="user-row-error">
      <span>{{ rowError }}</span>
      <span v-if="rowRequestId">{{ t('errors.requestId') }}: <code>{{ rowRequestId }}</code></span>
    </div>
    <LeStateBoundary :status="s.status.value" :error-kind="s.errorKind.value" :request-id="s.requestId.value" @retry="s.run">
      <div class="le-table-wrap">
        <table class="le-table le-table--stack" data-testid="users-table">
          <thead>
            <tr>
              <th scope="col">{{ t('users.col.email') }}</th>
              <th scope="col">{{ t('users.col.name') }}</th>
              <th scope="col">{{ t('users.col.status') }}</th>
              <th scope="col">{{ t('users.col.roles') }}</th>
              <th scope="col">{{ t('users.col.totp') }}</th>
              <th scope="col">{{ t('users.col.lastLogin') }}</th>
              <th scope="col">{{ t('users.col.actions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="u in s.data.value?.items ?? []" :key="u.uid" data-testid="user-row" :data-email="u.email">
              <td :data-label="t('users.col.email')">
                {{ u.email }}
                <span v-if="isSelf(u)" class="le-chip le-chip--info">{{ t('users.self') }}</span>
              </td>
              <td :data-label="t('users.col.name')">{{ u.display_name }}</td>
              <td :data-label="t('users.col.status')">
                <span class="le-chip" :class="statusChip(u.status)" data-testid="user-status">{{ t(`users.status.${u.status}`) }}</span>
                <span v-if="u.must_change_password" class="le-chip le-chip--warn">{{ t('users.mustChange') }}</span>
              </td>
              <td :data-label="t('users.col.roles')" data-testid="user-roles-cell">
                <template v-if="(u.role_codes ?? []).length > 0"><span v-for="(c, i) in u.role_codes" :key="c"><span class="le-nowrap">{{ c }}</span><span v-if="i < (u.role_codes?.length ?? 0) - 1">, </span></span></template>
                <template v-else>{{ t('users.noRoles') }}</template>
              </td>
              <td :data-label="t('users.col.totp')">{{ u.totp_enabled ? t('users.totpOn') : t('users.totpOff') }}</td>
              <td :data-label="t('users.col.lastLogin')" :title="formatUtc(u.last_login_at)">{{ when(u.last_login_at) }}</td>
              <td :data-label="t('users.col.actions')">
                <div class="le-table__actions">
                  <a-button v-if="canAssign" size="small" data-testid="user-roles" @click="openRoles(u)">{{ t('users.action.roles') }}</a-button>
                  <a-button v-if="canWrite && u.status === 'disabled'" size="small" data-testid="user-enable" @click="enable(u)">{{ t('users.action.enable') }}</a-button>
                  <a-button v-if="canWrite && u.status === 'locked'" size="small" data-testid="user-unlock" @click="unlock(u)">{{ t('users.action.unlock') }}</a-button>
                  <a-button v-if="canWrite && !isSelf(u) && u.status !== 'disabled'" size="small" data-testid="user-disable" @click="ask('disable', u)">{{ t('users.action.disable') }}</a-button>
                  <a-button v-if="canWrite && !isSelf(u)" size="small" data-testid="user-revoke" @click="ask('revoke', u)">{{ t('users.action.revoke') }}</a-button>
                  <a-button v-if="canWrite" size="small" status="danger" data-testid="user-reset" @click="ask('reset', u)">{{ t('users.action.reset') }}</a-button>
                  <a-button v-if="canWrite && !isSelf(u)" size="small" status="danger" data-testid="user-delete" @click="ask('delete', u)">{{ t('users.action.delete') }}</a-button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </LeStateBoundary>
    <PagerBar v-if="s.data.value && s.data.value.total > PAGE_SIZE" :page="page" :page-size="PAGE_SIZE" :total="s.data.value.total" @change="goto" />

    <a-modal v-model:visible="createOpen" unmount-on-close :footer="false" :mask-closable="false" :title="t('users.create.title')">
      <a-form :model="createForm" :rules="createRules" layout="vertical" auto-label-width data-testid="user-create-form" @submit-success="submitCreate">
        <a-form-item field="email" :label="t('users.col.email')" validate-trigger="blur">
          <a-input v-model="createForm.email" :input-attrs="{ 'data-testid': 'create-email', autocomplete: 'off' }" />
        </a-form-item>
        <a-form-item field="display_name" :label="t('users.col.name')" validate-trigger="blur">
          <a-input v-model="createForm.display_name" :input-attrs="{ 'data-testid': 'create-name', autocomplete: 'off' }" />
        </a-form-item>
        <a-form-item field="locale" :label="t('users.create.locale')">
          <a-radio-group v-model="createForm.locale">
            <a-radio value="zh-CN">{{ t('lang.zhCN') }}</a-radio>
            <a-radio value="en">{{ t('lang.en') }}</a-radio>
          </a-radio-group>
        </a-form-item>
        <fieldset v-if="roles.length > 0" class="le-fieldset">
          <legend class="le-dialog__label">{{ t('users.create.roles') }}</legend>
          <label v-for="r in roles" :key="r.uid" class="le-check">
            <input v-model="createForm.role_codes" type="checkbox" :value="r.code" :data-testid="`create-role-${r.code}`" />
            <span>{{ r.name }} <span class="le-mono">({{ r.code }})</span></span>
          </label>
        </fieldset>
        <a-form-item field="initial" :label="t('users.create.initial')" validate-trigger="blur" :extra="t('users.create.initialHint')">
          <a-input-password v-model="createForm.initial" :input-attrs="{ 'data-testid': 'create-initial', autocomplete: 'new-password' }" />
        </a-form-item>
        <p class="le-dialog__note" data-testid="create-reauth-hint">{{ t('users.create.reauthHint') }}</p>
        <div v-if="create.error.value" class="le-dialog__error" role="alert" data-testid="create-error">
          <span>{{ errorText(t, te, create.error.value) }}</span>
          <span v-if="isApiError(create.error.value)">{{ (create.error.value.details ?? []).map((d) => d.field).filter(Boolean).join(', ') }}</span>
        </div>
        <div class="le-dialog__actions">
          <a-button @click="createOpen = false">{{ t('common.cancel') }}</a-button>
          <a-button type="primary" html-type="submit" :loading="create.busy.value" data-testid="create-submit">{{ t('users.create.submit') }}</a-button>
        </div>
        <p class="le-dialog__audit" data-testid="create-audit">{{ t('danger.audit') }}</p>
      </a-form>
    </a-modal>

    <a-modal v-model:visible="rolesOpen" unmount-on-close :footer="false" :mask-closable="false" :title="t('users.roles.title', { name: rolesTarget?.email ?? '' })">
      <form class="le-dialog" data-testid="user-roles-form" @submit.prevent="submitRoles">
        <p v-if="roles.length === 0" class="le-notice" data-testid="user-roles-empty">{{ t('users.roles.none') }}</p>
        <label v-for="r in roles" :key="r.uid" class="le-check">
          <input v-model="rolesPick" type="checkbox" :value="r.code" :data-testid="`assign-role-${r.code}`" />
          <span>{{ r.name }} <span class="le-mono">({{ r.code }})</span></span>
        </label>
        <p class="le-dialog__note">{{ t('users.roles.reauthHint') }}</p>
        <div v-if="assign.error.value" class="le-dialog__error" role="alert" data-testid="assign-error">{{ errorText(t, te, assign.error.value) }}</div>
        <div class="le-dialog__actions">
          <a-button @click="rolesTarget = null">{{ t('common.cancel') }}</a-button>
          <a-button type="primary" html-type="submit" :loading="assign.busy.value" data-testid="assign-submit">{{ t('users.roles.save') }}</a-button>
        </div>
        <p class="le-dialog__audit">{{ t('danger.audit') }}</p>
      </form>
    </a-modal>

    <DangerDialog
      v-model:visible="dangerOpen"
      level="L2"
      :title="dangerTitle"
      :impact="dangerImpact"
      :ok-text="dangerOk"
      :reauth="danger?.kind === 'reset' || danger?.kind === 'delete'"
      :busy="dangerAction.busy.value"
      :error="dangerAction.error.value"
      @confirm="confirmDanger"
    >
      <div v-if="danger?.kind === 'reset'" class="le-dialog__typed">
        <label class="le-dialog__label" for="le-reset-manual">{{ t('users.reset.manual') }}</label>
        <a-input-password v-model="manualPw" :input-attrs="{ id: 'le-reset-manual', 'data-testid': 'reset-manual', autocomplete: 'new-password' }" />
        <span class="le-dialog__note">{{ t('users.reset.manualHint') }}</span>
        <span v-if="manualPwProblem" class="le-field__error" role="alert">{{ manualPwProblem }}</span>
      </div>
    </DangerDialog>

    <SecretDialog v-model:visible="secretOpen" :title="t(secretTitleKey)" :description="t('users.once.description')" :label="t('users.once.label')" :value="secretText" @closed="dropSecret" />
  </section>
</template>

<style scoped>
.le-fieldset {
  margin: 0 0 var(--le-space-3);
  padding: 0;
  border: 0;
  display: grid;
  gap: var(--le-space-1);
}
</style>
