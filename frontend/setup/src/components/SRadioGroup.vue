<script setup lang="ts">
import { useId } from 'vue';

export interface RadioOption {
  value: string;
  label: string;
  hint?: string;
}
const props = defineProps<{ modelValue: string; legend: string; name: string; options: RadioOption[]; disabled?: boolean }>();
const emit = defineEmits<{ 'update:modelValue': [value: string] }>();
const gid = useId();
</script>

<template>
  <fieldset class="radios" :disabled="props.disabled">
    <legend class="radios__legend">{{ props.legend }}</legend>
    <div v-for="o in props.options" :key="o.value" class="radios__item">
      <input :id="`${gid}-${o.value}`" class="radios__input" type="radio" :name="props.name" :value="o.value" :data-testid="`r-${props.name}-${o.value}`" :checked="props.modelValue === o.value" :aria-describedby="o.hint ? `${gid}-${o.value}-hint` : undefined" @change="emit('update:modelValue', o.value)" />
      <label class="radios__label" :for="`${gid}-${o.value}`">{{ o.label }}</label>
      <p v-if="o.hint" :id="`${gid}-${o.value}-hint`" class="field__hint radios__hint">{{ o.hint }}</p>
    </div>
  </fieldset>
</template>
