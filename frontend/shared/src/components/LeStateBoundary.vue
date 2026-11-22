<script setup lang="ts">
import type { LeErrorKind } from '../state/error-kind';
import type { LeStatus } from '../state/status';
import LeEmpty from './LeEmpty.vue';
import LeError from './LeError.vue';
import LeLoading from './LeLoading.vue';

const props = withDefaults(
  defineProps<{
    status: LeStatus;
    errorKind?: LeErrorKind;
    errorSubject?: string;
    requestId?: string;
    errorDetail?: string;
    emptyReason?: 'no-data' | 'no-results';
  }>(),
  { errorKind: 'unknown', errorSubject: undefined, requestId: undefined, errorDetail: undefined, emptyReason: 'no-data' },
);
const emit = defineEmits<{ retry: []; cancel: []; 'clear-filters': [] }>();
</script>

<template>
  <slot v-if="props.status === 'loading'" name="loading">
    <LeLoading @retry="emit('retry')" @cancel="emit('cancel')" />
  </slot>
  <slot v-else-if="props.status === 'error'" name="error">
    <LeError :kind="props.errorKind" :subject="props.errorSubject" :request-id="props.requestId" :detail="props.errorDetail" @retry="emit('retry')" />
  </slot>
  <slot v-else-if="props.status === 'empty'" name="empty">
    <LeEmpty :reason="props.emptyReason" @clear-filters="emit('clear-filters')">
      <template #action><slot name="empty-action" /></template>
    </LeEmpty>
  </slot>
  <slot v-else />
</template>
