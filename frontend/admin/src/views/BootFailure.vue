<script setup lang="ts">
import { LeError } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import { errorKind, isApiError } from '../api/errors';

const props = defineProps<{ error: unknown }>();
const { t } = useI18n();
const kind = errorKind(props.error);
const reload = () => window.location.reload();
const rid = isApiError(props.error) ? props.error.requestId : undefined;
</script>

<template>
  <main class="le-boot-failure">
    <LeError :kind="kind" :subject="t('boot.subject')" :request-id="rid" @retry="reload" />
  </main>
</template>

<style scoped>
.le-boot-failure {
  max-width: 560px;
  margin: var(--le-space-12) auto;
  padding: 0 var(--le-space-4);
}
</style>
