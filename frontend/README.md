# LingEdge（凌界云）前端 workspace

> Story 1.36 交付的基座（ADR-014）。本文件是前端工程约定的唯一入口：目录、命令、**兼容矩阵**、依赖与供应链规则、`shared` 边界、设计令牌、三态/格式化/i18n 约定、类型生成、构建隔离与 mock 检查。

## 目录

```
frontend/
├── package.json            根（private）：packageManager 固定、检查脚本入口
├── pnpm-workspace.yaml     工作区成员 + ignoreScripts（关闭生命周期脚本）
├── pnpm-lock.yaml          入库；CI 用 --frozen-lockfile
├── .npmrc                  save-exact / registry（以及旧工具链兜底的 ignore-scripts）
├── shared/                 无权限语义的共享包（令牌、三态组件、格式化、i18n、生成类型）
│   ├── tokens/tokens.json  设计令牌【单一来源】；tokens/generated/ 为生成物（入库、禁手改）
│   └── src/                components/ format/ i18n/ state/ api-types/（生成）
├── admin/ console/         互相独立的 Vite 工程（基座探针：只验证 shared 与 Arco 能构建；壳/路由/菜单由 1.38 起交付）
├── website/                占位（Nuxt 3 官网，Story 2.1）
├── isolation/              admin-route-prefixes.json（Admin 路由前缀清单，1.38 起随路由表同步登记）
└── scripts/                全部门禁脚本与其自测（*.test.mjs）
```

## 命令

在仓库根用 Make（已并入既有目标）：

| 目标 | 前端部分 |
|---|---|
| `make lint` | `frontend-lint`：依赖锁定、架构门禁、ESLint、令牌一致/无漂移、对比度、i18n 缺键、vue-tsc |
| `make test` | `frontend-test`：shared 的 vitest（三态组件、格式化、i18n） |
| `make gate-selftest` | `frontend-gate-selftest`：门禁脚本的正向/负向自测（含真实 vite 构建 e2e、pnpm 脚本关闭实测） |
| `make contract-check` | `frontend-types-check`：OpenAPI 生成类型漂移即失败 |
| `make verify-build` | `frontend`：admin/console 生产构建 + Console/Admin 构建隔离 + mock 入口检查 |
| `make frontend-check` | 以上全部（也是 `pnpm -C frontend run check`） |

等价的 pnpm 脚本：`check:deps` `lint:guards` `lint:eslint` `check:tokens` `check:contrast` `check:i18n` `check:types` `typecheck` `test` `build` `check:isolation` `check:mock`；生成：`gen:tokens`、`gen:types`。

首次：`pnpm -C frontend install --frozen-lockfile`（Node ≥ 26，pnpm 版本由 `packageManager` 固定；corepack 或 `npm i -g pnpm@12.18.0`）。

## 兼容矩阵（2026-11-22，本机真实验证）

验证环境：macOS arm64，Node v26.8.2，pnpm 12.18.0，registry 为公共 npm（`https://registry.npmjs.org/`）。方法：以 `npm view` 读取各包 `peerDependencies`/`engines`，用 `pnpm peers check`（无未满足/缺失 peer）、真实 `pnpm install --frozen-lockfile`、`vue-tsc`、`vitest`、`vite build`、ESLint 全部跑通为准；**不取 registry 最新**。

