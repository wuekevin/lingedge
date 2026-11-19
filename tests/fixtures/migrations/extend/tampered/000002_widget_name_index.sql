-- lingedge:class=reversible
-- lingedge:summary=被篡改的版本
-- +goose Up
ALTER TABLE widgets ADD INDEX idx_widgets_tenant_name (tenant_id, name), ADD INDEX idx_widgets_evil (tenant_id, legacy);

-- +goose Down
ALTER TABLE widgets DROP INDEX idx_widgets_tenant_name;
