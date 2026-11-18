-- lingedge:class=irreversible
-- +goose Up
CREATE USER 'someone'@'%' IDENTIFIED WITH caching_sha2_password;
