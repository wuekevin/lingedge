-- lingedge:class=expand
-- lingedge:summary=审计表：月分区含 pmax、禁 UPDATE/DELETE 触发器、仅追加授权
-- +goose Up
CREATE TABLE IF NOT EXISTS audit_logs (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  action VARCHAR(96) NOT NULL,
  PRIMARY KEY (id, created_at)
) ENGINE=InnoDB
PARTITION BY RANGE COLUMNS(created_at) (
  PARTITION p202611 VALUES LESS THAN ('2026-12-01 00:00:00'),
  PARTITION pmax VALUES LESS THAN (MAXVALUE)
);

DROP TRIGGER IF EXISTS trg_audit_logs_bu;
CREATE TRIGGER trg_audit_logs_bu BEFORE UPDATE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs is immutable';
DROP TRIGGER IF EXISTS trg_audit_logs_bd;
CREATE TRIGGER trg_audit_logs_bd BEFORE DELETE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs delete forbidden';

GRANT SELECT, INSERT ON `audit_logs` TO lingedge_app;
GRANT SELECT, ALTER, DROP ON `audit_logs` TO lingedge_maint;
