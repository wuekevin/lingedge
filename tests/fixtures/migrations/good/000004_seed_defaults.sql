-- lingedge:class=expand
-- lingedge:summary=种子数据（仅 DML，可重放，不含口令）
-- +goose Up
INSERT IGNORE INTO tenants (uid, name) VALUES ('01J00000000000000000000DEF', 'default');
