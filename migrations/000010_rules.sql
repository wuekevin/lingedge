-- lingedge:class=expand
-- lingedge:summary=统一规则：规则模板、站点规则（工作态）、规则版本（不可变快照 + 触发器）；先建表后 ALTER 追加 current/published_version_id 外键
-- 依据 database/README §2.2、§2.4、§5、§8.1，rules-cache-cert.md §1（FR-63：所有规则类型共用 site_rules 单表）。
-- 循环外键：rule_versions.rule_id → site_rules，而 site_rules.current_version_id/published_version_id → rule_versions；
-- 先建两表（后两列为无 FK 普通列），再 ALTER TABLE site_rules ADD CONSTRAINT（README §2.4）。MySQL 的 ADD CONSTRAINT 无原生守卫，
-- 这里用 information_schema 判断 + PREPARE 的重跑安全守卫（迁移可重入，README §8.3）。
-- site_rules.active_name 为 STORED 生成列：基列 name/deleted_at 无外键。template_id/current_version_id/published_version_id 为 SET NULL 外键，不被任何 CHECK 引用（ERROR 3823）。
-- 【CF-2 前决策，见 contract-changes/CC-1】rule_versions.(tenant_id, site_id, rule_id) → site_rules(tenant_id, site_id, id) 三列复合外键：版本行的租户/站点必与其规则一致。
-- site_rules.template_id（模板可为平台内置，tenant_id NULL）与 current/published_version_id（SET NULL）无法用复合外键，由 Service/Repository 校验（见开发记录）。
-- rule_versions 不可变：BEFORE UPDATE 一律拒绝；BEFORE DELETE 仅清理任务（@lingedge_purge=1）可删（README §5）。触发器创建需迁移账号具 SET_USER_ID 或 log_bin_trust_function_creators=1。
-- +goose Up

