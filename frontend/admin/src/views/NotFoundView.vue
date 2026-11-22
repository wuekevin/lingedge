<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import { ROUTE } from '../router/routes';

// 404：路径不存在，或页面所属模块未交付/运行模式不允许（避免暗示「存在但无权」，ia.md §4.4）。
const { t } = useI18n();
</script>

<template>
  <section class="le-status-page" data-testid="not-found-page">
    <p class="le-status-page__code" aria-hidden="true">404</p>
    <h1 class="le-status-page__title">{{ t('notFound.title') }}</h1>
    <p>{{ t('notFound.description') }}</p>
    <router-link :to="{ name: ROUTE.home }" class="le-btn-link">{{ t('notFound.back') }}</router-link>
  </section>
</template>