| 包 | 精确版本 | 选择依据与证据 |
|---|---|---|
| Node.js | 26.8.2（`engines: >=26.0.0`） | vitest 5.1.1 要求 `^22.12 \|\| ^24 \|\| >=26`；vite 8.3.8 要求 `^20.19 \|\| >=22.12`；本机仅在 26.8.2 上验证，故下限写 26 |
| pnpm | 12.18.0（`packageManager`） | 本机实装版本，与 Node 26 实测可用；dev-environment 的“查询值 12.21.0”未采用（未在本机验证）。**pnpm 12 行为变化见下节** |
| vue | 3.5.46 | 基线 Vue 3；plugin-vue 6.0.11 peer `^3.2.25 \|\| ^3.6.0-rc`、vue-i18n 11.4.23 peer `^3.0.0`、Arco 2.59.0 peer `>=3.1` 均满足 |
| vite | 8.3.8 | @vitejs/plugin-vue 6.0.11 peer `^5\|\|^6\|\|^7\|\|^8`、vitest 5.1.1 peer `^6.4\|\|^7\|\|^8` 均满足；admin/console 真实 `vite build` 通过 |
| @vitejs/plugin-vue | 6.0.11 | 同上 |
| **typescript** | **5.9.3（有意不取最新 7.0.2）** | npm 最新是 TypeScript 7.0.2，但：`typescript-eslint@8.73.0` peer `typescript >=4.8.4 <6.1.0`；`openapi-typescript@7.13.0` peer `typescript ^5.x`。5.9.3 同时满足二者与 `vue-tsc@3.3.14`（peer `>=5.0.0`）。升级到 6/7 须先等这两个工具放行，并重跑本矩阵 |
| vue-tsc | 3.3.14 | 与 TS 5.9.3 实测通过（admin/console/shared 三个 `vue-tsc --noEmit`；注入类型错误会被检出） |
| @arco-design/web-vue | 2.59.0 | peer vue `>=3.1`；admin/console 构建整包引入 + `arco-theme.css` 覆盖变量；`scripts/tokens.test.mjs` 断言我们覆盖的**每个**变量都是 Arco 2.59 自己定义的变量名，且色阶为 Arco 的 `r,g,b` 三元组格式 |
| Arco Pro（`arco-design-pro-vue`） | 未引入 | 它是脚手架模板而非运行时库；Admin 壳（布局/侧栏/多标签）在 Story 1.38 引入时，其自带依赖逐项对本矩阵核对，不得盲目升级 |
| vue-i18n | 11.4.23 | 基线固定；node `>=22`；`createLeI18n` 在 vitest 中验证回退/缺键行为 |
| vitest | 5.1.1 | + @vue/test-utils 2.6.0 + happy-dom 21.0.4（组件测试）；`projects` 配置：`shared`（happy-dom）与 `scripts`（node） |
| eslint / eslint-plugin-vue / typescript-eslint / vue-eslint-parser | 10.15.0 / 10.12.1 / 8.73.0 / 10.4.2 | plugin-vue peer `eslint ^8.57\|\|^9\|\|^10`、ts-eslint peer `eslint ^8.57\|\|^9\|\|^10`；`eslint . --max-warnings 0` 通过 |
| openapi-typescript | 7.13.0 | OpenAPI 3.1 实测可处理本仓 `docs/api/openapi.yaml`（486 个 operation，生成 ≈1.1MB 类型文件，0.6 秒） |
| tailwindcss（仅测试用 devDependency） | 3.4.19 | 用来**真实编译**令牌生成的 Tailwind preset（`scripts/tokens.test.mjs`）；官网依赖的 Tailwind 与 Nuxt 3 版本在 Story 2.1 核对 |
| postcss | 8.5.33 | tailwind 测试用 |
| yaml | 2.9.1 | 读 openapi.yaml / DESIGN.md frontmatter / pnpm 锁文件 |
| 未引入 | pinia、vue-router、echarts、nuxt | 分别属于各应用/图表/官网，随 1.38、4.x、2.1 引入并核对；**shared 禁止依赖 pinia/vue-router** |

### pnpm 12 的两个实测事实（影响 AC1，已落地）

1. **`.npmrc` 里的 `ignore-scripts=true` 对 pnpm 12.18.0 无效**：实测工作区自身的 `postinstall` 仍会执行（依赖的脚本被 pnpm 默认拦截并以 `ERR_PNPM_IGNORED_BUILDS` 失败）。有效开关是 `pnpm-workspace.yaml` 的 `ignoreScripts: true`（camelCase；写成 `ignore-scripts` 无效）。本仓两处都配置（`.npmrc` 仅作 npm/旧 pnpm 兜底），`scripts/check-deps.mjs` 强制 yaml 项存在，`scripts/pnpm-hardening.test.mjs` 用真实 `pnpm install` 验证“依赖脚本与工作区脚本都不执行”。
2. pnpm 12 的锁文件是**多文档 YAML**（首个文档记录 `packageManager` 环境锁），解析锁文件的工具须用多文档解析（`check-deps` 已处理）。

### 生命周期脚本审核

当前**没有任何**依赖需要安装期脚本，登记表为空。如未来某依赖确需构建脚本：先在此登记（包名、版本、脚本内容、审核人、理由），再修改 `check-deps` 的白名单规则并说明——`onlyBuiltDependencies` 等配置目前会被 `check-deps` 直接拒绝。

## 依赖锁定（`pnpm run check:deps`）

