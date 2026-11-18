-- lingedge:class=expand
-- +goose Up
CREATE TABLE IF NOT EXISTS audit_logs (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, created_at DATETIME(3) NOT NULL, PRIMARY KEY (id, created_at)) ENGINE=InnoDB PARTITION BY RANGE COLUMNS(created_at) (PARTITION pmax VALUES LESS THAN (MAXVALUE));
GRANT SELECT, INSERT, UPDATE, DELETE ON `audit_logs` TO lingedge_app;
GRANT SELECT, ALTER, DROP ON `audit_logs` TO lingedge_maint;
