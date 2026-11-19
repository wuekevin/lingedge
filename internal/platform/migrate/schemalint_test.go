package migrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

// lintSetOf 对一组迁移文件运行全部规则（文件名 → 内容）。
func lintSetOf(t *testing.T, files map[string]string) []Finding {
	t.Helper()
	fsys := fstest.MapFS{}
	for n, b := range files {
		fsys[n] = &fstest.MapFile{Data: []byte(b)}
	}
	_, fs, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// schema lint（Story 1.7 AC2）的单元测试：正向（合法写法不误伤）与负向（每条规则都能触发）。

const tgrant = "GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app;\n"

func tbl(body string) string { return hdr("expand") + body + "\n" + tgrant }

func TestSchemaLintAcceptsConventionalTables(t *testing.T) {
	// 租户表：索引以 tenant_id 打头；仅 uid 的唯一索引例外；主键例外；T?（可空 tenant_id）不受租户索引规则约束。
	wantClean(t, tbl(`CREATE TABLE IF NOT EXISTS t (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  uid CHAR(26) NOT NULL,
  tenant_id BIGINT UNSIGNED NOT NULL,
  name VARCHAR(64) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  note TEXT NULL,
  payload JSON NOT NULL,
  blob_col MEDIUMBLOB NULL,
  rate_permille INT UNSIGNED NOT NULL DEFAULT 0,
  PRIMARY KEY (id),
  UNIQUE KEY uk_t_uid (uid),
  UNIQUE KEY uk_t_name (tenant_id, name),
  KEY idx_t_created (tenant_id, created_at)
) ENGINE=InnoDB;`))
	wantClean(t, tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NULL, name VARCHAR(64) NOT NULL, UNIQUE KEY uk_t_name (name)) ENGINE=InnoDB;"))
	// DATE 允许（日期分片键）；SMALLINT/TINYINT 等整数不受影响。
	wantClean(t, tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, log_date DATE NOT NULL, n SMALLINT NOT NULL) ENGINE=InnoDB;"))
	// 带原因的跨租户索引豁免。
	wantClean(t, hdr("expand")+"-- lingedge:cross-tenant-index=idx_t_user 登录后按用户反查所属租户（跨租户查询）\n"+
		"CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, user_id BIGINT UNSIGNED NOT NULL, KEY idx_t_user (user_id)) ENGINE=InnoDB;\n"+tgrant)
	// 后续迁移新增的索引同样以 tenant_id 打头即可。
	wantClean(t, tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL) ENGINE=InnoDB;"))
}

func TestSchemaLintColumnTypeRules(t *testing.T) {
	cases := []struct{ name, col, rule string }{
		{"ENUM", "s ENUM('a','b') NOT NULL", RuleSchemaEnum},
		{"SET", "s SET('a','b') NOT NULL", RuleSchemaEnum},
		{"TEXT 字面量默认", "s TEXT NULL DEFAULT 'x'", RuleSchemaLOBDefault},
		{"MEDIUMTEXT 默认", "s MEDIUMTEXT NOT NULL DEFAULT ''", RuleSchemaLOBDefault},
		{"JSON 表达式默认", "s JSON NOT NULL DEFAULT (JSON_ARRAY())", RuleSchemaLOBDefault},
		{"BLOB 默认", "s BLOB NULL DEFAULT NULL", RuleSchemaLOBDefault},
		{"TIMESTAMP", "s TIMESTAMP NULL", RuleSchemaTime},
		{"TIME", "s TIME NULL", RuleSchemaTime},
		{"DATETIME 无精度", "s DATETIME NULL", RuleSchemaTime},
		{"DATETIME(6)", "s DATETIME(6) NULL", RuleSchemaTime},
		{"FLOAT", "s FLOAT NULL", RuleSchemaFloat},
		{"DOUBLE", "s DOUBLE NULL", RuleSchemaFloat},
		{"DECIMAL 金额", "amount DECIMAL(12,2) NOT NULL", RuleSchemaFloat},
		{"NUMERIC", "s NUMERIC(5,2) NULL", RuleSchemaFloat},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantRule(t, tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, "+c.col+") ENGINE=InnoDB;"), c.rule)
		})
	}
	// ALTER ADD COLUMN / MODIFY 同样检查。
	wantRule(t, hdr("expand")+"ALTER TABLE t ADD COLUMN s ENUM('a') NULL;\n", RuleSchemaEnum)
	wantRule(t, hdr("expand")+"ALTER TABLE t ADD COLUMN s DATETIME NULL;\n", RuleSchemaTime)
	wantRule(t, hdr("irreversible")+"ALTER TABLE t MODIFY COLUMN s DOUBLE NULL;\n", RuleSchemaFloat)
	// 字符串默认值里出现类型名不应误报；列名恰为 time/key 的写法由类型位置判定。
	wantClean(t, tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, kind VARCHAR(16) NOT NULL DEFAULT 'timestamp float enum') ENGINE=InnoDB;"))
}

