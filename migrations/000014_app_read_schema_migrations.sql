-- lingedge:class=expand
-- lingedge:summary=授权 lingedge_app 只读 schema_migrations（迁移版本表），使就绪检查能在不持有 migrate 凭据的前提下核对迁移版本（Story 1.13、CC-3）
-- 依据 docs/bmad/contract-changes/CC-3-app-read-schema-migrations.md、Story 1.12 遗留项、database/README §5.1（三类账号最小权限）。
-- schema_migrations 由迁移框架（goose 版本表）在首次迁移前自行创建，不由任何迁移文件建表；故这是唯一允许「授权目标表不由迁移创建」的例外，
-- 且框架 lint 强制：对 lingedge_app 只能是 SELECT（不得 INSERT/UPDATE/DELETE，不得带 GRANT OPTION）。写入权限仍由 migrate 账号独占。
-- 不授权 schema_migrations_runs（运行记录含错误文本，就绪检查不需要）。M1 冻结范围（000001–000013、61 张表）不变，本文件是其后的追加迁移。
-- +goose Up
GRANT SELECT ON schema_migrations TO lingedge_app;
