-- lingedge:class=expand
-- +goose Up
CREATE TRIGGER trg_t_bi BEFORE INSERT ON t FOR EACH ROW SET NEW.id = 1;
