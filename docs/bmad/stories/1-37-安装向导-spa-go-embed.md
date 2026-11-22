---
title: 'Story 1.37：安装向导 SPA（go:embed）'
type: 'feature'
created: '2026-11-22'
status: 'review'
route: 'dispatch'
review_loop_iteration: 0
baseline_commit: '59ecdbd'
waiting_env_verification: true
context:
  - docs/bmad/epics.md Story 1.37（AC1–AC5、测试要求、DoD）
  - docs/ux/screens.md §9、docs/architecture/tools-design.md §13/§13.1、ADR-026（UD-31）、ADR-014、CC-11/CC-12
  - docs/bmad/stories/1-24-*（向导后端 API 与 setup.* 动作）、1-35-*（token 与联调）、1-36-*、frontend/README.md
---

## Intent

**Problem:** Story 1.24 只交付了向导后端与一个占位页，运营者无法在浏览器里完成 8 步初始化；1.24/1.27b 还登记了「运行期凭据文件不支持 Unix socket」需随 1.37 以 CC 加法落地。

**Approach:** 新增 `frontend/setup`（Vue 3 + TS，使用 1.36 的 shared 令牌/三态/i18n/生成类型与全部门禁），生产构建入库于 `internal/platform/setup/webdist` 并 `go:embed` 进 `lingedge-server`；`/setup` 与 `/setup/assets/*` 由 setup mode 提供（严格 CSP），`install.lock` 之后统一 `410 setup.already_installed`。同时完成 CC-13：`SetupDbConfig.socket`（MySQL Unix socket 全链路）与 `SetupStepResult.migrations`（初始化步骤的结构化迁移明细）。

## 逐条验收

- **AC1**：未输入 token 前不渲染任何向导内容；`getSetupStatus` 验证 token 后进入 8 步（环境检查→MySQL→Redis→初始化数据库→管理员→基础配置→密钥→完成）；不允许跳过未完成步骤，已完成步骤可回看。
- **AC2**：每步结果区均有 Loading/Error/Empty 三态（`LeStateBoundary`；Empty 例：环境检查无项、初始化「已是最新」、表单「尚未提交」）；错误卡显示稳定错误码、Request ID（可复制）、处置提示与逐字段原因（401/400/409/410/429/网络中断均覆盖）；口令强度条 + 逐条规则（文字+标记，不只靠颜色）、二次确认、口令框 `type=password`/不回显/提交后清空；TOTP 二维码（纯 SVG 逐模块绘制，无 `v-html`）+ 恢复码（须勾选「已保存」）；中/英文；全键盘可操作（跳转链接、焦点环、步骤切换后焦点移到标题、Enter 提交、空格切换复选框）；对比度由真实 Chromium 对每页做 WCAG 审计（≥4.5:1，含审计器负向自测）。
- **AC3**：完成页显示 Admin URL（基础配置的访问地址）与「`master.key` 须与数据库备份分开保管」强提示（密钥步骤与完成页都有，完成页须勾选确认才能提交，完成后仍显示）；自动生成的迁移口令一次性展示并附 `sudo lingedge-server migrate credential init`；`install.lock` 之后 `/setup`、`/setup/*`、全部 `/public/setup/*` 统一 `410`。
- **AC4**：token 只在 API 客户端闭包内存；不进 URL（地址栏里的 token 参数被丢弃并移除）、不进 `sessionStorage/localStorage/cookie`；刷新后重输；验证失败或完成后清除。E2E 真实浏览器逐页断言。
- **AC5**：正常模式下 `/setup`、`/setup/`、`/setup/assets/*` 与 9 个向导 operation 一样 `410 setup.already_installed`（原先 `/setup` 为 404，已改并同步更新 2 处既有断言）。
- **1.35 AC6 / 1.24 遗留 Unix socket（CC-13）**：属本 Story 范围并已做完：`SetupDbConfig.socket`；`config.Database.Socket`（`database.socket`、`LINGEDGE_DATABASE__SOCKET`）；`dbcred`（文件含 `socket`、与 host/port 互斥、`Merge` 成组取用）；`provision`/setup 全部连接构造；`migrate credential init|reset` 与 `db credential rotate` 的 socket 路径；向导 MySQL 步骤「Unix socket」连接方式。本机用真实 mysqld 的 socket 全流程验证（浏览器与 Go 集成测试各一）。

