-- lingedge:class=expand
-- +goose Up
CREATE TABLE IF NOT EXISTS seed_items (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, name VARCHAR(32) NOT NULL) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON `seed_items` TO lingedge_app;
