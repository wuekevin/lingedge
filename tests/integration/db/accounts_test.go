package dbintegration

import (
	"fmt"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// 三类真实账号的权限负向测试（AC6、IMP-19、database/README §5.1）。
// 实例形态为 log-bin=ON、log_bin_trust_function_creators=0（生产形态）。

func wantMySQLErr(t *testing.T, what string, err error, want uint16) {
	t.Helper()
	n, ok := db.MySQLErrorNumber(err)
	if !ok || n != want {
		t.Errorf("%s: 期望 MySQL 错误 %d，实际 %v", what, want, err)
	}
}

func TestInstanceIsBinlogOnWithTrustOff(t *testing.T) {
	e := newEnv(t)
	if got := e.adminScalar(t, "SELECT CONCAT(@@log_bin, '|', @@log_bin_trust_function_creators)"); got != "1|0" {
		t.Fatalf("测试前提不成立（需 log_bin=1、trust=0），实际 %s：账号/触发器用例将失去意义", got)
	}
	if got := e.adminScalar(t, "SELECT @@port"); got != fmt.Sprint(e.inst.Port) {
		t.Fatalf("连到的不是临时实例: %s", got)
	}
}

func TestAppAccountPermissions_RealMySQL(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	ex := e.app.Executor(ctx)

	// 业务表：增删改查均可。
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO plain_notes (note) VALUES (?)"), "n"); err != nil {
		t.Fatalf("app 应能 INSERT 业务表: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("UPDATE plain_notes SET note = ? WHERE id = ?"), "n2", 1); err != nil {
		t.Fatalf("app 应能 UPDATE 业务表: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("DELETE FROM plain_notes WHERE id = ?"), 1); err != nil {
		t.Fatalf("app 应能 DELETE 业务表: %v", err)
	}

	// audit_logs：仅 SELECT、INSERT。
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO audit_logs (uid, action) VALUES (?, ?)"), "01J0000000000000000000AU01", "login"); err != nil {
		t.Fatalf("app 应能 INSERT audit_logs: %v", err)
	}
	var n int
	if err := ex.Get(ctx, &n, db.Q("SELECT COUNT(*) FROM audit_logs")); err != nil || n != 1 {
		t.Fatalf("app 应能 SELECT audit_logs: %d %v", n, err)
	}
	_, err := ex.Exec(ctx, db.Q("UPDATE audit_logs SET action = ? WHERE uid = ?"), "tampered", "01J0000000000000000000AU01")
	wantMySQLErr(t, "app UPDATE audit_logs", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("DELETE FROM audit_logs WHERE uid = ?"), "01J0000000000000000000AU01")
	wantMySQLErr(t, "app DELETE audit_logs", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("TRUNCATE TABLE audit_logs"))
	wantMySQLErr(t, "app TRUNCATE audit_logs", err, db.MySQLErrTableAccessDenied)

	// wallet_entries：追加只写。
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO wallet_entries (uid, tenant_id, amount) VALUES (?, ?, ?)"), "01J0000000000000000000WE01", 1, 100); err != nil {
		t.Fatalf("app 应能 INSERT wallet_entries: %v", err)
	}
	_, err = ex.Exec(ctx, db.Q("UPDATE wallet_entries SET amount = 1 WHERE id = 1"))
	wantMySQLErr(t, "app UPDATE wallet_entries", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("DELETE FROM wallet_entries WHERE id = 1"))
	wantMySQLErr(t, "app DELETE wallet_entries", err, db.MySQLErrTableAccessDenied)

	// 无 DDL 权限（ALTER/DROP/CREATE/INDEX）。
	_, err = ex.Exec(ctx, db.Q("ALTER TABLE sites ADD COLUMN evil INT NULL"))
	wantMySQLErr(t, "app ALTER 业务表", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("ALTER TABLE audit_logs DROP PARTITION p202611"))
	wantMySQLErr(t, "app ALTER 分区表", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("DROP TABLE plain_notes"))
	wantMySQLErr(t, "app DROP TABLE", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("CREATE TABLE evil (id INT)"))
	wantMySQLErr(t, "app CREATE TABLE", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("CREATE INDEX idx_evil ON sites (name)"))
	wantMySQLErr(t, "app CREATE INDEX", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("CREATE TRIGGER trg_evil BEFORE INSERT ON sites FOR EACH ROW SET NEW.name = 'x'"))
	if _, ok := db.MySQLErrorNumber(err); !ok {
		t.Errorf("app CREATE TRIGGER 必须被拒绝: %v", err)
	}
	// 数据没有被改动。
	if got := e.adminScalar(t, "SELECT action FROM `"+e.dbName+"`.audit_logs WHERE uid = ?", "01J0000000000000000000AU01"); got != "login" {
		t.Fatalf("audit_logs 被篡改: %s", got)
	}
}