- 所有 `package.json`（根与各成员）的 `dependencies/devDependencies/optionalDependencies/peerDependencies`、`overrides`、`pnpm.overrides`、workspace `catalog(s)` 必须是**精确版本**：`^ ~ * latest next x 区间 || git/http/file/link` 一律失败；成员之间用 `workspace:<成员版本>`（不用 `workspace:*`）。
- `packageManager` 必须是 `pnpm@<精确版本>`；`.npmrc` 的 `save-exact=true`；`pnpm-workspace.yaml` 的 `ignoreScripts: true`；不得出现安装期 `preinstall/install/postinstall/prepare` 脚本；不得出现工作区之外的 `package.json`；锁文件必须入库且其 `specifier` 同样精确。
- 自测（`scripts/deps-i18n.test.mjs`）覆盖上述每一类负向用例。

## `shared` 的边界（ADR-014 §2，`pnpm run lint:guards`）

`shared` 只放：设计令牌、三态组件、格式化、i18n 工具、生成类型。下列内容出现即失败（基于 TypeScript AST 与 Vue 模板 AST，不是 grep）：

| 规则 | 拦截 |
|---|---|
| `shared-no-forbidden-import` | 导入 `vue-router`、`pinia`、`axios`/`ky`/`ofetch`/`openapi-fetch`/`@tanstack/*`…、`@lingedge/{admin,console,website}` |
| `shared-no-route-or-menu` | `createRouter`/`RouteRecordRaw`/`useRouter`… 、`<router-link>`/`<router-view>`、声明名为 `routes`/`menu*`/`navItems`/`sidebar*`/`*Routes`/`*MenuItems` 的变量 |
| `shared-no-permission` | 等于任一 OpenAPI `x-permissions` 的字符串字面量（自动取自 openapi.yaml）、`resource:action` 形态字面量、`hasPermission`/`isAdmin`/`v-permission` 等 |
| `shared-no-session` | `document.cookie`、`localStorage/sessionStorage`、`le_admin_sid/le_console_sid`、`X-CSRF-Token`、`Authorization`、`login/logout/useAuth…` |
| `shared-no-api-client` | `axios`/`fetch`/`XMLHttpRequest`/`WebSocket`、`createClient`/`apiClient`、等于任一 OpenAPI 路径模板的字符串（含 `/api/v1` 前缀形式） |

全仓（含 admin/console/website/shared）另有：

| 规则 | 拦截 |
|---|---|
| `no-v-html` | `v-html`、`:innerHTML`/`:inner-html`、`innerHTML/outerHTML/insertAdjacentHTML/dangerouslySetInnerHTML`、`document.write`（ESLint `vue/no-v-html` 为第二道保险） |
| `no-hardcoded-color` | `#rgb/#rrggbb/#rrggbbaa`、`rgb()/rgba()/hsl()/…` 函数、颜色名（`red`/`white`…）。**只有 `shared/tokens/` 可以出现色值**；`rgb(var(--primary-6))` 这种变量引用、`href="#x"` 锚点、注释内文字不算 |
| `no-hardcoded-copy` | 模板文本节点、静态 `placeholder/title/alt/aria-label/label…` 属性、插值/表达式/脚本/模板字符串里的中文字面量；文案一律走 i18n 键。测试文件、`locales/*.json` 豁免。开发者向的英文 `Error` 消息不属于用户文案 |

豁免范围（刻意很窄）：`scripts/`（工具链本身）、`shared/tokens/`（色值唯一合法位置）、`shared/src/api-types/`（生成的纯类型）、`*.gen.ts`、`*.d.ts`。负向用例（每条规则至少一个必须失败的样例 + 不误报的正向样例）在 `scripts/guards.test.mjs`。

## 设计令牌（UD-14）

