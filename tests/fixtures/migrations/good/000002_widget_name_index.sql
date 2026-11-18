-- lingedge:class=reversible
-- lingedge:summary=按名称查询的索引
-- +goose Up
ALTER TABLE widgets ADD INDEX idx_widgets_tenant_name (tenant_id, name);

-- +goose Down
ALTER TABLE widgets DROP INDEX idx_widgets_tenant_name;
