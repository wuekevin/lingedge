<script setup lang="ts">
import { ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';

// 一次性明文展示（API Token、服务端生成的临时口令）：只展示这一次，可复制；必须显式关闭（不允许点遮罩/Esc 误关），关闭后父组件丢弃明文。
// 明文只存在于父组件的内存里：不写浏览器存储、不进 URL、不写日志。
defineProps<{ title: string; description: string; label: string; value: string }>();
const open = defineModel<boolean>('visible', { required: true });
const emit = defineEmits<{ closed: [] }>();
const { t } = useI18n();
const copied = ref<'idle' | 'ok' | 'failed'>('idle');
watch(open, (v) => {
  if (v) copied.value = 'idle';
});

async function copy(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
    copied.value = 'ok';
  } catch {
    copied.value = 'failed'; // 剪贴板不可用（非安全上下文/权限）：提示手动选择复制
  }
}
function close(): void {
  open.value = false;
  emit('closed');
}
</script>

<template>
  <a-modal v-model:visible="open" unmount-on-close :footer="false" :closable="false" :mask-closable="false" :esc-to-close="false" :title="title" modal-class="le-secret-modal">
    <div class="le-dialog" data-testid="secret-dialog" role="alertdialog">
      <p class="le-dialog__note le-dialog__note--warn" data-testid="secret-once">{{ t('oneTime.once') }}</p>
      <p>{{ description }}</p>
      <p class="le-dialog__label">{{ label }}</p>
      <code class="le-secret" data-testid="secret-value" tabindex="0">{{ value }}</code>
      <div class="le-dialog__actions">
        <a-button data-testid="secret-copy" @click="copy(value)">{{ t('oneTime.copy') }}</a-button>
        <a-button type="primary" data-testid="secret-close" @click="close">{{ t('oneTime.close') }}</a-button>
      </div>
      <p v-if="copied === 'ok'" class="le-dialog__note" role="status" data-testid="secret-copied">{{ t('oneTime.copied') }}</p>
      <p v-else-if="copied === 'failed'" class="le-dialog__note le-dialog__note--warn" role="status" data-testid="secret-copy-failed">{{ t('oneTime.copyFailed') }}</p>
    </div>
  </a-modal>
</template>
