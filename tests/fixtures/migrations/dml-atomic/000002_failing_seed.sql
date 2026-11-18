-- lingedge:class=expand
-- lingedge:summary=纯 DML；第二条语句违反 NOT NULL（ERROR 1048），第一条必须回滚
-- +goose Up
INSERT IGNORE INTO seed_items (id, name) VALUES (1, 'a'), (2, 'b');
UPDATE seed_items SET name = NULL WHERE id = 2;
