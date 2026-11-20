-- lingedge:class=expand
-- lingedge:summary=sessions 追加对外引用 uid（OpenAPI Session.session_ref / DELETE /auth/sessions/{sessionId} 为 ULID，不暴露自增 id、不暴露令牌哈希；Story 1.16、CC-4）
-- 依据 docs/bmad/contract-changes/CC-4-sessions-uid.md：database/platform-identity.md §2.7 把 sessions 定为 C2（无 uid），而 openapi.yaml 的 session_ref/sessionId 要求 Uid（ULID）。
-- 追加可空列 + 唯一索引（多个 NULL 不冲突，因此对「已存在行」安全；应用写入路径永远带 uid）。M1 冻结范围（000001–000013、61 张表）不变，本文件是其后的追加迁移。
-- 授权：sessions 已在 000001 对 lingedge_app 授予表级 SELECT/INSERT/UPDATE/DELETE，新增列自动适用，无需新授权。
-- +goose Up
ALTER TABLE sessions
  ADD COLUMN uid CHAR(26) NULL AFTER id,
  ADD UNIQUE KEY uk_sessions_uid (uid);