func TestSchemaLintTenantIndexRules(t *testing.T) {
	base := "CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL, user_id BIGINT UNSIGNED NOT NULL"
	for name, def := range map[string]string{
		"唯一索引未以 tenant_id 打头": ", UNIQUE KEY uk_t_name (name)",
		"普通索引未以 tenant_id 打头": ", KEY idx_t_name (name, tenant_id)",
		"CONSTRAINT UNIQUE":   ", CONSTRAINT uk_t_name UNIQUE (name)",
		"内联 UNIQUE":           ", code VARCHAR(8) NOT NULL UNIQUE",
		"uid 之外的单列唯一":         ", ref CHAR(26) NOT NULL, UNIQUE KEY uk_t_ref (ref)",
		"uid 复合唯一不豁免":         ", uid CHAR(26) NOT NULL, UNIQUE KEY uk_t_uid (uid, name)",
		"无名索引":                ", KEY (user_id)",
	} {
		t.Run(name, func(t *testing.T) { wantRule(t, tbl(base+def+") ENGINE=InnoDB;"), RuleSchemaTenantIndex) })
	}
	// 后续迁移追加的索引：CREATE INDEX 与 ALTER ADD INDEX 都要以 tenant_id 打头。
	set := func(second string) []Finding {
		return lintSetOf(t, map[string]string{
			"000001_a.sql": tbl(base + ", KEY idx_t_ok (tenant_id, name)) ENGINE=InnoDB;"),
			"000002_b.sql": hdr("expand") + second,
		})
	}
	if !hasRule(set("CREATE INDEX idx_t_user ON t (user_id);\n"), RuleSchemaTenantIndex) {
		t.Error("CREATE INDEX 未以 tenant_id 打头应被报告")
	}
	if !hasRule(set("ALTER TABLE t ADD INDEX idx_t_user (user_id);\n"), RuleSchemaTenantIndex) {
		t.Error("ALTER ADD INDEX 未以 tenant_id 打头应被报告")
	}
	if !hasRule(set("ALTER TABLE t ADD UNIQUE KEY uk_t_user (user_id);\n"), RuleSchemaTenantIndex) {
		t.Error("ALTER ADD UNIQUE 未以 tenant_id 打头应被报告")
	}
	if fs := set("CREATE INDEX idx_t_user ON t (tenant_id, user_id);\n"); hasRule(fs, RuleSchemaTenantIndex) {
		t.Errorf("以 tenant_id 打头的索引不应被报告: %v", fs)
	}
	// 豁免必须写原因；写了原因才放行。
	if fs := lintOne(hdr("expand") + "-- lingedge:cross-tenant-index=idx_t_user\n" + base + ", KEY idx_t_user (user_id)) ENGINE=InnoDB;\n" + tgrant); !hasRule(fs, RuleSchemaTenantIndex) {
		t.Errorf("缺原因的豁免应被报告: %v", fs)
	}
}

func TestSchemaLintTenantColumnRegistry(t *testing.T) {
	wantRule(t, hdr("expand")+"CREATE TABLE IF NOT EXISTS sites (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(64) NOT NULL) ENGINE=InnoDB;\n"+
		"GRANT SELECT, INSERT, UPDATE, DELETE ON `sites` TO lingedge_app;\n", RuleSchemaTenantColumn)
	// tenant_id 可空不算（T? 表）；NOT NULL 才满足登记表的要求。
	wantRule(t, hdr("expand")+"CREATE TABLE IF NOT EXISTS sites (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NULL) ENGINE=InnoDB;\n"+
		"GRANT SELECT, INSERT, UPDATE, DELETE ON `sites` TO lingedge_app;\n", RuleSchemaTenantColumn)
	wantClean(t, hdr("expand")+"CREATE TABLE IF NOT EXISTS sites (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, KEY idx_sites_tenant (tenant_id)) ENGINE=InnoDB;\n"+
		"GRANT SELECT, INSERT, UPDATE, DELETE ON `sites` TO lingedge_app;\n")
}

