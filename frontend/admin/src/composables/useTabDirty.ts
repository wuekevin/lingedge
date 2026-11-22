import { onBeforeUnmount } from 'vue';
import { useRoute } from 'vue-router';
import { useTabsStore } from '../stores/tabs';

/**
 * 页面用它声明「当前标签有未保存更改」：关闭该标签、退出登录、离开页面时外壳会先确认（EXPERIENCE.md · 多标签页）。
 *   const { setDirty } = useTabDirty(); watch(form, () => setDirty(true)); 保存成功后 setDirty(false)。
 */
export function useTabDirty() {
  const tabs = useTabsStore();
  const fullPath = useRoute().fullPath; // 页面实例随标签保活，路径在 setup 时固定
  const setDirty = (dirty: boolean): void => tabs.setDirty(fullPath, dirty);
  onBeforeUnmount(() => setDirty(false));
  return { setDirty };
}