-- ---- rule_templates：规则模板与配置模板（C1 + T?；tenant_id NULL = 平台内置；FR-50 模板功能 P2）----
CREATE TABLE IF NOT EXISTS rule_templates (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NULL,
  tenant_key BIGINT UNSIGNED AS (COALESCE(tenant_id, 0)) VIRTUAL NOT NULL,
  name VARCHAR(128) NOT NULL,
  description VARCHAR(512) NULL,
  rule_type VARCHAR(32) NOT NULL,
  phase VARCHAR(32) NOT NULL,
  doc_json JSON NOT NULL,
  params_schema_json JSON NULL,
  is_builtin TINYINT(1) NOT NULL DEFAULT 0,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  PRIMARY KEY (id),
  UNIQUE KEY uk_rule_templates_uid (uid),
  UNIQUE KEY uk_rule_templates (tenant_key, name),
  KEY idx_rule_templates_tenant (tenant_id, rule_type),
  CONSTRAINT ck_rule_templates_type CHECK (rule_type IN ('redirect', 'url_rewrite', 'origin_rule', 'request_header', 'origin_request_header', 'response_header', 'cache_rule', 'access_control', 'page_rule', 'error_page', 'hotlink', 'rate_limit')),
  CONSTRAINT ck_rule_templates_status CHECK (status IN ('active', 'deprecated')),
  CONSTRAINT fk_rule_templates_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON rule_templates TO lingedge_app;

-- ---- site_rules：站点规则的工作态（C1 + T + SD；已发布内容由 published_version_id 指向的不可变 rule_versions 决定）----
-- phase 由 rule_type 派生（只读，API 不接收）：ck_site_rules_type_phase 校验合法映射（rules-cache-cert.md §1.1）。
-- lingedge:cross-tenant-index=uk_site_rules_name 站点内活动规则名唯一（site_id 已隐含租户，复合外键保证 tenant 一致；软删除后名称可复用）
CREATE TABLE IF NOT EXISTS site_rules (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  rule_type VARCHAR(32) NOT NULL,
  phase VARCHAR(32) NOT NULL,
  name VARCHAR(128) NOT NULL,
  active_name VARCHAR(128) AS (IF(deleted_at IS NULL, name, NULL)) STORED,
  description VARCHAR(512) NULL,
  priority INT NOT NULL DEFAULT 1000,
  enabled TINYINT(1) NOT NULL DEFAULT 1,
  scope_json JSON NULL,
  doc_json JSON NOT NULL,
  doc_sha256 CHAR(64) NOT NULL,
  template_id BIGINT UNSIGNED NULL,
  current_version_id BIGINT UNSIGNED NULL,
  published_version_id BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_site_rules_uid (uid),
  UNIQUE KEY uk_site_rules_tenant_site_id (tenant_id, site_id, id),
  UNIQUE KEY uk_site_rules_name (site_id, active_name),
  KEY idx_site_rules_site_phase (tenant_id, site_id, phase, priority),
  KEY idx_site_rules_tenant (tenant_id, site_id, enabled),
  CONSTRAINT ck_site_rules_type CHECK (rule_type IN ('redirect', 'url_rewrite', 'origin_rule', 'request_header', 'origin_request_header', 'response_header', 'cache_rule', 'access_control', 'page_rule', 'error_page', 'hotlink', 'rate_limit')),
  CONSTRAINT ck_site_rules_phase CHECK (phase IN ('access_control', 'waf_cc', 'redirect', 'rewrite', 'cache_rule', 'origin_rule', 'origin_headers', 'upstream_response', 'response_headers', 'respond')),
  CONSTRAINT ck_site_rules_type_phase CHECK (
    (rule_type = 'redirect' AND phase = 'redirect')
    OR (rule_type IN ('url_rewrite', 'request_header') AND phase = 'rewrite')
    OR (rule_type = 'cache_rule' AND phase = 'cache_rule')
    OR (rule_type = 'origin_rule' AND phase = 'origin_rule')
    OR (rule_type = 'origin_request_header' AND phase = 'origin_headers')
    OR (rule_type = 'error_page' AND phase = 'upstream_response')
    OR (rule_type = 'response_header' AND phase = 'response_headers')
    OR (rule_type IN ('access_control', 'hotlink') AND phase = 'access_control')
    OR (rule_type = 'rate_limit' AND phase = 'waf_cc')
    OR (rule_type = 'page_rule')),
  CONSTRAINT ck_site_rules_priority CHECK (priority BETWEEN -100000 AND 100000),
  CONSTRAINT fk_site_rules_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_rules_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_site_rules_rule_templates FOREIGN KEY (template_id) REFERENCES rule_templates (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON site_rules TO lingedge_app;

-- ---- rule_versions：规则的不可变版本快照（C1 的 id/uid/created_at，无 updated_at；+ T；FR-67、NFR-20）----
-- lingedge:cross-tenant-index=uk_rule_versions 规则内版本号单调唯一（rule_id 已隐含租户）
CREATE TABLE IF NOT EXISTS rule_versions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NOT NULL,
  site_id BIGINT UNSIGNED NOT NULL,
  rule_id BIGINT UNSIGNED NOT NULL,
  version_no INT UNSIGNED NOT NULL,
  rule_type VARCHAR(32) NOT NULL,
  phase VARCHAR(32) NOT NULL,
  priority INT NOT NULL,
  enabled TINYINT(1) NOT NULL,
  scope_json JSON NULL,
  doc_json JSON NOT NULL,
  doc_sha256 CHAR(64) NOT NULL,
  change_note VARCHAR(512) NULL,
  rollback_of_version_id BIGINT UNSIGNED NULL,
  created_by BIGINT UNSIGNED NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_rule_versions_uid (uid),
  UNIQUE KEY uk_rule_versions (rule_id, version_no),
  KEY idx_rule_versions_tenant (tenant_id, site_id, rule_id),
  CONSTRAINT fk_rule_versions_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT,
  CONSTRAINT fk_rule_versions_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_rule_versions_site_rules FOREIGN KEY (tenant_id, site_id, rule_id) REFERENCES site_rules (tenant_id, site_id, id) ON DELETE RESTRICT,
  CONSTRAINT fk_rule_versions_rollback FOREIGN KEY (rollback_of_version_id) REFERENCES rule_versions (id) ON DELETE SET NULL,
  CONSTRAINT fk_rule_versions_users FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON rule_versions TO lingedge_app;

-- ---- 循环外键：site_rules.current_version_id / published_version_id → rule_versions(id) SET NULL ----
-- 重跑安全守卫：外键已存在则执行 DO 0（information_schema 判断 + PREPARE）；ADD CONSTRAINT 本身在 MySQL 无 IF NOT EXISTS。
-- lingedge:guard=经 information_schema.TABLE_CONSTRAINTS 判断外键是否已存在，已存在则 DO 0，重跑安全
SET @le_ddl = IF((SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'site_rules' AND CONSTRAINT_NAME = 'fk_site_rules_current_version' AND CONSTRAINT_TYPE = 'FOREIGN KEY') = 0, 'ALTER TABLE site_rules ADD CONSTRAINT fk_site_rules_current_version FOREIGN KEY (current_version_id) REFERENCES rule_versions (id) ON DELETE SET NULL', 'DO 0');
PREPARE le_stmt FROM @le_ddl;
EXECUTE le_stmt;
DEALLOCATE PREPARE le_stmt;

-- lingedge:guard=经 information_schema.TABLE_CONSTRAINTS 判断外键是否已存在，已存在则 DO 0，重跑安全
SET @le_ddl = IF((SELECT COUNT(*) FROM information_schema.TABLE_CONSTRAINTS WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME = 'site_rules' AND CONSTRAINT_NAME = 'fk_site_rules_published_version' AND CONSTRAINT_TYPE = 'FOREIGN KEY') = 0, 'ALTER TABLE site_rules ADD CONSTRAINT fk_site_rules_published_version FOREIGN KEY (published_version_id) REFERENCES rule_versions (id) ON DELETE SET NULL', 'DO 0');
PREPARE le_stmt FROM @le_ddl;
EXECUTE le_stmt;
DEALLOCATE PREPARE le_stmt;

-- ---- 不可变触发器（README §5）----
DROP TRIGGER IF EXISTS trg_rule_versions_bu;
CREATE TRIGGER trg_rule_versions_bu BEFORE UPDATE ON rule_versions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'rule_versions is immutable';

DROP TRIGGER IF EXISTS trg_rule_versions_bd;
-- +goose StatementBegin
CREATE TRIGGER trg_rule_versions_bd BEFORE DELETE ON rule_versions
FOR EACH ROW
BEGIN
  IF COALESCE(@lingedge_purge, 0) <> 1 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'rule_versions delete forbidden outside purge';
  END IF;
END;
-- +goose StatementEnd
