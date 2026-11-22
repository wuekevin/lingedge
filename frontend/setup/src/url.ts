// setup token 不得出现在 URL 中：若地址栏带了 token 类参数/片段，丢弃并从地址栏移除（历史记录里也不保留），提示安装者改为在页面里输入。
const TOKEN_PARAMS = ['token', 'setup_token', 'setup-token', 'setuptoken', 'x-setup-token'];

export function stripTokenFromUrl(win: Pick<Window, 'location' | 'history'> = window): boolean {
  const url = new URL(win.location.href);
  let found = false;
  for (const k of [...url.searchParams.keys()]) {
    if (TOKEN_PARAMS.includes(k.toLowerCase())) {
      url.searchParams.delete(k);
      found = true;
    }
  }
  if (url.hash && /token/i.test(url.hash)) {
    url.hash = '';
    found = true;
  }
  if (found) win.history.replaceState(null, '', url.pathname + url.search + url.hash);
  return found;
}
