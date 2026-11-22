<script setup lang="ts">
import { LeStateBoundary, formatDateTime, formatUtc } from '@lingedge/shared';
import { computed, onMounted, reactive, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import type { LoginLogFilter } from '../../api/admin';
import { useAdminApi } from '../../api/context';
import CursorBar from '../../components/CursorBar.vue';
import { useCursorList } from '../../composables/useCursorList';
import { localToIso, rangeProblem } from '../../composables/useDateRange';
import { useRefreshOnActivate } from '../../composables/useRefreshOnActivate';

// 登录日志（ia.md §1.11；GET /admin/login-logs，游标分页 + 过滤）。
const { t, locale } = useI18n();
const api = useAdminApi();
const EVENTS = ['login_success', 'login_failed', 'logout', 'totp_failed', 'locked', 'token_auth_failed'] as const;

const draft = reactive({ event: '', identifier: '', ip: '', from: '', to: '' });
const applied = ref<Omit<LoginLogFilter, 'cursor' | 'limit'>>({});
const problem = computed(() => rangeProblem(draft.from, draft.to));

const list = useCursorList((cursor, limit, signal) => api.listLoginLogs({ ...applied.value, limit, ...(cursor ? { cursor } : {}) }, signal));
const filtered = computed(() => Object.keys(applied.value).length > 0);

function apply(): void {
  if (problem.value) return;
  const from = localToIso(draft.from);
  const to = localToIso(draft.to);
  applied.value = {
    ...(draft.event ? { event: draft.event } : {}),
    ...(draft.identifier.trim() ? { identifier: draft.identifier.trim() } : {}),
    ...(draft.ip.trim() ? { ip: draft.ip.trim() } : {}),
    ...(from ? { from } : {}),
    ...(to ? { to } : {}),
  };
  void list.reload();
}
function clear(): void {
  Object.assign(draft, { event: '', identifier: '', ip: '', from: '', to: '' });
  applied.value = {};
  void list.reload();
}
onMounted(() => void list.run());
useRefreshOnActivate(() => void list.run());
const when = (v: string): string => formatDateTime(v, { locale: String(locale.value) });
const chip = (e: string): string => (e === 'login_success' || e === 'logout' ? 'le-chip--ok' : e === 'locked' ? 'le-chip--warn' : 'le-chip--danger');
</script>

<template>
  <section class="le-page" data-testid="login-logs-page">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.usersLoginLogs') }}</h1>
    </header>
    <form class="le-card le-filters" :aria-label="t('filters.label')" data-testid="login-filters" @submit.prevent="apply">
      <label class="le-field">
        <span>{{ t('loginLogs.col.event') }}</span>
        <select v-model="draft.event" data-testid="filter-event">
          <option value="">{{ t('filters.any') }}</option>
          <option v-for="e in EVENTS" :key="e" :value="e">{{ t(`loginLogs.event.${e}`) }}</option>
        </select>
      </label>
      <label class="le-field">
        <span>{{ t('loginLogs.col.identifier') }}</span>
        <input v-model="draft.identifier" type="text" autocomplete="off" data-testid="filter-identifier" />
      </label>
      <label class="le-field">
        <span>{{ t('loginLogs.col.ip') }}</span>
        <input v-model="draft.ip" type="text" autocomplete="off" data-testid="filter-ip" />
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
        <table class="le-table le-table--stack" data-testid="login-logs-table">
          <thead>
            <tr>
              <th scope="col">{{ t('loginLogs.col.time') }}</th>
              <th scope="col">{{ t('loginLogs.col.event') }}</th>
              <th scope="col">{{ t('loginLogs.col.identifier') }}</th>
              <th scope="col">{{ t('loginLogs.col.realm') }}</th>
              <th scope="col">{{ t('loginLogs.col.ip') }}</th>
              <th scope="col">{{ t('loginLogs.col.reason') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in list.data.value?.items ?? []" :key="r.uid" data-testid="login-log-row" :data-event="r.event">
              <td :data-label="t('loginLogs.col.time')" :title="formatUtc(r.created_at)">{{ when(r.created_at) }}</td>
              <td :data-label="t('loginLogs.col.event')">
                <span class="le-chip" :class="chip(r.event)">{{ t(`loginLogs.event.${r.event}`) }}</span>
              </td>
              <td :data-label="t('loginLogs.col.identifier')">{{ r.identifier }}</td>
              <td :data-label="t('loginLogs.col.realm')">{{ r.realm }}</td>
              <td :data-label="t('loginLogs.col.ip')" class="le-mono">{{ r.ip ?? '—' }}</td>
              <td :data-label="t('loginLogs.col.reason')">{{ r.reason ?? '—' }}</td>
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
  </section>
</template>
