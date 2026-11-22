---
title: 'Story 1.36：前端 workspace 基座（shared tokens / 三态 / i18n / 类型生成 / 门禁）'
type: 'feature'
created: '2026-11-22'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '0cb511b'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.36（AC1–AC6、测试要求、DoD）
  - docs/architecture/adr/ADR-014-frontend-structure.md、review-architecture.md IMP-14
  - docs/ux/DESIGN.md、docs/ux/EXPERIENCE.md（全局三态、数字单位）
  - docs/architecture/dev-environment.md §1.3 / §4
  - docs/api/openapi.yaml（x-tenant-scope / x-permissions）
---

## Intent

**Problem:** `frontend/` 只有四个 `.gitkeep`。Admin（1.38）、安装向导（1.37）、官网（2.1）开工前缺：可复现的 pnpm workspace、单一来源的设计令牌、无权限语义的共享件、i18n 与类型生成，以及让这些约束**不会被后续 Story 悄悄破坏**的门禁（`shared` 边界、硬编码色值/文案、Console 构建隔离、mock 入口）。

**Approach:** 沿用既有目录骨架，不推倒重来。全部检查以 `scripts/` 下的 Node 脚本实现（带正/负向自测），统一接入根 Makefile 的既有目标（lint / test / gate-selftest / contract-check / verify-build），并提供 `make frontend-check` 一次跑完。**没有改 OpenAPI、proto、迁移、冻结契约；没有新增 Go 依赖。** 详细约定、兼容矩阵与每条规则见 `frontend/README.md`。

## 逐条验收

- **AC1 workspace 与依赖**：`frontend/{admin,console,website,shared}` 全部是 pnpm 成员；`packageManager: pnpm@12.18.0`；全部依赖精确版本（29 条声明，`scripts/check-deps.mjs` 检查 `^ ~ * latest x 区间 git/file/link`、overrides/catalog、锁文件 specifier、`workspace:<精确版本>`、孤儿 package.json、安装期脚本、构建白名单）；`pnpm install --frozen-lockfile` 通过，`pnpm peers check` 无问题。**生命周期脚本**：见「实测发现 1」——pnpm 12 不读 `.npmrc` 的 `ignore-scripts`，有效开关写在 `pnpm-workspace.yaml` 的 `ignoreScripts: true`，两处都配并由检查强制、真实 `pnpm install` 自测。兼容矩阵（Arco / Vite / TypeScript / vue-tsc 等）在 `frontend/README.md`，不取最新：**TypeScript 采用 5.9.3 而非 7.0.2**（`typescript-eslint` peer `<6.1.0`、`openapi-typescript` peer `^5.x`），经 vue-tsc 三包类型检查、vite 两应用构建、vitest、ESLint 实测。
- **AC2 shared 内容**：设计令牌（`shared/tokens/tokens.json` 为单一来源 → 生成 `tokens.css`（亮/暗，`data-theme` 与 `arco-theme` 双开关）、Tailwind preset（零色值，经真实 tailwindcss 3.4.19 编译验证）、Arco 主题映射（只覆盖 Arco 自身定义的变量，逐个核对）；`check:tokens` 同时与 `docs/ux/DESIGN.md` frontmatter 逐项比对）；对比度脚本 82 个配对（亮/暗各算，文字 ≥ 4.5:1，图表 ≥ 3:1，已知例外设下限）；三态组件 `LeLoading/LeError/LeEmpty/LeStateBoundary`；格式化（日期/UTC/相对时间、字节 1024、带宽 bps 十进制、金额整数最小单位——BigInt 精确，>2^53 不丢精度）；i18n 工具（`createLeI18n`、`resolveLocale`、深合并、缺键回退与上报）与 CI 缺键检查；`openapi.gen.ts`（纯类型，头部记录 openapi.yaml sha256）。
- **AC3 lint 规则**：`scripts/lint-guards.mjs`（TypeScript AST + Vue 模板 AST）：shared 禁路由表/菜单/权限常量与判定/会话/API 客户端（含「等于任一 OpenAPI 路径模板或 `x-permissions` 的字符串字面量」，清单自动取自 openapi.yaml）；全仓禁 `v-html` 及等价写法、硬编码色值（含颜色函数与颜色名）、硬编码文案。80 个用例（每条规则至少一个必须失败的样例 + 不误报的正向样例，含「同样代码放 admin/console 合法」的边界用例）。ESLint 10（`--max-warnings 0`）作通用质量门禁与 `vue/no-v-html` 第二道保险。
- **AC4 构建隔离与 mock 检查**：`scripts/check-isolation.mjs`（清单由 `x-tenant-scope: platform` 自动生成：234 个 operation → 141 个精确路径模板；Console 产物另查 Admin 路由前缀与 Admin realm 常量，Admin 产物反向查 Console realm 常量）；`scripts/check-mock.mjs`（产物文件名/标记/sourcemap、源码静态 import mock、生产 `.env` 开关）。自测含**真实 vite 生产构建 e2e**。
- **AC5 UD-14**：令牌是提议，单一来源 `frontend/shared/tokens`；推翻只改 tokens.json 与 DESIGN.md（有脚本校验两者一致）。登记 [ASSUMPTION]（见下）。
- **AC6 IMP-14**：按 operation 路径模板精确匹配，不用 `/clusters`、`/nodes` 子串。自测对**真实 openapi**：每个 platform 模板出现在产物里都被检出（模板字面量与 `/api/v1` 前缀两种写法）、每个非 platform 路径都不误报；`/audit-logs`、`` `/sites/${id}/clusters` ``、`/clusters-info` 均不误报；静态兄弟路径（`/tenants/current` 之于 `/tenants/{tenantId}`）自动排除。