// 模型随 RENAME / DROP 演进：改名、删索引、删列后，租户表规则仍按真实结构判定（三路评审发现：模型与真实库脱节会漏报）。
func TestSchemaLintFollowsRenameAndDrop(t *testing.T) {
	base := "CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL, KEY idx_t_tenant (tenant_id, name)) ENGINE=InnoDB;\n"
	first := hdr("expand") + base + tgrant
	second := func(class, body string) []Finding {
		return lintSetOf(t, map[string]string{"000001_a.sql": first, "000002_b.sql": hdr(class) + body})
	}
	// 1) 改名后仍是租户表：新增非 tenant_id 打头的索引照样被发现。
	if fs := second("irreversible", "RENAME TABLE t TO t2;\n"); hasRule(fs, RuleSchemaTenantIndex) {
		t.Errorf("改名本身不应报租户索引: %v", fs)
	}
	fs := lintSetOf(t, map[string]string{
		"000001_a.sql": first,
		"000002_b.sql": hdr("irreversible") + "RENAME TABLE t TO t2;\n",
		"000003_c.sql": hdr("expand") + "ALTER TABLE t2 ADD INDEX idx_t2_name (name);\n",
	})
	if !hasRule(fs, RuleSchemaTenantIndex) {
		t.Errorf("改名后的租户表新增非 tenant_id 打头索引应被报告: %v", fs)
	}
	// 2) 改名成登记的租户表名而没有 tenant_id NOT NULL：报告缺列。
	plain := "CREATE TABLE IF NOT EXISTS plain (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, name VARCHAR(64) NOT NULL) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON `plain` TO lingedge_app;\n"
	fs = lintSetOf(t, map[string]string{"000001_a.sql": hdr("expand") + plain, "000002_b.sql": hdr("irreversible") + "RENAME TABLE plain TO sites;\nGRANT SELECT, INSERT, UPDATE, DELETE ON `sites` TO lingedge_app;\n"})
	if !hasRule(fs, RuleSchemaTenantColumn) {
		t.Errorf("改名为登记的租户表名却没有 tenant_id 应被报告: %v", fs)
	}
	fs = lintSetOf(t, map[string]string{"000001_a.sql": hdr("expand") + plain, "000002_b.sql": hdr("irreversible") + "ALTER TABLE plain RENAME TO sites;\n"})
	if !hasRule(fs, RuleSchemaTenantColumn) {
		t.Errorf("ALTER … RENAME TO 同样应重新匹配登记清单: %v", fs)
	}
	// 3) 删除索引后不再受其约束；DROP INDEX 语句与 ALTER DROP INDEX 都会更新模型。
	bad := hdr("expand") + "CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL) ENGINE=InnoDB;\n" + tgrant
	// 4) 删除 tenant_id 列后不再是租户表（其余索引不再判定）；RENAME COLUMN 同理；把别的列改名为 tenant_id 且 NOT NULL 则成为租户表。
	fs = lintSetOf(t, map[string]string{
		"000001_a.sql": bad,
		"000002_b.sql": hdr("irreversible") + "ALTER TABLE t RENAME COLUMN tenant_id TO org_id;\n",
		"000003_c.sql": hdr("expand") + "ALTER TABLE t ADD INDEX idx_t_name (name);\n",
	})
	if hasRule(fs, RuleSchemaTenantIndex) {
		t.Errorf("tenant_id 改名之后不再是租户表: %v", fs)
	}
	fs = lintSetOf(t, map[string]string{
		"000001_a.sql": bad,
		"000002_b.sql": hdr("irreversible") + "ALTER TABLE t DROP COLUMN tenant_id;\n",
		"000003_c.sql": hdr("expand") + "ALTER TABLE t ADD INDEX idx_t_name (name);\n",
	})
	if hasRule(fs, RuleSchemaTenantIndex) {
		t.Errorf("tenant_id 被删除后不再是租户表: %v", fs)
	}
	// 5) 删除违规索引之后，同名新索引按新定义判定；DROP INDEX 之后不再有该索引。
	fs = lintSetOf(t, map[string]string{
		"000001_a.sql": hdr("expand") + "CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL) ENGINE=InnoDB;\n" + tgrant,
		"000002_b.sql": hdr("expand") + "-- lingedge:cross-tenant-index=idx_t_name 测试：跨租户唯一\nCREATE INDEX idx_t_name ON t (name);\n",
		"000003_c.sql": hdr("irreversible") + "DROP INDEX idx_t_name ON t;\n",
		"000004_d.sql": hdr("expand") + "CREATE INDEX idx_t_name ON t (tenant_id, name);\n",
	})
	if hasRule(fs, RuleSchemaTenantIndex) {
		t.Errorf("豁免登记的索引被删除后，同名新索引以 tenant_id 打头，不应报告: %v", fs)
	}
}

