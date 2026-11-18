-- lingedge:class=expand
-- +goose Up
ALTER TABLE t ADD COLUMN c INT NOT NULL;
