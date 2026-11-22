<script setup lang="ts">
import { Message } from '@arco-design/web-vue';
import { LeStateBoundary, formatDateTime, formatUtc } from '@lingedge/shared';
import { computed, onMounted, reactive, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { newIdempotencyKey } from '../../api/admin';
import { useAdminApi } from '../../api/context';
import { isApiError } from '../../api/errors';
import { errorText } from '../../api/messages';
import type { ApiToken } from '../../api/types';
import DangerDialog from '../../components/DangerDialog.vue';
import PagerBar from '../../components/PagerBar.vue';
import SecretDialog from '../../components/SecretDialog.vue';
import { useAction } from '../../composables/useAction';
import { useAsyncState } from '../../composables/useAsyncState';
import { localToIso } from '../../composables/useDateRange';
import { useFormRules } from '../../composables/useFormRules';
import { useRefreshOnActivate } from '../../composables/useRefreshOnActivate';
import { useSessionStore } from '../../stores/session';

// 个人 API Token（ia.md §1.11；FR-25）：创建时明文只在创建响应里出现一次（可复制，关闭后无法再看到，库内仅存哈希）；
// scopes 必须是创建者权限的子集——候选项取自当前身份的权限集合，服务端仍是最终裁决。轮换 = 生成新令牌并吊销旧令牌；吊销立即失效。
const { t, te, locale } = useI18n();
const api = useAdminApi();
const session = useSessionStore();
const rules = useFormRules();

const page = ref(1);
const PAGE_SIZE = 20;
const s = useAsyncState(
  (signal) => api.listApiTokens({ page: page.value, page_size: PAGE_SIZE, sort: '-created_at' }, signal),
  (d) => d.items.length === 0,
);
onMounted(() => void s.run());
useRefreshOnActivate(() => void s.run());
function goto(p: number): void {
  page.value = p;
  void s.run();
}
const when = (v: string | null | undefined): string => formatDateTime(v, { locale: String(locale.value) });
const toast = (key: string): void => void Message.success(t(key));

// ---- 一次性明文 ----
const secretOpen = ref(false);
const secretText = ref('');
const secretTitleKey = ref('tokens.once.createdTitle');
function showSecret(titleKey: string, value: string): void {
  secretTitleKey.value = titleKey;
  secretText.value = value;
  secretOpen.value = true;
}
const dropSecret = (): void => {
  secretText.value = '';
};

// ---- 创建 ----
const createOpen = ref(false);
const form = reactive({ name: '', cidrs: '', expires: '', scopes: [] as string[] });
const scopeFilter = ref('');
const create = useAction();
let createKey = '';
const candidates = computed(() => {
  const f = scopeFilter.value.trim().toLowerCase();
  return [...(session.me?.permissions ?? [])].filter((p) => !f || p.includes(f)).sort();
});
const createRules = computed(() => ({ name: [rules.required(t('tokens.col.name')), rules.maxLength(t('tokens.col.name'), 128)] }));
const scopeProblem = ref(false);
const expiresProblem = ref(false);
function openCreate(): void {
  Object.assign(form, { name: '', cidrs: '', expires: '', scopes: [] });
  scopeFilter.value = '';
  scopeProblem.value = false;
  expiresProblem.value = false;
  create.reset();
  createKey = newIdempotencyKey();
  createOpen.value = true;
}
async function submitCreate(): Promise<void> {
  scopeProblem.value = form.scopes.length === 0;
  const exp = localToIso(form.expires);
  expiresProblem.value = exp === null || (typeof exp === 'string' && exp <= new Date().toISOString());
  if (scopeProblem.value || expiresProblem.value) return;
  const cidrs = form.cidrs
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);
  const r = await create.run(() =>
    api.createApiToken({ name: form.name.trim(), scopes: [...form.scopes].sort(), ...(cidrs.length > 0 ? { allowed_cidrs: cidrs } : {}), ...(exp ? { expires_at: exp } : {}) }, createKey),
  );
  if (!r.ok) return;
  createOpen.value = false;
  showSecret('tokens.once.createdTitle', r.value.token);
  void s.run();
}

