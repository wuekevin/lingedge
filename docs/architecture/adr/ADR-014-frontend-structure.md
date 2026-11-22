# ADR-014 前端工程结构：Admin / Console 隔离

- 状态：已接受（架构代理非交互决策）｜日期：2026-11-16｜关联：基线 §2.2、§2.3、§6、§7、§20；PRD FR-30、FR-31、FR-37、NFR-4、NFR-29；architecture AD-14｜阶段：M1（Admin、website、shared）/ P5（Console）

## 背景

基线 §2.2 要求 Admin 与 Console 是**不同的前端入口**，可复用组件但**权限必须隔离**；FR-37 要求 Admin 能力不得经 Console 入口可达，且「Console 构建产物不含 Admin 专属路由与接口调用」。基线 §20 已规定 `frontend/{website,admin,console,shared}/`。需要决定工程管理方式、共享边界与隔离的可验证手段。

## 决策

1. `frontend/` 为 **pnpm workspace**（包管理器版本由根 `package.json` 的 `packageManager` 固定；pnpm 与 Node 26 兼容性实现时核对，见 dev-environment §1.3）：
   - `frontend/admin`：Vue 3 + TypeScript + Vite + Arco Design Pro（`arco-design-pro-vue` 脚手架，其自带依赖版本实现时核对）+ Pinia + Vue Router + Vue I18n + ECharts。
   - `frontend/console`：同栈，**独立的** Vite 工程、`index.html`、路由表、菜单、Pinia stores、构建输出（`dist/console`）。
   - `frontend/website`：Nuxt 3（3.x 线，ADR-015），**不使用 Arco**。
   - `frontend/shared`：仅放**无权限语义**的内容：设计令牌、通用 UI 组件（表格/表单封装、三态组件 Loading/Error/Empty、图表封装）、i18n 工具与公共词条、日期/金额/字节格式化、由 `openapi.yaml` 生成的 **TypeScript 类型**。
2. **共享包禁止包含**：路由表、菜单定义、权限常量与判定逻辑、会话/登录逻辑、API 客户端实例、对具体业务端点的调用。这些各应用自带。
3. **API 层**：类型由 `docs/api/openapi.yaml` 生成（工具如 openapi-typescript，实现时核对），每个应用只 import 自己需要的 operation；`admin` 独占 **`x-tenant-scope: platform` 的全部 operation**（含 `/api/v1/admin/*`、`/nodes*`、`/clusters*`、`/deployments*`、`/sites/{siteId}/cluster-bindings`、系统类端点；清单以 `openapi.yaml` 的 `x-tenant-scope` 为唯一来源，不在此手写）；`console` 只允许 `tenant|either|public` 的端点（审计读取为 `/audit-logs`、自己的登录历史为 `/auth/login-logs`，均已移出 `/admin/*`）。两个应用使用**不同的 Cookie realm**（`le_admin_sid` / `le_console_sid`，ADR-018）、不同的 `baseURL` 配置入口，并且**部署时必须使用互不相同的主机名**（Admin / Console / 官网三者互异，Cookie 为 host-only、不设 `Domain`；由 Tools 在安装预检中强制，tools-design §11.3）。
4. **权限在两端各自执行**：动态菜单与路由守卫由 `/auth/me` 返回的权限串驱动（ADR-018），但前端裁剪只是体验层，**真正的授权在服务端**（AD-9、AD-18）。
5. **构建隔离检查（CI，FR-37）**：构建后对 `console` 产物检查：不得出现任何 **`x-tenant-scope: platform` operation 的路径模板**（由 `openapi.yaml` 生成的脚本按模板**精确匹配**，如 `/nodes/{nodeId}`、`/clusters/{clusterId}/revisions`，匹配时把 `{param}` 视为单段通配）及 Admin 路由 path 前缀清单；**不再用 `/nodes`、`/clusters` 子串 grep**（该做法会被 `/sites/{siteId}/…` 等租户路径误报，或迫使放弃检查）。对 `admin` 产物反向检查不含 Console 专属 realm 常量。清单自动生成而非手写；`/audit-logs` 等 `either` 端点允许出现在两个产物中。
6. 「未交付模块不渲染假页面」（FR-31、NFR-1）：菜单项由「后端能力清单（`/auth/me` 的 `features` + 阶段开关）∩ 权限」决定；尚未发布的模块无路由、无占位假数据。生产构建禁止包含 mock 入口（ADR-023）。
7. 所有文案走 Vue I18n 键，中英两套；缺失键由 CI 检测（NFR-29）。每个页面实现加载/失败/空数据三态（FR-30）。

