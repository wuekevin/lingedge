# CF-2 迁移冻结评审记录（`migrations-m1`）

- **日期**：2026-11-20
- **对应 Story**：1.9「M1 迁移 C：配置修订 / 发布队列 / 部署 / 观测（000012–000013）与契约冻结点 CF-2」（`docs/bmad/stories/1-9-m1-迁移-c-配置修订-发布队列-部署-观测-000012-000013-与契约冻结点-cf-2.md`）
- **签收方式**：同 CF-1（UD-25）——机器复核全绿 + 开发代理出具评审记录，**未经人工签收**；用户可事后推翻（推翻则 `migrations-m1` 作废，以新的 expand 迁移与变更说明修正）。
- **结论**：**通过**。允许打标签 `migrations-m1`（annotated tag，仅本地创建；标签指向的提交以 `git rev-parse migrations-m1^{commit}` 为准——本记录随该提交入库，故不在文内写死哈希）。

## 1. 冻结对象

| 对象 | 内容 |
|---|---|
| 迁移目录 | `migrations/000001`–`000013`，共 13 个文件、**61 张 M1 表**，全部 `expand`、`irreversible_count=0` |
| 哈希清单 | `migrations/CHECKSUMS` 登记 13 个文件的 SHA-256（`make migrations-seal` 追加；此后改动/删除任何已登记文件即 `make migrations-check` 失败） |
| 结构文档 | `docs/database/*.md`、`docs/database/verification/m1-fk-skeleton.sql`（与迁移一致；全表核对见 `TestM1CAllTablesMatchDatabaseDocsChecklist`） |

此后 M1 表结构仅允许**新增 expand 迁移**变更，并须在 `docs/bmad/contract-changes/` 新增变更说明（模板 `_template.md`）。

## 2. 冻结前决策：CC-1

跨租户外键的库层强制（复合外键 `(tenant_id, id)`）已在封存前以变更说明 [`CC-1`](CC-1-m1-cross-tenant-fk.md) 处理（Story 1.8 延后的中危项）。封存的是 CC-1 之后的迁移文本。

## 3. 复核项与实测结果

| 项 | 结果 | 证据 |
|---|---|---|
| 空库全量 `up`（13 文件） | 通过（本机 8.0.46，0 错误，61 表，版本 1–13） | `TestM1CUpOnEmptyDatabaseThenReplayIsIdempotent` |
| 重放幂等（含 3 条循环外键守卫、触发器重建、GRANT） | 通过 | 同上：库结构/授权/触发器/外键数不变；`up` 再次为空操作 |
| 全 `expand`、`irreversible_count=0`；降级 | 通过 | `TestM1CEmbeddedMigrationsAreLintCleanAndWellFormed`；expand 拒绝回退（框架层 `ErrDownNotAllowed`）；1.6 的逐步降级对 reversible 的覆盖见 `framework_test.go` |
| 触发器负向（`config_revisions`/`config_blocks` UPDATE 与未 purge 的 DELETE 被拒；`@lingedge_purge=1` 放行） | 通过（root 与 `lingedge_app` 均被拒） | `TestM1CConstraintsOnRealDatabase`、`TestM1CUp…` |
| 最小权限（逐表授权 61 条、DDL/GRANT 负向、maint 仅两张分区表） | 通过 | `TestM1CUp…` |
| DDL 陷阱预检（CHECK×FK、STORED×FK CASCADE、前向外键、复合 FK SET NULL） | 通过（静态规则 + 真实 MySQL 错误号 3823/1215/1824/1830） | `TestM1CDDLTraps…` |
| `make migrate-test`、`make contract-check` | 绿 | 见 Story 1.9 开发记录 |
| **8.0.16 最低版本空库全量 `up`** | **等待环境验证**（修订日志 §4 ①；本机仅 8.0.46，不得因 8.0.46 通过而标完成） | — |

## 4. 未覆盖 / 仍开放

- 修订日志 §4 的 5 项待环境验证仍开放（8.0.16 最低版本、PoC-5/3/1、Agent 单元内 `openresty -t`）。
- 缺少独立人工评审（UD-25 默认接受）；`contract-v1`/`migrations-m1` 标签仅在本地，推送后 CI 的 `make contract-check` 须同时拿到两枚标签。
- 1.9 的 `deployments` 同集群至多一个活动部署由 `publish.dispatch` 持有集群行锁保证（node-deploy §3.1），未做数据库级唯一约束（文档与骨架均未要求）。
