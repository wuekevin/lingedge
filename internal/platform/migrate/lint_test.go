package migrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

// lintOne 对单个文件内容运行全部规则（孤立集合，序号 000001）。
func lintOne(body string) []Finding {
	_, fs, _ := Load(fstest.MapFS{"000001_case.sql": {Data: []byte(body)}})
	return fs
}

func wantRule(t *testing.T, body, rule string) {
	t.Helper()
	fs := lintOne(body)
	if !hasRule(fs, rule) {
		t.Fatalf("期望规则 %q 被触发；实际发现：%v\n--- 文件 ---\n%s", rule, fs, body)
	}
}

func wantClean(t *testing.T, body string) {
	t.Helper()
	if fs := lintOne(body); len(fs) != 0 {
		t.Fatalf("期望通过 lint；实际发现：%v\n--- 文件 ---\n%s", fs, body)
	}
}

func hdr(class string) string { return "-- lingedge:class=" + class + "\n-- +goose Up\n" }

const grantT = "GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app;\n"
const createT = "CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB;\n"

func TestLintAcceptsWellFormedMigrations(t *testing.T) {
	wantClean(t, hdr("expand")+createT+grantT)
	wantClean(t, hdr("reversible")+createT+grantT+"-- +goose Down\nDROP TABLE IF EXISTS t;\n")
	wantClean(t, hdr("irreversible")+"ALTER TABLE t DROP COLUMN old;\n-- +goose Down\n-- lingedge:reason=列已删除\nSELECT 'irreversible';\n")
	wantClean(t, hdr("irreversible")+"ALTER TABLE t DROP COLUMN old;\n") // Down 为空：无需原因
	// 含 BEGIN...END 的触发器：StatementBegin/End + 先 DROP IF EXISTS。
	wantClean(t, hdr("expand")+"DROP TRIGGER IF EXISTS trg_t_bi;\n-- +goose StatementBegin\nCREATE TRIGGER trg_t_bi BEFORE INSERT ON t FOR EACH ROW BEGIN SET NEW.id = NEW.id; END;\n-- +goose StatementEnd\n")
	// 种子 DML：可重放。
	wantClean(t, hdr("expand")+"INSERT IGNORE INTO permissions (code) VALUES ('a:b');\nINSERT INTO permissions (code) VALUES ('c:d') ON DUPLICATE KEY UPDATE code = VALUES(code);\n")
	// 单语句 ALTER 文件（MySQL 8 单条 DDL 原子）无需 guard。
	wantClean(t, hdr("reversible")+"ALTER TABLE t ADD INDEX idx_t_a (a);\n-- +goose Down\nALTER TABLE t DROP INDEX idx_t_a;\n")
}

func TestLintClassRules(t *testing.T) {
	t.Run("expand 含 DROP", func(t *testing.T) {
		wantRule(t, hdr("expand")+"ALTER TABLE t DROP COLUMN c;\n", "class-expand")
	})
	t.Run("expand 含 MODIFY", func(t *testing.T) {
		wantRule(t, hdr("expand")+"ALTER TABLE t MODIFY COLUMN c BIGINT NULL;\n", "class-expand")
	})
	t.Run("expand 含 RENAME", func(t *testing.T) {
		wantRule(t, hdr("expand")+"ALTER TABLE t RENAME COLUMN a TO b;\n", "class-expand")
		wantRule(t, hdr("expand")+"RENAME TABLE t TO t2;\n", "class-expand")
	})
	t.Run("expand 含 DROP TABLE", func(t *testing.T) {
		wantRule(t, hdr("expand")+"DROP TABLE IF EXISTS t;\n", "class-expand")
	})
	t.Run("expand 的 Down 里可以 DROP", func(t *testing.T) {
		wantClean(t, hdr("expand")+createT+grantT+"-- +goose Down\nDROP TABLE IF EXISTS t;\n")
	})
	t.Run("reversible 没有 Down", func(t *testing.T) {
		wantRule(t, hdr("reversible")+"ALTER TABLE t ADD COLUMN c INT NULL;\n", "class-reversible")
	})
	t.Run("reversible 只有 irreversible 标记", func(t *testing.T) {
		wantRule(t, hdr("reversible")+"ALTER TABLE t ADD COLUMN c INT NULL;\n-- +goose Down\nSELECT 'irreversible';\n", "class-reversible")
	})
	t.Run("irreversible 提供了真实 Down", func(t *testing.T) {
		wantRule(t, hdr("irreversible")+"ALTER TABLE t DROP COLUMN c;\n-- +goose Down\n-- lingedge:reason=x\nALTER TABLE t ADD COLUMN c INT NULL;\n", "class-irreversible")
	})
	t.Run("irreversible 的 Down 非空但没有原因", func(t *testing.T) {
		wantRule(t, hdr("irreversible")+"ALTER TABLE t DROP COLUMN c;\n-- +goose Down\nSELECT 'irreversible';\n", "class-irreversible")
	})
}

