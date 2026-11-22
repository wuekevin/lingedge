import { defineStore } from 'pinia';
import type { AdminApi } from '../api/admin';
import type { BrandSettings } from '../api/types';

// 品牌设置（FR-36）：Admin 与官网读取同一份（GET /admin/settings/brand）。顶栏品牌名取自这里；无 setting:read 或读取失败时退回内置词条（不影响使用）。
export const useBrandStore = defineStore('brand', {
  state: () => ({ brand: null as BrandSettings | null }),
  actions: {
    async load(api: AdminApi): Promise<void> {
      try {
        this.brand = await api.getBrand();
      } catch {
        this.brand = null;
      }
    },
    set(b: BrandSettings): void {
      this.brand = b;
    },
    clear(): void {
      this.brand = null;
    },
    /** 按界面语言选品牌名；英文缺失时退回中文名。 */
    nameFor(locale: string): string {
      const b = this.brand;
      if (!b) return '';
      return (locale === 'en' ? b.name_en || b.name : b.name || b.name_en) ?? '';
    },
  },
});
