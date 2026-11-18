-- lingedge:class=expand
-- lingedge:summary=租户与示例业务表
-- +goose Up
CREATE TABLE IF NOT EXISTS tenants (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  uid CHAR(26) NOT NULL,
  name VARCHAR(128) NOT NULL,
  UNIQUE KEY uk_tenants_uid (uid)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS widgets (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  tenant_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL,
  legacy VARCHAR(64) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_widgets_tenant (tenant_id, id),
  CONSTRAINT fk_widgets_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT
) ENGINE=InnoDB;

GRANT SELECT, INSERT, UPDATE, DELETE ON `tenants` TO lingedge_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON `widgets` TO lingedge_app;
