import { useI18n } from 'vue-i18n';
import { codeKey, langOf, localizeCheck, localizeReason, localizeStepMessage, type MsgPart } from './servermsg';

/** 一段要显示的文字：localized=true 表示来自本地词条（随界面语言）；false 表示回落到服务端原文（lang 为原文语言）。 */
export interface ServerText {
  text: string;
  lang: string | undefined;
  localized: boolean;
}

/** 把服务端的错误码/检查项/步骤回执/reason 渲染成当前界面语言；未知内容回落到服务端原文。 */
export function useServerText() {
  const { t, te, locale } = useI18n();

  const render = (p: MsgPart): string => {
    const params = { ...(p.params ?? {}) };
    // 步骤键（complete step X first）翻译成步骤名
    if (p.id === 'reason.stepFirst' && params.step && te(`steps.${params.step}`)) params.step = t(`steps.${params.step}`);
    return t(`serverMsg.${p.id}`, params);
  };
  const raw = (text: string): ServerText => ({ text, lang: langOf(text), localized: false });
  const own = (text: string): ServerText => ({ text, lang: undefined, localized: true });

  return {
    /** 错误标题：词条 errors.code.<code>；未知码回落到服务端 message。 */
    errorMessage(code: string, serverMessage: string): ServerText | null {
      const key = `errors.code.${codeKey(code)}`;
      if (te(key)) return own(t(key));
      return serverMessage ? raw(serverMessage) : null;
    },
    check(item: { key: string; message: string }): ServerText {
      const p = localizeCheck(item);
      return p ? own(render(p)) : raw(item.message);
    },
    step(step: string, message: string): ServerText | null {
      if (!message) return null;
      const parts = localizeStepMessage(step, message);
      if (!parts) return raw(message);
      return own(parts.map(render).join(locale.value === 'zh-CN' ? '' : ' '));
    },
    reason(reason: string): ServerText {
      const p = localizeReason(reason);
      return p ? own(render(p)) : raw(reason);
    },
  };
}
