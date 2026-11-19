-- lingedge:class=expand
-- +goose Up
CREATE INDEX idx_t_name ON t (name);
