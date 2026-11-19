-- lingedge:class=expand
-- +goose Up
CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL, KEY idx_t_tenant (tenant_id, name)) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app;
