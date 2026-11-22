<script setup lang="ts">
import { LeStateBoundary, formatDateTime, formatUtc } from '@lingedge/shared';
import { computed, onMounted, reactive, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute } from 'vue-router';
import type { AuditFilter } from '../../api/admin';
import { useAdminApi } from '../../api/context';
import type { AuditLog } from '../../api/types';
import CursorBar from '../../components/CursorBar.vue';
import { useCursorList } from '../../composables/useCursorList';
import { localToIso, rangeProblem } from '../../composables/useDateRange';
import { useRefreshOnActivate } from '../../composables/useRefreshOnActivate';

// 审计日志（ia.md §1.15 / §1.11；GET /audit-logs，游标分页 + 过滤）。规范页面只有这一个（UX-3）：
// 「用户管理 → 操作审计」是别名入口，共用本组件，仅预置「用户操作」筛选（resource_type=user，可清除）。
const { t, locale } = useI18n();
const route = useRoute();
const api = useAdminApi();
const RESULTS = ['success', 'failure', 'denied'] as const;

const preset: { resource_type?: string } = route.meta.pageKey === 'users.audit' ? { resource_type: 'user' } : {};
const draft = reactive({ action: '', resource_type: preset.resource_type ?? '', resource_uid: '', actor_uid: '', result: '', request_id: '', from: '', to: '' });
type Applied = Omit<AuditFilter, 'cursor' | 'limit'>;
const applied = ref<Applied>({ ...preset });
const problem = computed(() => rangeProblem(draft.from, draft.to));
const filtered = computed(() => Object.keys(applied.value).length > 0);
const detail = ref<AuditLog | null>(null);
const detailOpen = computed({ get: () => detail.value !== null, set: (v: boolean) => (v ? undefined : (detail.value = null)) });

const list = useCursorList((cursor, limit, signal) => api.listAuditLogs({ ...applied.value, limit, ...(cursor ? { cursor } : {}) }, signal));

function apply(): void {
  if (problem.value) return;
  const from = localToIso(draft.from);
  const to = localToIso(draft.to);
  const a: Applied = {};
  for (const k of ['action', 'resource_type', 'resource_uid', 'actor_uid', 'request_id'] as const) if (draft[k].trim()) a[k] = draft[k].trim();
  if (draft.result) a.result = draft.result;
  if (from) a.from = from;
  if (to) a.to = to;
  applied.value = a;
  void list.reload();
}
function clear(): void {
  Object.assign(draft, { action: '', resource_type: '', resource_uid: '', actor_uid: '', result: '', request_id: '', from: '', to: '' });
  applied.value = {};
  void list.reload();
}
onMounted(() => void list.run());
useRefreshOnActivate(() => void list.run());
const when = (v: string): string => formatDateTime(v, { locale: String(locale.value) });
const chip = (r: string): string => (r === 'success' ? 'le-chip--ok' : r === 'denied' ? 'le-chip--warn' : 'le-chip--danger');
const changesText = computed(() => (detail.value?.changes ? JSON.stringify(detail.value.changes, null, 2) : ''));
</script>