func TestLintIdempotencyRules(t *testing.T) {
	wantRule(t, hdr("expand")+"CREATE TABLE t (id INT) ENGINE=InnoDB;\n"+grantT, "idempotency")
	wantRule(t, hdr("irreversible")+"DROP TABLE t;\n", "idempotency")
	wantRule(t, hdr("expand")+"-- +goose StatementBegin\nCREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW BEGIN SET NEW.id = 1; END;\n-- +goose StatementEnd\n", "idempotency")
	// 多语句里的 ADD CONSTRAINT 没有原生守卫：需单语句文件或 guard 标注。
	multi := hdr("expand") + createT + grantT + "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES t(id);\n"
	wantRule(t, multi, "idempotency")
	guarded := hdr("expand") + createT + grantT + "-- lingedge:guard=经 information_schema 判断后再 PREPARE 执行\nALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (id) REFERENCES t(id);\n"
	wantClean(t, guarded)
	wantRule(t, hdr("expand")+"INSERT INTO permissions (code) VALUES ('a:b');\n", "idempotency")
}

func TestLintDDLAndDMLMustBeSplit(t *testing.T) {
	wantRule(t, hdr("expand")+createT+grantT+"INSERT IGNORE INTO t (id) VALUES (1);\n", "ddl-dml-split")
}

func TestLintCompoundStatementNeedsStatementBegin(t *testing.T) {
	wantRule(t, hdr("expand")+"DROP TRIGGER IF EXISTS trg;\nCREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW BEGIN SET NEW.id = 1; END;\n", "compound-statement")
}

