<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import './state.css';

const props = withDefaults(
  defineProps<{
    /** 骨架行数（结构应与最终布局一致，调用方按需调整）。 */
    lines?: number;
    /** 超过该毫秒数追加“仍在加载…”与取消/重试（EXPERIENCE.md：>10s）。 */
    slowAfterMs?: number;
  }>(),
  { lines: 3, slowAfterMs: 10000 },
);
const emit = defineEmits<{ cancel: []; retry: [] }>();

const { t } = useI18n();
const slow = ref(false);
let timer: ReturnType<typeof setTimeout> | undefined;

onMounted(() => {
  timer = setTimeout(() => {
    slow.value = true;
  }, props.slowAfterMs);
});
onBeforeUnmount(() => {
  if (timer !== undefined) clearTimeout(timer);
});
</script>

<template>
  <div class="le-state le-loading" role="status" aria-live="polite" aria-busy="true" data-le-state="loading">
    <span class="le-sr-only">{{ t('state.loading.label') }}</span>
    <div class="le-loading__lines" aria-hidden="true">
      <div v-for="n in props.lines" :key="n" class="le-loading__line" />
    </div>
    <div v-if="slow" class="le-loading__slow" data-le-slow>
      <span>{{ t('state.loading.slow') }}</span>
      <button type="button" class="le-state-btn" @click="emit('cancel')">{{ t('state.loading.cancel') }}</button>
      <button type="button" class="le-state-btn le-state-btn--primary" @click="emit('retry')">{{ t('state.loading.retry') }}</button>
    </div>
  </div>
</template>

<style scoped>
.le-loading__lines {
  display: grid;
  gap: var(--le-space-3);
}
.le-loading__line {
  height: var(--le-space-4);
  border-radius: var(--le-radius-sm);
  background: linear-gradient(90deg, var(--le-color-bg-sunken) 25%, var(--le-color-border-default) 50%, var(--le-color-bg-sunken) 75%);
  background-size: 200% 100%;
  animation: le-shimmer 1.4s ease-in-out infinite;
}
.le-loading__line:last-child {
  width: 60%;
}
.le-loading__slow {
  display: flex;
  align-items: center;
  gap: var(--le-space-2);
  margin-top: var(--le-space-3);
}
@keyframes le-shimmer {
  from {
    background-position: 200% 0;
  }
  to {
    background-position: -200% 0;
  }
}
@media (prefers-reduced-motion: reduce) {
  .le-loading__line {
    animation: none;
  }
}
</style>
