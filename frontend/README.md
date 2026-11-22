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
├── admin/ [Admin 管理员后台]  独立 Vite 工程（Story 1.38）：登录/TOTP、动态菜单、守卫、多标签、搜索壳、代管横幅、主题/中英文/响应式；会话/路由/菜单/权限/API 客户端只在这里（见下文「Admin」）
├── console/                Console 独立 Vite 工程（基座探针：只验证 shared 与 Arco 能构建；P5 交付）
├── setup/                  安装向导 SPA（Story 1.37）：Vue 3 + TS，产物经 go:embed 嵌入 lingedge-server，不依赖 Admin Web；src/（组件/步骤/API/状态）、e2e/（Playwright）
├── website/                占位（Nuxt 3 官网，Story 2.1）
├── isolation/              admin-route-prefixes.json（Admin 专属路由前缀清单；新增 Admin 专属前缀须同步登记）
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
| `make setup-web` / `make setup-web-check` | 安装向导：构建并同步到 `internal/platform/setup/webdist`（go:embed 来源）/ 重新构建并逐字节比对（漂移、缺失即失败；已含在 `make verify-build`） |
| `make e2e` | 浏览器端到端（Playwright + 真实 server + 真实 MySQL/Redis）：安装向导（见「安装向导」）与 Admin（见「Admin」）；Admin 所需的两份构建由 `make admin-e2e-build` 先行构建 |

等价的 pnpm 脚本：`check:deps` `lint:guards` `lint:eslint` `check:tokens` `check:contrast` `check:arco-contrast` `check:i18n` `check:setup-codes` `check:types` `typecheck` `test` `build` `check:isolation` `check:mock`；生成：`gen:tokens`、`gen:types`；测试：`test:shared` `test:setup` `test:admin` `test:selftest`。

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
| Arco Pro（`arco-design-pro-vue`） | 2.7.3（仅作为架构参考，**不作为依赖**） | `npm view` 实测：它只是脚手架 CLI 包（依赖 fs-extra/minimist/prettier，无运行时代码）。Admin 壳（Story 1.38）采用其布局架构——顶栏 + 侧栏菜单 + 多标签栏 + 内容区、路由元信息驱动菜单与守卫——用本矩阵里的精确版本（Arco 2.59.0 + vue-router 4.6.5 + pinia 3.0.4）自行实现；模板自带的 axios/mockjs/echarts/dayjs 等**一律不引入**（axios 另被 shared 的边界规则禁止，图表随 4.x） |
| vue-router | 4.6.5 | Admin 路由（仅 admin；shared 禁止依赖）。peer `vue ^3.5.0` 满足；**有意不取 5.5.1**（它要求 `vue ^3.5.34 或 ^4`、`vite ^7.3 或 ^8` 且并入文件路由插件，本 Story 不需要，待有收益时再升并重跑矩阵）；`pnpm peers check` 无问题 |
| pinia | 3.0.4 | Admin 会话/标签状态（仅 admin）。peer `vue ^3.5.11`、`typescript >=4.5.0` 满足；有意不取 4.1.0（peer 要求 `typescript >=5.6` 与 devtools-api 8，非必要不升）；`pnpm peers check` 无问题 |
| @playwright/test（admin 与 setup 的 devDependency） | 1.65.1 | 与 setup 同版本，共用本机 Chromium（`playwright install chromium`）；仅 e2e 使用 |
| vue-i18n | 11.4.23 | 基线固定；node `>=22`；`createLeI18n` 在 vitest 中验证回退/缺键行为 |
| vitest | 5.1.1 | + @vue/test-utils 2.6.0 + happy-dom 21.0.4（组件测试）；`projects` 配置：`shared`（happy-dom）与 `scripts`（node） |
| eslint / eslint-plugin-vue / typescript-eslint / vue-eslint-parser | 10.15.0 / 10.12.1 / 8.73.0 / 10.4.2 | plugin-vue peer `eslint ^8.57\|\|^9\|\|^10`、ts-eslint peer `eslint ^8.57\|\|^9\|\|^10`；`eslint . --max-warnings 0` 通过 |
| openapi-typescript | 7.13.0 | OpenAPI 3.1 实测可处理本仓 `docs/api/openapi.yaml`（486 个 operation，生成 ≈1.1MB 类型文件，0.6 秒） |
| tailwindcss（仅测试用 devDependency） | 3.4.19 | 用来**真实编译**令牌生成的 Tailwind preset（`scripts/tokens.test.mjs`）；官网依赖的 Tailwind 与 Nuxt 3 版本在 Story 2.1 核对 |
| postcss | 8.5.33 | tailwind 测试用 |
| yaml | 2.9.1 | 读 openapi.yaml / DESIGN.md frontmatter / pnpm 锁文件 |
| 未引入 | echarts、nuxt | 分别属于图表（4.x）与官网（2.1），引入时核对；**shared 禁止依赖 pinia/vue-router**（Story 1.38 起 admin 已引入二者） |

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
  - `text-disabled`（禁用态，DESIGN 已声明；light 3.09:1；WCAG 1.4.3 明确不要求不可用控件满足对比度）。这是目前**唯一**的例外。
  - **已处置（Story 1.37b）**：light `chart-3` 原 `#D9822B` 在白底仅 2.93:1，调为 `#D17A1F`（3.22:1，色相不变、明度略降），例外条目删除；仍属 UD-14 提议令牌，待确认。
  - **已处置（Story 1.37b）**：Arco 2.59 的实心控件文字色在 `arco.css` 里写死 `#fff`（24 条规则、32 个选择器）或 `var(--color-white)`，暗色主题下主按钮是 `#7C93FF` 底白字仅 2.81:1。处置：①暗色下对「暗色白字不合格」的 38 个选择器做**仅 `color`** 的窄覆盖（取 `on-brand` 令牌，不用 `!important`；清单在 `tokens.json` 的 `arco`，生成进 `arco-theme.css`）；②亮色悬停态（Arco 的 5 级）原先白字仅 3.9–4.2:1，色阶函数改为 5 级的底色占比取第一个使白字 ≥4.5:1 的值。`check:arco-contrast` 解析 Arco 真实 CSS，要求覆盖清单「不漏、无死条目」（Arco 升级引入新规则会失败）；`make e2e` 的审计器自测在真实 Chromium 渲染四种实心按钮（亮/暗、含悬停）逐个量对比度，并有「去掉覆盖则失败」的负向对照。禁用态（WCAG 1.4.3 不要求）保持 Arco 原样。**范围外、已登记**：Arco 头像（灰底白字，亮色 `fill-4`）等非调色板底色的白字组件由使用它们的 Story（1.38 起）核对。
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

