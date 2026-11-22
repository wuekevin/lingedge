<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute } from 'vue-router';
import { ROUTE } from '../router/routes';

// 403：统一「无权限」页（ia.md §4.4）：显示所需权限点与「联系管理员」，并提供返回首页；不渲染半成品页面再被接口 403 打断。
const { t } = useI18n();
const route = useRoute();
const need = computed(() => (typeof route.query.need === 'string' ? route.query.need.split(',').filter(Boolean) : []));
</script>

<template>
  <section class="le-status-page" role="alert" data-testid="forbidden-page">
    <p class="le-status-page__code" aria-hidden="true">403</p>
    <h1 class="le-status-page__title">{{ t('forbidden.title') }}</h1>
    <p>{{ t('forbidden.description') }}</p>
    <p v-if="need.length > 0" data-testid="forbidden-need">{{ t('forbidden.need', { perms: need.join(', ') }) }}</p>
    <p>{{ t('forbidden.contact') }}</p>
    <router-link :to="{ name: ROUTE.home }" class="le-btn-link">{{ t('forbidden.back') }}</router-link>
  </section>
</template>
