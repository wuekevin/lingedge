-- lingedge:class=reversible
-- +goose Up
ALTER TABLE rv_b ADD COLUMN note VARCHAR(64) NULL;

-- +goose Down
ALTER TABLE rv_b DROP COLUMN note;
