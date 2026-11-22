<script setup lang="ts">
import { computed, useId } from 'vue';

const props = withDefaults(defineProps<{ modelValue: boolean; label: string; name: string; hint?: string; disabled?: boolean }>(), { hint: undefined, disabled: false });
const emit = defineEmits<{ 'update:modelValue': [value: boolean] }>();
const id = useId();
const describedBy = computed(() => (props.hint ? `${id}-hint` : undefined));
</script>

<template>
  <div class="check">
    <input :id="id" class="check__box" type="checkbox" :name="props.name" :data-testid="`c-${props.name}`" :checked="props.modelValue" :disabled="props.disabled" :aria-describedby="describedBy" @change="emit('update:modelValue', ($event.target as HTMLInputElement).checked)" />
    <label class="check__label" :for="id">{{ props.label }}</label>
    <p v-if="props.hint" :id="`${id}-hint`" class="field__hint check__hint">{{ props.hint }}</p>
  </div>
</template>
