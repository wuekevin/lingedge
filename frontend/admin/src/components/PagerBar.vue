<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';

// 页码分页条（page/page_size/total，OpenAPI PageMeta）。
const props = defineProps<{ page: number; pageSize: number; total: number }>();
const emit = defineEmits<{ change: [page: number] }>();
const { t } = useI18n();
const pages = computed(() => Math.max(1, Math.ceil(props.total / props.pageSize)));
</script>

<template>
  <nav class="le-pager" :aria-label="t('pager.label')" data-testid="pager">
    <span class="le-pager__info" data-testid="pager-info">{{ t('pager.info', { page, pages, total }) }}</span>
    <a-button size="small" :disabled="page <= 1" data-testid="pager-prev" @click="emit('change', page - 1)">{{ t('pager.prev') }}</a-button>
    <a-button size="small" :disabled="page >= pages" data-testid="pager-next" @click="emit('change', page + 1)">{{ t('pager.next') }}</a-button>
  </nav>
</template>
