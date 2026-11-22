import { onBeforeUnmount, onMounted, ref } from 'vue';

/** 断点沿用 Arco 栅格（DESIGN.md）：sm 以下（<768）为手机/窄屏：侧栏变抽屉、隐藏多标签栏；md（768–991）侧栏折叠为图标。 */
export const BREAKPOINT_MD = 768;
export const BREAKPOINT_LG = 992;
export type ViewportClass = 'mobile' | 'tablet' | 'desktop';

export function classify(width: number): ViewportClass {
  if (width < BREAKPOINT_MD) return 'mobile';
  if (width < BREAKPOINT_LG) return 'tablet';
  return 'desktop';
}

export function useViewport() {
  const cls = ref<ViewportClass>(classify(typeof window === 'undefined' ? BREAKPOINT_LG : window.innerWidth));
  const update = () => {
    cls.value = classify(window.innerWidth);
  };
  onMounted(() => {
    update();
    window.addEventListener('resize', update);
  });
  onBeforeUnmount(() => window.removeEventListener('resize', update));
  return cls;
}
