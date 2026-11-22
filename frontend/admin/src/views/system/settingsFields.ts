// 系统设置表单的字段定义与纯函数（校验规则与后端 sysconfig 注册表一一对应；服务端仍是最终裁决，前端只给出同一规则的即时提示）。
export type FieldKind = 'url' | 'text' | 'email' | 'color' | 'int' | 'bool' | 'locales' | 'timezone';
export interface FieldDef {
  /** 设置键（点分，如 general.base_url）；品牌页使用品牌字段名（name、name_en…）。 */
  key: string;
  kind: FieldKind;
  /** 空值 = null（恢复默认），仅 url。 */
  nullable?: boolean;
  required?: boolean;
  min?: number;
  max?: number;
  maxLen?: number;
}
export type DraftValue = string | boolean | string[];
export type Draft = Record<string, DraftValue>;
export const LOCALES = ['zh-CN', 'en'] as const;

/** 键 → i18n 段（词条键不能含 "."）。 */
export const fieldSeg = (key: string): string => key.replaceAll('.', '_');

export function toDraft(f: FieldDef, v: unknown): DraftValue {
  switch (f.kind) {
    case 'bool':
      return v === true;
    case 'locales':
      return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [...LOCALES];
    case 'int':
      return typeof v === 'number' ? String(v) : '';
    default:
      return typeof v === 'string' ? v : '';
  }
}

/** 草稿 → 提交值；无法转换（校验未通过）时返回 undefined。 */
export function fromDraft(f: FieldDef, d: DraftValue | undefined): unknown {
  switch (f.kind) {
    case 'bool':
      return d === true;
    case 'locales':
      return Array.isArray(d) ? [...LOCALES].filter((l) => d.includes(l)) : [];
    case 'int': {
      const s = typeof d === 'string' ? d.trim() : '';
      return /^-?\d+$/.test(s) ? Number(s) : undefined;
    }
    default: {
      const s = typeof d === 'string' ? d.trim() : '';
      return s === '' && f.nullable ? null : s;
    }
  }
}

const validUrl = (s: string): boolean => {
  try {
    const u = new URL(s);
    return u.protocol === 'http:' || u.protocol === 'https:';
  } catch {
    return false;
  }
};
const validTimezone = (s: string): boolean => {
  try {
    new Intl.DateTimeFormat('en', { timeZone: s });
    return true;
  } catch {
    return false;
  }
};

export type Problem = { key: string; named?: Record<string, unknown> } | null;

/** 返回 i18n 键（form.rule.*）与占位参数；通过返回 null。 */
export function validateField(f: FieldDef, d: DraftValue | undefined): Problem {
  switch (f.kind) {
    case 'bool':
      return null;
    case 'locales':
      return Array.isArray(d) && d.length > 0 ? null : { key: 'form.rule.locales' };
    case 'int': {
      const s = typeof d === 'string' ? d.trim() : '';
      if (s === '') return { key: 'form.rule.required' };
      if (!/^-?\d+$/.test(s)) return { key: 'form.rule.integer' };
      const n = Number(s);
      if ((f.min !== undefined && n < f.min) || (f.max !== undefined && n > f.max)) return { key: 'form.rule.range', named: { min: f.min ?? '', max: f.max ?? '' } };
      return null;
    }
    default: {
      const s = typeof d === 'string' ? d.trim() : '';
      if (s === '') return f.required ? { key: 'form.rule.required' } : null;
      if (f.maxLen !== undefined && s.length > f.maxLen) return { key: 'form.rule.maxLength', named: { max: f.maxLen } };
      if (f.kind === 'url' && !validUrl(s)) return { key: 'form.rule.url' };
      if (f.kind === 'email' && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(s)) return { key: 'form.rule.email' };
      if (f.kind === 'color' && !/^#[0-9A-Fa-f]{6}$/.test(s)) return { key: 'form.rule.color' };
      if (f.kind === 'timezone' && !validTimezone(s)) return { key: 'form.rule.timezone' };
      return null;
    }
  }
}

/** 与原始值不同的字段（按提交值比较；null 与空串等价）。 */
export function changedFields(fields: readonly FieldDef[], draft: Draft, original: Draft): FieldDef[] {
  return fields.filter((f) => JSON.stringify(fromDraft(f, draft[f.key])) !== JSON.stringify(fromDraft(f, original[f.key])));
}
