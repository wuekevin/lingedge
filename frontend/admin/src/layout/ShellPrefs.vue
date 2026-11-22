<script setup lang="ts">
import { LOCALES, resolveLocale } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import { useLocale } from '../composables/useLocale';
import { useTheme } from '../composables/useTheme';
import { THEME_PREFS, parseThemePref } from '../theme/theme';

// 语言与主题（登录页与顶栏共用）：原生 select——键盘/触控/读屏天然可用，颜色全部来自令牌。
const { t } = useI18n();
const { locale, setLocale } = useLocale();
const { pref, setPref } = useTheme();
</script>

<template>
  <div class="le-prefs">
    <label class="le-prefs__item">
      <span class="le-prefs__label">{{ t('lang.label') }}</span>
      <select class="le-select" data-testid="lang-select" :value="locale" @change="(e) => setLocale(resolveLocale((e.target as HTMLSelectElement).value))">
        <option v-for="l in LOCALES" :key="l" :value="l">{{ t(`lang.${l === 'zh-CN' ? 'zhCN' : l}`) }}</option>
      </select>
    </label>
    <label class="le-prefs__item">
      <span class="le-prefs__label">{{ t('theme.label') }}</span>
      <select class="le-select" data-testid="theme-select" :value="pref" @change="(e) => setPref(parseThemePref((e.target as HTMLSelectElement).value))">
        <option v-for="p in THEME_PREFS" :key="p" :value="p">{{ t(`theme.${p}`) }}</option>
      </select>
    </label>
  </div>
</template>

<style scoped>
.le-prefs {
  display: flex;
  gap: var(--le-space-3);
  align-items: center;
}
.le-prefs__item {
  display: inline-flex;
  align-items: center;
  gap: var(--le-space-1);
}
.le-prefs__label {
  position: absolute;
  width: 1px;
  height: 1px;
  overflow: hidden;
  clip-path: inset(50%);
  white-space: nowrap;
}
.le-select {
  height: 32px;
  padding: 0 var(--le-space-2);
  border: 1px solid var(--le-color-border-strong);
  border-radius: var(--le-radius-md);
  background: var(--le-color-bg-surface);
  color: var(--le-color-text-primary);
  font: inherit;
}
.le-select:focus-visible {
  outline: 2px solid var(--le-color-brand-primary);
  outline-offset: 2px;
}
</style>
