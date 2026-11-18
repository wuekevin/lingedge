-- lingedge:class=expand
-- +goose Up
CREATE TABLE t (id BIGINT UNSIGNED NOT NULL PRIMARY KEY) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app;
