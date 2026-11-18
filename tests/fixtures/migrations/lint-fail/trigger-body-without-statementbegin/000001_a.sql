-- lingedge:class=expand
-- +goose Up
DROP TRIGGER IF EXISTS trg_t_bi;
CREATE TRIGGER trg_t_bi BEFORE INSERT ON t FOR EACH ROW BEGIN
  SET NEW.id = 1;
END;
