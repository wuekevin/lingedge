<script setup lang="ts">
import { Message } from '@arco-design/web-vue';
import { LeStateBoundary } from '@lingedge/shared';
import { computed, onMounted, reactive, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { newIdempotencyKey } from '../../api/admin';
import { useAdminApi } from '../../api/context';
import { errorText } from '../../api/messages';
import type { Permission, Role } from '../../api/types';
import DangerDialog from '../../components/DangerDialog.vue';
import { useAction } from '../../composables/useAction';
import { useAsyncState } from '../../composables/useAsyncState';
import { useCan } from '../../composables/useCan';
import { useFormRules } from '../../composables/useFormRules';
import { useTabDirty } from '../../composables/useTabDirty';

// 权限角色（ia.md §1.17，规范页面；「用户管理 → 用户角色」是别名入口，共用本组件）：角色列表 + 权限矩阵（资源 × 动作）。
// 内置角色只读；自定义角色可改名、分配权限、删除。创建/改权限/删除在服务端是 L4（x-reauth）：窗口外由 API 客户端弹出再认证框并自动重试。
// 防提权由服务端裁决（只能授予自己持有的权限）；矩阵只负责如实呈现与提交。
const { t, te } = useI18n();
const api = useAdminApi();
const can = useCan();
const rules = useFormRules();
const { setDirty } = useTabDirty();
const canWrite = computed(() => can('role:write'));

interface Catalog {
  roles: Role[];
  permissions: Permission[];
}
async function loadAll(signal: AbortSignal): Promise<Catalog> {
  const roles = (await api.listRoles({ page_size: 200 }, signal)).items;
  const permissions: Permission[] = [];
  for (let page = 1; page <= 20; page++) {
    const r = await api.listPermissions({ page, page_size: 200 }, signal);
    permissions.push(...r.items);
    if (permissions.length >= r.total || r.items.length === 0) break;
  }
  return { roles, permissions };
}
const s = useAsyncState(loadAll, (d) => d.roles.length === 0);
onMounted(() => void s.run());

const selectedUid = ref<string | null>(null);
const selected = computed(() => s.data.value?.roles.find((r) => r.uid === selectedUid.value) ?? null);
const picked = ref<Set<string>>(new Set());
const filter = ref('');
const readOnly = computed(() => !canWrite.value || selected.value?.is_builtin === true);

function select(r: Role): void {
  selectedUid.value = r.uid;
  picked.value = new Set(r.permissions ?? []);
  meta.name = r.name;
  meta.description = r.description ?? '';
  save.reset();
  setDirty(false);
}
const dirty = computed(() => {
  const r = selected.value;
  if (!r) return false;
  const cur = new Set(r.permissions ?? []);
  return cur.size !== picked.value.size || [...picked.value].some((p) => !cur.has(p));
});
watch(dirty, (v) => setDirty(v));

// ---- 权限矩阵：行 = 资源，列 = 动作；该角色范围（platform/tenant）不允许的权限不渲染复选框 ----
const allowedScopes = (scope: string): string[] => (scope === 'platform' ? ['platform', 'both'] : ['tenant', 'both']);
const matrix = computed(() => {
  const r = selected.value;
  const perms = s.data.value?.permissions ?? [];
  const f = filter.value.trim().toLowerCase();
  const byRes = new Map<string, Map<string, Permission>>();
  const actions = new Set<string>();
  for (const p of perms) {
    if (f && !p.code.toLowerCase().includes(f)) continue;
    actions.add(p.action);
    if (!byRes.has(p.resource)) byRes.set(p.resource, new Map());
    byRes.get(p.resource)?.set(p.action, p);
  }
  return {
    actions: [...actions].sort(),
    rows: [...byRes.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([resource, m]) => ({ resource, cells: m, allowed: (p: Permission) => !r || allowedScopes(r.scope).includes(p.scope) })),
  };
});
function toggle(code: string, on: boolean): void {
  const next = new Set(picked.value);
  if (on) next.add(code);
  else next.delete(code);
  picked.value = next;
}

// ---- 保存权限 / 改名 ----
const save = useAction();
async function savePerms(): Promise<void> {
  const r = selected.value;
  if (!r) return;
  const res = await save.run(() => api.setRolePermissions(r.uid, [...picked.value].sort()));
  if (!res.ok) return;
  replace(res.value);
  Message.success(t('roles.toast.permsSaved'));
}
const meta = reactive({ name: '', description: '' });
const metaRules = computed(() => ({ name: [rules.required(t('roles.col.name')), rules.maxLength(t('roles.col.name'), 128)], description: [rules.maxLength(t('roles.col.description'), 512)] }));
const rename = useAction();
async function saveMeta(): Promise<void> {
  const r = selected.value;
  if (!r) return;
  const res = await rename.run(() => api.updateRole(r.uid, { name: meta.name.trim(), description: meta.description }));
  if (!res.ok) return;
  replace(res.value);
  Message.success(t('roles.toast.metaSaved'));
}
function replace(updated: Role): void {
  const d = s.data.value;
  if (!d) return;
  d.roles = d.roles.map((r) => (r.uid === updated.uid ? updated : r));
  picked.value = new Set(updated.permissions ?? []);
  setDirty(false);
}

// ---- 创建 ----
const createOpen = ref(false);
const form = reactive({ scope: 'platform', code: '', name: '', description: '' });
const create = useAction();
let createKey = '';
const createRules = computed(() => ({
  code: [
    rules.required(t('roles.col.code')),
    { validator: (v: unknown, cb: (e?: string) => void) => cb(typeof v === 'string' && v !== '' && !/^[a-z][a-z0-9_]{1,63}$/.test(v) ? t('roles.create.codeInvalid') : undefined) },
  ],
  name: [rules.required(t('roles.col.name')), rules.maxLength(t('roles.col.name'), 128)],
  description: [rules.maxLength(t('roles.col.description'), 512)],
}));
function openCreate(): void {
  Object.assign(form, { scope: 'platform', code: '', name: '', description: '' });
  create.reset();
  createKey = newIdempotencyKey();
  createOpen.value = true;
}
async function submitCreate(): Promise<void> {
  const res = await create.run(() =>
    api.createRole({ scope: form.scope as 'platform' | 'tenant', code: form.code, name: form.name.trim(), ...(form.description ? { description: form.description } : {}) }, createKey),
  );
  if (!res.ok) return;
  createOpen.value = false;
  Message.success(t('roles.toast.created'));
  await s.run();
  const made = s.data.value?.roles.find((r) => r.uid === res.value.uid);
  if (made) select(made);
}

// ---- 删除（L4：输入角色 code 确认 + 再认证）----
const delOpen = ref(false);
const del = useAction();
function askDelete(): void {
  del.reset();
  delOpen.value = true;
}
async function confirmDelete(): Promise<void> {
  const r = selected.value;
  if (!r) return;
  const res = await del.run(() => api.deleteRole(r.uid));
  if (!res.ok) return;
  delOpen.value = false;
  selectedUid.value = null;
  setDirty(false);
  Message.success(t('roles.toast.deleted'));
  void s.run();
}
const permsError = computed(() => (save.error.value ? errorText(t, te, save.error.value) : ''));
</script>

<template>
  <section class="le-page" data-testid="roles-page">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.settingsRoles') }}</h1>
      <div class="le-page__actions">
        <a-button v-if="canWrite" type="primary" data-testid="role-create" @click="openCreate">{{ t('roles.create.open') }}</a-button>
      </div>
    </header>
    <LeStateBoundary :status="s.status.value" :error-kind="s.errorKind.value" :request-id="s.requestId.value" @retry="s.run">
      <div class="le-table-wrap">
        <table class="le-table le-table--stack" data-testid="roles-table">
          <thead>
            <tr>
              <th scope="col">{{ t('roles.col.name') }}</th>
              <th scope="col">{{ t('roles.col.code') }}</th>
              <th scope="col">{{ t('roles.col.scope') }}</th>
              <th scope="col">{{ t('roles.col.kind') }}</th>
              <th scope="col">{{ t('roles.col.perms') }}</th>
              <th scope="col"><span class="le-sr-only">{{ t('users.col.actions') }}</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in s.data.value?.roles ?? []" :key="r.uid" data-testid="role-row" :data-code="r.code" :aria-selected="r.uid === selectedUid">
              <td :data-label="t('roles.col.name')">{{ r.name }}</td>
              <td :data-label="t('roles.col.code')" class="le-mono">{{ r.code }}</td>
              <td :data-label="t('roles.col.scope')">{{ t(`roles.scope.${r.scope}`) }}</td>
              <td :data-label="t('roles.col.kind')">
                <span class="le-chip" :class="r.is_builtin ? 'le-chip--muted' : 'le-chip--info'">{{ r.is_builtin ? t('roles.builtin') : t('roles.custom') }}</span>
              </td>
              <td :data-label="t('roles.col.perms')">{{ r.permissions?.length ?? 0 }}</td>
              <td :data-label="t('users.col.actions')">
                <a-button size="small" :type="r.uid === selectedUid ? 'primary' : 'secondary'" data-testid="role-select" @click="select(r)">{{ t('roles.select') }}</a-button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="selected" class="le-card" data-testid="role-detail" :data-code="selected.code">
        <h2 class="le-card__title">{{ t('roles.detail', { name: selected.name }) }}</h2>
        <p v-if="selected.is_builtin" class="le-notice" data-testid="role-builtin-note">{{ t('roles.builtinNote') }}</p>
        <a-form v-else-if="canWrite" :model="meta" :rules="metaRules" layout="vertical" auto-label-width @submit-success="saveMeta">
          <div class="le-form-grid">
            <a-form-item field="name" :label="t('roles.col.name')" validate-trigger="blur">
              <a-input v-model="meta.name" :input-attrs="{ 'data-testid': 'role-name' }" />
            </a-form-item>
            <a-form-item field="description" :label="t('roles.col.description')" validate-trigger="blur">
              <a-input v-model="meta.description" :input-attrs="{ 'data-testid': 'role-description' }" />
            </a-form-item>
          </div>
          <div v-if="rename.error.value" class="le-dialog__error" role="alert" data-testid="role-meta-error">{{ errorText(t, te, rename.error.value) }}</div>
          <div class="le-page__actions">
            <a-button type="primary" html-type="submit" :loading="rename.busy.value" data-testid="role-meta-save">{{ t('roles.saveMeta') }}</a-button>
            <a-button status="danger" data-testid="role-delete" @click="askDelete">{{ t('roles.delete.open') }}</a-button>
          </div>
        </a-form>

        <h3 class="le-card__title">{{ t('roles.matrix.title') }}</h3>
        <label class="le-field">
          <span>{{ t('roles.matrix.filter') }}</span>
          <input v-model="filter" type="search" autocomplete="off" data-testid="perm-filter" />
        </label>
        <div class="le-matrix">
          <table class="le-table" data-testid="perm-matrix">
            <thead>
              <tr>
                <th scope="col">{{ t('roles.matrix.resource') }}</th>
                <th v-for="a in matrix.actions" :key="a" scope="col">{{ a }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in matrix.rows" :key="row.resource">
                <th scope="row" class="le-mono">{{ row.resource }}</th>
                <td v-for="a in matrix.actions" :key="a">
                  <template v-if="row.cells.get(a)">
                    <input
                      v-if="row.allowed(row.cells.get(a) as Permission)"
                      type="checkbox"
                      :checked="picked.has((row.cells.get(a) as Permission).code)"
                      :disabled="readOnly"
                      :aria-label="(row.cells.get(a) as Permission).code"
                      :data-testid="`perm-${(row.cells.get(a) as Permission).code}`"
                      @change="toggle((row.cells.get(a) as Permission).code, ($event.target as HTMLInputElement).checked)"
                    />
                    <span v-else :title="t('roles.matrix.notApplicable')">—</span>
                  </template>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-if="permsError" class="le-dialog__error" role="alert" data-testid="role-perms-error">{{ permsError }}</div>
        <div v-if="!readOnly" class="le-page__actions">
          <a-button type="primary" :disabled="!dirty" :loading="save.busy.value" data-testid="role-perms-save" @click="savePerms">{{ t('roles.matrix.save') }}</a-button>
          <span class="le-dialog__note">{{ t('roles.matrix.reauthHint') }}</span>
        </div>
      </div>
      <p v-else class="le-page__desc" data-testid="role-none-selected">{{ t('roles.noneSelected') }}</p>
    </LeStateBoundary>

    <a-modal v-model:visible="createOpen" unmount-on-close :footer="false" :mask-closable="false" :title="t('roles.create.title')">
      <a-form :model="form" :rules="createRules" layout="vertical" auto-label-width data-testid="role-create-form" @submit-success="submitCreate">
        <a-form-item field="scope" :label="t('roles.col.scope')">
          <a-radio-group v-model="form.scope">
            <a-radio value="platform">{{ t('roles.scope.platform') }}</a-radio>
            <a-radio value="tenant">{{ t('roles.scope.tenant') }}</a-radio>
          </a-radio-group>
        </a-form-item>
        <a-form-item field="code" :label="t('roles.col.code')" validate-trigger="blur" :extra="t('roles.create.codeHint')">
          <a-input v-model="form.code" :input-attrs="{ 'data-testid': 'create-role-code', autocomplete: 'off' }" />
        </a-form-item>
        <a-form-item field="name" :label="t('roles.col.name')" validate-trigger="blur">
          <a-input v-model="form.name" :input-attrs="{ 'data-testid': 'create-role-name', autocomplete: 'off' }" />
        </a-form-item>
        <a-form-item field="description" :label="t('roles.col.description')" validate-trigger="blur">
          <a-input v-model="form.description" :input-attrs="{ 'data-testid': 'create-role-description', autocomplete: 'off' }" />
        </a-form-item>
        <p class="le-dialog__note">{{ t('roles.create.reauthHint') }}</p>
        <div v-if="create.error.value" class="le-dialog__error" role="alert" data-testid="role-create-error">{{ errorText(t, te, create.error.value) }}</div>
        <div class="le-dialog__actions">
          <a-button @click="createOpen = false">{{ t('common.cancel') }}</a-button>
          <a-button type="primary" html-type="submit" :loading="create.busy.value" data-testid="role-create-submit">{{ t('roles.create.submit') }}</a-button>
        </div>
        <p class="le-dialog__audit">{{ t('danger.audit') }}</p>
      </a-form>
    </a-modal>

    <DangerDialog
      v-model:visible="delOpen"
      level="L3"
      reauth
      :title="t('roles.delete.title', { name: selected?.name ?? '' })"
      :impact="t('roles.delete.impact', { code: selected?.code ?? '' })"
      :confirm-word="selected?.code"
      :ok-text="t('roles.delete.ok')"
      :busy="del.busy.value"
      :error="del.error.value"
      @confirm="confirmDelete"
    />
  </section>
</template>
