<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useServerText } from '../api/useServerText';

// 成功结果：本地化的成功语句 + 服务端回执（按步骤标识映射到当前界面语言；对不上的句式回落到服务端原文并标注其语言）。
const props = defineProps<{ message: string; serverMessage?: string | undefined; step: string; testid: string }>();
const { t } = useI18n();
const st = useServerText();
const note = computed(() => (props.serverMessage ? st.step(props.step, props.serverMessage) : null));
</script>

<template>
  <div class="result result--ok" role="status" :data-testid="props.testid">
    <p class="result__msg"><span class="result__mark" aria-hidden="true">✓</span> {{ props.message }}</p>
    <p v-if="note" class="result__server">
      <span class="result__server-label">{{ t('app.serverNote') }}:</span>
      {{ ' ' }}
      <span :lang="note.lang" :data-localized="note.localized" data-testid="server-message">{{ note.text }}</span>
    </p>
  </div>
</template>
