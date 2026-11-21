# 契约变更说明 CC-9：商业类 operation 加 `x-feature: commercial`；`Me.features`；`PublicSiteInfo.console_installed`（加法）；无迁移变更

> 用法与格式见 `_template.md`。对应 Story 1.22（`docs/bmad/stories/1-22-系统设置-运行模式开关与能力注册表.md`），发生在 `contract-v1`（CF-1）与 `migrations-m1`（CF-2）之后。**本变更没有任何迁移改动**：`system_settings`、`system_installations`（`000002`）与 `lingedge_app` 的表级 `SELECT, INSERT, UPDATE, DELETE` 授权、`setting:read|write`、`system:read` 权限串（`000004` 种子）经核对已满足本 Story 全部需求；运行模式切换只是 `system_settings` 里的一行（FR-24），不需要迁移。

- **日期**：2026-11-21
- **基线标签**：contract-v1
- **影响范围**：
  1. `docs/api/openapi.yaml`（仅加法）：① 新增 operation 扩展 **`x-feature`**（取值 `commercial`），标注在全部商业类 operation 上——`billing` 标签下的 43 个 operation 与不带该标签的商业入口 `register`、`listPublicPlans`、`createTenant`（共 46 个）。语义：运行模式为 `self_hosted` 时返回 `403 feature.disabled`（认证之后、授权之前；该错误码、HTTP 403 与 `Forbidden` 响应早已在契约里声明，见 ADR-017 §3 / ADR-018 §11）。没有新增/删除/改名任何 operation、参数、必填项、`x-permissions|x-tenant-scope|x-audit|x-reauth|x-phase`；② 新增 schema `MeFeatures`（`run_mode`、`modules_delivered[]`、`capability_status{}`）并在 `Me` 上新增**可选**属性 `features`（原有 `capabilities` 不变，二者同源）；③ `PublicSiteInfo` 新增**可选**属性 `console_installed`（布尔：控制台是否已安装，FR-36/Story 1.22 AC4）；④ `info.description` 增补 `x-feature` 说明。
  2. `docs/api/README.md`：路由一致性规则（§一致性）增补 `x-feature` 约束。
  3. CI（`scripts/contract`）：新增 `checkFeatureTags`——商业类 operation（`billing` 标签与显式清单 `commercialOps`）必须声明 `x-feature: commercial`、取值须已知；运行时路由清单（`httpx.ManifestEntry.feature`）与契约的 `x-feature` 一致（`route-meta-mismatch`）；带负向样本自测（`TestFeatureTags`）。
  4. 迁移、proto：无改动。
- **是否破坏**：否（新增扩展与可选响应属性；`make openapi-breaking` 相对 `contract-v1` 无破坏性变更）
- **关联 ADR**：N/A（非破坏；依据 ADR-018 §9「`GET /auth/me` 返回 features」、§11「运行模式 self_hosted 下商业类 API 返回 403 feature.disabled」，实现细化见 ADR-018 文末「修订记录 2026-11-21（Story 1.22）」）
- **变更动机**：FR-24 要求自用模式下商业类 API 返回 `403 feature.disabled`，但契约里没有任何机器可读的「哪些 operation 是商业类」声明，路由注册层无从强制，CI 也无法发现「商业类路由漏标注 → 自用模式下不受保护」。`x-feature` 把该声明放进契约（与 `x-permissions` 同级），运行时由 `httpx.RouteMeta.Feature` 与功能开关中间件执行，CI 双向核对。`Me.features` 把能力清单（运行模式、已交付模块、高级能力状态）以前端可直接消费的形态固定下来（FR-31，NFR-27）；`console_installed` 补齐 Story 1.22 AC4 要求的「控制台是否已安装」。
- **兼容与迁移方案**：无迁移；已有客户端不受影响（`x-feature` 是文档/路由元数据；`features` 与 `console_installed` 是新增可选属性，旧客户端忽略）。商业模式（`commercial`）下行为不变。
- **验证**：`make openapi-check`（含 `openapi-breaking`、`checkFeatureTags`、运行时路由清单一致）、`make contract-check`、`tests/integration/sysconfig`（getMe / 公开接口 / 设置等响应经 kin-openapi 校验）。
- **评审**：开发代理（bmad-method:bmad-build）依据机器验证出具，2026-11-21；未经人工签收，用户可事后推翻（推翻 `x-feature` 则以新的契约变更删除该扩展；删除扩展属契约收紧，须走 ADR-017 §10 流程）。