## 实测发现（均已落地并有回归测试）

1. **pnpm 12.18.0 不读取 `.npmrc` 的 `ignore-scripts`**：实测工作区自身 `postinstall` 仍执行；依赖脚本被默认拦截并以 `ERR_PNPM_IGNORED_BUILDS` 失败。有效开关是 `pnpm-workspace.yaml` 的 `ignoreScripts: true`（camelCase）。epics 原文「`.npmrc` 关闭」在 pnpm 12 上不起作用，故两处都配（`.npmrc` 兜底旧工具链）；`scripts/pnpm-hardening.test.mjs` 用真实 `pnpm install` 验证。
2. **pnpm 12 锁文件是多文档 YAML**：`check-deps` 用多文档解析。
3. **TypeScript 最新版 7.0.2 与工具链不兼容**：见 AC1。
4. **路径模板匹配的真实误报源**：对真实 openapi 全量自测时，`/tenants/current`（静态路径）会被 `/tenants/{tenantId}` 的通配误报；改为从 openapi 自动算出「更具体的非 platform 兄弟路径」并排除。
5. **mock 标记会被摇树优化掉**：真实 vite 构建 e2e 显示，只导出一个 `LE_MOCK_ENTRY` 常量时静态 import 的 mock 模块的标记会消失（产物里只剩函数体）；约定改为「标记必须是被执行代码里的字符串字面量」，同时源码层面禁止静态 import mock（两道防线）。
6. **light 主题 `chart-3`（`#D9822B`）在白底仅 2.93:1**（<非文本 3:1），DESIGN 未声明；登记为例外（下限 2.9 防恶化）并交 UD-14 复核，不擅自改色。
7. **Arco 2.59 实心按钮等文字色写死 `#fff`（24 处）**：暗色主题 `#7C93FF` 底白字 2.81:1；变量层无法修正，真实 Chrome 截图确认。登记给 1.38 / UD-14（见遗留）。
8. 门禁在自家代码上抓到的问题：`format` 的开发者向 `RangeError` 消息原为中文，被「硬编码文案」规则拦截，改为英文（开发者消息不是用户文案）。

