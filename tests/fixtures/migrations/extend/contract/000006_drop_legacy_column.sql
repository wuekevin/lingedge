-- lingedge:class=irreversible
-- lingedge:summary=contract：删除旧列（不可逆）
-- +goose Up
ALTER TABLE widgets DROP COLUMN legacy;

-- +goose Down
-- lingedge:reason=列及其数据已被删除，只能从备份恢复
SELECT 'irreversible';
