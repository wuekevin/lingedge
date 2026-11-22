// 主题切换：只设置属性，亮/暗由 tokens.css / arco-theme.css 的 CSS 变量完成，组件内不写主题分支。
export type LeTheme = 'light' | 'dark';

/** 同时写 <html data-theme> 与 <body arco-theme>（Arco 的暗色开关）。 */
export function applyTheme(theme: LeTheme, doc: Document = document): void {
  doc.documentElement.setAttribute('data-theme', theme);
  if (theme === 'dark') doc.body.setAttribute('arco-theme', 'dark');
  else doc.body.removeAttribute('arco-theme');
}
