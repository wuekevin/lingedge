-- lingedge:class=expand
-- +goose Up
ALTER TABLE t RENAME COLUMN a TO b;