**向导的服务端消息本地化（Story 1.37b，`pnpm run check:setup-codes`）**：服务端的稳定标识（错误码 `error.code`、环境检查项 `items[].key`、步骤 `step`）是契约，文案是服务端固定语言。前端按稳定标识映射到词条：错误说明 `errors.code.<code>`（错误码里的 `password` 在词条键里一律写成 `pw`，G-6）、检查项/回执/`details[].reason` 的固定句式在 `setup/src/api/servermsg.ts`（动态值——时间、路径、版本、数量——从原文按句式取出，词条在 `serverMsg.*`）。**对不上的一律回落服务端原文并标注其语言（`data-localized="false"`），错误码与 Request ID 的展示不受影响。** 对账脚本从 Go 源码（`internal/platform/setup`、auth 引导路径、`apierr/codes.go`）与 OpenAPI（`/public/setup/*` 声明的错误响应）自动推导可能返回的错误码、检查项与有回执的步骤，缺任一词条/句式即失败；`servermsg.test.ts` 用从 Go 源码抽取的真实文案样本验证每个句式能命中，E2E 在真实 server 下断言英文界面零回落、零中文。

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

## 安装向导（Story 1.37，`frontend/setup`）

**是什么**：`/setup` 的 8 步向导（环境检查 → MySQL → Redis → 初始化数据库 → 管理员 → 基础配置 → 密钥 → 完成），Vue 3 + TS，使用 `@lingedge/shared` 的令牌（`tokens.css`）、三态组件（`LeLoading/LeError/LeEmpty/LeStateBoundary`）、i18n 工厂（词条在 `setup/src/locales`，缺键由 `check:i18n` 校验）与由 OpenAPI 生成的类型（`SetupDbConfig` 等，只 `import type`）。**不引入 Arco、路由、状态库**：向导不需要它们，产物只有约 230 KB JS + 19 KB CSS（向导主题跟随系统 `prefers-color-scheme`，顶栏可显式选浅色/深色；Story 1.37b）。全部 1.36 门禁照常适用（无 `v-html`、无硬编码色值/文案，`setup/**` 的测试与 e2e 同样被扫描）。

