<script setup lang="ts">
import { computed, defineComponent, h, inject, onBeforeUnmount, onMounted, ref, watch, type Component } from 'vue';
import { useI18n } from 'vue-i18n';
import { useRoute, useRouter } from 'vue-router';
import { useViewport } from '../composables/useViewport';
import { buildMenu, buildSearchIndex, evaluatePage } from '../registry/visibility';
import { ROUTE } from '../router/routes';
import { useSessionStore } from '../stores/session';
import { MAX_TABS, useTabsStore } from '../stores/tabs';
import ActingBanner from './ActingBanner.vue';
import GlobalSearch from './GlobalSearch.vue';
import { REGISTRY_KEY } from './keys';
import MobileNotice from './MobileNotice.vue';
import ShellPrefs from './ShellPrefs.vue';
import SideMenu from './SideMenu.vue';
import TabBar from './TabBar.vue';
import TopBar from './TopBar.vue';

// 后台外壳（screens.md §0）：顶栏 48px + 代管横幅 32px + 侧栏（≥lg 展开 / md 折叠为图标 / 手机抽屉）+ 多标签栏 36px（手机隐藏，改面包屑 + 返回）+ 内容区。
const { t } = useI18n();
const route = useRoute();
const router = useRouter();
const session = useSessionStore();
const tabs = useTabsStore();
const registry = inject(REGISTRY_KEY);
const viewport = useViewport();

const groups = computed(() => (session.access && registry ? buildMenu(registry.pages, session.access) : []));
const searchEntries = computed(() => (session.access && registry ? buildSearchIndex(registry.pages, session.access) : []));
const page = computed(() => (route.meta.pageKey ? registry?.byKey(route.meta.pageKey) : undefined));

// ---- 侧栏 ----
const userCollapsed = ref(false);
const drawerOpen = ref(false);
const collapsed = computed(() => (viewport.value === 'tablet' ? true : viewport.value === 'desktop' ? userCollapsed.value : false));
function toggleNav(): void {
  if (viewport.value === 'mobile') drawerOpen.value = !drawerOpen.value;
  else if (viewport.value === 'desktop') userCollapsed.value = !userCollapsed.value;
}
watch(viewport, (v) => {
  if (v !== 'mobile') drawerOpen.value = false;
});

// ---- 标签页：打开/激活；刷新后恢复仍然可访问的标签 ----
function titleKeyFor(fullPath: string): string | null {
  const r = router.resolve(fullPath);
  if (r.name === ROUTE.home) return 'home.title';
  const p = r.meta.pageKey ? registry?.byKey(r.meta.pageKey) : undefined;
  const a = session.access;
  return p && a && evaluatePage(p, a).ok ? p.titleKey : null;
}
tabs.restore(titleKeyFor);
tabs.ensureHome();
watch(
  () => route.fullPath,
  () => {
    if (route.meta.tab && route.meta.titleKey) tabs.open(route.fullPath, route.meta.titleKey);
    drawerOpen.value = false;
  },
  { immediate: true },
);

// 页面实例随标签保活（切换标签不丢失表单内容；关闭标签即销毁）：按 fullPath 给页面套一层具名壳，KeepAlive 用 include 控制保留集合。
const wrappers = new Map<string, Component>();
function wrap(vnodeComponent: unknown, fullPath: string): Component {
  let w = wrappers.get(fullPath);
  if (!w) {
    w = defineComponent({ name: fullPath, setup: () => () => h(vnodeComponent as Component) });
    wrappers.set(fullPath, w);
  }
  return w;
}
const keepNames = computed(() => tabs.tabs.map((x) => x.fullPath));
watch(keepNames, (names) => {
  for (const k of [...wrappers.keys()]) if (!names.includes(k) && k !== route.fullPath) wrappers.delete(k);
});

// ---- 搜索 ----
const searchOpen = ref(false);
function onKeydown(e: KeyboardEvent): void {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault();
    searchOpen.value = true;
  }
}
// ---- 未保存更改：刷新/关闭页面前提示 ----
function onBeforeUnload(e: BeforeUnloadEvent): void {
  if (tabs.hasDirty()) {
    e.preventDefault();
    e.returnValue = '';
  }
}
onMounted(() => {
  window.addEventListener('keydown', onKeydown);
  window.addEventListener('beforeunload', onBeforeUnload);
});
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown);
  window.removeEventListener('beforeunload', onBeforeUnload);
});

// ---- 退出登录：关闭全部标签；有未保存更改先确认 ----
const logoutConfirm = ref(false);
async function doLogout(): Promise<void> {
  logoutConfirm.value = false;
  await session.logout();
  tabs.closeAll();
  await router.replace({ name: ROUTE.login });
}
function requestLogout(): void {
  if (tabs.hasDirty()) logoutConfirm.value = true;
  else void doLogout();
}

