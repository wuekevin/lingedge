-- lingedge:class=irreversible
-- +goose Up
ALTER TABLE t DROP COLUMN c;
-- +goose Down
-- lingedge:reason=x
ALTER TABLE t ADD COLUMN c INT NULL;
