<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import { useRoute } from 'vue-router';
import type { MenuGroup } from '../registry/visibility';
import { ROUTE } from '../router/routes';

// 动态侧栏：只渲染传入的可见菜单（已交付 ∧ 权限 ∧ 运行模式；一级菜单下无可见页面则根本不会传入）。没有灰显占位、没有「敬请期待」。
defineProps<{ groups: MenuGroup[]; collapsed?: boolean }>();
const emit = defineEmits<{ navigate: [] }>();
const { t } = useI18n();
const route = useRoute();
const initial = (s: string) => Array.from(s)[0] ?? '';
</script>

<template>
  <nav class="le-nav" :class="{ 'le-nav--collapsed': collapsed }" :aria-label="t('nav.menu')" data-testid="side-menu">
    <ul class="le-nav__list">
      <li>
        <router-link :to="{ name: ROUTE.home }" class="le-nav__link" :class="{ 'le-nav__link--active': route.name === ROUTE.home }" :title="t('nav.home')" @click="emit('navigate')">
          <span class="le-nav__badge" aria-hidden="true">{{ initial(t('nav.home')) }}</span>
          <span class="le-nav__text">{{ t('nav.home') }}</span>
        </router-link>
      </li>
      <li v-for="g in groups" :key="g.key" class="le-nav__group" :data-menu-group="g.key">
        <p class="le-nav__group-title">{{ t(g.titleKey) }}</p>
        <ul class="le-nav__list">
          <li v-for="c in g.children" :key="c.key">
            <router-link
              :to="c.path"
              class="le-nav__link"
              :class="{ 'le-nav__link--active': route.path === c.path }"
              :title="t(c.titleKey)"
              :data-menu-item="c.key"
              @click="emit('navigate')"
            >
              <span class="le-nav__badge" aria-hidden="true">{{ initial(t(c.titleKey)) }}</span>
              <span class="le-nav__text">{{ t(c.titleKey) }}</span>
            </router-link>
          </li>
        </ul>
      </li>
    </ul>
  </nav>
</template>

<style scoped>
.le-nav {
  padding: var(--le-space-2);
}
.le-nav__list {
  list-style: none;
  margin: 0;
  padding: 0;
}
.le-nav__group-title {
  margin: var(--le-space-3) 0 var(--le-space-1);
  padding: 0 var(--le-space-2);
  font-size: var(--le-text-caption-size);
  font-weight: var(--le-text-label-weight);
  color: var(--le-color-text-secondary);
}
.le-nav__link {
  display: flex;
  align-items: center;
  gap: var(--le-space-2);
  min-height: 40px;
  padding: 0 var(--le-space-2);
  border-radius: var(--le-radius-md);
  color: var(--le-color-text-primary);
  text-decoration: none;
}
.le-nav__link:hover {
  background: var(--le-color-bg-sunken);
}
.le-nav__link:focus-visible {
  outline: 2px solid var(--le-color-brand-primary);
  outline-offset: -2px;
}
.le-nav__link--active {
  background: var(--le-color-info-bg);
  color: var(--le-color-info);
  font-weight: 600;
}
.le-nav__badge {
  display: none;
  width: 28px;
  height: 28px;
  flex: none;
  align-items: center;
  justify-content: center;
  border-radius: var(--le-radius-md);
  background: var(--le-color-bg-sunken);
  color: var(--le-color-text-primary);
  font-size: var(--le-text-label-size);
}
.le-nav--collapsed .le-nav__badge {
  display: inline-flex;
}
.le-nav--collapsed .le-nav__text,
.le-nav--collapsed .le-nav__group-title {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip-path: inset(50%);
  white-space: nowrap;
}
.le-nav--collapsed .le-nav__link {
  justify-content: center;
}
/* 触控：手机上的导航项不小于 44px（UX-23） */
@media (max-width: 767px) {
  .le-nav__link {
    min-height: 44px;
  }
}
</style>