// ---- 轮换 / 吊销（L2：影响范围 + 审计提示）----
type Ask = { kind: 'rotate' | 'revoke'; token: ApiToken };
const ask = ref<Ask | null>(null);
const askOpen = computed({ get: () => ask.value !== null, set: (v: boolean) => (v ? undefined : (ask.value = null)) });
const act = useAction();
function openAsk(kind: Ask['kind'], token: ApiToken): void {
  act.reset();
  ask.value = { kind, token };
}
async function confirmAsk(): Promise<void> {
  const a = ask.value;
  if (!a) return;
  const r = await act.run(async () => {
    if (a.kind === 'rotate') return api.rotateApiToken(a.token.uid);
    await api.revokeApiToken(a.token.uid);
    return undefined;
  });
  if (!r.ok) return;
  ask.value = null;
  void s.run();
  if (a.kind === 'rotate' && r.value) showSecret('tokens.once.rotatedTitle', r.value.token);
  else toast('tokens.toast.revoked');
}
const askTitle = computed(() => (ask.value ? t(`tokens.danger.${ask.value.kind}.title`, { name: ask.value.token.name }) : ''));
const askImpact = computed(() => (ask.value ? t(`tokens.danger.${ask.value.kind}.impact`, { name: ask.value.token.name }) : ''));
const askOk = computed(() => (ask.value ? t(`tokens.danger.${ask.value.kind}.ok`) : ''));
const tokenState = (tk: ApiToken): 'revoked' | 'expired' | 'active' => (tk.revoked_at ? 'revoked' : tk.expires_at && new Date(tk.expires_at).getTime() <= Date.now() ? 'expired' : 'active');
</script>