## 测试

- **vitest 267 个**：`shared` 68（三态组件含计时器/可访问性/复制/XSS 文本渲染、格式化含进位与 >2^53 金额、i18n 回退与缺键上报）；`scripts` 199（令牌/对比度/DESIGN 一致/Tailwind 真实编译/Arco 变量名核对；guards 负向矩阵；依赖锁定与 i18n 负向；隔离与 mock；类型漂移；真实 vite 构建 e2e；真实 pnpm 脚本关闭）。
- **真实浏览器**：用 headless Chrome 对三态组件 + Arco 组件在亮/暗两个主题下截图人工核对（令牌与 Arco 映射生效、主题切换生效、zh/en 文案）；截图为一次性核对，未入库（发现 7 由此而来）。
- **变异验证**：对门禁与组件关键逻辑做临时改动 → 对应测试必须失败 → 还原。脚本侧 31 条，首轮检出 28 条；3 条未检出：权限字面量仅靠 openapi 清单识别的样例缺失（测试盲区，补用例后检出）、路径字面量检查的第一分支与第二分支等价（已删冗余代码）、依赖检查里 `^` 的专用分支（下游兜底仍会失败，属等价变异）。组件/格式化侧 17 条，16 条检出，1 条（`resolveLocale` 不识别 `zh`）因默认语言即 zh-CN 属等价变异。另对**真实构建产物**注入 `"/clusters"` / `LE_MOCK_ENTRY` 后 CLI 分别以退出码 1 失败。

## 对既有测试/防护的改动（逐条，均为随功能交付的必要更新，无放宽）

| 文件 | 改动 |
|---|---|
| `tests/scaffold/scaffold_test.go` | `TestMakefileTargets`：`frontend` 从「未实现必须输出 NOT_IMPLEMENTED(story 1.36)」清单移到「已实现」清单，并追加 6 个新目标；`TestMakefileWiresGates`：新增对 lint / test / gate-selftest / contract-check / verify-build / frontend / frontend-check 展开结果的断言（前端任一检查从配方消失即失败）。仅加强，无放宽 |
| `Makefile` | `lint`/`test`/`gate-selftest`/`contract-check`/`verify-build` 各追加前端子目标（原有子命令一条未删）；`frontend` 由占位改为真实目标；新增 `frontend-*` 目标；`help` 文案 |
| `.github/workflows/ci.yml` | 追加 setup-node（固定 SHA，Node 26.8.2）与 pnpm 12.18.0 安装步骤；步骤说明更新；原有步骤与命令未删 |
| `docs/architecture/dev-environment.md`、`ADR-014` | §1.3 pnpm/TypeScript 行补实测结论；ADR-014 追加修订记录 |

**未改动**：`scripts/gates` 与全部 Go 门禁、`.golangci.yml`、迁移、OpenAPI/proto（冻结契约 contract-v1、migrations-m1、CF-6）、`internal/**`、`docs/ux/*`。

## 与任务/通常做法不同的设计决定（须披露）

1. **`.npmrc` 之外增加 `pnpm-workspace.yaml` 的 `ignoreScripts`**（实测发现 1）。
2. **TypeScript 5.9.3**，不取 registry 最新（发现 3）。
3. **`website` 只是占位成员**（package.json 仅依赖 shared）：Nuxt 3.x 与官网 Tailwind 版本在 Story 2.1 核对；本 Story 用 devDependency `tailwindcss@3.4.19` 真实编译验证 preset，故 preset 的 Tailwind 4（CSS-first）兼容性未验证。
4. **admin/console 是「基座探针」**：index.html + 空 `<main>`，只为让 shared、Arco、令牌在各自的 Vite 构建里真实编译，并给隔离/mock 检查提供真实产物；不渲染假页面（FR-31）。
5. **字节标注 KB/MB/GB 而非 KiB/MiB**：沿用 UX-26（1024 进制标注 GB/MB）。
6. **对比度例外两条**（`text-disabled`、light `chart-3`）以下限形式登记在 tokens.json，而不是放宽阈值。
7. **门禁用 Node 脚本 + vitest 而非 ESLint 自定义插件**：架构规则需要 AST、Vue 模板与 OpenAPI 清单联动，且要有独立的正/负向自测；ESLint 仍承担通用质量门禁。
8. **`make lint/test/contract-check/verify-build` 现在依赖 Node 26 + pnpm**（用户要求并入既有目标）；缺失时这些目标失败，不静默跳过。

