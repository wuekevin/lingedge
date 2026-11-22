<script setup lang="ts">
import { computed } from 'vue';
import { useI18n } from 'vue-i18n';
import { useAdminApi } from '../../api/context';
import type { BrandSettings } from '../../api/types';
import { useCan } from '../../composables/useCan';
import { useBrandStore } from '../../stores/brand';
import SettingsForm from './SettingsForm.vue';
import type { FieldDef } from './settingsFields';

// 品牌设置（FR-36）：Admin 与官网读取同一份。保存后立即以服务端返回值刷新顶栏品牌名（Admin 读到新值）；官网侧读取在 2.4/2.6 验证。
// logo/favicon 是媒体资源引用（媒体库属官网 CMS，P2）：这里只读展示，保存时原样带回（PUT 是整体替换，缺省会被重置为默认）。
const { t } = useI18n();
const api = useAdminApi();
const brand = useBrandStore();
const can = useCan();
const canWrite = computed(() => can('setting:write'));

const FIELDS: readonly FieldDef[] = [
  { key: 'name', kind: 'text', required: true, maxLen: 64 },
  { key: 'name_en', kind: 'text', required: true, maxLen: 64 },
  { key: 'primary_color', kind: 'color', required: true, maxLen: 7 },
  { key: 'support_email', kind: 'email', maxLen: 254 },
];
let media: Pick<BrandSettings, 'logo_media_uid' | 'favicon_media_uid'> = { logo_media_uid: null, favicon_media_uid: null };
async function load(signal: AbortSignal): Promise<Record<string, unknown>> {
  const b = await api.getBrand(signal);
  media = { logo_media_uid: b.logo_media_uid ?? null, favicon_media_uid: b.favicon_media_uid ?? null };
  return { ...b };
}
async function save(_changed: Record<string, unknown>, full: Record<string, unknown>): Promise<Record<string, unknown>> {
  const b = await api.putBrand({ ...media, ...(full as BrandSettings) });
  media = { logo_media_uid: b.logo_media_uid ?? null, favicon_media_uid: b.favicon_media_uid ?? null };
  brand.set(b);
  return { ...b };
}
</script>

<template>
  <section class="le-page" data-testid="settings-brand-page">
    <header class="le-page__head">
      <h1 class="le-page__title">{{ t('pages.settingsBrand') }}</h1>
    </header>
    <p class="le-page__desc">{{ t('settings.brand.desc') }}</p>
    <SettingsForm testid="settings-brand-form" i18n-prefix="settings.brand" :fields="FIELDS" :can-write="canWrite" :load="load" :save="save">
      <template #after>
        <p class="le-dialog__note" data-testid="brand-media-note">{{ t('settings.brand.mediaNote') }}</p>
      </template>
    </SettingsForm>
  </section>
</template>
