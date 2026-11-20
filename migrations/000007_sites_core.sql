-- lingedge:class=expand
-- lingedge:summary=站点核心：站点分组、站点（含复合外键目标 UNIQUE(tenant_id,id)，last_deployment_id 先建为无 FK 列）
-- 依据 database/README §2.4、§8.1，tenant-site.md §2.1、§2.2。均为租户范围表（C1 + T），指向 tenants(id) 的外键一律 RESTRICT。
-- sites.last_deployment_id 先建为无 FK 的普通列：deployments 在 000012 才创建，届时 ALTER 追加 FK → deployments(id) SET NULL（循环外键，README §2.4）。
-- sites.active_name 为 STORED 生成列（软删除后站点名可复用）：基列 name/deleted_at 无外键，满足 README §2.2 生成列规则。
-- sites.group_id / published_by 为 SET NULL 外键，不被任何 CHECK 引用（否则 ERROR 3823）。
-- 每张表建成后紧跟 lingedge_app 的逐表授权（README §5.1）。
-- +goose Up

-- ---- site_groups：站点分组（C1 + T；FR-33）----
CREATE TABLE IF NOT EXISTS site_groups (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL,
  description VARCHAR(512) NULL,
  sort_order INT NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  UNIQUE KEY uk_site_groups_uid (uid),
  UNIQUE KEY uk_site_groups (tenant_id, name),
  CONSTRAINT fk_site_groups_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON site_groups TO lingedge_app;

-- ---- sites：租户下的加速对象，站点聚合根（C1 + T + SD；ADR-002 工作态 vs 已发布态）----
-- uk_sites_tenant_id(tenant_id, id) 是站点子表复合外键 (tenant_id, site_id) 的目标：子行租户与父行不一致在库层不可能（ERROR 1452，README §2.4）。
-- published_* 仅在部署成功后前移（D-PUB）；ck_sites_published_version：published_config_version <= config_version。
CREATE TABLE IF NOT EXISTS sites (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  tenant_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(128) NOT NULL,
  active_name VARCHAR(128) AS (IF(deleted_at IS NULL, name, NULL)) STORED,
  group_id BIGINT UNSIGNED NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  description VARCHAR(512) NULL,
  config_version BIGINT UNSIGNED NOT NULL DEFAULT 1,
  published_config_version BIGINT UNSIGNED NOT NULL DEFAULT 0,
  published_block_sha256 CHAR(64) NULL,
  published_version_no BIGINT UNSIGNED NOT NULL DEFAULT 0,
  published_at DATETIME(3) NULL,
  published_by BIGINT UNSIGNED NULL,
  last_deployment_id BIGINT UNSIGNED NULL,
  purge_requested_at DATETIME(3) NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_sites_uid (uid),
  UNIQUE KEY uk_sites_tenant_id (tenant_id, id),
  UNIQUE KEY uk_sites_active_name (tenant_id, active_name),
  KEY idx_sites_tenant_status (tenant_id, status),
  KEY idx_sites_group (tenant_id, group_id),
  CONSTRAINT ck_sites_status CHECK (status IN ('active', 'disabled', 'deleting')),
  CONSTRAINT ck_sites_published_version CHECK (published_config_version <= config_version),
  CONSTRAINT fk_sites_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_sites_site_groups FOREIGN KEY (group_id) REFERENCES site_groups (id) ON DELETE SET NULL,
  CONSTRAINT fk_sites_users FOREIGN KEY (published_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON sites TO lingedge_app;