- **单一来源**：`shared/tokens/tokens.json`。`pnpm run gen:tokens` 生成 `tokens/generated/{tokens.css, arco-theme.css, tailwind-preset.js, tailwind-preset.d.ts}`（入库；`check:tokens` 重新生成比对，漂移失败）。
- **与设计文档的一致性**：`check:tokens` 同时解析 `docs/ux/DESIGN.md` 的 frontmatter，与 tokens.json 的颜色（亮/暗）、字体、圆角、间距逐项比对；改色值须两处同步。
- **输出**：`--le-color-*`/`--le-text-*`/`--le-radius-*`/`--le-space-*`/`--le-layout-*`/`--le-shadow-overlay`/`--le-gradient-brand`。亮色在 `:root`，暗色在 `:root[data-theme='dark']` 与 `[arco-theme='dark']`；组件内不写主题分支，用 `applyTheme('dark')` 同时设置两个属性。
- **Tailwind preset**：颜色全部是 `var(--le-…)` 引用（无色值），类名形如 `bg-le-bg-surface`、`text-le-text-primary`、`rounded-le-md`、`font-le-mono`、`text-le-body`、`bg-le-brand`；经真实 tailwindcss 3.4.19 编译验证。
- **Arco 映射**：只覆盖 Arco 的 CSS 变量（`--primary-1..10` 等三元组、`--color-bg-*`/`--color-text-*`/`--color-border*`/`--border-radius-*`），不改 Arco 源码、无 `!important`；引入顺序：Arco 自带 `arco.css` → `tokens.css` → `arco-theme.css`。色阶 1–5/8–10 由基色与悬停色/底色确定性混合得到（提议值，随 UD-14 复核）。
- **对比度**（`check:contrast`）：`tokens.json.contrast` 列出文字配对（正文/次级文字 × 四种底色、品牌色、白字 × 品牌/悬停/渐变端点/危险、语义色 × 自身底色与页面底色…），亮/暗各算一遍，**文字 ≥ 4.5:1**，图表序列（非文本）≥ 3:1，共 82 个配对。已知例外各自设下限防止继续恶化：
  - `text-disabled`（禁用态，DESIGN 已声明；light 3.09:1）。
  - **新发现**：light 主题 `chart-3`（`#D9822B`）在白底仅 **2.93:1**，低于非文本 3:1。DESIGN 未声明此项；本 Story 不擅自改色，登记为 UD-14 视觉复核项（下限 2.9 防恶化；图表须按 DESIGN 铁律不得仅靠颜色区分系列）。
  - **新发现（Arco 侧，未在本 Story 修正）**：Arco 2.59 的实心按钮等组件文字色在 `arco.css` 里写死 `#fff`（24 处，不走 CSS 变量），暗色主题下主按钮是 `#7C93FF` 底白字，仅 **2.81:1**（DESIGN 的 `on-brand-dark` 要求深色字，与深色字为 6.62:1）。这是令牌/变量层够不到的问题；真实 Chrome 截图（亮/暗）已确认。处置建议（留给 1.38 Admin 壳与 UD-14）：在主题层对 `body[arco-theme='dark'] .arco-btn-primary` 等少数类做仅改颜色的窄覆盖，或调整暗色品牌色；本 Story 自家的三态组件已按 `on-brand` 取色，不受影响。
- **UD-14**：主色/字体/圆角是 UX 提议。采纳与否由用户决定；推翻时只改 `tokens.json` 与 `DESIGN.md` frontmatter（两者一致性有脚本校验），然后 `gen:tokens`。

## 三态组件（FR-30，`@lingedge/shared`）

`LeLoading`（骨架 + `role=status`/`aria-busy`；`slowAfterMs`（默认 10s）后追加“仍在加载…”与取消/重试）、`LeError`（区块级错误卡，`role=alert`；按 `LeErrorKind`——网络/服务端/403/404/409/412/400/429/未知——给不同文案；Request ID 可复制；403/404/400 默认不提供重试；服务端 `detail` 以文本插值渲染不当 HTML）、`LeEmpty`（`no-data` 与 `no-results` 两种；后者带“清除筛选”；下一步主操作走 `action` 插槽；不含示例数字）、`LeStateBoundary`（按 `status` 切换并转发事件）。`errorKindFromStatus(status)` 做 HTTP 状态码到类别的映射。只使用 `--le-*` 令牌。

## 格式化约定

| 函数 | 约定 |
|---|---|
| `formatDateTime/formatUtc/formatRelativeTime` | API 时间为 RFC 3339 UTC；显示为用户时区（可传 `timeZone`），hover 用 `formatUtc`；空/无效值显示“—” |
| `formatBytes` | **1024 进制**，标注 KB/MB/GB…（UX-26；`[ASSUMPTION]` 计费口径 P5 可覆盖，导出表头须注明进制）；舍入进位处理（1048575 字节显示 1 MB） |
| `formatBandwidth` | **bps 十进制**（1000）：bps/Kbps/Mbps/Gbps |
| `formatMoney(amountMinor, currency)` | 入参是**整数最小货币单位**（number 安全整数 / bigint / 十进制整数字符串，对应 OpenAPI `amount_minor`），用 BigInt 拆整数与小数再交给 `Intl.NumberFormat`（精确十进制字符串输入），全程无浮点；最小单位位数取自 Intl（CNY=2、JPY=0）；小数/不安全整数抛错 |

## i18n 约定（NFR-29，`pnpm run check:i18n`）

