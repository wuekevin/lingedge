<script setup lang="ts">
import { useI18n } from 'vue-i18n';
import type { PasswordAssessment, RuleKey } from '../password';

const props = defineProps<{ assessment: PasswordAssessment; id: string }>();
const { t } = useI18n();
const RULES: RuleKey[] = ['length', 'classes', 'distinct', 'noEmail'];
</script>

<template>
  <div :id="props.id" class="strength" data-testid="strength">
    <p class="strength__label">
      <span>{{ t('admin.strength.label') }}:</span>
      <strong data-testid="strength-text" :data-score="props.assessment.score">{{ t(`admin.strength.${props.assessment.score}`) }}</strong>
    </p>
    <div class="strength__bar" aria-hidden="true">
      <span v-for="n in 4" :key="n" class="strength__seg" :class="{ 'strength__seg--on': n <= props.assessment.score, [`strength__seg--s${props.assessment.score}`]: n <= props.assessment.score }" />
    </div>
    <p class="strength__rules-title">{{ t('admin.rule.title') }}</p>
    <ul class="strength__rules">
      <li v-for="r in RULES" :key="r" :data-rule="r" :data-met="props.assessment.rules[r]">
        <span class="strength__mark" aria-hidden="true">{{ props.assessment.rules[r] ? '✓' : '○' }}</span>
        <span>{{ t(`admin.rule.${r}`) }}</span>
        <span class="le-sr-only">{{ props.assessment.rules[r] ? t('admin.rule.met') : t('admin.rule.unmet') }}</span>
      </li>
    </ul>
    <p class="field__hint">{{ t('admin.rule.server') }}</p>
  </div>
</template>
