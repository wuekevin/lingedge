-- lingedge:class=expand
-- +goose Up
SET foreign_key_checks = 0;
CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app;
