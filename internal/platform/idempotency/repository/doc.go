// Package repository 是 idempotency.Store 的 MySQL 实现（idempotency_keys 表，migrations/000002）。
//
// idempotency_keys 是平台级表（无 tenant_id）：隔离靠 principal_key（`<platform|tenant>:<actor>:<tenant>`），
// 故导出方法在文档注释中登记 `lingedge:platform-table=idempotency_keys` 以豁免 Scope 首参（repo-check）。
// 只接受 app 连接池（lingedge_app 对该表有 SELECT/INSERT/UPDATE/DELETE）；所有 SQL 均为常量，值经占位符绑定。
package repository
