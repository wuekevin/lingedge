<script setup lang="ts">
import { LeEmpty } from '@lingedge/shared';
import { computed, inject } from 'vue';
import { useI18n } from 'vue-i18n';
import { REGISTRY_KEY } from '../layout/keys';
import { buildMenu } from '../registry/visibility';
import { useSessionStore } from '../stores/session';

// 外壳首页：只展示真实来自 /auth/me 的事实（身份、运行模式、角色、已交付模块、可访问页面数）；没有任何示例数字或占位卡片。
const { t } = useI18n();
const session = useSessionStore();
const registry = inject(REGISTRY_KEY);
const groups = computed(() => (session.access && registry ? buildMenu(registry.pages, session.access) : []));
const pageCount = computed(() => groups.value.reduce((n, g) => n + g.children.length, 0));
const modules = computed(() => [...(session.access?.modules ?? [])].sort());
</script>

<template>
  <section class="le-home" data-testid="home-page">
    <h1 class="le-home__title">{{ t('home.welcome', { name: session.user?.display_name ?? '' }) }}</h1>
    <dl class="le-home__facts">
      <div>
        <dt>{{ t('home.runMode') }}</dt>
        <dd data-testid="home-run-mode">{{ session.access ? t(`runMode.${session.access.runMode}`) : t('home.none') }}</dd>
      </div>
      <div>
        <dt>{{ t('home.roles') }}</dt>
        <dd data-testid="home-roles">{{ session.me?.roles.join(', ') || t('home.none') }}</dd>
      </div>
      <div>
        <dt>{{ t('home.modules') }}</dt>
        <dd data-testid="home-modules">{{ modules.join(', ') || t('home.none') }}</dd>
      </div>
      <div>
        <dt>{{ t('home.pages') }}</dt>
        <dd data-testid="home-pages">{{ pageCount }}</dd>
      </div>
    </dl>
    <LeEmpty v-if="pageCount === 0" :title="t('home.empty.title')" :description="t('home.empty.description')" />
  </section>
</template>

<style scoped>
.le-home__title {
  margin: 0 0 var(--le-space-4);
  font-size: var(--le-text-page-title-size);
  font-weight: var(--le-text-page-title-weight);
  color: var(--le-color-text-primary);
}
.le-home__facts {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: var(--le-space-4);
  margin: 0 0 var(--le-space-6);
}
.le-home__facts > div {
  padding: var(--le-space-4);
  background: var(--le-color-bg-surface);
  border: 1px solid var(--le-color-border-default);
  border-radius: var(--le-radius-md);
}
dt {
  color: var(--le-color-text-secondary);
  font-size: var(--le-text-caption-size);
}
dd {
  margin: var(--le-space-1) 0 0;
  color: var(--le-color-text-primary);
  overflow-wrap: anywhere;
}
</style>
