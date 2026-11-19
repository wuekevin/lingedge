-- lingedge:class=expand
-- +goose Up
-- lingedge:cross-tenant-index=idx_t_user
CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, user_id BIGINT UNSIGNED NOT NULL, KEY idx_t_user (user_id)) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app;
