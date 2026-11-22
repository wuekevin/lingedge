<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import './state.css';

const props = withDefaults(
  defineProps<{
    /** no-data：尚无数据（新对象）；no-results：筛选无结果（提供清除筛选）。 */
    reason?: 'no-data' | 'no-results';
    /** 覆盖默认标题/说明（已翻译）。数据类空态不得带示例数字。 */
    title?: string;
    description?: string;
  }>(),
  { reason: 'no-data', title: undefined, description: undefined },
);
const emit = defineEmits<{ 'clear-filters': [] }>();
const { t } = useI18n();
const defaultTitle = () => (props.reason === 'no-results' ? t('state.empty.noResults.title') : t('state.empty.noData.title'));
const defaultDescription = () => (props.reason === 'no-results' ? t('state.empty.noResults.description') : t('state.empty.noData.description'));
</script>

<template>
  <div class="le-state le-empty" role="status" data-le-state="empty" :data-le-empty-reason="props.reason">
    <p class="le-empty__title">{{ props.title ?? defaultTitle() }}</p>
    <p class="le-empty__desc">{{ props.description ?? defaultDescription() }}</p>
    <div class="le-empty__actions">
      <button v-if="props.reason === 'no-results'" type="button" class="le-state-btn" data-le-clear-filters @click="emit('clear-filters')">
        {{ t('state.empty.noResults.clearFilters') }}
      </button>
      <!-- 下一步主操作由调用方经 action 插槽提供（每个空态至多一个主操作） -->
      <slot name="action" />
    </div>
  </div>
</template>

<style scoped>
.le-empty {
  display: grid;
  justify-items: center;
  gap: var(--le-space-2);
  padding: var(--le-space-8) var(--le-space-4);
  text-align: center;
}
.le-empty__title {
  margin: 0;
  font-size: var(--le-text-heading-3-size);
  font-weight: var(--le-text-heading-3-weight);
  color: var(--le-color-text-primary);
}
.le-empty__desc {
  margin: 0;
}
.le-empty__actions {
  display: flex;
  gap: var(--le-space-2);
  margin-top: var(--le-space-2);
}
</style>
