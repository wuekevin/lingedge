package migrate

import (
	"strings"
	"testing"
)

// run 按顺序检查多条语句，返回全部违规的规则 ID。
func run(c *Checker, stmts ...string) []string {
	var out []string
	for _, s := range stmts {
		for _, v := range c.Check(s) {
			out = append(out, v.Rule)
		}
	}
	return out
}

func wantRules(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("违规 = %v，期望 %v", got, want)
	}
}

const tenantsDDL = "CREATE TABLE tenants (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB"

func TestDDLCheckGeneratedColumnFK(t *testing.T) {
	gen := func(kind, action string) string {
		return "CREATE TABLE t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NULL, code VARCHAR(64) NOT NULL, " +
			"uniq_key VARCHAR(160) AS (CONCAT(COALESCE(tenant_id,0),':',code)) " + kind + " NOT NULL, UNIQUE KEY uk (uniq_key), " +
			"CONSTRAINT fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) " + action + ") ENGINE=InnoDB"
	}
	for _, c := range []struct {
		kind, action string
		bad          bool
	}{
		{"STORED", "ON DELETE CASCADE", true},
		{"STORED", "ON DELETE SET NULL", true},
		{"STORED", "ON UPDATE CASCADE", true},
		{"STORED", "ON DELETE SET DEFAULT", true},
		{"STORED", "ON DELETE RESTRICT", false},
		{"STORED", "", false},
		{"STORED", "ON DELETE NO ACTION", false},
		{"VIRTUAL", "ON DELETE CASCADE", false},
		{"VIRTUAL", "ON DELETE SET NULL", false},
		{"", "ON DELETE CASCADE", false}, // 省略 STORED/VIRTUAL 即 VIRTUAL
	} {
		got := run(NewChecker(), tenantsDDL, gen(c.kind, c.action))
		if c.bad {
			wantRules(t, got, RuleGeneratedFK)
		} else {
			wantRules(t, got)
		}
	}
	// 基列与外键列不相交时不受限。
	wantRules(t, run(NewChecker(), tenantsDDL,
		"CREATE TABLE t2 (id BIGINT UNSIGNED PRIMARY KEY, tenant_id BIGINT UNSIGNED NULL, code VARCHAR(8) NOT NULL, k VARCHAR(8) AS (code) STORED, CONSTRAINT fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE)"))
	// 外键经 ALTER 追加同样检查。
	wantRules(t, run(NewChecker(), tenantsDDL,
		"CREATE TABLE t3 (id BIGINT UNSIGNED PRIMARY KEY, tenant_id BIGINT UNSIGNED NULL, k VARCHAR(8) AS (CONCAT('a', tenant_id)) STORED)",
		"ALTER TABLE t3 ADD CONSTRAINT fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) ON DELETE CASCADE"), RuleGeneratedFK)
}