## [ASSUMPTION]

- **[ASSUMPTION] UD-14 视觉令牌**：主色 `#3B4FD8`、字体栈、圆角、间距、Arco 色阶（1–5/8–10 级由基色确定性混合得到）均为 UX 提议，采纳与否由 UD-14 决定；本 Story 只保证「单一来源 + 生成 + 与 DESIGN.md 一致 + 对比度」。
- [ASSUMPTION] 文字配对清单（`tokens.json.contrast`）覆盖正文/次级/品牌文字 × 四种底色、白字 × 品牌/悬停/渐变端点/危险、语义色 × 自身底与页面底；组件实际使用到其他组合时须追加配对。
- [ASSUMPTION] Admin 路由前缀清单暂为 `/admin`（`frontend/isolation/admin-route-prefixes.json`），随 1.38 路由表同步登记。
- [ASSUMPTION] Console 的 API 层使用模板字面量或完整字面量书写端点（字符串拼接无法被路径模板匹配，README 已写明）。
- [ASSUMPTION] 浏览器支持 `Intl.NumberFormat` 的精确十进制字符串输入（Chrome 106+/Safari 15.4+/Firefox 116+），金额格式化依赖它；官网/后台的浏览器基线需与此一致。
- [ASSUMPTION] 账期/计费口径对字节进制的覆盖留给 P5（沿用 UX-26）。

## 遗留逐项处置

| 项 | 处置 | 归属 |
|---|---|---|
| 远端 CI 首次实际运行（Node 26.8.2 / pnpm 12.18.0 步骤、`make verify-build` 含前端） | **待环境验证**：仓库无远端，`ci.yml` 已写，首次结果以实际为准 | 随首次推送；汇总于 1.45 |
| Arco 实心按钮等 `#fff` 文字在暗色主题下对比度 2.81:1（发现 7） | 变量层无法解决；需主题层窄覆盖或调整暗色品牌色 | **1.38 Admin 壳 + UD-14**（本 Story 不擅自改色/覆盖） |
| light `chart-3` 2.93:1（发现 6） | 已登记例外 | UD-14 视觉复核；图表实现（P4+）时核对 |
| Nuxt 3.x、官网 Tailwind、官网页面 | 本 Story 仅占位 | **2.1** |
| 安装向导 SPA（go:embed、其 i18n 词条与页面） | 使用本基座的 shared | **1.37** |
| Admin 壳、路由表、菜单、会话/API 层（使用 `import type` 的 openapi 类型）、echarts、Arco Pro 壳依赖核对；登记 Admin 路由前缀 | 本基座已提供门禁与约定 | **1.38 / 1.39** |
| Console 工程 | 与 Admin 同栈的独立工程骨架已在，业务在 P5 | **P5** |
| 表格 `LeTable`、状态胶囊、发布条、代码块等业务组件（DESIGN「shared 封装业务组件」） | 不在 AC2 清单内（AC2 为令牌/三态/格式化/i18n/类型） | 随首次使用它们的 Story（1.38/1.39）加入 shared，并受同一套边界门禁 |
| 真实浏览器矩阵（Safari/Firefox、移动端）视觉验收 | 本机仅 Chrome 截图核对；跨浏览器属验收 | **1.45** 前端验收 |
| 字节进制导出表头注明、计费口径 | UX-26 | **P5** |
