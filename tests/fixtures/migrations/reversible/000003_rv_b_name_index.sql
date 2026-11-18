-- lingedge:class=reversible
-- +goose Up
ALTER TABLE rv_b ADD INDEX idx_rv_b_name (name);

-- +goose Down
ALTER TABLE rv_b DROP INDEX idx_rv_b_name;