**安全约定（AC4）**：setup token 只保存在 API 客户端的闭包内存里——不写 URL、不写 `sessionStorage/localStorage/cookie`；刷新后需重新输入；地址栏里携带的 `token` 类参数在启动时被丢弃并从历史记录移除（页面提示）；验证失败或完成后立即清除。口令类输入是 `type=password`（不回显）、`autocomplete=new-password`、关闭拼写检查，提交成功后清空；一次性机密（TOTP 密钥/恢复码/迁移口令）只存在于组件状态，离开步骤即销毁。唯一写入 `sessionStorage` 的是非机密草稿（访问地址、界面语言）。构建隔离脚本新增 `setup` 角色：向导产物不得含任何 platform 端点模板、Admin 路由前缀或任一 realm 会话常量（端点以完整路径字面量书写，因为裸的 `"/admin"` 段会被判为 Admin 路由）。

**嵌入（go:embed）与可重现流程**：`make setup-web` = `vite build`（`base=/setup/`、`assetsInlineLimit=0`、无 sourcemap）→ `scripts/sync-setup-embed.mjs` 同步到 `internal/platform/setup/webdist/`，由 `internal/platform/setup/web.go` 以 `//go:embed all:webdist` 嵌入。**产物入库**，并有漂移/缺失检查：`make verify-build`（经 `frontend` 目标）与 `make setup-web-check` 重新构建并与入库内容逐字节比对（缺文件/多文件/内容不同都失败；构建已实测两次哈希一致），Go 侧 `TestEmbeddedWizardIsARealBuild`/`TestVerifyWebRejectsBrokenBuilds` 保证嵌入内容是真实构建且完整（入口存在、引用的资源都在、无内联脚本/mock 标记/sourcemap，缺失时 setup mode 拒绝启动而不是给空白页）。**选择入库而非「只由 make 生成」的理由**：①`go build`/`go test`/发行流水线（`scripts/release`）与 Tools 的安装包构建都不依赖 Node 与前端供应链，Go 工程师不装 Node 也能构建出可用的二进制；②仓库已有同类先例（令牌生成物、OpenAPI 生成类型都入库 + 漂移检查）；③嵌入内容在 PR 里可见可审，且「忘记重新构建」会被门禁拦下而不是悄悄发行过期前端。代价是 `webdist` 会随前端变动产生二进制 diff（体积约 250 KB）。服务端响应头：入口 `no-store`，资源带内容哈希长缓存；CSP `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self' …`（无 `unsafe-inline/eval`），`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`。`install.lock` 之后（正常模式）`/setup` 与 `/setup/*` 一律 `410 setup.already_installed`。

**测试**：`make frontend-test` 含向导的 vitest 组件测试（`setup/src/**/*.test.ts`：API 客户端、状态机、各步骤三态/校验/口令不回显/TOTP 两阶段/完成页提示、键盘与 aria、中英文）；`frontend-gate-selftest` 含嵌入同步与 `setup` 隔离角色的负向用例。**E2E（`make e2e`，ENV-BROWSER）**：`setup/e2e/*.spec.ts` 由 Go 侧 `tests/integration/setup/browser_test.go` 拉起——真实 `lingedge-server`（setup mode，内嵌真实 SPA）+ 独立临时 mysqld/redis（随机口令，G-6），Playwright 驱动真实 Chromium；覆盖全流程（TCP+TOTP+中文、Unix socket+英文）、错 token（含 URL token 与限流）、非空库、中断续做（early：409 要求重新提供迁移凭据；late：从基础配置继续）、键盘可操作、逐页 WCAG 对比度审计（≥4.5:1，含审计器的负向自测）、`localStorage/sessionStorage/cookie/URL` 不含 token、控制台/CSP 零违规、完成后 `/setup` 410，并对页面里展示过的一次性机密做日志/审计/凭据文件泄漏扫描。浏览器环境缺失时这些 Go 测试以「等待环境验证」跳过（`LINGEDGE_REQUIRE_BROWSER=1` 视为失败；`make e2e` 设置它）。首次需要 `pnpm -C frontend/setup exec playwright install chromium`。

## Admin（Story 1.38，`frontend/admin`）

**是什么**：管理员后台的外壳与登录。Vue 3 + TS + Arco 2.59.0 + vue-router + pinia + vue-i18n（共享词条 + `admin/src/locales`）。业务页面（系统设置、用户与角色、API Token、审计…）由 1.39 起接入（见下一节「Admin 系统管理页」）；**1.38 本身不渲染任何业务页面或占位页**。

