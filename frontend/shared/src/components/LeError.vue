<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { isRetryableKind, type LeErrorKind } from '../state/error-kind';
import './state.css';

const props = withDefaults(
  defineProps<{
    kind?: LeErrorKind;
    /** 已翻译的对象名，如“站点列表”；缺省时用通用标题。 */
    subject?: string;
    /** 响应头 X-Request-ID（可复制）。 */
    requestId?: string;
    /** 服务端给出的补充说明（已本地化或原样展示，组件不解析）。 */
    detail?: string;
    /** 缺省由 kind 决定（403/404/400 不可重试）。 */
    retryable?: boolean;
  }>(),
  { kind: 'unknown', subject: undefined, requestId: undefined, detail: undefined, retryable: undefined },
);
const emit = defineEmits<{ retry: [] }>();

const { t } = useI18n();
const showRetry = computed(() => props.retryable ?? isRetryableKind(props.kind));
const copyState = ref<'idle' | 'ok' | 'failed'>('idle');
let resetTimer: ReturnType<typeof setTimeout> | undefined;

async function copyRequestId() {
  if (!props.requestId) return;
  try {
    await navigator.clipboard.writeText(props.requestId);
    copyState.value = 'ok';
  } catch {
    copyState.value = 'failed';
  }
  if (resetTimer !== undefined) clearTimeout(resetTimer);
  resetTimer = setTimeout(() => {
    copyState.value = 'idle';
  }, 2000);
}
onBeforeUnmount(() => {
  if (resetTimer !== undefined) clearTimeout(resetTimer);
});
</script>

<template>
  <div class="le-state le-error" role="alert" :data-le-state="'error'" :data-le-error-kind="props.kind">
    <p class="le-error__title">
      {{ props.subject ? t('state.error.title', { subject: props.subject }) : t('state.error.titleGeneric') }}
    </p>
    <p class="le-error__reason">{{ t(`state.error.kind.${props.kind}`) }}</p>
    <p v-if="props.detail" class="le-error__detail">{{ props.detail }}</p>
    <p v-if="props.requestId" class="le-error__rid">
      <span>{{ t('state.error.requestId') }}:</span>
      <code data-le-request-id>{{ props.requestId }}</code>
      <button type="button" class="le-state-btn" @click="copyRequestId">{{ t('state.error.copy') }}</button>
      <span v-if="copyState === 'ok'" role="status">{{ t('state.error.copied') }}</span>
      <span v-else-if="copyState === 'failed'" role="status">{{ t('state.error.copyFailed') }}</span>
    </p>
    <div v-if="showRetry" class="le-error__actions">
      <button type="button" class="le-state-btn le-state-btn--primary" data-le-retry @click="emit('retry')">
        {{ t('state.error.retry') }}
      </button>
    </div>
  </div>
</template>

<style scoped>
.le-error {
  padding: var(--le-space-4);
  border: 1px solid var(--le-color-danger);
  border-radius: var(--le-radius-lg);
  background: var(--le-color-danger-bg);
  color: var(--le-color-text-primary);
}
.le-error__title {
  margin: 0 0 var(--le-space-1);
  font-size: var(--le-text-heading-3-size);
  font-weight: var(--le-text-heading-3-weight);
  color: var(--le-color-danger);
}
.le-error__reason,
.le-error__detail {
  margin: 0 0 var(--le-space-2);
}
.le-error__rid {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--le-space-2);
  margin: 0 0 var(--le-space-2);
  font-size: var(--le-text-caption-size);
}
.le-error__rid code {
  font-family: var(--le-font-mono);
  user-select: all;
}
.le-error__actions {
  margin-top: var(--le-space-2);
}
</style>
