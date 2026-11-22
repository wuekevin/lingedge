import { onActivated } from 'vue';

/**
 * 页面随标签保活（KeepAlive）：切回标签时重新读取当前数据，避免在别的标签里做了修改（如创建了用户/令牌）后这里仍显示旧列表。
 * 首次挂载时的激活不重复读取（页面自己已经在 onMounted 里读取）。表单类页面不使用它，避免覆盖用户尚未保存的编辑。
 */
export function useRefreshOnActivate(refresh: () => void): void {
  let first = true;
  onActivated(() => {
    if (first) {
      first = false;
      return;
    }
    refresh();
  });
}
