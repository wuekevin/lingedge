<script setup lang="ts">
import { Message } from '@arco-design/web-vue';
import { LeStateBoundary } from '@lingedge/shared';
import { computed, onMounted, reactive, ref, watch } from 'vue';
import { useI18n } from 'vue-i18n';
import { isApiError } from '../../api/errors';
import { errorText } from '../../api/messages';
import { useAction } from '../../composables/useAction';
import { useAsyncState } from '../../composables/useAsyncState';
import { useTabDirty } from '../../composables/useTabDirty';
import { LOCALES, changedFields, fieldSeg, fromDraft, toDraft, validateField, type Draft, type DraftValue, type FieldDef } from './settingsFields';

// 通用设置表单：加载 → 编辑（即时校验）→ 保存（只提交变更的键）。基本/品牌/登录设置共用；
// 未保存更改会标记当前标签（useTabDirty）；无写权限时只读；保存失败按错误码如实提示（再认证由 API 客户端统一处理）。
const props = defineProps<{
  fields: readonly FieldDef[];
  /** 词条前缀：<prefix>.field.<键> / <prefix>.hint.<键>。 */
  i18nPrefix: string;
  canWrite: boolean;
  load: (signal: AbortSignal) => Promise<Record<string, unknown>>;
  /** changed = 变更的键 → 值；full = 全部字段的当前提交值（整体替换型端点用）。返回保存后的服务端值。 */
  save: (changed: Record<string, unknown>, full: Record<string, unknown>) => Promise<Record<string, unknown>>;
  /** 跨字段约束：返回 键 → 错误词条键。 */
  cross?: (values: Record<string, unknown>) => Record<string, string>;
  testid: string;
}>();
const emit = defineEmits<{ saved: [values: Record<string, unknown>] }>();
const { t, te } = useI18n();
const { setDirty } = useTabDirty();

const s = useAsyncState(props.load);
const draft = reactive<Draft>({});
const original = ref<Draft>({});
const touched = ref(false);
const savedOnce = ref(false);
const action = useAction();

function fill(values: Record<string, unknown>): void {
  const d: Draft = {};
  for (const f of props.fields) d[f.key] = toDraft(f, values[f.key]);
  original.value = JSON.parse(JSON.stringify(d)) as Draft;
  for (const f of props.fields) draft[f.key] = Array.isArray(d[f.key]) ? [...(d[f.key] as string[])] : (d[f.key] as DraftValue);
  touched.value = false;
}
watch(s.data, (v) => {
  if (v) fill(v);
});
onMounted(() => void s.run());

const label = (f: FieldDef): string => t(`${props.i18nPrefix}.field.${fieldSeg(f.key)}`);
const hint = (f: FieldDef): string => {
  const k = `${props.i18nPrefix}.hint.${fieldSeg(f.key)}`;
  return te(k) ? t(k) : '';
};
const problems = computed<Record<string, string>>(() => {
  const out: Record<string, string> = {};
  for (const f of props.fields) {
    const p = validateField(f, draft[f.key]);
    if (p) out[f.key] = t(p.key, { field: label(f), ...(p.named ?? {}) });
  }
  if (props.cross && Object.keys(out).length === 0) {
    const values: Record<string, unknown> = {};
    for (const f of props.fields) values[f.key] = fromDraft(f, draft[f.key]);
    for (const [k, msgKey] of Object.entries(props.cross(values))) out[k] = t(msgKey);
  }
  return out;
});
const changed = computed(() => changedFields(props.fields, draft, original.value));
const dirty = computed(() => changed.value.length > 0);
watch(dirty, (v) => setDirty(v));
const valid = computed(() => Object.keys(problems.value).length === 0);
const showProblem = (key: string): string => (touched.value ? (problems.value[key] ?? '') : '');

