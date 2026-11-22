<script setup lang="ts">
import { computed } from 'vue';
import qrcode from 'qrcode-generator';

// 用纯 SVG 元素逐模块绘制二维码（不使用 v-html / innerHTML）。颜色走令牌：前景 text-primary、背景 bg-surface（亮色主题下是深色模块+白底，扫描友好）。
const props = defineProps<{ value: string; label: string }>();
const QUIET = 4;

const matrix = computed(() => {
  const qr = qrcode(0, 'M');
  qr.addData(props.value);
  qr.make();
  const n = qr.getModuleCount();
  const cells: { x: number; y: number }[] = [];
  for (let y = 0; y < n; y++) for (let x = 0; x < n; x++) if (qr.isDark(y, x)) cells.push({ x: x + QUIET, y: y + QUIET });
  return { size: n + QUIET * 2, cells };
});
</script>

<template>
  <svg class="qr" role="img" :aria-label="props.label" data-testid="totp-qr" :viewBox="`0 0 ${matrix.size} ${matrix.size}`" shape-rendering="crispEdges">
    <rect class="qr__bg" x="0" y="0" :width="matrix.size" :height="matrix.size" />
    <rect v-for="c in matrix.cells" :key="`${c.x}-${c.y}`" class="qr__cell" :x="c.x" :y="c.y" width="1" height="1" />
  </svg>
</template>
