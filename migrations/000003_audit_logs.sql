-- lingedge:class=expand
-- lingedge:summary=审计日志：按月 RANGE 分区（含 pmax 兜底）、禁 UPDATE/DELETE 触发器、lingedge_app 仅 SELECT/INSERT、lingedge_maint 表级 SELECT/ALTER/DROP
-- 依据 database/README §5、§5.1、§8.1，platform-identity.md §3.1（FR-23）。
-- 分区表无外键（实测 ERROR 1506：分区表既不能作父也不能作子）；唯一键必含分区键 created_at。
-- 兜底分区 pmax 使越界写入永远不报 ERROR 1526；Worker 的内部周期作业 partition.maintain 用 lingedge_maint 把 pmax REORGANIZE 出未来月份分区，
-- 保留期用 DROP PARTITION（不触发行级 DELETE 触发器，实测 README §9 第 12 行）。初始分区覆盖 2026-11 至 2027-12（2026-11 分区同时容纳更早的时间戳）。
-- 不可变触发器的创建需迁移账号具 SET_USER_ID 或 log_bin_trust_function_creators=1（README §5.1，迁移前的账号探针已校验）。
-- +goose Up
CREATE TABLE IF NOT EXISTS audit_logs (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  actor_type VARCHAR(16) NOT NULL,
  actor_user_id BIGINT UNSIGNED NULL,
  actor_token_id BIGINT UNSIGNED NULL,
  actor_label VARCHAR(255) NOT NULL,
  actor_tenant_id BIGINT UNSIGNED NULL,
  tenant_id BIGINT UNSIGNED NULL,
  acting_as TINYINT(1) NOT NULL DEFAULT 0,
  action VARCHAR(96) NOT NULL,
  resource_type VARCHAR(48) NOT NULL,
  resource_uid VARCHAR(64) NULL,
  result VARCHAR(16) NOT NULL,
  changes_json JSON NULL,
  meta_json JSON NULL,
  request_id CHAR(26) NOT NULL,
  ip VARBINARY(16) NULL,
  user_agent VARCHAR(512) NULL,
  PRIMARY KEY (id, created_at),
  UNIQUE KEY uk_audit_logs_uid (uid, created_at),
  KEY idx_audit_logs_tenant (tenant_id, created_at),
  KEY idx_audit_logs_actor (actor_user_id, created_at),
  KEY idx_audit_logs_resource (resource_type, resource_uid, created_at),
  KEY idx_audit_logs_request (request_id),
  KEY idx_audit_logs_action (action, created_at)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(created_at) (
  PARTITION p202611 VALUES LESS THAN ('2026-12-01 00:00:00'),
  PARTITION p202612 VALUES LESS THAN ('2027-01-01 00:00:00'),
  PARTITION p202701 VALUES LESS THAN ('2027-02-01 00:00:00'),
  PARTITION p202702 VALUES LESS THAN ('2027-03-01 00:00:00'),
  PARTITION p202703 VALUES LESS THAN ('2027-04-01 00:00:00'),
  PARTITION p202704 VALUES LESS THAN ('2027-05-01 00:00:00'),
  PARTITION p202705 VALUES LESS THAN ('2027-06-01 00:00:00'),
  PARTITION p202706 VALUES LESS THAN ('2027-07-01 00:00:00'),
  PARTITION p202707 VALUES LESS THAN ('2027-08-01 00:00:00'),
  PARTITION p202708 VALUES LESS THAN ('2027-09-01 00:00:00'),
  PARTITION p202709 VALUES LESS THAN ('2027-10-01 00:00:00'),
  PARTITION p202710 VALUES LESS THAN ('2027-11-01 00:00:00'),
  PARTITION p202711 VALUES LESS THAN ('2027-12-01 00:00:00'),
  PARTITION p202712 VALUES LESS THAN ('2028-01-01 00:00:00'),
  PARTITION pmax VALUES LESS THAN (MAXVALUE)
);

DROP TRIGGER IF EXISTS trg_audit_logs_bu;
CREATE TRIGGER trg_audit_logs_bu BEFORE UPDATE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs is immutable';

DROP TRIGGER IF EXISTS trg_audit_logs_bd;
CREATE TRIGGER trg_audit_logs_bd BEFORE DELETE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs delete forbidden';

-- 追加只写：应用账号对审计表仅 SELECT, INSERT；维护账号表级 SELECT, ALTER, DROP（REORGANIZE/DROP PARTITION），不能 DROP 其它表（ERROR 1142）。
GRANT SELECT, INSERT ON audit_logs TO lingedge_app;
GRANT SELECT, ALTER, DROP ON audit_logs TO lingedge_maint;
