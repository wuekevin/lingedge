-- lingedge:class=irreversible
-- +goose Up
ALTER TABLE t DROP COLUMN c;
-- +goose Down
SELECT 'irreversible';
