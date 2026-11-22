<script setup lang="ts">
import { useI18n } from 'vue-i18n';

// 游标分页条（OpenAPI CursorMeta：next_cursor 为 null = 已到末尾）。游标不透明，上一页由调用方的游标栈回退。
defineProps<{ pageNo: number; hasPrev: boolean; hasNext: boolean; limit: number; busy?: boolean }>();
const emit = defineEmits<{ prev: []; next: []; 'update:limit': [limit: number] }>();
const { t } = useI18n();
const LIMITS = [20, 50, 100];
</script>

<template>
  <nav class="le-pager" :aria-label="t('pager.label')" data-testid="cursor-bar">
    <span class="le-pager__info" data-testid="cursor-info">{{ t('pager.cursorInfo', { page: pageNo }) }}</span>
    <label class="le-pager__limit">
      <span>{{ t('pager.limit') }}</span>
      <select :value="limit" data-testid="cursor-limit" @change="emit('update:limit', Number(($event.target as HTMLSelectElement).value))">
        <option v-for="n in LIMITS" :key="n" :value="n">{{ n }}</option>
      </select>
    </label>
    <a-button size="small" :disabled="!hasPrev || busy" data-testid="cursor-prev" @click="emit('prev')">{{ t('pager.prev') }}</a-button>
    <a-button size="small" :disabled="!hasNext || busy" data-testid="cursor-next" @click="emit('next')">{{ t('pager.next') }}</a-button>
  </nav>
</template>