## Binds / Prevents / Rule

- 约束：前端各应用开发者、`shared` 维护者、CI。
- 防止：权限逻辑在两端复制出分歧；Console 产物夹带 Admin 能力；共享包变成「万能后门」。
- 规则（AD-14）：**Admin 与 Console 为两个独立 Vite 工程与构建产物，仅共享无权限逻辑的 `shared` 包。**

## 备选方案与权衡

- **单应用 + 运行时按角色裁剪路由**：否决——Admin 代码与端点字符串会进入租户可下载的 bundle，违反 FR-37；也让一次前端漏洞扩大到平台级能力。
- **Nx/Turborepo 等 monorepo 编排器**：否决（M1）——对 4 个包收益小，增加工具链依赖；pnpm workspace + Makefile 足够，规模增长后可复议。
- **Admin 与 Console 各自复制组件而不共享**：否决——违背基线「可复用组件」，且 i18n/设计令牌会漂移。

## 后果

- 两个应用有重复的壳（布局、登录页、请求封装）；用 `shared` 的无状态组件缓解，代价可接受。
- 需要维护 `console` 可用端点清单与 CI 检查脚本；脚本从 OpenAPI 元数据生成，避免手工漂移。
- Arco Pro 模板与 Vite/TypeScript 最新版的兼容性未知，可能迫使固定到较旧的工具链（实现时核对）。

## 可被推翻处

若用户希望首版即合并为单一前端（降低工程量），须放弃 FR-37 的「产物不含 Admin 路由」验收，并改为服务端强制隔离 + 路由懒加载；影响 `ADR-018` 的 realm 设计与 FR-37 验证方式。

## 验证

- CI：console 产物 grep 检查；`openapi.yaml` 的 `x-tenant-scope` 与前端路由清单一致性检查。
- 前端组件测试（vitest，版本实现时核对）：三态、表单校验、权限守卫；E2E（Playwright）：租户账号无法登录 Admin 入口、Admin 账号无法使用 Console realm（对应 FR-37 可验证项）。
- 生产构建中不得包含 mock 入口（ADR-023 的前端检查）。

## 修订记录 2026-11-16

- **IMP-14**：§3、§5 的 Console 隔离检查由“子串 grep（`/admin/`、`/nodes`、`/clusters`）”改为“按 `x-tenant-scope: platform` operation 的路径模板精确匹配”。原因：`GET /admin/audit-logs` 为 `either`、`/sites/{siteId}/clusters` 路径含 `/clusters`，子串规则要么误报要么形同虚设。配套：审计读取移出 `/admin/*`（`/audit-logs`），站点集群绑定改为平台独占 `/sites/{siteId}/cluster-bindings`，租户不再有“可绑定集群”列表（BLK-02，见 ADR-009 §5）。
- **IMP-17**：Admin / Console / 官网必须使用互不相同的主机名（host-only Cookie），由 Tools 强制，避免仅靠 Cookie 名隔离 realm。

## 修订记录 2026-11-22（Story 1.36 实现核对）

- **工具链版本以实测为准**：TypeScript 采用 5.9.3（不是 registry 最新 7.0.2——`typescript-eslint` 与 `openapi-typescript` 的 peer 范围不放行）；pnpm 12.18.0；Vite 8.3.8；vue-tsc 3.3.14；Arco Design Vue 2.59.0。完整矩阵与证据见 `frontend/README.md`「兼容矩阵」。
- **生命周期脚本开关位置**：pnpm 12 不读取 `.npmrc` 的 `ignore-scripts`，有效配置为 `pnpm-workspace.yaml` 的 `ignoreScripts: true`；`.npmrc` 仅作兜底。由 `check-deps` 强制并有真实 `pnpm install` 自测。
- **§5 构建隔离的落地细节**：路径模板匹配的左右边界（引号/反引号/`/api/v1` 起、非路径字符止）、同形状下更具体静态路径的排除、同形状混合 scope 的路径单列不入清单；Admin 路由前缀清单为 `frontend/isolation/admin-route-prefixes.json`（现 `/admin`，随 1.38 路由表登记）；Admin 产物反向检查 Console realm 常量，Console 产物同时检查 Admin realm 常量。已知限制：字符串拼接的端点无法被模板匹配，API 层须用模板字面量。