function setValue(key: string, v: DraftValue): void {
  draft[key] = v;
  onEdit();
}
function onEdit(): void {
  touched.value = true;
  savedOnce.value = false;
}
function setLocale(key: string, l: string, on: boolean): void {
  const cur = new Set((draft[key] as string[]) ?? []);
  if (on) cur.add(l);
  else cur.delete(l);
  draft[key] = [...LOCALES].filter((x) => cur.has(x));
  onEdit();
}
function reset(): void {
  if (s.data.value) fill(s.data.value);
  action.reset();
}
async function submit(): Promise<void> {
  touched.value = true;
  if (!valid.value || !dirty.value) return;
  const full: Record<string, unknown> = {};
  for (const f of props.fields) full[f.key] = fromDraft(f, draft[f.key]);
  const diff: Record<string, unknown> = {};
  for (const f of changed.value) diff[f.key] = full[f.key];
  const r = await action.run(() => props.save(diff, full));
  if (!r.ok) return;
  s.data.value = r.value;
  fill(r.value);
  setDirty(false);
  savedOnce.value = true;
  Message.success(t('settings.saved'));
  emit('saved', r.value);
}
const errMsg = computed(() => (action.error.value ? errorText(t, te, action.error.value) : ''));
const errFields = computed(() => (isApiError(action.error.value) ? action.error.value.details.map((d) => d.field).filter(Boolean).join(', ') : ''));
const errRequestId = computed(() => (isApiError(action.error.value) ? action.error.value.requestId : ''));
</script>

<template>
  <LeStateBoundary :status="s.status.value" :error-kind="s.errorKind.value" :request-id="s.requestId.value" @retry="s.run">
    <form class="le-card" :data-testid="testid" novalidate @submit.prevent="submit">
      <slot name="before" />
      <div class="le-form-grid">
        <template v-for="f in fields" :key="f.key">
          <fieldset v-if="f.kind === 'locales'" class="le-field le-fieldset-plain" :data-testid="`field-${f.key}`">
            <legend>{{ label(f) }}</legend>
            <label v-for="l in LOCALES" :key="l" class="le-check">
              <input type="checkbox" :checked="(draft[f.key] as string[])?.includes(l)" :disabled="!canWrite" :data-testid="`field-${f.key}-${l}`" @change="setLocale(f.key, l, ($event.target as HTMLInputElement).checked)" />
              <span>{{ t(l === 'en' ? 'lang.en' : 'lang.zhCN') }}</span>
            </label>
            <span v-if="hint(f)" class="le-dialog__note">{{ hint(f) }}</span>
            <span v-if="showProblem(f.key)" class="le-field__error" role="alert" :data-testid="`field-error-${f.key}`">{{ showProblem(f.key) }}</span>
          </fieldset>
          <div v-else-if="f.kind === 'bool'" class="le-field">
            <label class="le-check">
              <input type="checkbox" :checked="draft[f.key] === true" :disabled="!canWrite" :data-testid="`field-${f.key}`" @change="setValue(f.key, ($event.target as HTMLInputElement).checked)" />
              <span>{{ label(f) }}</span>
            </label>
            <span v-if="hint(f)" class="le-dialog__note">{{ hint(f) }}</span>
          </div>
          <label v-else class="le-field">
            <span>{{ label(f) }}</span>
            <input
              type="text"
              :value="draft[f.key] as string"
              :inputmode="f.kind === 'int' ? 'numeric' : undefined"
              :readonly="!canWrite"
              :aria-invalid="showProblem(f.key) !== ''"
              autocomplete="off"
              spellcheck="false"
              :data-testid="`field-${f.key}`"
              @input="setValue(f.key, ($event.target as HTMLInputElement).value)"
            />
            <span v-if="hint(f)" class="le-dialog__note">{{ hint(f) }}</span>
            <span v-if="showProblem(f.key)" class="le-field__error" role="alert" :data-testid="`field-error-${f.key}`">{{ showProblem(f.key) }}</span>
          </label>
        </template>
      </div>
      <slot name="after" />
      <div v-if="action.error.value" class="le-dialog__error" role="alert" data-testid="settings-error">
        <span>{{ errMsg }}</span>
        <span v-if="errFields">{{ errFields }}</span>
        <span v-if="errRequestId">{{ t('errors.requestId') }}: <code>{{ errRequestId }}</code></span>
      </div>
      <p v-if="savedOnce" class="le-notice" role="status" data-testid="settings-saved">{{ t('settings.saved') }}</p>
      <div v-if="canWrite" class="le-page__actions">
        <a-button type="primary" html-type="submit" :disabled="!dirty" :loading="action.busy.value" data-testid="settings-save">{{ t('settings.save') }}</a-button>
        <a-button :disabled="!dirty" data-testid="settings-reset" @click="reset">{{ t('settings.reset') }}</a-button>
      </div>
      <p v-else class="le-dialog__note" data-testid="settings-readonly">{{ t('settings.readonly') }}</p>
    </form>
  </LeStateBoundary>
</template>

<style scoped>
.le-fieldset-plain {
  border: 0;
  margin: 0;
  padding: 0;
}
</style>