**已交付页面注册表（FR-31、UX-1）**：`src/registry/pages.ts` 的 `productionPages` 是唯一入口——页面只有登记后才存在于路由表、侧栏和全局搜索，没有登记就不存在（没有「敬请期待」、没有灰显占位）。1.38 时为空，1.39 起登记系统管理页（登录/改口令/403/404/首页是外壳自己的系统路由，不在清单里）。登记一个页面：

```ts
{ key: 'ops.tasks', path: '/ops/tasks', group: 'ops', titleKey: 'menu.page.opsTasks', module: 'tasks',
  permissions: ['task:read'], component: () => import('./views/TasksView.vue') }
```

可见性 = **页面已登记 ∧ `module` 在 `/auth/me` 的 `features.modules_delivered` 里 ∧ 具备全部 `permissions` ∧ `runModes` 允许当前运行模式**（`src/registry/visibility.ts`，纯函数，vitest 全覆盖）。一级菜单（17 个，ia.md §1 固定顺序）下可见页面为 0 则整组不渲染。直访：模块未交付/运行模式不符/未登记 → **404**（不暗示存在）；仅权限不足 → **403**（显示所需权限点、联系管理员、返回首页，不渲染页面）。`aliasOf` 表示别名入口（UX-3：共用组件、搜索不重复索引）；`menu: false` 为不进侧栏的详情页；`complexEdit: true` 的页面在手机宽度下自动显示「可用但不保证体验」标注（UX-23）。`registry.test.ts` 对生产清单自动对账：模块必须是后端已交付模块（读 `internal/platform/sysconfig/capability.go`）、权限必须是 OpenAPI 的 `x-permissions`、Admin 专属一级菜单（节点/调度/运维）的路径前缀必须已登记到 `isolation/admin-route-prefixes.json`。

**会话与 API 客户端**（只放 admin，ADR-014 §2）：`src/api/client.ts` 同源 `/api/v1`，Cookie 会话 `le_admin_sid`（HttpOnly，脚本不接触），写请求自动带 `X-CSRF-Token`（403 `auth.csrf_invalid` 时刷新一次重试），仅在调用方明确 `actingAs` 时带 `X-Tenant-ID`，401 统一进入会话过期流程。登录固定 `realm: admin`；TOTP 第二步与恢复码走同一个 `/auth/totp/verify`（6 位数字按 TOTP，否则按恢复码，单次有效）；`must_change_password` 的账号只能到改口令页（服务端同样只放行 getMe/csrf/logout/changePassword）。租户账号在 Admin 入口得到与错口令**逐字相同**的中性提示（服务端 401 `auth.invalid_credentials`），不泄露账号是否存在。令牌、口令、TOTP 码不写任何浏览器存储。身份启动时读取一次（`/auth/me`）：401=匿名，网络/5xx 显示启动失败页而不是误判为已退出。`/auth/me` 的现有字段（`permissions`、`features.modules_delivered`、`features.run_mode`、`realm`、`csrf_token`）已足够驱动菜单，**未新增 OpenAPI/CC-14**。

**外壳**：固定顶栏 48px、其下 32px 的「代管默认租户」横幅（UX-24，不可关闭；有 `tenant:read` 时显示真实的默认租户名，否则退回通用文案）、侧栏（≥lg 展开可折叠 / md 折叠为图标 / 手机抽屉）、多标签栏（同页不重复、最多 12 个并淘汰最久未用的无更改标签、有未保存更改先确认、页面随标签保活、刷新恢复标签但不恢复表单、退出/换身份关闭全部；页面用 `useTabDirty()` 声明未保存更改；手机宽度改为面包屑 + 返回）、全局搜索壳（Ctrl/⌘+K，只索引已交付且可见的页面）、语言与主题（亮/暗/跟随系统，`localStorage` 记忆，**[ASSUMPTION]** EXPERIENCE.md 希望存服务端用户偏好，契约暂无字段）。统一三态：`useAsyncState()` 驱动 shared 的 `LeStateBoundary`；统一表单校验：`useFormRules()`（Arco Form rules，文案全走词条）。