func TestMaintAccountPermissions_RealMySQL(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	ex := e.maint.Executor(ctx)

	// 允许：分区表上的 SELECT / ALTER ... REORGANIZE / DROP PARTITION（滚动预建与保留期清理）。
	var n int
	if err := ex.Get(ctx, &n, db.Q("SELECT COUNT(*) FROM audit_logs")); err != nil {
		t.Fatalf("maint 应能 SELECT 分区表: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("ALTER TABLE audit_logs REORGANIZE PARTITION pmax INTO (PARTITION p202701 VALUES LESS THAN ('2027-02-01 00:00:00'), PARTITION pmax VALUES LESS THAN (MAXVALUE))")); err != nil {
		t.Fatalf("maint 应能 REORGANIZE PARTITION: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("ALTER TABLE audit_logs DROP PARTITION p202611")); err != nil {
		t.Fatalf("maint 应能 DROP PARTITION: %v", err)
	}
	if got := e.adminScalar(t, "SELECT COUNT(*) FROM information_schema.partitions WHERE table_schema = ? AND table_name = 'audit_logs' AND partition_name = 'p202611'", e.dbName); got != "0" {
		t.Fatalf("p202611 应已被删除: %s", got)
	}

	// 拒绝：不能 DROP 非分区表、不能改其他表、不能读写业务数据、不能写分区表的行。
	_, err := ex.Exec(ctx, db.Q("DROP TABLE plain_notes"))
	wantMySQLErr(t, "maint DROP 非分区表", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("DROP TABLE sites"))
	wantMySQLErr(t, "maint DROP sites", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("ALTER TABLE sites ADD COLUMN evil INT NULL"))
	wantMySQLErr(t, "maint ALTER 业务表", err, db.MySQLErrTableAccessDenied)
	err = ex.Get(ctx, &n, db.Q("SELECT COUNT(*) FROM sites"))
	wantMySQLErr(t, "maint SELECT 业务表", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("INSERT INTO audit_logs (uid, action) VALUES ('01J0000000000000000000AU09', 'x')"))
	wantMySQLErr(t, "maint INSERT 分区表", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("DELETE FROM audit_logs"))
	wantMySQLErr(t, "maint DELETE 分区表行", err, db.MySQLErrTableAccessDenied)
	_, err = ex.Exec(ctx, db.Q("ALTER TABLE wallet_entries ADD COLUMN evil INT NULL"))
	wantMySQLErr(t, "maint ALTER wallet_entries", err, db.MySQLErrTableAccessDenied)
}

func TestMigrateAccountCanDoDDLAndTriggers_RealMySQL(t *testing.T) {
	e := newEnv(t) // newEnv 已用 migrate 账号建表（含分区表与触发器）并授权
	ctx := ctxT(t)
	ex := e.migrate.Executor(ctx)
	if _, err := ex.Exec(ctx, db.Q("CREATE TABLE t_probe (id INT NOT NULL PRIMARY KEY) ENGINE=InnoDB")); err != nil {
		t.Fatalf("migrate 应能 CREATE TABLE: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("ALTER TABLE t_probe ADD COLUMN c INT NULL")); err != nil {
		t.Fatalf("migrate 应能 ALTER: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("CREATE TRIGGER trg_t_probe_bu BEFORE UPDATE ON t_probe FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'immutable'")); err != nil {
		t.Fatalf("持有全局 SET_USER_ID 的 migrate 账号应能建触发器（binlog=ON、trust=0）: %v", err)
	}
	if _, err := ex.Exec(ctx, db.Q("DROP TABLE t_probe")); err != nil {
		t.Fatalf("migrate 应能 DROP: %v", err)
	}
	// 触发器真实生效：audit_logs 不可变（BEFORE UPDATE SIGNAL）——由 migrate 账号自己验证（app 本无 UPDATE 权限，maint 也没有）。
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO audit_logs (uid, action) VALUES ('01J0000000000000000000AU11', 'x')")); err != nil {
		t.Fatal(err)
	}
	_, err := ex.Exec(ctx, db.Q("UPDATE audit_logs SET action = 'y' WHERE uid = '01J0000000000000000000AU11'"))
	if n, ok := db.MySQLErrorNumber(err); !ok || n != 1644 {
		t.Fatalf("触发器应以 SQLSTATE 45000（ER_SIGNAL_EXCEPTION 1644）拒绝 UPDATE: %v", err)
	}
}

// README §5.1 / §9 第 13 行：binlog=ON 且 trust=0 时，只有 ALL ON db.* 的账号建触发器报 ERROR 1419；
// 授予全局 SET_USER_ID 或在 trust=1 的实例上即可建成（安装向导预检的依据）。
func TestTriggerPrivilegeNeedsSetUserIDWhenBinlogOn_RealMySQL(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	user, pw := mysqltest.RandomName("nosu"), mysqltest.RandomSecret(t)
	e.inst.Exec(t, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", user, pw))
	e.inst.DropUserOnCleanup(t, user)
	e.inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+user+"'@'%'")
	p, err := db.OpenMigrate(ctx, e.cfg(user, pw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	const trig = "CREATE TRIGGER trg_plain_notes_bu BEFORE UPDATE ON plain_notes FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'immutable'"
	_, err = p.Executor(ctx).Exec(ctx, db.Q(trig))
	wantMySQLErr(t, "无 SET_USER_ID 建触发器", err, db.MySQLErrTriggerNoSuper)

	e.inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+user+"'@'%'")
	p2, err := db.OpenMigrate(ctx, e.cfg(user, pw)) // 新连接才会拿到新授权
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p2.Close() }()
	if _, err := p2.Executor(ctx).Exec(ctx, db.Q(trig)); err != nil {
		t.Fatalf("授予 SET_USER_ID 后应能建触发器: %v", err)
	}
}

func TestTriggerCreationWithTrustFunctionCreatorsOn_RealMySQL(t *testing.T) {
	inst := mysqltest.Start(t, mysqltest.Options{TrustFunctionCreators: true})
	ctx := ctxT(t)
	name := inst.NewDatabase(t)
	user, pw := mysqltest.RandomName("trust"), mysqltest.RandomSecret(t)
	inst.Exec(t, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", user, pw))
	inst.DropUserOnCleanup(t, user)
	inst.Exec(t, "GRANT ALL ON `"+name+"`.* TO '"+user+"'@'%'")
	p, err := db.OpenMigrate(ctx, db.Config{Host: inst.Host(), Port: inst.Port, Name: name, User: user, Password: config.NewSecret(pw)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ex := p.Executor(ctx)
	if _, err := ex.Exec(ctx, db.Q("CREATE TABLE t (id INT NOT NULL PRIMARY KEY) ENGINE=InnoDB")); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(ctx, db.Q("CREATE TRIGGER trg_t_bu BEFORE UPDATE ON t FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'immutable'")); err != nil {
		t.Fatalf("log_bin_trust_function_creators=1 时无需 SET_USER_ID 即可建触发器: %v", err)
	}
}

// 设计发现（供 Story 1.6/1.7 的迁移遵循）：库级授权无法在表级收窄。
// README §5.1 写作 `app: SELECT,INSERT,UPDATE,DELETE ON db.*`，同时要求 audit_logs/wallet_entries 仅 SELECT、INSERT——
// 二者在 MySQL 中不能同时成立（权限取并集，且不存在「从库级授权中撤销某表的权限」），因此 app 必须在迁移内逐表授权。
func TestDBLevelGrantCannotBeNarrowedPerTable_RealMySQL(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	user, pw := mysqltest.RandomName("dblvl"), mysqltest.RandomSecret(t)
	e.inst.Exec(t, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", user, pw))
	e.inst.DropUserOnCleanup(t, user)
	e.inst.Exec(t, "GRANT SELECT, INSERT, UPDATE, DELETE ON `"+e.dbName+"`.* TO '"+user+"'@'%'")

	_, err := e.inst.Admin(t).ExecContext(ctx, "REVOKE UPDATE, DELETE ON `"+e.dbName+"`.`wallet_entries` FROM '"+user+"'@'%'")
	if me, ok := db.MySQLErrorNumber(err); !ok || me != 1147 { // ER_NONEXISTING_TABLE_GRANT
		t.Fatalf("库级授权上做表级 REVOKE 本应报 1147；若行为变化，README §5.1 的授权方式可简化，请更新设计: %v", err)
	}
	p, err := db.OpenApp(ctx, e.cfg(user, pw))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	ex := p.Executor(ctx)
	if _, err := ex.Exec(ctx, db.Q("INSERT INTO wallet_entries (uid, tenant_id, amount) VALUES ('01J0000000000000000000WE02', 1, 5)")); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(ctx, db.Q("UPDATE wallet_entries SET amount = 6 WHERE uid = '01J0000000000000000000WE02'")); err != nil {
		t.Fatalf("库级授权下 UPDATE 追加表本应仍然可行（这正是必须逐表授权的原因）: %v", err)
	}
}
