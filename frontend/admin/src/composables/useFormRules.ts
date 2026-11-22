import { useI18n } from 'vue-i18n';

/** 统一表单校验（Arco Form 的 rules 形态）：文案全部来自 i18n（form.rule.*），校验时机统一为失焦/提交。 */
export function useFormRules() {
  const { t } = useI18n();
  type Validator = (value: unknown, callback: (error?: string) => void) => void;
  const required = (field: string) => ({ required: true, message: t('form.rule.required', { field }) });
  const email = (field: string): { validator: Validator } => ({
    validator: (v, cb) => {
      const s = typeof v === 'string' ? v.trim() : '';
      if (s && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(s)) cb(t('form.rule.email', { field }));
      else cb();
    },
  });
  const minLength = (field: string, min: number): { validator: Validator } => ({
    validator: (v, cb) => {
      if (typeof v === 'string' && v.length > 0 && v.length < min) cb(t('form.rule.minLength', { field, min }));
      else cb();
    },
  });
  const maxLength = (field: string, max: number): { validator: Validator } => ({
    validator: (v, cb) => {
      if (typeof v === 'string' && v.length > max) cb(t('form.rule.maxLength', { field, max }));
      else cb();
    },
  });
  const sameAs = (field: string, other: () => string): { validator: Validator } => ({
    validator: (v, cb) => {
      if (typeof v === 'string' && v.length > 0 && v !== other()) cb(t('form.rule.same', { field }));
      else cb();
    },
  });
  return { required, email, minLength, maxLength, sameAs };
}
