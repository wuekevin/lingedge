<script setup lang="ts">
import { LeEmpty } from '@lingedge/shared';
import { computed, nextTick, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRouter } from 'vue-router';
import { filterSearch } from '../registry/search';
import type { SearchEntry } from '../registry/visibility';

// 全局搜索壳（EXPERIENCE.md · 全局搜索；ia.md §4.8）：⌘K / Ctrl+K 打开，↑↓ 选择、Enter 打开、Esc 关闭。
// 本 Story 只索引「已交付且当前身份可见」的页面（entries 由外壳按可见性公式传入）；站点/域名/节点等对象与命令由其模块交付时扩展索引。
const props = defineProps<{ entries: SearchEntry[] }>();
const open = defineModel<boolean>('open', { required: true });
const { t } = useI18n();
const router = useRouter();

const query = ref('');
const cursor = ref(0);
const input = ref<{ focus: () => void } | null>(null);
const results = computed(() => filterSearch(props.entries, query.value, (k) => t(k)));

watch(open, async (v) => {
  if (!v) return;
  query.value = '';
  cursor.value = 0;
  await nextTick();
  input.value?.focus();
});
watch(query, () => {
  cursor.value = 0;
});

async function go(e: SearchEntry | undefined): Promise<void> {
  if (!e) return;
  open.value = false;
  await router.push(e.path);
}
function onKey(ev: KeyboardEvent): void {
  if (ev.key === 'ArrowDown') {
    ev.preventDefault();
    cursor.value = Math.min(cursor.value + 1, Math.max(results.value.length - 1, 0));
  } else if (ev.key === 'ArrowUp') {
    ev.preventDefault();
    cursor.value = Math.max(cursor.value - 1, 0);
  } else if (ev.key === 'Enter') {
    ev.preventDefault();
    void go(results.value[cursor.value]);
  }
}
</script>

<template>
  <a-modal v-model:visible="open" unmount-on-close :title="t('search.title')" :footer="false" :mask-closable="true" :esc-to-close="true" modal-class="le-search-modal">
    <a-input
      ref="input"
      v-model="query"
      :placeholder="t('search.placeholder')"
      allow-clear
      :input-attrs="{ 'data-testid': 'search-input', role: 'combobox', 'aria-expanded': 'true', 'aria-controls': 'le-search-results', 'aria-label': t('search.title') }"
      @keydown="onKey"
    />
    <p class="le-search__hint">{{ t('search.hint') }}</p>
    <ul v-if="results.length > 0" id="le-search-results" class="le-search__list" role="listbox" :aria-label="t('search.results')" data-testid="search-results">
      <li v-for="(r, i) in results" :key="r.key" role="option" :aria-selected="i === cursor" class="le-search__item" :class="{ 'le-search__item--active': i === cursor }" data-testid="search-result" @click="go(r)" @mousemove="cursor = i">
        <span>{{ t(r.titleKey) }}</span>
        <span class="le-search__group">{{ t(r.groupTitleKey) }}</span>
      </li>
    </ul>
    <LeEmpty v-else-if="entries.length === 0" :title="t('search.empty.title')" :description="t('search.empty.description')" />
    <LeEmpty v-else reason="no-results" @clear-filters="query = ''" />
  </a-modal>
</template>

<style scoped>
.le-search__hint {
  margin: var(--le-space-2) 0;
  font-size: var(--le-text-caption-size);
  color: var(--le-color-text-secondary);
}
.le-search__list {
  list-style: none;
  margin: 0;
  padding: 0;
  max-height: 320px;
  overflow: auto;
}
.le-search__item {
  display: flex;
  justify-content: space-between;
  gap: var(--le-space-3);
  padding: var(--le-space-2) var(--le-space-3);
  border-radius: var(--le-radius-md);
  color: var(--le-color-text-primary);
  cursor: pointer;
}
.le-search__item--active {
  background: var(--le-color-info-bg);
  color: var(--le-color-info);
}
.le-search__group {
  color: inherit;
  opacity: 1;
  font-size: var(--le-text-caption-size);
}
</style>
