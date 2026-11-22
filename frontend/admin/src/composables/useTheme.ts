import { onBeforeUnmount, onMounted, ref } from 'vue';
import { applyResolvedTheme, localStore, readThemePref, watchSystemTheme, writeThemePref, type ThemePref } from '../theme/theme';

/** 主题偏好（亮/暗/跟随系统）：auto 时实时跟随系统切换。 */
export function useTheme() {
  const pref = ref<ThemePref>(readThemePref(localStore()));
  let stop: () => void = () => undefined;
  function apply(): void {
    applyResolvedTheme(pref.value);
  }
  function setPref(p: ThemePref): void {
    pref.value = p;
    writeThemePref(localStore(), p);
    apply();
  }
  onMounted(() => {
    apply();
    stop = watchSystemTheme(() => {
      if (pref.value === 'auto') apply();
    });
  });
  onBeforeUnmount(() => stop());
  return { pref, setPref };
}
