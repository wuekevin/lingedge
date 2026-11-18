-- lingedge:class=reversible
-- +goose Up
ALTER TABLE t ADD COLUMN c INT NULL;
