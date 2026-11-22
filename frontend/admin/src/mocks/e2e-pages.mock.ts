/* eslint-disable vue/one-component-per-file, vue/component-definition-name-casing -- 夹具文件：每个夹具页一个具名占位组件 */
import { defineComponent, h, ref, type Component } from 'vue';
import type { AdminI18n } from '../i18n';
import type { PageDef } from '../registry/types';
import { useTabDirty } from '../composables/useTabDirty';

// 仅用于 e2e 构建（vite --mode e2e → dist/admin-e2e）的夹具页面。生产构建不含本文件（动态 import 受 import.meta.env.MODE === 'e2e' 保护，check:mock 校验）。
// 它们存在的唯一目的：让真实浏览器 E2E 能对「真实 server + 真实 /auth/me」验证外壳在**没有对应真实页面**时的行为——
// 模块未交付（nodelist、sched）、仅商业模式（billing）、复杂编辑页的手机标注（rules）、未保存更改（dirty），以及节点/任务类页面（Story 1.39 之后真实页面仍未交付的菜单组：
// tasks、rollback、maintenance）。Story 1.39 起用户/角色/设置/审计都是**真实页面**（生产构建里可用，登记在 registry/pages.ts），这里不再提供它们的占位页：
// 外壳的菜单/搜索/403/标签/手机宽度的 E2E 都直接对真实页面验证。页面本身只是带键名的占位内容；模块键与权限点取自真实后端
// （模块是否已交付由真实 /auth/me 决定，夹具不能改变它）。
// 标题全部是英文夹具文案（不是产品文案，不进 locales）。

const page = (key: string, extra?: () => ReturnType<typeof h>[]): (() => Promise<Component>) => {
  return () =>
    Promise.resolve(
      defineComponent({
        name: `E2EFixture_${key}`,
        setup: () => () => h('section', { 'data-testid': 'fixture-page', 'data-page': key }, [h('h1', `E2E fixture page: ${key}`), ...(extra ? extra() : [])]),
      }),
    );
};

const dirtyPage = (): Promise<Component> =>
  Promise.resolve(
    defineComponent({
      name: 'E2EFixture_dirty',
      setup() {
        const { setDirty } = useTabDirty();
        const text = ref('');
        return () =>
          h('section', { 'data-testid': 'fixture-page', 'data-page': 'fx.dirty' }, [
            h('h1', 'E2E fixture page: fx.dirty'),
            h('input', {
              'data-testid': 'dirty-input',
              'aria-label': 'dirty input',
              value: text.value,
              onInput: (e: Event) => {
                text.value = (e.target as HTMLInputElement).value;
                setDirty(text.value !== '');
              },
            }),
          ]);
      },
    }),
  );

const T = 'fx.title.';

export function bootE2EPages(i18n: AdminI18n): PageDef[] {
  console.warn('LE_MOCK_ENTRY e2e fixture pages active');
  const titles: Record<string, string> = {
    tasks: 'Task status and alerts',
    rollback: 'Trigger rollback',
    maintenance: 'Node maintenance',
    nodelist: 'Node list (module not delivered)',
    rules: 'Rule editor (complex editing)',
    billing: 'Billing orders (commercial only)',
    sched: 'Scheduling policies (module not delivered)',
    dirty: 'Dirty form',
  };
  const msgs = Object.fromEntries(Object.entries(titles).map(([k, v]) => [k, v]));
  for (const l of ['zh-CN', 'en'] as const) i18n.global.mergeLocaleMessage(l, { fx: { title: msgs } });
  const p = (k: string, path: string, group: PageDef['group'], module: string, permissions: string[], more: Partial<PageDef> = {}): PageDef => ({
    key: `fx.${k}`,
    path,
    group,
    titleKey: `${T}${k}`,
    module,
    permissions,
    component: page(`fx.${k}`),
    ...more,
  });
  return [
    p('tasks', '/ops/tasks', 'ops', 'tasks', ['task:read']),
    p('rollback', '/ops/rollback', 'ops', 'tasks', ['config:rollback']),
    p('dirty', '/ops/dirty-form', 'ops', 'tasks', ['task:read'], { component: dirtyPage }),
    p('maintenance', '/nodes/maintenance', 'nodes', 'system', ['node:manage']),
    p('nodelist', '/nodes/list', 'nodes', 'nodes', ['node:read']),
    p('rules', '/settings/rule-editor', 'settings', 'system', ['setting:read'], { complexEdit: true }),
    p('billing', '/billing/orders', 'billing', 'auth', ['billing:read'], { runModes: ['commercial'] }),
    p('sched', '/scheduling/policies', 'scheduling', 'scheduler', ['scheduling:read']),
  ];
}