**构建、部署与「不含 mock」**：
- `vite build` → `frontend/dist/admin`（**生产构建**，纳入 `check:isolation`（不含 `le_console_sid`）与 `check:mock`）；`vite build --mode e2e` → `frontend/dist/admin-e2e`（额外含 `src/mocks/e2e-pages.mock.ts` 的夹具页面，**仅供 Playwright，不是发行物**，不被任何检查当作生产产物）。夹具经 `import.meta.env.MODE === 'e2e'` 保护的动态 `import()` 引入，生产构建里被编译期折叠；`scripts/admin-build.test.mjs` 用真实 vite 构建证明：生产产物不含 `LE_MOCK_ENTRY`/夹具，同一份代码按 e2e 模式构建会被 `check:mock` 检出，往真实 Admin 产物里放入 Console realm 常量会被隔离检查检出。
- **部署形态（沿用既有约定，没有新增嵌入）**：Admin 是**静态前端，由 Tools 的 `admin-web` 组件（Story 1.40）产出 Nginx 站点片段并托管**（tools-design §11.3：admin-web 无监听端口、需要 Web 服务器托管；与 Server 同源，Nginx 把 `/api/` 反代到 `127.0.0.1:8080` 且**必须保留原始 Host**——CSRF 的 Origin 校验依赖它）。不像安装向导那样 `go:embed`：向导必须在没有 Admin 的 setup mode 下可用，而 Admin 依赖已安装的 Server 与独立主机名（IMP-17）。SPA 需要回退（`try_files $uri /index.html`）。建议响应头与 `admin/e2e/serve.mjs` 的测试服务一致：入口 `no-store`、资源带内容哈希长缓存、CSP `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`（`style-src 'unsafe-inline'` 仅因 Arco 使用行内样式属性；E2E 在该 CSP 下断言零违规）。**Admin 产物的打包/发行（admin-web 组件包与清单）属 Story 1.40**，本 Story 只保证 `frontend/dist/admin` 是可直接托管的完整静态目录。
- 开发：`pnpm -C frontend/admin dev`（Vite 代理 `/api` 到 `LE_ADMIN_DEV_API`，缺省 `http://127.0.0.1:8080`；Cookie 为 host-only，必须同源）。

**测试**：`make frontend-test` 含 Admin 的 vitest（`admin/src/**/*.test.ts`：注册表校验与权限/模块对账、可见性/菜单裁剪/搜索索引、守卫、API 客户端、会话、标签、三态、登录页、整个外壳的挂载级测试）；`make e2e` 含 Admin 的 Playwright 场景（`tests/integration/setup/adminweb_browser_test.go` 拉起：真实 `lingedge-server`（先经 setup 向导完成安装并切到正常模式，管理员绑定 TOTP）+ 独立临时 mysqld/redis + 额外账号（只读审计员、租户账号、须改口令的管理员）+ 真实 Chromium；静态服务 `admin/e2e/serve.mjs` 等价于生产 Nginx 形态）：登录/TOTP/恢复码（单次有效）/租户账号被拒/须改口令/Cookie 与 CSRF、空菜单生产外壳、无权限菜单隐藏、直访 403/404、标签与搜索、代管横幅、手机宽度、亮/暗/跟随系统的逐页对比度审计（≥4.5:1，含搜索/确认/账号菜单弹层与手机抽屉；审计前等待浮层进出场动画结束——浮层在 `*-enter-from` 阶段 opacity 为 0，只看 `getAnimations()` 会审到透明元素，已用 `settled()` 修正并在截图中人工核对）；审计器自测见 `admin/e2e/selftest.spec.ts`。夹具页面只是占位内容，**模块与权限来自真实后端**：`fx.*` 页面选用真实已交付模块（tasks/audit/auth/system）与真实权限点，并用「未交付模块」（nodes/scheduler）与「仅商业模式」页面验证 404 路径。

## Admin 系统管理页（Story 1.39，`frontend/admin/src/views/system`）

**页面清单**（`registry/pages.ts`，生产构建里可用；ia.md §1.11/§1.15/§1.17 的 M1 页面，不多不少，`registry.test.ts` 精确对账）：用户列表 `/users/list`（别名：`/settings/admins`）、权限角色 `/settings/roles`（别名：`/users/roles`）、API Token `/users/api-tokens`、登录日志 `/users/login-logs`、审计日志 `/ops/audit`（别名：`/users/audit`，预置「用户操作」筛选）、基本设置 `/settings/basic`（含运行模式）、品牌设置 `/settings/brand`、登录设置 `/settings/login`、版本信息 `/settings/version`。邮件/API/系统安全/数据保留/系统日志（P2）**不登记 = 不渲染**。别名与规范页面共用同一个组件加载器（测试断言 `component` 同一引用），搜索只索引规范页面（UX-3）。

