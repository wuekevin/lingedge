// i18n 工具（zh-CN / en）。运行时用 Vue I18n（ADR-014 §1）；本模块只提供：语言枚举与解析、共享词条、
// 与应用词条深合并的工厂。缺键由 CI（scripts/check-i18n.mjs）检测，运行时缺键回退到 en，仍缺则显示键本身并通过 onMissing 上报。
import { createI18n } from 'vue-i18n';
import en from './locales/en.json';
import zhCN from './locales/zh-CN.json';

export const LOCALES = ['zh-CN', 'en'] as const;
export type LeLocale = (typeof LOCALES)[number];
export const DEFAULT_LOCALE: LeLocale = 'zh-CN';
export const FALLBACK_LOCALE: LeLocale = 'en';

export interface MessageTree {
  [key: string]: string | MessageTree;
}

export const sharedMessages: Record<LeLocale, MessageTree> = { 'zh-CN': zhCN, en };

/** 把浏览器/配置里的语言标签归一到受支持的语言；无法识别时回退到默认语言。 */
export function resolveLocale(input: string | null | undefined): LeLocale {
  if (!input) return DEFAULT_LOCALE;
  const tag = input.trim().toLowerCase().replace('_', '-');
  if (tag === 'zh' || tag.startsWith('zh-')) return 'zh-CN';
  if (tag === 'en' || tag.startsWith('en-')) return 'en';
  return DEFAULT_LOCALE;
}

/** 深合并：后者覆盖前者；不修改入参。 */
export function mergeMessages(base: MessageTree, extra: MessageTree): MessageTree {
  const out: MessageTree = { ...base };
  for (const [k, v] of Object.entries(extra)) {
    const cur = out[k];
    out[k] = typeof v === 'object' && typeof cur === 'object' ? mergeMessages(cur, v) : v;
  }
  return out;
}

/** 把词条树压平成 "a.b.c" -> 文案（脚本与测试用）。 */
export function flattenMessages(tree: MessageTree, prefix = ''): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(tree)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (typeof v === 'string') out[key] = v;
    else Object.assign(out, flattenMessages(v, key));
  }
  return out;
}

export interface CreateLeI18nOptions {
  locale?: string | null;
  /** 应用自己的词条（覆盖/扩展共享词条）。 */
  messages?: Partial<Record<LeLocale, MessageTree>>;
  /** 缺键回调：回退链上每一级未命中都会调用（locale 为未命中的那一级）；最后一级 FALLBACK_LOCALE 仍未命中即全局缺键。 */
  onMissing?: (locale: string, key: string) => void;
}

export function createLeI18n(options: CreateLeI18nOptions = {}) {
  const messages: Record<string, MessageTree> = {};
  for (const l of LOCALES) messages[l] = mergeMessages(sharedMessages[l], options.messages?.[l] ?? {});
  return createI18n({
    legacy: false,
    locale: resolveLocale(options.locale),
    fallbackLocale: FALLBACK_LOCALE,
    messages,
    missingWarn: false,
    fallbackWarn: false,
    missing: (locale, key) => {
      options.onMissing?.(String(locale), key);
      return key;
    },
  });
}