<template>
  <section class="le-page" data-testid="tokens-page">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.usersTokens') }}</h1>
      <div class="le-page__actions">
        <a-button type="primary" data-testid="token-create" @click="openCreate">{{ t('tokens.create.open') }}</a-button>
      </div>
    </header>
    <p class="le-page__desc">{{ t('tokens.scope') }}</p>
    <LeStateBoundary :status="s.status.value" :error-kind="s.errorKind.value" :request-id="s.requestId.value" @retry="s.run">
      <div class="le-table-wrap">
        <table class="le-table le-table--stack" data-testid="tokens-table">
          <thead>
            <tr>
              <th scope="col">{{ t('tokens.col.name') }}</th>
              <th scope="col">{{ t('tokens.col.prefix') }}</th>
              <th scope="col">{{ t('tokens.col.scopes') }}</th>
              <th scope="col">{{ t('tokens.col.status') }}</th>
              <th scope="col">{{ t('tokens.col.expires') }}</th>
              <th scope="col">{{ t('tokens.col.lastUsed') }}</th>
              <th scope="col">{{ t('users.col.actions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="tk in s.data.value?.items ?? []" :key="tk.uid" data-testid="token-row" :data-name="tk.name" :data-state="tokenState(tk)">
              <td :data-label="t('tokens.col.name')">{{ tk.name }}</td>
              <td :data-label="t('tokens.col.prefix')" class="le-mono" data-testid="token-prefix">{{ tk.token_prefix }}</td>
              <td :data-label="t('tokens.col.scopes')" data-testid="token-scopes">{{ tk.scopes.join(', ') }}</td>
              <td :data-label="t('tokens.col.status')">
                <span class="le-chip" :class="tokenState(tk) === 'active' ? 'le-chip--ok' : 'le-chip--muted'">{{ t(`tokens.state.${tokenState(tk)}`) }}</span>
              </td>
              <td :data-label="t('tokens.col.expires')" :title="formatUtc(tk.expires_at)">{{ tk.expires_at ? when(tk.expires_at) : t('tokens.noExpiry') }}</td>
              <td :data-label="t('tokens.col.lastUsed')">{{ tk.last_used_at ? when(tk.last_used_at) : t('tokens.neverUsed') }}</td>
              <td :data-label="t('users.col.actions')">
                <div v-if="tokenState(tk) === 'active'" class="le-table__actions">
                  <a-button size="small" data-testid="token-rotate" @click="openAsk('rotate', tk)">{{ t('tokens.action.rotate') }}</a-button>
                  <a-button size="small" status="danger" data-testid="token-revoke" @click="openAsk('revoke', tk)">{{ t('tokens.action.revoke') }}</a-button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <template #empty>
        <div class="le-card" data-testid="tokens-empty">
          <p class="le-card__title">{{ t('tokens.empty.title') }}</p>
          <p class="le-page__desc">{{ t('tokens.empty.description') }}</p>
        </div>
      </template>
    </LeStateBoundary>
    <PagerBar v-if="s.data.value && s.data.value.total > PAGE_SIZE" :page="page" :page-size="PAGE_SIZE" :total="s.data.value.total" @change="goto" />

    <a-modal v-model:visible="createOpen" unmount-on-close :footer="false" :mask-closable="false" :title="t('tokens.create.title')">
      <a-form :model="form" :rules="createRules" layout="vertical" auto-label-width data-testid="token-create-form" @submit-success="submitCreate">
        <a-form-item field="name" :label="t('tokens.col.name')" validate-trigger="blur">
          <a-input v-model="form.name" :input-attrs="{ 'data-testid': 'token-name', autocomplete: 'off' }" />
        </a-form-item>
        <fieldset class="le-fieldset">
          <legend class="le-dialog__label">{{ t('tokens.create.scopes') }}</legend>
          <p class="le-dialog__note">{{ t('tokens.create.scopesHint') }}</p>
          <label class="le-field">
            <span>{{ t('tokens.create.scopeFilter') }}</span>
            <input v-model="scopeFilter" type="search" autocomplete="off" data-testid="token-scope-filter" />
          </label>
          <div class="le-scope-list" data-testid="token-scope-list">
            <label v-for="p in candidates" :key="p" class="le-check">
              <input v-model="form.scopes" type="checkbox" :value="p" :data-testid="`scope-${p}`" />
              <span class="le-mono">{{ p }}</span>
            </label>
          </div>
          <span v-if="scopeProblem" class="le-field__error" role="alert" data-testid="token-scope-problem">{{ t('tokens.create.scopesRequired') }}</span>
        </fieldset>
        <label class="le-field">
          <span>{{ t('tokens.create.cidrs') }}</span>
          <textarea v-model="form.cidrs" rows="2" data-testid="token-cidrs" />
        </label>
        <label class="le-field">
          <span>{{ t('tokens.col.expires') }}</span>
          <input v-model="form.expires" type="datetime-local" data-testid="token-expires" />
          <span class="le-dialog__note">{{ t('tokens.create.expiresHint') }}</span>
          <span v-if="expiresProblem" class="le-field__error" role="alert" data-testid="token-expires-problem">{{ t('tokens.create.expiresInvalid') }}</span>
        </label>
        <div v-if="create.error.value" class="le-dialog__error" role="alert" data-testid="token-create-error">
          <span>{{ errorText(t, te, create.error.value) }}</span>
          <span v-if="isApiError(create.error.value)">{{ (create.error.value.details ?? []).map((d) => d.field).filter(Boolean).join(', ') }}</span>
        </div>
        <div class="le-dialog__actions">
          <a-button @click="createOpen = false">{{ t('common.cancel') }}</a-button>
          <a-button type="primary" html-type="submit" :loading="create.busy.value" data-testid="token-create-submit">{{ t('tokens.create.submit') }}</a-button>
        </div>
        <p class="le-dialog__audit">{{ t('danger.audit') }}</p>
      </a-form>
    </a-modal>

    <DangerDialog v-model:visible="askOpen" level="L2" :title="askTitle" :impact="askImpact" :ok-text="askOk" :busy="act.busy.value" :error="act.error.value" @confirm="confirmAsk" />
    <SecretDialog v-model:visible="secretOpen" :title="t(secretTitleKey)" :description="t('tokens.once.description')" :label="t('tokens.once.label')" :value="secretText" @closed="dropSecret" />
  </section>
</template>

<style scoped>
.le-fieldset {
  margin: 0 0 var(--le-space-3);
  padding: 0;
  border: 0;
  display: grid;
  gap: var(--le-space-2);
}
.le-scope-list {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
  gap: var(--le-space-1) var(--le-space-3);
  max-height: 200px;
  overflow: auto;
  padding: var(--le-space-2);
  border: 1px solid var(--le-color-border-default);
  border-radius: var(--le-radius-sm);
}
</style>