**危险操作与再认证（UX-12、IMP-12）**：`components/DangerDialog.vue` 统一 L2/L3（L4 = 再认证）对话框——说明影响范围、确认键用动词、L3 要求逐字键入确认词（确认键初始禁用）、**页脚固定审计提示**（「此操作将记录到审计日志（操作人、时间、对象、Request ID）。」）。运行模式切换 = L3（键入目标模式）；删除用户/重置口令/吊销会话/停用/吊销或轮换 Token = L2；删除角色 = L3（键入角色代码）。**L4 再认证走后端现成的 `POST /auth/reauth` 与 `x-reauth: true`（1.17/1.18/1.22），没有新增契约、没有 CC-14、前端不做任何判定**：API 客户端（`api/client.ts`）收到 `403 auth.reauth_required` → `stores/reauth.ts` 弹出验证框（口令；账号已启用 TOTP 则另加 6 位验证码）→ 服务端校验通过后**自动重试原请求一次**（同请求体、同幂等键）；取消 → `client.reauth_cancelled`，原请求不重试；重试后仍被要求 → 如实报错不循环；口令错误（401）验证框保持打开且不被当作会话过期（`auth:false`）。再认证窗口 `auth.reauth_window_s`（默认 300 秒）可在「登录设置」里改，E2E 用 10 秒窗口实测「窗口外再次被要求验证」。

**一次性明文**（`components/SecretDialog.vue`）：API Token 创建/轮换、服务端生成的临时口令——只展示一次、可复制（剪贴板不可用时如实提示手动复制）、不允许 Esc/遮罩误关、关闭后父组件丢弃并从 DOM 移除；库内只存哈希由 E2E 在库里逐个核对（令牌明文的 SHA-256 恰好匹配一行 `token_hash`，明文不得原样入库），明文本身经 sink 文件交给 Go 侧扫描服务端日志/审计/凭据文件。

**列表与表单**：游标分页（`composables/useCursorList.ts`，游标栈支持上一页，只认最后一次请求）+ 过滤（审计：action/资源类型/资源 uid/操作人 uid/结果/Request ID/起止时间；登录日志：事件/账号/IP/起止时间；时间转 RFC 3339 UTC，起止颠倒被拦）；设置表单 `SettingsForm.vue`（校验规则与后端 `sysconfig` 注册表一一对应，只提交变更的键，未保存更改标记标签）；表格在手机宽度下改为逐行卡片（`le-table--stack`，单元格带 `data-label`）；页面随标签保活，切回时重新读取（`useRefreshOnActivate`）。品牌名取自 `GET /admin/settings/brand`（顶栏），保存后立即刷新。

**UX-27（没有任何「执行命令」入口）静态检查**：`pnpm run check:no-command`（已并入 `pnpm run lint`）——词条（admin/console 的 zh-CN/en）里不得出现「执行命令/运行脚本/网页终端/Shell」类措辞、页面注册表的 key/path 不得是 exec/terminal/shell/command/script/ssh 入口、应用依赖里不得引入 xterm/ssh2/node-pty 类库；`scripts/no-command-entry.test.mjs` 用临时目录里的负向样本证明每一类都能检出，且对真实仓库断言干净。运行期另有 Playwright 逐页文字检查。

**测试**：vitest（`views/system/system.test.ts` 等：再认证全路径、一次性明文、游标/过滤参数、别名预置、L2/L3 对话框、只读、三态、中英文无漏键、手机卡片结构）；Playwright（`admin/e2e/system.spec.ts`、`system-settings.spec.ts`，生产构建 × 真实 server × 真实 Chromium，由 `TestBrowserAdminSystem`/`TestBrowserAdminSystemSettings` 拉起，含 Go 侧对库与审计的独立核对）。1.38 的 e2e 夹具页面（`mocks/e2e-pages.mock.ts`）只保留没有真实页面的菜单组（节点、任务、回滚）与「模块未交付/仅商业模式/复杂编辑/未保存更改」场景；用户/角色/设置/审计的外壳测试一律对真实页面。E2E 的 `settled()` 现在忽略无限循环动画（按钮加载中的旋转图标），仍等待有限时长的进出场动画（自测覆盖）。

## 未在本 Story 内的事项

- `website`（Nuxt 3.x、Tailwind、官网页面）：Story 2.1；本 Story 只占位成员并验证 preset。
- Admin 业务页面：系统管理页 1.39 已交付（见上），其余业务页面随各模块 Story 经注册表接入；Admin 静态产物的 Tools 安装与发行：Story 1.40；Console：P5。echarts 等图表库：4.x。
- 远端 CI 尚未实际运行（仓库无远端），`ci.yml` 已加入 Node 26.8.2 与 pnpm 12.18.0 步骤，首次运行结果以实际为准。
