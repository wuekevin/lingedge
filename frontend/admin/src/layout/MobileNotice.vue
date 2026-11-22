<script setup lang="ts">
import { useI18n } from 'vue-i18n';

// 手机宽度下的如实标注（UX-23）：复杂编辑页可用但不保证体验，不阻断操作。
const { t } = useI18n();
</script>

<template>
  <a-alert type="info" class="le-mobile-notice" role="note" data-testid="mobile-complex-notice">{{ t('mobile.complexEdit') }}</a-alert>
</template>

<style scoped>
.le-mobile-notice {
  margin-bottom: var(--le-space-3);
}
</style>
