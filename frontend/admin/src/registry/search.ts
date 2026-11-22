import type { SearchEntry } from './visibility';

/** 全局搜索过滤：按「页面标题 / 所属菜单 / 关键字」做不区分大小写的子串匹配（已翻译文本）；空查询 = 全部（已交付且可见的页面）。 */
export function filterSearch(entries: readonly SearchEntry[], query: string, tr: (key: string) => string): SearchEntry[] {
  const q = query.trim().toLowerCase();
  if (!q) return [...entries];
  return entries.filter((e) => [tr(e.titleKey), tr(e.groupTitleKey), ...e.keywordKeys.map(tr)].some((s) => s.toLowerCase().includes(q)));
}