func TestDDLCheckCheckConstraintFK(t *testing.T) {
	mk := func(action string) string {
		return "CREATE TABLE t (id BIGINT UNSIGNED PRIMARY KEY, scope VARCHAR(16) NOT NULL, tenant_id BIGINT UNSIGNED NULL, " +
			"CONSTRAINT ck CHECK (scope <> 'platform' OR tenant_id IS NULL), CONSTRAINT fk FOREIGN KEY (tenant_id) REFERENCES tenants(id) " + action + ")"
	}
	for _, c := range []struct {
		action string
		bad    bool
	}{
		{"ON DELETE CASCADE", false},
		{"ON DELETE RESTRICT", false},
		{"ON DELETE NO ACTION", false},
		{"", false},
		{"ON DELETE SET NULL", true},
		{"ON DELETE SET DEFAULT", true},
		{"ON DELETE RESTRICT ON UPDATE CASCADE", true},
		{"ON UPDATE SET NULL", true},
	} {
		got := run(NewChecker(), tenantsDDL, mk(c.action))
		if c.bad {
			wantRules(t, got, RuleCheckFK)
		} else {
			wantRules(t, got)
		}
	}
	// 列级 CHECK 与 NOT ENFORCED。
	wantRules(t, run(NewChecker(), tenantsDDL,
		"CREATE TABLE a (id BIGINT UNSIGNED PRIMARY KEY, tid BIGINT UNSIGNED NULL CHECK (tid > 0), CONSTRAINT fk FOREIGN KEY (tid) REFERENCES tenants(id) ON DELETE SET NULL)"), RuleCheckFK)
	wantRules(t, run(NewChecker(), tenantsDDL,
		"CREATE TABLE b (id BIGINT UNSIGNED PRIMARY KEY, tid BIGINT UNSIGNED NULL, CONSTRAINT ck CHECK (tid > 0) NOT ENFORCED, CONSTRAINT fk FOREIGN KEY (tid) REFERENCES tenants(id) ON DELETE SET NULL)"))
	// CHECK 不引用外键列时不受限（字符串里出现列名不算引用）。
	wantRules(t, run(NewChecker(), tenantsDDL,
		"CREATE TABLE c (id BIGINT UNSIGNED PRIMARY KEY, scope VARCHAR(16) NOT NULL, tid BIGINT UNSIGNED NULL, CONSTRAINT ck CHECK (scope <> 'tid'), CONSTRAINT fk FOREIGN KEY (tid) REFERENCES tenants(id) ON DELETE SET NULL)"))
}

func TestDDLCheckForwardFK(t *testing.T) {
	child := "CREATE TABLE child (id BIGINT UNSIGNED PRIMARY KEY, p_id BIGINT UNSIGNED NULL, CONSTRAINT fk FOREIGN KEY (p_id) REFERENCES parent(id))"
	wantRules(t, run(NewChecker(), child), RuleForwardFK)
	wantRules(t, run(NewChecker(), "CREATE TABLE parent (id BIGINT UNSIGNED PRIMARY KEY)", child))
	// 自引用 FK 合法。
	wantRules(t, run(NewChecker(), "CREATE TABLE tree (id BIGINT UNSIGNED PRIMARY KEY, parent_id BIGINT UNSIGNED NULL, CONSTRAINT fk FOREIGN KEY (parent_id) REFERENCES tree(id))"))
	// 循环 FK：先建表、再 ALTER ADD CONSTRAINT 合法；ALTER 指向未创建的表非法。
	wantRules(t, run(NewChecker(),
		"CREATE TABLE a (id BIGINT UNSIGNED PRIMARY KEY, b_id BIGINT UNSIGNED NULL)",
		"CREATE TABLE b (id BIGINT UNSIGNED PRIMARY KEY, a_id BIGINT UNSIGNED NULL, CONSTRAINT fk_b_a FOREIGN KEY (a_id) REFERENCES a(id))",
		"ALTER TABLE a ADD CONSTRAINT fk_a_b FOREIGN KEY (b_id) REFERENCES b(id)"))
	wantRules(t, run(NewChecker(),
		"CREATE TABLE a (id BIGINT UNSIGNED PRIMARY KEY, b_id BIGINT UNSIGNED NULL)",
		"ALTER TABLE a ADD CONSTRAINT fk_a_b FOREIGN KEY (b_id) REFERENCES nowhere(id)"), RuleForwardFK)
	// DROP 之后再引用同样是前向。
	wantRules(t, run(NewChecker(), "CREATE TABLE parent (id BIGINT UNSIGNED PRIMARY KEY)", "DROP TABLE IF EXISTS parent", child), RuleForwardFK)
	// 迁移之外已存在的表可经 NewChecker(known...) 声明。
	wantRules(t, run(NewChecker("parent"), child))
	// 带库名限定、反引号、大小写。
	wantRules(t, run(NewChecker(), "CREATE TABLE IF NOT EXISTS `db`.`Parent` (id BIGINT UNSIGNED PRIMARY KEY)",
		"CREATE TABLE child (id BIGINT UNSIGNED PRIMARY KEY, p_id BIGINT UNSIGNED NULL, FOREIGN KEY (p_id) REFERENCES PARENT (id))"))
}

