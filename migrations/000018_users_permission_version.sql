-- lingedge:class=expand
-- lingedge:summary=users 追加 permission_version：权限集版本号，权限/角色变更时递增，是权限缓存键的一部分（Story 1.18、CC-6）
-- 依据 docs/bmad/contract-changes/CC-6-rbac-permission-version.md：ADR-018 §8「权限集缓存键含 permission_version，变更时递增并经 Redis pub/sub 清缓存，
-- 缓存失效兜底靠短 TTL」。用户的角色授予变化、其任一角色的权限变化时，同一事务内对受影响用户 permission_version + 1；
-- 认证钩子每次读会话时同行取得该值，缓存键含版本号，旧版本的缓存项即刻不可达（TTL ≤ 60 秒后自然过期）。
-- NOT NULL DEFAULT 0：对已存在行与旧二进制的 INSERT 安全；users 已在 000001 对 lingedge_app 授予表级 SELECT/INSERT/UPDATE/DELETE，新列自动适用。
-- +goose Up
ALTER TABLE users ADD COLUMN permission_version BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER totp_last_step;
