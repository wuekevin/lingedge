# 契约变更说明 CC-1：M1 跨租户外键的库层强制（复合外键 (tenant_id, id)）

> 用法与格式见 `_template.md`。本说明对应 Story 1.9（`docs/bmad/stories/1-9-m1-迁移-c-配置修订-发布队列-部署-观测-000012-000013-与契约冻结点-cf-2.md`），发生在 `contract-v1`（CF-1）之后、`migrations-m1`（CF-2）封存之前。

- **日期**：2026-11-20
- **基线标签**：contract-v1
- **影响范围**：`docs/database/`（README §2.4、tenant-site.md §2.3/§2.4/§3、rules-cache-cert.md §1/§2/§3）与 `docs/database/verification/m1-fk-skeleton.sql` 的外键定义；迁移 000008（`certificates`、`certificate_deployments`）、000009（`site_domains`、`site_settings`、`origins`、`origin_health`）、000010（`site_rules`、`rule_versions`）、000011（`cache_tasks`、`cache_task_nodes`）。OpenAPI 与 proto 无任何改动。
- **是否破坏**：否
- **关联 ADR**：N/A（非破坏；约束依据 ADR-009 §5 R-T1 租户隔离与 database/README §2.4 的复合外键「租户纵深防御」策略）
- **变更动机**：Story 1.8 评审登记的延后中危项：`site_domains.certificate_id`、`site_settings.default_certificate_id` 是指向 `certificates(id)` 的普通外键，租户 A 的域名可绑定租户 B 的证书；`certificate_deployments`、`origin_health`、`cache_task_nodes`、`rule_versions` 的 `tenant_id` 在数据库层不强制与父行同租户。仅靠 Service 校验时，一次遗漏即可造成跨租户引用（含私钥所在证书）。CF-2 封存后再改只能走新 expand 迁移，故在封存前决策。
- **兼容与迁移方案**：
  1. **采用复合外键**（用户倾向方案，未发现不宜之处）：父表增 `UNIQUE(tenant_id, id)`（`certificates`、`origins`、`cache_tasks`；`site_rules` 为 `UNIQUE(tenant_id, site_id, id)`），子表外键改为 `(tenant_id, <fk>) → 父表(tenant_id, id)`；`rule_versions` 为三列 `(tenant_id, site_id, rule_id) → site_rules(tenant_id, site_id, id)`。跨租户写入报 ERROR 1452。涉及 6 条外键：`certificate_deployments.certificate_id`、`site_domains.certificate_id`、`site_settings.default_certificate_id`、`origin_health.origin_id`、`cache_task_nodes.cache_task_id`、`rule_versions.rule_id`。
  2. **行为变化（唯一的语义调整）**：复合外键不能 `ON DELETE SET NULL`（`tenant_id NOT NULL` 冲突，ERROR 1830，已有 ddl-rules 用例），故 `site_domains.certificate_id`、`site_settings.default_certificate_id` 由 `SET NULL` 改为 `RESTRICT`：被绑定的证书不可物理删除，须先解绑。证书本身是软删除（`deleted_at`），Story 1.8 文档的「回滚前校验被引用证书未删除」不变；物理清理由保留期任务按依赖顺序先解绑再删，不影响 API 契约。`certificate_deployments`（本就 RESTRICT）、`origin_health`/`cache_task_nodes`（CASCADE）语义不变。
  3. **索引**：`idx_site_domains_cert` 改为 `(tenant_id, certificate_id)`（同时支撑外键，且符合「租户表索引以 tenant_id 打头」），`site_settings` 增 `idx_site_settings_cert(tenant_id, default_certificate_id)`。
  4. **不适用复合外键的残余项（决策：不用复合外键，改文档化的 Service/Repository 约束，并以测试守护清单）**：复合外键无法 `SET NULL`，或父行可为平台级（`tenant_id` NULL）。包括 `sites.group_id`、`site_rules.template_id`（模板可为平台内置）、`site_rules.current/published_version_id`、`deployments.trigger_site_id`、`publish_queue.site_version_id/deployment_id`、各 `task_id` 弱引用、`user_roles.role_id`（角色可为平台级）等，完整清单与逐项理由见 `tests/migrations/m1c_constraints_test.go` 的 `TestM1CCrossTenantForeignKeyInventory`：任何新增的「子表与父表都有 tenant_id、而外键不含 tenant_id」的外键，未登记即测试失败，迫使作者做出显式决策。这些引用多为追溯/弱引用，租户侧查询一律带 `tenant_id` 条件，不构成越权读取路径；相应的同租户校验由 Service 层（1.18、1.20 起）实现并要求隔离测试。
  4a. **评审加固（同类问题，同一决策）**：修订与集群一致性——`config_revisions` 增 `UNIQUE(cluster_id, id)`，`rollback_of_revision_id`、`deployments.revision_id`、`node_cluster_state.desired/applied_revision_id`、`clusters.confirmed_revision_id` 改复合外键 `(cluster_id, …) → config_revisions(cluster_id, id)`（RESTRICT，动作不变），跨集群引用修订在库层不可能；`publish_queue.queued_key` 改 `COALESCE(site_id, 0)`，使每集群至多一个排队中的集群回滚。
  5. 并行泳道返工：无（Server/Node/Admin 尚无依赖这些外键形态的代码；API 与 proto 不变）。不升 `contract-vN`；000001–000013 随 CF-2 封存。
  6. P2 的 `certificate_orders.certificate_id`（SET NULL）创建时按同一规则处理（README §2.4 已记）。
- **验证**：本机 MySQL 8.0.46 独立临时实例：`tests/migrations`（`TestM1CConstraintsOnRealDatabase` 的 CC-1 用例：6 条外键的跨租户写入均为 ERROR 1452、同租户通过、NULL 引用不检查、被绑定证书 RESTRICT=1451、复合外键 SET NULL=1830；`TestM1CCrossTenantForeignKeyInventory`；`TestM1BConstraintsOnRealDatabase` 更新后的证书用例；骨架 `TestSkeleton*` 与 `ddl_rules_test` 全绿）；`make migrations-check`、`make freeze-check`（迁移顺序与骨架一致、M1 61 表）、`make contract-check` 全绿（见 Story 1.9 开发记录）。
- **评审**：开发代理（bmad-build）依据机器验证出具，2026-11-20；未经人工签收，用户可事后推翻（推翻则以新的 expand 迁移回退，成本很低：仅外键与索引）。
