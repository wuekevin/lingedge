<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useSessionStore } from '../stores/session';

// 固定顶栏下的「代管租户」横幅（UX-24，ia.md §4.6）：M1 只有默认租户（D-13），管理员在 Admin 里代默认租户操作。
// 不可关闭；P5 多租户后切换到非默认租户时改为强调色（届时由租户切换器驱动，本 Story 只有默认租户）。
const { t } = useI18n();
const session = useSessionStore();
const tenant = computed(() => session.actingTenant?.name ?? t('banner.defaultTenant'));
</script>

<template>
  <div class="le-acting" role="status" data-testid="acting-banner">
    <span class="le-acting__text">{{ t('banner.text', { tenant }) }}</span>
  </div>
</template>

<style scoped>
.le-acting {
  position: sticky;
  top: 48px;
  z-index: 90;
  display: flex;
  align-items: center;
  min-height: 32px;
  padding: 0 var(--le-layout-gutter-mobile);
  background: var(--le-color-info-bg);
  color: var(--le-color-info);
  font-size: var(--le-text-label-size);
  font-weight: var(--le-text-label-weight);
  border-bottom: 1px solid var(--le-color-border-default);
}
@media (min-width: 992px) {
  .le-acting {
    padding: 0 var(--le-layout-gutter-desktop);
  }
}
</style>
