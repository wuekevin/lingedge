-- lingedge:class=reversible
-- lingedge:summary=Down 引用不存在的列，回退时必然失败
-- +goose Up
CREATE TABLE IF NOT EXISTS rv_x (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB;
GRANT SELECT, INSERT, UPDATE, DELETE ON `rv_x` TO lingedge_app;

-- +goose Down
ALTER TABLE rv_x DROP COLUMN nope;