## 改动清单（主要文件）

- 新增：`frontend/setup/**`（`src/{api,state,steps,components,locales,styles,testing}`、`e2e/*.spec.ts`、`vite.config.ts` 等）；`internal/platform/setup/{web.go,web_test.go,webdist/**}`；`frontend/scripts/{sync-setup-embed.mjs,lib/embed-sync.mjs,embed-sync.test.mjs}`；`tests/integration/setup/{browser_test.go,socket_test.go}`；`docs/bmad/contract-changes/CC-13-*.md`。
- 修改：`docs/api/openapi.yaml`（仅加法/放宽，CC-13）、`frontend/shared/src/api-types/openapi.gen.ts`（生成）、`frontend/{package.json,pnpm-workspace.yaml,pnpm-lock.yaml,vitest.config.ts,README.md}`、`frontend/scripts/{check-isolation,check-mock}.mjs`、`lib/{isolation,paths}.mjs`（新增 `setup` 角色/路径）、`Makefile`（`setup-web`/`setup-web-check`/`e2e`，`frontend` 含 `check:embed`，`frontend-test` 含向导组件测试）、`.gitattributes`、`internal/platform/setup/{http,types,steps_db,steps_post,service,fromfile}.go`、`internal/platform/{config,db,dbcred,migrate,migrate/provision}`、`cmd/lingedge-server/{assembly/assembly,dbcmd,lineagecmd}.go`、`configs/server.example.yaml`、`docs/{operations/server-config,architecture/tools-design,architecture/adr/ADR-026,ux/screens}.md`；删除 `internal/platform/setup/static/setup.html`（占位页）。
- 既有测试/防护的必要调整（逐条，均为随行为变更的同步或加强，**无任何放宽、无新增豁免**）：①`tests/integration/setup/process_test.go`、`e2e_test.go`：正常模式 `/setup` 由「应 404」改为「应 410 setup.already_installed」（并追加 `/setup/`、`/setup/assets/*`）；②`internal/platform/setup/http_test.go`：删除占位页断言（`TestStaticPagePlaceholder…`、正常模式 `/setup` 404），由 `web_test.go` 的真实构建/CSP/410 用例取代；③`tests/scaffold/scaffold_test.go`：`e2e` 从「NOT_IMPLEMENTED(story 1.37)」清单移到已实现清单，并给 `test/verify-build/frontend/setup-web/setup-web-check/e2e` 增加配方接线断言（`run test:setup`、`run check:embed` 等，任一项消失即失败）；④分支内为通过 G-6 与「生成类型只含类型」门禁而改了自己的命名（词条键不含 `password/secret` 字样、测试夹具口令运行期随机生成、E2E 环境变量用 `*_PW`、OpenAPI 字段名 `category` 而非 `class`），未改任何门禁。

## 设计决定与说明

