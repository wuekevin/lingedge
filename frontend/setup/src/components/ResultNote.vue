<script setup lang="ts">
import { useI18n } from 'vue-i18n';

// 成功结果：本地化的成功语句 + 服务端回执原文（含指纹等细节；服务端文案语言固定，仅作补充）。
const props = defineProps<{ message: string; serverMessage?: string | undefined; testid: string }>();
const { t } = useI18n();
</script>

<template>
  <div class="result result--ok" role="status" :data-testid="props.testid">
    <p class="result__msg"><span class="result__mark" aria-hidden="true">✓</span> {{ props.message }}</p>
    <p v-if="props.serverMessage" class="result__server">
      <span class="result__server-label">{{ t('app.serverNote') }}:</span>
      {{ ' ' }}
      <!-- 服务端回执文案固定为中文（含指纹等细节）：标注语言，读屏软件按中文朗读 -->
      <span lang="zh-CN" data-testid="server-message">{{ props.serverMessage }}</span>
    </p>
  </div>
</template>
