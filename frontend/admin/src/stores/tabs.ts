import { defineStore } from 'pinia';

// 多标签页（EXPERIENCE.md · 多标签页）：同一页面（同参数）不重复开标签；关闭有未保存更改的标签须先确认；最多 12 个，超出时关闭最久未用的「无更改」标签；
// 刷新页面恢复标签但不恢复未保存表单（只存路径与标题键，不含任何业务数据）；切换身份/退出登录时关闭全部标签（防跨租户残留视图）。

export const MAX_TABS = 12;
/** 首页标签固定存在、不可关闭。 */
export const HOME_PATH = '/';
const STORAGE_KEY = 'le.admin.tabs';

export interface Tab {
  /** 路由 fullPath（含参数）；同一 fullPath 只有一个标签。 */
  fullPath: string;
  titleKey: string;
  dirty: boolean;
  /** 最近一次激活的时间戳（毫秒），用于淘汰最久未用的标签。 */
  usedAt: number;
}

export type CloseResult = 'closed' | 'needs-confirm' | 'not-found';

function storage(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

export const useTabsStore = defineStore('tabs', {
  state: () => ({ tabs: [] as Tab[], active: '' }),
  actions: {
    /** 打开或激活；返回被淘汰的标签（超出上限时）。 */
    open(fullPath: string, titleKey: string, now: number = Date.now()): Tab[] {
      const evicted: Tab[] = [];
      const existing = this.tabs.find((t) => t.fullPath === fullPath);
      if (existing) {
        existing.usedAt = now;
        existing.titleKey = titleKey;
      } else {
        this.tabs.push({ fullPath, titleKey, dirty: false, usedAt: now });
        while (this.tabs.length > MAX_TABS) {
          const candidates = this.tabs.filter((t) => !t.dirty && t.fullPath !== fullPath).sort((a, b) => a.usedAt - b.usedAt);
          const victim = candidates[0];
          if (!victim) break; // 全部有未保存更改：不强行关闭任何一个
          this.tabs = this.tabs.filter((t) => t !== victim);
          evicted.push(victim);
        }
      }
      this.active = fullPath;
      this.persist();
      return evicted;
    },
    /** 首页标签固定在最前。 */
    ensureHome(): void {
      if (this.tabs.some((x) => x.fullPath === HOME_PATH)) return;
      this.tabs.unshift({ fullPath: HOME_PATH, titleKey: 'home.title', dirty: false, usedAt: 0 });
      this.persist();
    },
    setDirty(fullPath: string, dirty: boolean): void {
      const t = this.tabs.find((x) => x.fullPath === fullPath);
      if (t) t.dirty = dirty;
    },
    /** 关闭一个标签：有未保存更改且未 force → 需确认。返回关闭后应激活的标签路径（由调用方导航）。 */
    close(fullPath: string, force = false): CloseResult {
      const t = this.tabs.find((x) => x.fullPath === fullPath);
      if (!t || fullPath === HOME_PATH) return 'not-found';
      if (t.dirty && !force) return 'needs-confirm';
      this.tabs = this.tabs.filter((x) => x !== t);
      this.persist();
      return 'closed';
    },
    /** 关闭其他：返回因有未保存更改而保留的标签数。 */
    closeOthers(keep: string, force = false): number {
      const kept = this.tabs.filter((t) => t.fullPath === keep || t.fullPath === HOME_PATH || (t.dirty && !force));
      const blocked = kept.filter((t) => t.fullPath !== keep && t.fullPath !== HOME_PATH).length;
      this.tabs = kept;
      this.persist();
      return blocked;
    },
    hasDirty(): boolean {
      return this.tabs.some((t) => t.dirty);
    },
    /** 切换身份/退出登录：关闭全部标签并清除持久化。 */
    closeAll(): void {
      this.tabs = [];
      this.active = '';
      try {
        storage()?.removeItem(STORAGE_KEY);
      } catch {
        /* 忽略 */
      }
    },
    /** 刷新后恢复：只恢复仍然可访问的标签（accept 由路由可见性判定）；标签内容（表单）不恢复。 */
    restore(accept: (fullPath: string) => string | null): void {
      let saved: { fullPath: string; titleKey: string }[] = [];
      try {
        const raw = storage()?.getItem(STORAGE_KEY);
        if (raw) saved = JSON.parse(raw) as typeof saved;
      } catch {
        saved = [];
      }
      const now = Date.now();
      this.tabs = saved
        .filter((s) => typeof s?.fullPath === 'string' && typeof s?.titleKey === 'string')
        .map((s, i) => ({ fullPath: s.fullPath, titleKey: accept(s.fullPath) ?? '', dirty: false, usedAt: now - (saved.length - i) }))
        .filter((t) => t.titleKey !== '')
        .slice(0, MAX_TABS);
    },
    persist(): void {
      try {
        storage()?.setItem(STORAGE_KEY, JSON.stringify(this.tabs.map((t) => ({ fullPath: t.fullPath, titleKey: t.titleKey }))));
      } catch {
        /* 存储不可用：仅内存 */
      }
    },
  },
});
