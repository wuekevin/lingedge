<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import type { ViewportClass } from '../composables/useViewport';
import { useSessionStore } from '../stores/session';
import ShellPrefs from './ShellPrefs.vue';

// 顶栏 48px：品牌、全局搜索入口、语言/主题、用户菜单（告警/通知入口随 P2 模块交付，不放占位）。
const props = defineProps<{ viewport: ViewportClass; collapsed: boolean }>();
const emit = defineEmits<{ 'toggle-nav': []; 'open-search': []; logout: [] }>();
const { t } = useI18n();
const session = useSessionStore();
const name = computed(() => session.user?.display_name || session.user?.email || '');
const initial = computed(() => Array.from(name.value)[0]?.toUpperCase() ?? '');
const navLabel = computed(() => (props.viewport === 'mobile' ? t('nav.open') : props.collapsed ? t('nav.expand') : t('nav.collapse')));
</script>

<template>
  <header class="le-topbar" data-testid="top-bar">
    <button type="button" class="le-topbar__btn" :aria-label="navLabel" data-testid="nav-toggle" @click="emit('toggle-nav')">
      <span aria-hidden="true">☰</span>
    </button>
    <span class="le-topbar__brand">{{ t('topbar.brand') }}</span>
    <button type="button" class="le-topbar__search" :aria-label="t('search.open')" data-testid="search-open" @click="emit('open-search')">
      <span aria-hidden="true">⌕</span>
      <span class="le-topbar__search-text">{{ t('search.placeholder') }}</span>
      <kbd class="le-topbar__kbd" aria-hidden="true">{{ t('search.shortcut') }}</kbd>
    </button>
    <div class="le-topbar__right">
      <ShellPrefs v-if="viewport !== 'mobile'" />
      <a-dropdown trigger="click" position="br" @select="emit('logout')">
        <button type="button" class="le-topbar__account" :aria-label="t('account.menu')" data-testid="account-menu">
          <span class="le-topbar__initial" aria-hidden="true">{{ initial }}</span>
          <span class="le-topbar__name">{{ name }}</span>
        </button>
        <template #content>
          <a-doption data-testid="logout">{{ t('account.logout') }}</a-doption>
        </template>
      </a-dropdown>
    </div>
  </header>
</template>

<style scoped>
.le-topbar {
  position: sticky;
  top: 0;
  z-index: 100;
  display: flex;
  align-items: center;
  gap: var(--le-space-3);
  box-sizing: border-box;
  height: 48px;
  padding: 0 var(--le-layout-gutter-mobile);
  background: var(--le-color-bg-surface);
  border-bottom: 1px solid var(--le-color-border-default);
}
.le-topbar__btn {
  width: 44px;
  height: 44px;
  margin-left: -10px;
  border: 0;
  background: none;
  color: var(--le-color-text-primary);
  font-size: 18px;
  cursor: pointer;
}
.le-topbar__brand {
  font-weight: 700;
  color: var(--le-color-brand-primary);
}
.le-topbar__search {
  display: inline-flex;
  align-items: center;
  gap: var(--le-space-2);
  height: 32px;
  min-width: 44px;
  padding: 0 var(--le-space-3);
  border: 1px solid var(--le-color-border-default);
  border-radius: var(--le-radius-md);
  background: var(--le-color-bg-sunken);
  color: var(--le-color-text-secondary);
  font: inherit;
  cursor: pointer;
}
.le-topbar__search-text,
.le-topbar__kbd {
  display: none;
}
.le-topbar__kbd {
  padding: 0 var(--le-space-1);
  border: 1px solid var(--le-color-border-strong);
  border-radius: var(--le-radius-sm);
  font-family: var(--le-font-mono);
  font-size: var(--le-text-caption-size);
}
.le-topbar__right {
  display: flex;
  align-items: center;
  gap: var(--le-space-3);
  margin-left: auto;
}
.le-topbar__account {
  display: inline-flex;
  align-items: center;
  gap: var(--le-space-2);
  min-height: 44px;
  padding: 0 var(--le-space-1);
  border: 0;
  background: none;
  color: var(--le-color-text-primary);
  font: inherit;
  cursor: pointer;
}
.le-topbar__initial {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border-radius: var(--le-radius-full);
  background: var(--le-color-brand-primary);
  color: var(--le-color-on-brand);
  font-size: var(--le-text-label-size);
  font-weight: 600;
}
.le-topbar__name {
  display: none;
}
.le-topbar__btn:focus-visible,
.le-topbar__search:focus-visible,
.le-topbar__account:focus-visible {
  outline: 2px solid var(--le-color-brand-primary);
  outline-offset: 2px;
}
@media (min-width: 768px) {
  .le-topbar__search-text,
  .le-topbar__kbd,
  .le-topbar__name {
    display: inline;
  }
  .le-topbar__search {
    min-width: 240px;
  }
}
@media (min-width: 992px) {
  .le-topbar {
    padding: 0 var(--le-layout-gutter-desktop);
  }
}
</style>
