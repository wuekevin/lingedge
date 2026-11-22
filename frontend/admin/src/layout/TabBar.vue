<script setup lang="ts">
import { computed, ref } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';
import { ROUTE } from '../router/routes';
import { useTabsStore, type Tab } from '../stores/tabs';

// 多标签栏（≥md 显示；手机宽度隐藏，由面包屑 + 返回取代，EXPERIENCE.md · 响应式）。
const { t } = useI18n();
const tabs = useTabsStore();
const route = useRoute();
const router = useRouter();

const pending = ref<Tab | null>(null);
const confirmOpen = computed({ get: () => pending.value !== null, set: (v) => !v && (pending.value = null) });

async function afterClose(closedPath: string): Promise<void> {
  if (route.fullPath !== closedPath) return;
  const next = tabs.tabs[tabs.tabs.length - 1];
  await router.push(next ? next.fullPath : { name: ROUTE.home });
}
async function close(tab: Tab, force = false): Promise<void> {
  const r = tabs.close(tab.fullPath, force);
  if (r === 'needs-confirm') {
    pending.value = tab;
    return;
  }
  if (r === 'closed') await afterClose(tab.fullPath);
}
async function confirmClose(): Promise<void> {
  const tab = pending.value;
  pending.value = null;
  if (tab) await close(tab, true);
}
async function closeOthers(): Promise<void> {
  tabs.closeOthers(route.fullPath);
  if (!tabs.tabs.some((x) => x.fullPath === route.fullPath)) await router.push({ name: ROUTE.home });
}
async function closeAll(): Promise<void> {
  const blocked = tabs.tabs.filter((x) => x.dirty);
  tabs.tabs = tabs.tabs.filter((x) => x.fullPath === '/' || x.dirty); // 首页固定；有未保存更改的标签保留，逐个确认关闭
  tabs.persist();
  if (blocked.length === 0) await router.push({ name: ROUTE.home });
}
const title = (tab: Tab) => t(tab.titleKey);
</script>

<template>
  <div class="le-tabs" role="toolbar" :aria-label="t('tabs.label')" data-testid="tab-bar">
    <ul class="le-tabs__list">
      <li v-for="tab in tabs.tabs" :key="tab.fullPath" class="le-tab" :class="{ 'le-tab--active': route.fullPath === tab.fullPath }" data-testid="tab">
        <router-link :to="tab.fullPath" class="le-tab__link" :aria-current="route.fullPath === tab.fullPath ? 'page' : undefined">
          {{ title(tab) }}<span v-if="tab.dirty" class="le-tab__dirty" :title="t('tabs.unsaved')" role="img" :aria-label="t('tabs.unsaved')">*</span>
        </router-link>
        <button v-if="tab.fullPath !== '/'" type="button" class="le-tab__close" :aria-label="t('tabs.close', { title: title(tab) })" data-testid="tab-close" @click="close(tab)">×</button>
      </li>
    </ul>
    <div class="le-tabs__actions">
      <button type="button" class="le-tabs__btn" data-testid="tabs-close-others" @click="closeOthers">{{ t('tabs.closeOthers') }}</button>
      <button type="button" class="le-tabs__btn" data-testid="tabs-close-all" @click="closeAll">{{ t('tabs.closeAll') }}</button>
    </div>
    <a-modal v-model:visible="confirmOpen" unmount-on-close :title="t('tabs.confirm.title')" :ok-text="t('tabs.confirm.ok')" :cancel-text="t('tabs.confirm.cancel')" @ok="confirmClose">
      <span data-testid="tab-dirty-confirm">{{ t('tabs.confirm.body', { title: pending ? title(pending) : '' }) }}</span>
    </a-modal>
  </div>
</template>

<style scoped>
.le-tabs {
  display: flex;
  align-items: stretch;
  justify-content: space-between;
  height: 36px;
  background: var(--le-color-bg-surface);
  border-bottom: 1px solid var(--le-color-border-default);
}
.le-tabs__list {
  display: flex;
  list-style: none;
  margin: 0;
  padding: 0;
  overflow-x: auto;
}
.le-tab {
  display: flex;
  align-items: center;
  white-space: nowrap;
  border-right: 1px solid var(--le-color-border-default);
  color: var(--le-color-text-secondary);
}
.le-tab--active {
  background: var(--le-color-bg-page);
  color: var(--le-color-text-primary);
  font-weight: 600;
  box-shadow: inset 0 -2px 0 var(--le-color-brand-primary);
}
.le-tab__link {
  padding: 0 var(--le-space-3);
  line-height: 36px;
  color: inherit;
  text-decoration: none;
}
.le-tab__dirty {
  margin-left: 2px;
  color: var(--le-color-warning);
}
.le-tab__close {
  width: 24px;
  height: 24px;
  margin-right: var(--le-space-1);
  border: 0;
  border-radius: var(--le-radius-sm);
  background: none;
  color: inherit;
  font-size: 16px;
  cursor: pointer;
}
.le-tab__close:hover {
  background: var(--le-color-bg-sunken);
}
.le-tabs__actions {
  display: flex;
  flex: none;
}
.le-tabs__btn {
  padding: 0 var(--le-space-3);
  border: 0;
  border-left: 1px solid var(--le-color-border-default);
  background: none;
  color: var(--le-color-text-secondary);
  font: inherit;
  font-size: var(--le-text-caption-size);
  cursor: pointer;
}
.le-tabs__btn:hover {
  color: var(--le-color-brand-primary);
}
.le-tab__link:focus-visible,
.le-tab__close:focus-visible,
.le-tabs__btn:focus-visible {
  outline: 2px solid var(--le-color-brand-primary);
  outline-offset: -2px;
}
</style>