- **产物入库 + 漂移/缺失检查**（而非只由 make 生成）：`go build/test`、发行与 Tools 安装包构建不依赖 Node；与令牌生成物、生成类型的既有先例一致；嵌入内容可在 PR 审查；`make verify-build`（经 `frontend`）与 `make setup-web-check` 重新构建并逐字节比对（构建实测可重现），缺失或漂移失败；Go 侧用例保证嵌入的是真实完整构建，缺失时 setup mode 拒绝启动。代价：`webdist` 随前端变动产生约 250 KB 的生成物 diff（`.gitattributes` 标 `-text -diff linguist-generated`）。
- **不引入 Arco/路由/状态库**：向导只需表单与三态；产物约 230 KB JS；规避 Arco 暗色 `#fff` 文字对比度问题；向导固定亮色主题（**→ 已由 Story 1.37b 改为跟随系统/令牌的暗色**）。
- **token 仅内存**（满足「内存/sessionStorage」中更严的一侧，也满足「刷新后重输」）；非机密草稿（访问地址、语言）用 `sessionStorage`。
- **服务端回执文案为中文**：成功/检查项的 `message` 由服务端固定中文生成；界面主文案全部走 i18n 键，服务端回执作为补充细节展示并标 `lang="zh-CN"`。英文界面里它仍是中文（**→ 已由 Story 1.37b 按稳定错误码/检查项/步骤标识映射到前端词条，未知回落服务端原文**，不改 OpenAPI 形状）。
- **应用账号口令**：建账号模式留空时前端本地生成 32 位随机口令（只加密存于运行期凭据文件，无需记忆）。
- **TOTP 两阶段**：沿用 CC-12；第二次提交需再次带口令，所以口令在 TOTP 确认完成前保留在组件内存，完成即清空。
- **初始化步骤的断线恢复**：POST 长时间运行遇网络中断时轮询 `GET status`（OpenAPI 约定的进度通道）直到 `init_db` 不再 running。
- **偏离 UX 线框**（已写入 `docs/ux/screens.md` §9 实现说明）：无「下载恢复材料」按钮（`master.key` 只由 Tools 生成，向导不持有内容）；无「用户名」字段（API 只有 email/display_name）；新增 Unix socket 连接方式。

## 测试（真实、无 Mock 冒充生产）

- vitest：`frontend/setup/src/**`（API 客户端、URL token、状态机、各步骤三态/校验/口令不回显/TOTP/完成页/键盘与 aria/中英文、口令强度、校验工具；网络层替身仅存在于 `src/testing`，不被生产代码引用，`check:mock` 通过）；`scripts` 自测含嵌入同步（漂移/缺失/多余/空转）与 `setup` 隔离角色负向用例。
- Go：`web_test.go`（真实构建完整性、验证器负向、CSP/路径穿越/类型/缓存、正常模式 410）；`socket_test.go`（CC-13 全流程与校验）。
- **Playwright E2E（本机真跑，ENV-BROWSER 可用）**：真实 `lingedge-server`（setup mode，内嵌真实 SPA）+ 独立临时 mysqld（binlog=ON、trust=0）/redis + 真实 Chromium（Playwright 1.65.1 / chromium-1255）：全流程（TCP+TOTP+中文、Unix socket+英文）、错 token（含 URL token、限流后正确 token 也被拒）、非空库（数据原样保留）、中断续做 early（SIGKILL 重启后初始化 409 → 回 MySQL 步骤重提交）/late、键盘、逐页对比度审计、控制台/CSP 零违规、token 不在任何存储/URL、完成后 `/setup` 410、一次性机密对日志/审计/凭据文件的泄漏扫描、管理员可登录（含 TOTP 第二步）；截图已人工逐页核对（亮色、中英文、桌面宽度）。

## 遗留与风险

- **等待环境验证**：ENV-BT——宝塔 MySQL 的 socket 对 `lingedge` 用户的可访问性（本机只验证代码路径与真实 mysqld socket）；ENV-LNX 项沿用 1.24/1.35 登记；跨浏览器/移动端视觉验收（1.45，本机仅 Chromium 桌面）；远端 CI 首跑（Playwright 浏览器下载与 `make e2e`）。
- **属后续 Story**：Admin 本体（1.38）。（服务端回执文案国际化、向导暗色主题已由 Story 1.37b 完成。）
- 已知限制：刷新页面时若 TOTP 密钥/恢复码尚未保存则无法在向导内重新取得（只展示一次，设计如此；管理员可登录后在 Admin 中重新绑定）。