func TestDDLCheckPartitionRules(t *testing.T) {
	part := func(extra, parts string) string {
		return "CREATE TABLE a (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, created_at DATETIME(3) NOT NULL, tid BIGINT UNSIGNED NULL" + extra +
			", PRIMARY KEY (id, created_at)) PARTITION BY RANGE COLUMNS(created_at) (" + parts + ")"
	}
	const pmax = "PARTITION pmax VALUES LESS THAN (MAXVALUE)"
	wantRules(t, run(NewChecker(), part("", "PARTITION p1 VALUES LESS THAN ('2026-12-01'), "+pmax)))
	wantRules(t, run(NewChecker(), part("", "PARTITION p1 VALUES LESS THAN ('2026-12-01')")), RulePartitionPmax)
	wantRules(t, run(NewChecker(), part("", "PARTITION p1 VALUES LESS THAN ('2026-12-01'), PARTITION plast VALUES LESS THAN MAXVALUE")), RulePartitionPmax) // 兜底分区必须叫 pmax
	wantRules(t, run(NewChecker(), part("", "PARTITION p1 VALUES LESS THAN ('2026-12-01'), PARTITION pmax VALUES LESS THAN MAXVALUE")))
	// 分区表作为 FK 子表 / 父表。
	wantRules(t, run(NewChecker(), tenantsDDL, part(", CONSTRAINT fk FOREIGN KEY (tid) REFERENCES tenants(id)", pmax)), RulePartitionFK)
	wantRules(t, run(NewChecker(), part("", pmax), "CREATE TABLE r (id BIGINT UNSIGNED PRIMARY KEY, aid BIGINT UNSIGNED NULL, CONSTRAINT fk FOREIGN KEY (aid) REFERENCES a(id))"), RulePartitionFK)
	// LIST/HASH 分区不要求 pmax。
	wantRules(t, run(NewChecker(), "CREATE TABLE h (id BIGINT UNSIGNED PRIMARY KEY) PARTITION BY HASH(id) PARTITIONS 4"))
	// REORGANIZE 滚动预建不引入违规。
	wantRules(t, run(NewChecker(), part("", pmax),
		"ALTER TABLE a REORGANIZE PARTITION pmax INTO (PARTITION p2 VALUES LESS THAN ('2027-02-01'), PARTITION pmax VALUES LESS THAN (MAXVALUE))"))
	// 把已有表改成分区表且已有外键。
	wantRules(t, run(NewChecker(), tenantsDDL,
		"CREATE TABLE x (id BIGINT UNSIGNED NOT NULL, created_at DATETIME(3) NOT NULL, tid BIGINT UNSIGNED NULL, PRIMARY KEY (id, created_at), CONSTRAINT fk FOREIGN KEY (tid) REFERENCES tenants(id))",
		"ALTER TABLE x PARTITION BY RANGE COLUMNS(created_at) (PARTITION pmax VALUES LESS THAN (MAXVALUE))"), RulePartitionFK)
}

func TestDDLCheckForbidsForeignKeyChecksBypass(t *testing.T) {
	for _, s := range []string{"SET foreign_key_checks = 0", "SET FOREIGN_KEY_CHECKS=0", "SET @@foreign_key_checks = 0", "SET SESSION foreign_key_checks = 0"} {
		wantRules(t, run(NewChecker(), s), RuleFKChecks)
	}
	wantRules(t, run(NewChecker(), "SET NAMES utf8mb4"))
}

func TestDDLCheckReportsViolationOnlyOnce(t *testing.T) {
	c := NewChecker()
	got := run(c, tenantsDDL,
		"CREATE TABLE t (id BIGINT UNSIGNED PRIMARY KEY, tid BIGINT UNSIGNED NULL, k VARCHAR(8) AS (CONCAT('a', tid)) STORED, CONSTRAINT fk FOREIGN KEY (tid) REFERENCES tenants(id) ON DELETE CASCADE)",
		"ALTER TABLE t ADD COLUMN note VARCHAR(8) NULL",
		"CREATE TABLE u (id BIGINT UNSIGNED PRIMARY KEY)")
	wantRules(t, got, RuleGeneratedFK)
}

