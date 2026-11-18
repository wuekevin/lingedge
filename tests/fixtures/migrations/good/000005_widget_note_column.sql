-- lingedge:class=reversible
-- lingedge:summary=加可空列（旧二进制不受影响）
-- +goose Up
ALTER TABLE widgets ADD COLUMN note VARCHAR(255) NULL;

-- +goose Down
ALTER TABLE widgets DROP COLUMN note;