<template>
  <section class="le-page" data-testid="audit-page" :data-preset="preset.resource_type ?? ''">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.opsAudit') }}</h1>
    </header>
    <p v-if="preset.resource_type" class="le-notice" data-testid="audit-alias-note">{{ t('audit.aliasNote') }}</p>
    <form class="le-card le-filters" :aria-label="t('filters.label')" data-testid="audit-filters" @submit.prevent="apply">
      <label class="le-field">
        <span>{{ t('audit.col.action') }}</span>
        <input v-model="draft.action" type="text" autocomplete="off" data-testid="filter-action" />
      </label>
      <label class="le-field">
        <span>{{ t('audit.filter.resourceType') }}</span>
        <input v-model="draft.resource_type" type="text" autocomplete="off" data-testid="filter-resource-type" />
      </label>
      <label class="le-field">
        <span>{{ t('audit.filter.resourceUid') }}</span>
        <input v-model="draft.resource_uid" type="text" autocomplete="off" data-testid="filter-resource-uid" />
      </label>
      <label class="le-field">
        <span>{{ t('audit.filter.actorUid') }}</span>
        <input v-model="draft.actor_uid" type="text" autocomplete="off" data-testid="filter-actor-uid" />
      </label>
      <label class="le-field">
        <span>{{ t('audit.col.result') }}</span>
        <select v-model="draft.result" data-testid="filter-result">
          <option value="">{{ t('filters.any') }}</option>
          <option v-for="r in RESULTS" :key="r" :value="r">{{ t(`audit.result.${r}`) }}</option>
        </select>
      </label>
      <label class="le-field">
        <span>{{ t('audit.col.requestId') }}</span>
        <input v-model="draft.request_id" type="text" autocomplete="off" data-testid="filter-request-id" />
      </label>
      <label class="le-field">
        <span>{{ t('filters.from') }}</span>
        <input v-model="draft.from" type="datetime-local" data-testid="filter-from" />
      </label>
      <label class="le-field">
        <span>{{ t('filters.to') }}</span>
        <input v-model="draft.to" type="datetime-local" data-testid="filter-to" />
      </label>
      <div class="le-page__actions">
        <a-button type="primary" html-type="submit" :disabled="problem !== null" data-testid="filter-apply">{{ t('filters.apply') }}</a-button>
        <a-button data-testid="filter-clear" @click="clear">{{ t('filters.clear') }}</a-button>
      </div>
      <p v-if="problem" class="le-field__error" role="alert" data-testid="filter-problem">{{ t(`filters.problem.${problem}`) }}</p>
    </form>
    <LeStateBoundary
      :status="list.status.value"
      :error-kind="list.errorKind.value"
      :request-id="list.requestId.value"
      :empty-reason="filtered ? 'no-results' : 'no-data'"
      @retry="list.run"
      @clear-filters="clear"
    >
      <div class="le-table-wrap">
        <table class="le-table le-table--stack" data-testid="audit-table">
          <thead>
            <tr>
              <th scope="col">{{ t('audit.col.time') }}</th>
              <th scope="col">{{ t('audit.col.actor') }}</th>
              <th scope="col">{{ t('audit.col.action') }}</th>
              <th scope="col">{{ t('audit.col.resource') }}</th>
              <th scope="col">{{ t('audit.col.result') }}</th>
              <th scope="col">{{ t('audit.col.requestId') }}</th>
              <th scope="col"><span class="le-sr-only">{{ t('audit.col.detail') }}</span></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in list.data.value?.items ?? []" :key="r.uid" data-testid="audit-row" :data-action="r.action">
              <td :data-label="t('audit.col.time')" :title="formatUtc(r.created_at)">{{ when(r.created_at) }}</td>
              <td :data-label="t('audit.col.actor')">{{ r.actor_label }}</td>
              <td :data-label="t('audit.col.action')" class="le-mono">{{ r.action }}</td>
              <td :data-label="t('audit.col.resource')">
                <div>
                  <div class="le-mono" data-testid="audit-resource-type">{{ r.resource_type }}</div>
                  <div v-if="r.resource_uid" class="le-mono">{{ r.resource_uid }}</div>
                </div>
              </td>
              <td :data-label="t('audit.col.result')">
                <span class="le-chip" :class="chip(r.result)">{{ t(`audit.result.${r.result}`) }}</span>
              </td>
              <td :data-label="t('audit.col.requestId')" class="le-mono">{{ r.request_id }}</td>
              <td :data-label="t('audit.col.detail')">
                <a-button size="small" data-testid="audit-detail" @click="detail = r">{{ t('audit.detail') }}</a-button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </LeStateBoundary>
    <CursorBar
      v-if="list.status.value === 'ready' || list.hasPrev.value"
      :page-no="list.pageNo.value"
      :has-prev="list.hasPrev.value"
      :has-next="list.hasNext.value"
      :limit="list.limit.value"
      :busy="list.status.value === 'loading'"
      @prev="list.goPrev"
      @next="list.goNext"
      @update:limit="list.setLimit"
    />
    <a-drawer v-model:visible="detailOpen" :footer="false" :title="t('audit.detail')" width="min(480px, 100vw)" unmount-on-close>
      <dl v-if="detail" class="le-kv" data-testid="audit-detail-panel">
        <dt>{{ t('audit.col.time') }}</dt>
        <dd>{{ when(detail.created_at) }}</dd>
        <dt>{{ t('audit.col.actor') }}</dt>
        <dd>{{ detail.actor_label }} ({{ t(`audit.actorType.${detail.actor_type}`) }})</dd>
        <dt>{{ t('audit.col.action') }}</dt>
        <dd class="le-mono">{{ detail.action }}</dd>
        <dt>{{ t('audit.col.resource') }}</dt>
        <dd class="le-mono">{{ detail.resource_type }} {{ detail.resource_uid ?? '' }}</dd>
        <dt>{{ t('audit.col.result') }}</dt>
        <dd>{{ t(`audit.result.${detail.result}`) }}</dd>
        <dt>{{ t('audit.col.requestId') }}</dt>
        <dd class="le-mono">{{ detail.request_id }}</dd>
        <dt>{{ t('audit.detailIp') }}</dt>
        <dd class="le-mono">{{ detail.ip ?? '—' }}</dd>
        <dt>{{ t('audit.detailChanges') }}</dt>
        <dd>
          <pre v-if="changesText" class="le-secret" data-testid="audit-changes">{{ changesText }}</pre>
          <span v-else>{{ t('audit.noChanges') }}</span>
        </dd>
      </dl>
    </a-drawer>
  </section>
</template>
