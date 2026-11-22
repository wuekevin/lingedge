<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue';
import { useI18n } from 'vue-i18n';

const props = defineProps<{ value: string; testid?: string }>();
const { t } = useI18n();
const state = ref<'idle' | 'ok' | 'failed'>('idle');
let timer: ReturnType<typeof setTimeout> | undefined;

async function copy() {
  try {
    await navigator.clipboard.writeText(props.value);
    state.value = 'ok';
  } catch {
    state.value = 'failed';
  }
  if (timer !== undefined) clearTimeout(timer);
  timer = setTimeout(() => {
    state.value = 'idle';
  }, 2000);
}
onBeforeUnmount(() => {
  if (timer !== undefined) clearTimeout(timer);
});
</script>

<template>
  <span class="copy">
    <button type="button" class="btn btn--small" :data-testid="props.testid" @click="copy">{{ t('common.copy') }}</button>
    <span v-if="state === 'ok'" role="status" class="copy__state">{{ t('common.copied') }}</span>
    <span v-else-if="state === 'failed'" role="status" class="copy__state copy__state--bad">{{ t('common.copyFailed') }}</span>
  </span>
</template>
