<script setup lang="ts">
import { useI18n } from 'vue-i18n';

const { t } = useI18n();
</script>

<template>
  <aside class="notice notice--danger" role="note" data-testid="master-key-notice">
    <h3 class="notice__title">{{ t('masterKey.title') }}</h3>
    <p class="notice__body">{{ t('masterKey.body') }}</p>
    <slot />
  </aside>
</template>