func TestSchemaLintDeadExemptionIsReported(t *testing.T) {
	// 索引名拼写错误：豁免没有对应任何索引，且真正的违规索引照样被报告。
	fs := lintOne(hdr("expand") + "-- lingedge:cross-tenant-index=idx_t_usr 登录后按用户反查所属租户\n" +
		"CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, user_id BIGINT UNSIGNED NOT NULL, KEY idx_t_user (user_id)) ENGINE=InnoDB;\n" + tgrant)
	var dead, idx int
	for _, f := range fs {
		if f.Rule == RuleSchemaTenantIndex {
			if contains(f.Message, "没有对应") {
				dead++
			} else {
				idx++
			}
		}
	}
	if dead != 1 || idx != 1 {
		t.Errorf("应各报告一次死豁免与一次违规索引，实际 dead=%d idx=%d: %v", dead, idx, fs)
	}
	// 正确登记且被消费：不报。
	wantClean(t, hdr("expand")+"-- lingedge:cross-tenant-index=idx_t_user 登录后按用户反查所属租户\n"+
		"CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, user_id BIGINT UNSIGNED NOT NULL, KEY idx_t_user (user_id)) ENGINE=InnoDB;\n"+tgrant)
	// ALTER ADD INDEX 与 CREATE INDEX 也消费豁免。
	if fs := lintSetOf(t, map[string]string{
		"000001_a.sql": tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, user_id BIGINT UNSIGNED NOT NULL) ENGINE=InnoDB;"),
		"000002_b.sql": hdr("expand") + "-- lingedge:cross-tenant-index=idx_t_user 跨租户查询\nALTER TABLE t ADD INDEX idx_t_user (user_id);\n",
	}); len(fs) != 0 {
		t.Errorf("ALTER ADD INDEX 的豁免应被消费: %v", fs)
	}
}

func TestSchemaLintTypeSynonymsAndInlinePrimaryKeyTenant(t *testing.T) {
	for _, col := range []string{"s FLOAT4 NULL", "s FLOAT8 NULL", "s DOUBLE PRECISION NULL"} {
		wantRule(t, tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, "+col+") ENGINE=InnoDB;"), RuleSchemaFloat)
	}
	for _, col := range []string{"s LONG NULL DEFAULT 'x'", "s MULTIPOINT NULL DEFAULT NULL", "s GEOMETRYCOLLECTION NULL DEFAULT NULL"} {
		wantRule(t, tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, "+col+") ENGINE=InnoDB;"), RuleSchemaLOBDefault)
	}
	// 主键列隐含 NOT NULL：tenant_id 作内联主键时视为租户表（其余索引照样判定）。
	wantRule(t, hdr("expand")+"CREATE TABLE IF NOT EXISTS t (tenant_id BIGINT UNSIGNED PRIMARY KEY, name VARCHAR(8) NOT NULL, KEY idx_t_name (name)) ENGINE=InnoDB;\n"+tgrant, RuleSchemaTenantIndex)
	// 租户登记表以内联主键 tenant_id 定义时不误报缺列。
	wantClean(t, hdr("expand")+"CREATE TABLE IF NOT EXISTS sites (tenant_id BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON `sites` TO lingedge_app;\n")
	// ALTER ADD INDEX 以 tenant_id 打头：正向路径。
	if fs := lintSetOf(t, map[string]string{
		"000001_a.sql": tbl("CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(64) NOT NULL) ENGINE=InnoDB;"),
		"000002_b.sql": hdr("expand") + "ALTER TABLE t ADD INDEX idx_t_name (tenant_id, name);\n",
	}); len(fs) != 0 {
		t.Errorf("ALTER ADD INDEX 以 tenant_id 打头应通过: %v", fs)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