func TestLintGrantRules(t *testing.T) {
	t.Run("新表缺少 lingedge_app 授权", func(t *testing.T) { wantRule(t, hdr("expand")+createT, "grant-app") })
	t.Run("授权权限集合不对", func(t *testing.T) {
		wantRule(t, hdr("expand")+createT+"GRANT ALL PRIVILEGES ON `t` TO lingedge_app;\n", "grant-app")
		wantRule(t, hdr("expand")+createT+"GRANT SELECT ON `t` TO lingedge_app;\n", "grant-app")
		wantRule(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE, ALTER ON `t` TO lingedge_app;\n", "grant-app")
	})
	t.Run("追加只写表不得授 UPDATE/DELETE", func(t *testing.T) {
		audit := "CREATE TABLE IF NOT EXISTS audit_logs (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, created_at DATETIME(3) NOT NULL, PRIMARY KEY (id, created_at)) ENGINE=InnoDB;\n"
		wantRule(t, hdr("expand")+audit+"GRANT SELECT, INSERT, UPDATE, DELETE ON audit_logs TO lingedge_app;\n", "grant-app")
		wantClean(t, hdr("expand")+audit+"GRANT SELECT, INSERT ON audit_logs TO lingedge_app;\n")
	})
	t.Run("授权必须是不带库名的表级", func(t *testing.T) {
		wantRule(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON mydb.t TO lingedge_app;\n", "grant-app")
		wantRule(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON *.* TO lingedge_app;\n", "grant-app")
		wantRule(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON mydb.* TO lingedge_app;\n", "grant-app")
	})
	t.Run("只允许授权给 app/maint", func(t *testing.T) {
		wantRule(t, hdr("expand")+createT+grantT+"GRANT SELECT ON t TO some_other;\n", "grant-app")
	})
	t.Run("授权须在建表之后", func(t *testing.T) {
		wantRule(t, hdr("expand")+grantT+createT, "grant-app")
	})
	part := "CREATE TABLE IF NOT EXISTS pt (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, created_at DATETIME(3) NOT NULL, PRIMARY KEY (id, created_at)) ENGINE=InnoDB " +
		"PARTITION BY RANGE COLUMNS(created_at) (PARTITION p1 VALUES LESS THAN ('2026-12-01'), PARTITION pmax VALUES LESS THAN (MAXVALUE));\n"
	appPT := "GRANT SELECT, INSERT, UPDATE, DELETE ON pt TO lingedge_app;\n"
	t.Run("分区表缺少 maint 授权", func(t *testing.T) { wantRule(t, hdr("expand")+part+appPT, "grant-maint") })
	t.Run("分区表 maint 授权正确", func(t *testing.T) {
		wantClean(t, hdr("expand")+part+appPT+"GRANT SELECT, ALTER, DROP ON pt TO lingedge_maint;\n")
	})
	t.Run("maint 权限集合不对 / 非分区表", func(t *testing.T) {
		wantRule(t, hdr("expand")+part+appPT+"GRANT SELECT, ALTER, DROP, INSERT ON pt TO lingedge_maint;\n", "grant-maint")
		wantRule(t, hdr("expand")+createT+grantT+"GRANT SELECT, ALTER, DROP ON t TO lingedge_maint;\n", "grant-maint")
	})
	t.Run("带账号主机的授权同样被识别", func(t *testing.T) {
		wantClean(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO 'lingedge_app'@'localhost';\n")
	})
}

func TestLintRunsDDLRulesAcrossMigrations(t *testing.T) {
	// 000001 建父表；000002 的外键指向 000001 的表 → 合法；指向不存在的表 → 前向外键。
	fsys := fstest.MapFS{
		"000001_a.sql": {Data: []byte(hdr("expand") + "CREATE TABLE IF NOT EXISTS p (id BIGINT UNSIGNED NOT NULL PRIMARY KEY) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON p TO lingedge_app;\n")},
		"000002_b.sql": {Data: []byte(hdr("expand") + "CREATE TABLE IF NOT EXISTS c (id BIGINT UNSIGNED NOT NULL PRIMARY KEY, p_id BIGINT UNSIGNED NULL, CONSTRAINT fk_c_p FOREIGN KEY (p_id) REFERENCES p(id)) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON c TO lingedge_app;\n")},
	}
	if _, fs, _ := Load(fsys); len(fs) != 0 {
		t.Fatalf("跨迁移的合法外键不应有发现: %v", fs)
	}
	fsys["000002_b.sql"] = &fstest.MapFile{Data: []byte(strings.ReplaceAll(string(fsys["000002_b.sql"].Data), "REFERENCES p(id)", "REFERENCES later(id)"))}
	_, fs, _ := Load(fsys)
	if !hasRule(fs, RuleForwardFK) {
		t.Fatalf("指向后续迁移才创建的表必须报前向外键: %v", fs)
	}
}

func TestLintGrantHardening(t *testing.T) {
	t.Run("多个账号", func(t *testing.T) {
		wantRule(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app, other_user;\n", "grant-app")
		wantRule(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app, lingedge_maint;\n", "grant-app")
	})
	t.Run("WITH GRANT OPTION / AS / REQUIRE", func(t *testing.T) {
		wantRule(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app WITH GRANT OPTION;\n", "grant-app")
		wantRule(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app REQUIRE SSL;\n", "grant-app")
	})
	t.Run("裸词形态的 user@host 被正确识别", func(t *testing.T) {
		wantClean(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app@localhost;\n")
		wantClean(t, hdr("expand")+createT+"GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app@'%';\n")
		wantRule(t, hdr("expand")+createT+grantT+"GRANT SELECT ON t TO evil@localhost;\n", "grant-app")
	})
	t.Run("RENAME TABLE 后新名字需要重新授权（表级权限不随表改名）", func(t *testing.T) {
		wantRule(t, hdr("irreversible")+"RENAME TABLE t TO t2;\n", "grant-app")
		wantRule(t, hdr("irreversible")+"RENAME TABLE t TO t2;\nGRANT SELECT, INSERT, UPDATE, DELETE ON t2 TO lingedge_app;\n", "idempotency") // 带授权后只剩「RENAME 无原生守卫」
	})
}

func TestLintForbiddenStatements(t *testing.T) {
	for _, stmt := range []string{
		"CREATE USER 'x'@'%';", "DROP USER 'x'@'%';", "ALTER USER 'x'@'%' PASSWORD EXPIRE;", "SET PASSWORD FOR 'x'@'%' = RANDOM PASSWORD;",
		"REVOKE SELECT ON t FROM lingedge_app;", "SET GLOBAL max_connections = 1;", "SET PERSIST max_connections = 1;", "USE other_db;",
		"CREATE DATABASE other_db;", "DROP DATABASE IF EXISTS other_db;", "FLUSH PRIVILEGES;", "RENAME USER a TO b;",
	} {
		if fs := lintOne(hdr("irreversible") + stmt + "\n"); !hasRule(fs, "forbidden-statement") {
			t.Errorf("%q 必须被禁止: %v", stmt, fs)
		}
	}
	// 合法：会话变量、PREPARE 守卫、RENAME TABLE。
	wantClean(t, hdr("expand")+"SET @lingedge_sql = 'SELECT 1';\n")
	// Down 里允许 REVOKE（撤销本迁移授予的权限）。
	wantClean(t, hdr("reversible")+createT+grantT+"-- +goose Down\nREVOKE SELECT, INSERT, UPDATE, DELETE ON `t` FROM lingedge_app;\nDROP TABLE IF EXISTS t;\n")
}

func TestLintExecutableCommentsAndSameLineStatements(t *testing.T) {
	wantRule(t, hdr("expand")+"/*!40014 SET FOREIGN_KEY_CHECKS=0 */;\n", "executable-comment")
	wantRule(t, hdr("expand")+"/*! DROP TABLE x */;\n", "executable-comment")
	wantRule(t, hdr("expand")+createT+grantT+"CREATE TABLE IF NOT EXISTS u (id INT) ENGINE=InnoDB; GRANT SELECT, INSERT, UPDATE, DELETE ON u TO lingedge_app;\n", "syntax")
	wantRule(t, hdr("expand")+"INSERT IGNORE INTO permissions (code) VALUES ('a;\nb');\n", "syntax")
	wantClean(t, hdr("expand")+"/* 普通注释 */ INSERT IGNORE INTO permissions (code) VALUES ('a;b');\n")
}

func TestLintExpandMustKeepPreviousBinaryWorking(t *testing.T) {
	wantRule(t, hdr("expand")+"ALTER TABLE t ADD COLUMN c INT NOT NULL;\n", "class-expand")
	wantRule(t, hdr("expand")+"ALTER TABLE t ADD c VARCHAR(8) NOT NULL;\n", "class-expand")
	wantRule(t, hdr("expand")+"ALTER TABLE t ADD COLUMN (a INT NULL, b INT NOT NULL);\n", "class-expand")
	wantRule(t, hdr("expand")+"DELETE FROM t WHERE id = 1;\n", "class-expand")
	// 可空、带默认值、自增、生成列、索引/外键都不受此规则限制。
	wantClean(t, hdr("expand")+"ALTER TABLE t ADD COLUMN c INT NULL;\n")
	wantClean(t, hdr("expand")+"ALTER TABLE t ADD COLUMN c INT NOT NULL DEFAULT 0;\n")
	wantClean(t, hdr("expand")+"ALTER TABLE t ADD COLUMN c VARCHAR(8) AS (CONCAT('a', id)) VIRTUAL;\n")
	wantClean(t, hdr("expand")+"ALTER TABLE t ADD INDEX idx_t_c (c);\n")
}

func TestLoadFlagsFilesGooseWouldIgnore(t *testing.T) {
	_, fs, _ := Load(fstest.MapFS{
		"000001_a.SQL":       {Data: []byte(goodExpand)},
		"000002_backfill.go": {Data: []byte("package migrations\n")},
		"embed.go":           {Data: []byte("package migrations\n")},
		"CHECKSUMS":          {Data: []byte("")},
	})
	var name int
	for _, f := range fs {
		if f.Rule == "name" {
			name++
		}
	}
	if name != 2 {
		t.Fatalf("大写 .SQL 与 Go 迁移文件应各报一项（embed.go/CHECKSUMS 不报）: %v", fs)
	}
}

// expand 与可重入规则不得互相矛盾：触发器/视图/过程/函数/事件的「DROP … IF EXISTS + CREATE」是重建守卫，不是破坏；孤立的 DROP 才是。
func TestLintExpandAllowsRecreateGuardsButNotLoneDrops(t *testing.T) {
	wantClean(t, hdr("expand")+"DROP VIEW IF EXISTS v_t;\nCREATE OR REPLACE VIEW v_t AS SELECT 1 AS x;\n")
	wantClean(t, hdr("expand")+"DROP VIEW IF EXISTS v_t;\nCREATE VIEW v_t AS SELECT 1 AS x;\n")
	wantClean(t, hdr("expand")+"DROP PROCEDURE IF EXISTS p_t;\n-- +goose StatementBegin\nCREATE PROCEDURE p_t() BEGIN SELECT 1; END;\n-- +goose StatementEnd\n")
	wantClean(t, hdr("expand")+"DROP FUNCTION IF EXISTS f_t;\n-- +goose StatementBegin\nCREATE FUNCTION f_t() RETURNS INT DETERMINISTIC BEGIN RETURN 1; END;\n-- +goose StatementEnd\n")
	wantRule(t, hdr("expand")+"DROP VIEW IF EXISTS v_t;\n", "class-expand")
	wantRule(t, hdr("expand")+"DROP TRIGGER IF EXISTS trg_t_bi;\n", "class-expand")
	// 名字不同：不是重建，是删除。
	wantRule(t, hdr("expand")+"DROP VIEW IF EXISTS v_old;\nCREATE VIEW v_new AS SELECT 1 AS x;\n", "class-expand")
}
