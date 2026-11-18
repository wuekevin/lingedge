-- lingedge:class=expand
-- lingedge:summary=三张表；第二张依赖运维才会创建的 fault_gate（首次执行时不存在 → 中途失败）
-- +goose Up
CREATE TABLE IF NOT EXISTS fault_a (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, note VARCHAR(32) NOT NULL) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS fault_b LIKE fault_gate;
CREATE TABLE IF NOT EXISTS fault_c (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, a_id BIGINT UNSIGNED NOT NULL, KEY idx_fault_c_a (a_id)) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON `fault_a` TO lingedge_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON `fault_b` TO lingedge_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON `fault_c` TO lingedge_app;
