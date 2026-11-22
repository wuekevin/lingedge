<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { isApiError } from '../api/errors';
import { errorText } from '../api/messages';

// 危险操作对话框（EXPERIENCE.md · 危险操作分级 L1–L4；UX-12）：
//   L2 确认：说明影响范围与后果，确认键用动词；
//   L3 输入确认：在 L2 基础上要求输入确认词，确认键初始禁用；
//   L4：在 L3 基础上要求重新验证身份——验证由 API 客户端在服务端要求时（403 auth.reauth_required）弹出再认证框并自动重试，这里只做提示。
// 所有 L2 及以上对话框页脚固定显示审计提示。
const props = withDefaults(
  defineProps<{
    level: 'L2' | 'L3' | 'L4';
    title: string;
    /** 影响范围与后果（已翻译）。 */
    impact: string;
    okText: string;
    /** L3/L4：必须逐字键入的确认词（如目标运行模式、对象名称）。 */
    confirmWord?: string;
    /** 该操作在服务端是 L4（x-reauth）：窗口外会弹出再认证框；这里只在对话框里提前告知。 */
    reauth?: boolean;
    busy?: boolean;
    error?: unknown;
  }>(),
  { confirmWord: undefined, reauth: false, busy: false, error: undefined },
);
const emit = defineEmits<{ confirm: []; cancel: [] }>();
const open = defineModel<boolean>('visible', { required: true });
const { t, te } = useI18n();

const typed = ref('');
const needsTyping = computed(() => props.level !== 'L2' && !!props.confirmWord);
const matches = computed(() => !needsTyping.value || typed.value === props.confirmWord);
const errorMsg = computed(() => (props.error ? errorText(t, te, props.error) : ''));
const requestId = computed(() => (isApiError(props.error) ? props.error.requestId : ''));
watch(open, (v) => {
  if (v) typed.value = '';
});

function submit(): void {
  if (props.busy || !matches.value) return;
  emit('confirm');
}
function cancel(): void {
  open.value = false;
  emit('cancel');
}
</script>

<template>
  <a-modal v-model:visible="open" unmount-on-close :footer="false" :mask-closable="false" :title="title" modal-class="le-danger" @cancel="emit('cancel')">
    <form class="le-dialog" data-testid="danger-dialog" :data-level="level" @submit.prevent="submit">
      <p class="le-dialog__level">
        <span class="le-chip le-chip--danger" data-testid="danger-level">{{ t(`danger.level.${level}`) }}</span>
      </p>
      <p class="le-dialog__impact" data-testid="danger-impact">{{ impact }}</p>
      <slot />
      <p v-if="level === 'L4' || reauth" class="le-dialog__note" data-testid="danger-reauth-hint">{{ t('danger.reauthHint') }}</p>
      <div v-if="needsTyping" class="le-dialog__typed">
        <label class="le-dialog__label" for="le-danger-typed">{{ t('danger.typeToConfirm', { word: confirmWord ?? '' }) }}</label>
        <a-input v-model="typed" allow-clear :input-attrs="{ id: 'le-danger-typed', 'data-testid': 'danger-confirm-input', autocomplete: 'off', spellcheck: 'false' }" />
      </div>
      <div v-if="error" class="le-dialog__error" role="alert" data-testid="danger-error">
        <span>{{ errorMsg }}</span>
        <span v-if="requestId">{{ t('errors.requestId') }}: <code>{{ requestId }}</code></span>
      </div>
      <div class="le-dialog__actions">
        <a-button data-testid="danger-cancel" @click="cancel">{{ t('common.cancel') }}</a-button>
        <a-button status="danger" type="primary" html-type="submit" :disabled="!matches" :loading="busy" data-testid="danger-ok">{{ okText }}</a-button>
      </div>
      <p class="le-dialog__audit" data-testid="danger-audit">{{ t('danger.audit') }}</p>
    </form>
  </a-modal>
</template>
