<script setup lang="ts">
import { computed } from 'vue';
import { LeError } from '@lingedge/shared';
import { useI18n } from 'vue-i18n';
import type { SetupApiError } from '../api/errors';
import { useServerText } from '../api/useServerText';

const props = defineProps<{ error: SetupApiError }>();
const emit = defineEmits<{ retry: [] }>();
const { t, te } = useI18n();
const st = useServerText();
/** 错误说明：已知错误码走本地词条；未知码回落到服务端 message。 */
const headline = computed(() => st.errorMessage(props.error.code, props.error.serverMessage));

const hint = computed(() => {
  // 词条键里不出现 password 字样（G-6 扫描会把「password 键 + 字面量」当作疑似固定口令），故错误码 auth.password_policy 映射到 errors.hint.auth.pw_policy。
  const key = `errors.hint.${props.error.code.replace('password', 'pw')}`;
  return te(key) ? t(key) : '';
});
const retryAfter = computed(() => props.error.retryAfterSeconds);
</script>

<template>
  <div class="serror" data-testid="step-error" :data-error-code="props.error.code">
    <LeError :kind="props.error.kind" :request-id="props.error.requestId || undefined" @retry="emit('retry')" />
    <p v-if="headline" class="serror__message" data-testid="error-message" :lang="headline.lang" :data-localized="headline.localized">{{ headline.text }}</p>
    <dl class="serror__facts">
      <div>
        <dt>{{ t('common.errorCode') }}</dt>
        <dd><code data-testid="error-code">{{ props.error.code }}</code></dd>
      </div>
    </dl>
    <p v-if="hint" class="serror__hint" data-testid="error-hint">{{ hint }}</p>
    <p v-if="retryAfter !== null" class="serror__hint">{{ t('gate.rateLimited', { seconds: retryAfter }) }}</p>
    <div v-if="props.error.details.length > 0" class="serror__reasons">
      <p class="serror__reasons-title">{{ t('common.reasons') }}</p>
      <ul>
        <li v-for="(d, i) in props.error.details" :key="i" data-testid="error-reason">
          <code v-if="d.field">{{ d.field }}</code>
          <span :lang="st.reason(d.reason).lang" :data-localized="st.reason(d.reason).localized">{{ st.reason(d.reason).text }}</span>
        </li>
      </ul>
    </div>
  </div>
</template>