// ---- 手机：面包屑 + 返回 ----
const crumbs = computed(() => {
  const p = page.value;
  const items: string[] = [t('nav.home')];
  if (p) items.push(t(`menu.group.${p.group}`), t(p.titleKey));
  else if (route.meta.titleKey && route.name !== ROUTE.home) items.push(t(route.meta.titleKey));
  return items;
});
function goBack(): void {
  if (window.history.length > 1 && window.history.state?.back) router.back();
  else void router.push({ name: ROUTE.home });
}
</script>

<template>
  <div class="le-shell" :data-viewport="viewport" data-testid="admin-shell">
    <a class="le-skip" href="#le-main" data-testid="skip-link">{{ t('skip') }}</a>
    <TopBar :viewport="viewport" :collapsed="collapsed" @toggle-nav="toggleNav" @open-search="searchOpen = true" @logout="requestLogout" />
    <ActingBanner />
    <div class="le-shell__body">
      <aside v-if="viewport !== 'mobile'" class="le-shell__side" :class="{ 'le-shell__side--collapsed': collapsed }" data-testid="sidebar">
        <SideMenu :groups="groups" :collapsed="collapsed" />
      </aside>
      <a-drawer v-else v-model:visible="drawerOpen" placement="left" :footer="false" :title="t('nav.menu')" :width="280" unmount-on-close>
        <SideMenu :groups="groups" @navigate="drawerOpen = false" />
        <div class="le-shell__drawer-prefs"><ShellPrefs /></div>
      </a-drawer>
      <div class="le-shell__main">
        <TabBar v-if="viewport !== 'mobile'" />
        <nav v-else class="le-crumbs" :aria-label="t('nav.breadcrumb')" data-testid="mobile-crumbs">
          <button type="button" class="le-crumbs__back" data-testid="mobile-back" @click="goBack">‹ {{ t('nav.back') }}</button>
          <span class="le-crumbs__trail">{{ crumbs.join(' / ') }}</span>
        </nav>
        <main id="le-main" class="le-shell__content" tabindex="-1">
          <MobileNotice v-if="viewport === 'mobile' && page?.complexEdit" />
          <router-view v-slot="{ Component: Page, route: r }">
            <KeepAlive :include="keepNames" :max="MAX_TABS">
              <component :is="wrap(Page, r.fullPath)" v-if="Page" :key="r.fullPath" />
            </KeepAlive>
          </router-view>
        </main>
      </div>
    </div>
    <GlobalSearch v-model:open="searchOpen" :entries="searchEntries" />
    <a-modal v-model:visible="logoutConfirm" unmount-on-close :title="t('account.logout')" :ok-text="t('account.logout')" :cancel-text="t('common.cancel')" @ok="doLogout">
      <span data-testid="logout-confirm">{{ t('account.logoutConfirm') }}</span>
    </a-modal>
  </div>
</template>

<style scoped>
.le-shell {
  min-height: 100vh;
  background: var(--le-color-bg-page);
  color: var(--le-color-text-primary);
}
.le-skip {
  position: absolute;
  left: var(--le-space-2);
  top: -48px;
  z-index: 200;
  padding: var(--le-space-2) var(--le-space-3);
  background: var(--le-color-bg-surface);
  color: var(--le-color-brand-primary);
  border: 2px solid var(--le-color-brand-primary);
  border-radius: var(--le-radius-md);
}
.le-skip:focus {
  top: var(--le-space-2);
}
.le-shell__body {
  display: flex;
  align-items: flex-start;
}
.le-shell__side {
  position: sticky;
  top: 80px;
  box-sizing: border-box;
  flex: none;
  width: var(--le-layout-sidebar-width);
  height: calc(100vh - 80px);
  overflow-y: auto;
  background: var(--le-color-bg-surface);
  border-right: 1px solid var(--le-color-border-default);
}
.le-shell__side--collapsed {
  width: var(--le-layout-sidebar-width-collapsed);
}
.le-shell__main {
  flex: 1;
  min-width: 0;
}
.le-shell__content {
  max-width: var(--le-layout-content-max-admin);
  margin: 0 auto;
  padding: var(--le-layout-gutter-mobile);
  outline: none;
}
.le-shell__drawer-prefs {
  padding: var(--le-space-4) var(--le-space-2);
}
.le-crumbs {
  display: flex;
  align-items: center;
  gap: var(--le-space-2);
  min-height: 44px;
  padding: 0 var(--le-layout-gutter-mobile);
  background: var(--le-color-bg-surface);
  border-bottom: 1px solid var(--le-color-border-default);
}
.le-crumbs__back {
  min-width: 44px;
  min-height: 44px;
  border: 0;
  background: none;
  color: var(--le-color-brand-primary);
  font: inherit;
  cursor: pointer;
}
.le-crumbs__trail {
  color: var(--le-color-text-secondary);
  font-size: var(--le-text-caption-size);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
@media (min-width: 992px) {
  .le-shell__content {
    padding: var(--le-layout-gutter-desktop);
  }
}
</style>
