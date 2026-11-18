-- lingedge:class=expand
-- +goose Up
ALTER TABLE t MODIFY COLUMN c BIGINT NULL;