- 运行时用 Vue I18n（`createLeI18n({ locale, messages })`：与共享词条深合并；`zh-CN` 与 `en`；缺键回退 `en`，仍缺显示键本身并经 `onMissing` 回调）。
- 每个 `locales/` 目录必须同时有 `zh-CN.json` 与 `en.json`，**键集完全一致、占位符 `{name}` 一致、无空值、无未转义的 Vue I18n 特殊字符（`@ | { } $` 字面量写成 `{'@'}`）**；源码里 `t('键')`/`$t('键')` 的静态键必须存在，动态键 `` t(`前缀.${x}`) `` 校验前缀存在（无静态前缀的动态键无法校验，会在输出里计数）。
- 键命名：小写点分层，如 `state.error.kind.network`；应用词条放各应用自己的 `locales/`。

## OpenAPI 类型生成与契约联动（AC2、AC6）

`pnpm run gen:types`：`docs/api/openapi.yaml` → `shared/src/api-types/openapi.gen.ts`（纯类型；头部记录 openapi.yaml 的 sha256 与生成器版本）。**`make contract-check` 含 `frontend-types-check`：重新生成并逐字节比对，OpenAPI 变动而生成物未同步即失败**。生成物不含任何运行时代码或 API 客户端；每个应用只 `import type` 自己需要的 operation（`@lingedge/shared/api-types`）。

## 构建隔离（FR-37、IMP-14，`pnpm run check:isolation`）

- **清单由 openapi.yaml 自动生成**（`scripts/lib/openapi.mjs`）：取所有 `x-tenant-scope: platform` 的 operation，得到“只被 platform 使用的路径模板”（当前 234 个 platform operation → 141 个精确模板）。缺少/非法 `x-tenant-scope` 直接失败。
- **按路径模板精确匹配，不用子串**：`{param}` 视为单段通配（也认模板字面量 `${…}` 与原样 `{param}`）；左边界必须是引号/反引号/括号/空白/`=`/`,`/`:`/`[`，或紧跟 `/api/v1`；右边界不得继续是路径字符。因此 `` `/sites/${id}/clusters` ``、`/audit-logs`、`/clusters-info` 不会被 `/clusters` 误报，而 `"/api/v1/clusters"` 与 `` `/nodes/${id}` `` 会被检出。同形状下更具体的非 platform 静态路径（如 `/tenants/current` 之于 `/tenants/{tenantId}`）自动从通配里排除；同一路径形状同时承载 platform 与非 platform operation 时，不进 grep 清单并在输出里单列（授权靠服务端，ADR-014 §4）。
- **Console 产物**还不得含 Admin 路由前缀（`isolation/admin-route-prefixes.json`，现为 `/admin`）与 Admin realm 常量 `le_admin_sid`；**Admin 产物**反向不得含 `le_console_sid`。扫描 js/css/html/json/map 等文本文件；目录缺失、无 js/html 一律失败（不空转通过）。
- 已知限制：字符串拼接（`"/nodes/"+id`）无法被路径模板匹配——Console 代码须用模板字面量或完整字面量写端点（1.38 起的 API 层约定）；这是静态产物检查的固有边界，隔离的根本保证仍是服务端授权。
- 自测：`scripts/isolation.test.mjs`（对真实 openapi 清单：每个 platform 模板都能被检出、每个非 platform 路径都不误报）与 `scripts/e2e-build.test.mjs`（用真实 vite 生产构建产出违规/合规产物再检查）。

## mock 检查（NFR-1、ADR-023，`pnpm run check:mock`）

约定：mock 模块放在 `mocks/` 目录或命名 `*.mock.ts`；其入口函数执行时输出含 `LE_MOCK_ENTRY` 的字符串（必须是被执行代码里的字面量——只导出常量会被摇树掉，e2e 已实测）；只能经受 `import.meta.env.MODE` 等编译期条件保护的动态 `import()` 引入。检查：产物里无 mock 文件名/标记（`LE_MOCK_ENTRY`、`mockServiceWorker`、`msw/browser`、`setupWorker(`、`__LE_MOCK__`、`VITE_ENABLE_MOCK/VITE_USE_MOCK`）、sourcemap 不引用 mock 源文件；源码不得静态 import mock 模块；生产相关 `.env*` 不得打开 mock 开关。

## 未在本 Story 内的事项

- `website`（Nuxt 3.x、Tailwind、官网页面）：Story 2.1；本 Story 只占位成员并验证 preset。
- admin/console 的壳、路由、菜单、会话、API 层、echarts 等：Story 1.38 起；Console 在 P5。当前两个应用只是“能构建的基座探针”，不渲染任何假页面。
- 远端 CI 尚未实际运行（仓库无远端），`ci.yml` 已加入 Node 26.8.2 与 pnpm 12.18.0 步骤，首次运行结果以实际为准。
