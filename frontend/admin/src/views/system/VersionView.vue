<script setup lang="ts">
import { LeStateBoundary, formatDateTime } from '@lingedge/shared';
import { onMounted } from 'vue';
import { useI18n } from 'vue-i18n';
import { useAdminApi } from '../../api/context';
import { useAsyncState } from '../../composables/useAsyncState';
import { useRefreshOnActivate } from '../../composables/useRefreshOnActivate';

// 版本信息（ia.md §1.17；FR-36）：真实 Server 版本、API 版本、构建、安装 ID、迁移版本与已安装组件——全部来自 GET /admin/system/version，没有任何前端写死的版本号。
const { t, locale } = useI18n();
const api = useAdminApi();
const s = useAsyncState((signal) => api.getVersion(signal));
onMounted(() => void s.run());
useRefreshOnActivate(() => void s.run());
const when = (v: string | null | undefined): string => formatDateTime(v, { locale: String(locale.value) });
</script>

<template>
  <section class="le-page" data-testid="version-page">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.settingsVersion') }}</h1>
    </header>
    <LeStateBoundary :status="s.status.value" :error-kind="s.errorKind.value" :request-id="s.requestId.value" @retry="s.run">
      <template v-if="s.data.value">
        <div class="le-card">
          <h2 class="le-card__title">{{ t('version.core') }}</h2>
          <dl class="le-kv">
            <dt>{{ t('version.server') }}</dt>
            <dd class="le-mono" data-testid="version-server">{{ s.data.value.server_version }}</dd>
            <dt>{{ t('version.api') }}</dt>
            <dd class="le-mono" data-testid="version-api">{{ s.data.value.api_version }}</dd>
            <dt>{{ t('version.build') }}</dt>
            <dd class="le-mono" data-testid="version-build">{{ s.data.value.build ?? t('version.none') }}</dd>
            <dt>{{ t('version.installation') }}</dt>
            <dd class="le-mono" data-testid="version-installation">{{ s.data.value.installation_id ?? t('version.none') }}</dd>
            <dt>{{ t('version.migration') }}</dt>
            <dd class="le-mono" data-testid="version-migration">{{ s.data.value.schema_migration?.current ?? t('version.none') }}</dd>
            <dt>{{ t('version.pending') }}</dt>
            <dd data-testid="version-pending">{{ s.data.value.schema_migration?.pending ?? t('version.none') }}</dd>
          </dl>
        </div>
        <div class="le-card">
          <h2 class="le-card__title">{{ t('version.components') }}</h2>
          <p v-if="s.data.value.components.length === 0" class="le-page__desc" data-testid="version-no-components">{{ t('version.noComponents') }}</p>
          <div v-else class="le-table-wrap">
            <table class="le-table le-table--stack" data-testid="version-components">
              <thead>
                <tr>
                  <th scope="col">{{ t('version.col.component') }}</th>
                  <th scope="col">{{ t('version.col.instance') }}</th>
                  <th scope="col">{{ t('version.col.host') }}</th>
                  <th scope="col">{{ t('version.col.version') }}</th>
                  <th scope="col">{{ t('version.col.status') }}</th>
                  <th scope="col">{{ t('version.col.installedAt') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="c in s.data.value.components" :key="`${c.component}/${c.host}/${c.instance_name ?? ''}`">
                  <td :data-label="t('version.col.component')">{{ c.component }}</td>
                  <td :data-label="t('version.col.instance')">{{ c.instance_name ?? t('version.none') }}</td>
                  <td :data-label="t('version.col.host')" class="le-mono">{{ c.host }}</td>
                  <td :data-label="t('version.col.version')" class="le-mono">{{ c.version }}</td>
                  <td :data-label="t('version.col.status')">{{ t(`version.status.${c.status}`) }}</td>
                  <td :data-label="t('version.col.installedAt')">{{ when(c.installed_at) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </template>
    </LeStateBoundary>
  </section>
</template>