func TestRuleMySQLErrorCodes(t *testing.T) {
	want := map[string]int{RuleGeneratedFK: 1215, RuleCheckFK: 3823, RuleForwardFK: 1824, RulePartitionFK: 1506, RulePartitionPmax: 1526}
	for r, code := range want {
		if RuleMySQLError(r) != code {
			t.Errorf("%s → %d，期望 %d", r, RuleMySQLError(r), code)
		}
	}
}

func TestDDLCheckLikeCopyIsIndependentOfSource(t *testing.T) {
	c := NewChecker()
	wantRules(t, run(c, tenantsDDL,
		"CREATE TABLE a (id BIGINT UNSIGNED PRIMARY KEY, tid BIGINT UNSIGNED NULL, k VARCHAR(8) AS (CONCAT('a', tid)) STORED, CONSTRAINT fk FOREIGN KEY (tid) REFERENCES tenants(id) ON DELETE RESTRICT)",
		"CREATE TABLE b LIKE a",
		"ALTER TABLE b DROP FOREIGN KEY fk",
		"ALTER TABLE b DROP COLUMN k"))
	// 对副本的 ALTER 不得改动源表：给 a 的外键改成 CASCADE 仍应命中规则 1（a 的 STORED 列 k 与外键列 tid 仍在）。
	got := run(c, "ALTER TABLE a DROP FOREIGN KEY fk", "ALTER TABLE a ADD CONSTRAINT fk2 FOREIGN KEY (tid) REFERENCES tenants(id) ON DELETE CASCADE")
	wantRules(t, got, RuleGeneratedFK)
}

func TestDDLCheckUnknownTableIsNotRegisteredByAlter(t *testing.T) {
	// 拼错表名的 ALTER 不能让之后指向它的外键绕过前向外键检查。
	c := NewChecker()
	wantRules(t, run(c,
		"ALTER TABLE tennants ADD COLUMN x INT NULL",
		"CREATE TABLE child (id BIGINT UNSIGNED PRIMARY KEY, t BIGINT UNSIGNED NULL, CONSTRAINT fk FOREIGN KEY (t) REFERENCES tennants(id))"), RuleForwardFK)
}

func TestDDLCheckTracksRenames(t *testing.T) {
	c := NewChecker()
	wantRules(t, run(c, "CREATE TABLE old_name (id BIGINT UNSIGNED PRIMARY KEY)",
		"ALTER TABLE old_name RENAME TO new_name",
		"CREATE TABLE child (id BIGINT UNSIGNED PRIMARY KEY, p BIGINT UNSIGNED NULL, CONSTRAINT fk FOREIGN KEY (p) REFERENCES new_name(id))"))
	wantRules(t, run(c, "CREATE TABLE child2 (id BIGINT UNSIGNED PRIMARY KEY, p BIGINT UNSIGNED NULL, CONSTRAINT fk2 FOREIGN KEY (p) REFERENCES old_name(id))"), RuleForwardFK)
	wantRules(t, run(c, "RENAME TABLE new_name TO newer_name",
		"CREATE TABLE child3 (id BIGINT UNSIGNED PRIMARY KEY, p BIGINT UNSIGNED NULL, CONSTRAINT fk3 FOREIGN KEY (p) REFERENCES newer_name(id))"))
	// RENAME COLUMN 之后基列变化：STORED 生成列引用的列改名后，外键列仍受规则 1 约束。
	c2 := NewChecker()
	wantRules(t, run(c2, tenantsDDL,
		"CREATE TABLE t (id BIGINT UNSIGNED PRIMARY KEY, tid BIGINT UNSIGNED NULL, k VARCHAR(8) AS (CONCAT('a', tid)) STORED, CONSTRAINT fk FOREIGN KEY (tid) REFERENCES tenants(id) ON DELETE RESTRICT)",
		"ALTER TABLE t RENAME COLUMN tid TO tenant_ref"))
}
