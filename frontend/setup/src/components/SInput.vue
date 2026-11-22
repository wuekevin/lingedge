<script setup lang="ts">
import { computed, useId } from 'vue';
import { useI18n } from 'vue-i18n';

const props = withDefaults(
  defineProps<{
    modelValue: string;
    label: string;
    /** 同时用作 name 与 data-testid（f-<name>），便于测试与审计定位。 */
    name: string;
    type?: 'text' | 'password' | 'email' | 'number' | 'url';
    hint?: string;
    error?: string;
    required?: boolean;
    /** 不显示“必填/可选”标记（如令牌输入）。 */
    hideMarker?: boolean;
    autocomplete?: string;
    placeholder?: string;
    inputmode?: 'text' | 'numeric' | 'email' | 'url';
    maxlength?: number;
    list?: string;
    disabled?: boolean;
  }>(),
  { type: 'text', hint: undefined, error: undefined, required: false, hideMarker: false, autocomplete: 'off', placeholder: undefined, inputmode: undefined, maxlength: undefined, list: undefined, disabled: false },
);
const emit = defineEmits<{ 'update:modelValue': [value: string] }>();
const { t } = useI18n();
const id = useId();
const describedBy = computed(() => [props.hint ? `${id}-hint` : '', props.error ? `${id}-err` : ''].filter(Boolean).join(' ') || undefined);
// 口令类输入：不回显（type=password）、关闭拼写检查/自动大写/自动填充建议，不进浏览器历史。
const sensitive = computed(() => props.type === 'password');

function onInput(e: Event) {
  emit('update:modelValue', (e.target as HTMLInputElement).value);
}
</script>

<template>
  <div class="field" :class="{ 'field--error': !!props.error }">
    <label class="field__label" :for="id">
      <span>{{ props.label }}</span>
      <span v-if="!props.hideMarker" class="field__marker">{{ props.required ? t('common.required') : t('common.optional') }}</span>
    </label>
    <input
      :id="id"
      class="field__input"
      :class="{ 'field__input--mono': sensitive || props.type === 'url' }"
      :type="props.type"
      :name="props.name"
      :data-testid="`f-${props.name}`"
      :value="props.modelValue"
      :autocomplete="props.autocomplete"
      :placeholder="props.placeholder"
      :inputmode="props.inputmode"
      :maxlength="props.maxlength"
      :list="props.list"
      :disabled="props.disabled"
      :required="props.required"
      :aria-required="props.required ? 'true' : undefined"
      :aria-invalid="props.error ? 'true' : undefined"
      :aria-describedby="describedBy"
      :spellcheck="sensitive ? false : undefined"
      :autocapitalize="sensitive ? 'off' : undefined"
      autocorrect="off"
      @input="onInput"
    />
    <p v-if="props.hint" :id="`${id}-hint`" class="field__hint">{{ props.hint }}</p>
    <p v-if="props.error" :id="`${id}-err`" class="field__error" role="alert">{{ props.error }}</p>
  </div>
</template>
