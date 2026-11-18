-- lingedge:class=expand
-- lingedge:summary=expand：加可空列
-- +goose Up
ALTER TABLE widgets ADD COLUMN flags INT NULL;
