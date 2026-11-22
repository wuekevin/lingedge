<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue';
import { useI18n } from 'vue-i18n';

const props = defineProps<{ title: string; description?: string; step: number; total: number; testid: string }>();
const { t } = useI18n();
const heading = ref<HTMLElement | null>(null);

// 切换步骤后把键盘焦点移到标题：键盘与读屏用户不会停留在已卸载的控件上，并能听到新步骤的标题。
onMounted(() => {
  void nextTick(() => heading.value?.focus({ preventScroll: false }));
});
</script>

<template>
  <section class="step" :data-testid="props.testid" :aria-labelledby="`${props.testid}-title`">
    <p class="step__counter">{{ t('nav.stepOf', { n: props.step, total: props.total }) }}</p>
    <h2 :id="`${props.testid}-title`" ref="heading" class="step__title" tabindex="-1">{{ props.title }}</h2>
    <p v-if="props.description" class="step__desc">{{ props.description }}</p>
    <slot />
  </section>
</template>
