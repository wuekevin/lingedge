package dbintegration

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// 与 database/README 一致的最小表集（真实列约定：C1、T、复合外键、分区表 + pmax、仅追加表）。
var schemaDDL = []string{
	`CREATE TABLE tenants (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, name VARCHAR(128) NOT NULL,
  UNIQUE KEY uk_tenants_uid (uid)) ENGINE=InnoDB`,
	`CREATE TABLE sites (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL,
  tenant_id BIGINT UNSIGNED NOT NULL, name VARCHAR(128) NOT NULL, status VARCHAR(16) NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3), deleted_at DATETIME(3) NULL,
  UNIQUE KEY uk_sites_uid (uid), UNIQUE KEY uk_sites_tenant_id (tenant_id, id), KEY idx_sites_tenant_name (tenant_id, name),
  CONSTRAINT fk_sites_tenants FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE RESTRICT) ENGINE=InnoDB`,
	`CREATE TABLE site_domains (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL,
  tenant_id BIGINT UNSIGNED NOT NULL, site_id BIGINT UNSIGNED NOT NULL, hostname VARCHAR(255) NOT NULL,
  UNIQUE KEY uk_site_domains_uid (uid), UNIQUE KEY uk_site_domains_host (tenant_id, hostname),
  CONSTRAINT fk_site_domains_sites FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id) ON DELETE RESTRICT) ENGINE=InnoDB`,
	`CREATE TABLE audit_logs (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT, uid CHAR(26) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  tenant_id BIGINT UNSIGNED NULL, action VARCHAR(96) NOT NULL,
  PRIMARY KEY (id, created_at), UNIQUE KEY uk_audit_logs_uid (uid, created_at), KEY idx_audit_tenant (tenant_id, created_at)
) ENGINE=InnoDB PARTITION BY RANGE COLUMNS(created_at) (
  PARTITION p202611 VALUES LESS THAN ('2026-12-01 00:00:00'),
  PARTITION p202612 VALUES LESS THAN ('2027-01-01 00:00:00'),
  PARTITION pmax VALUES LESS THAN (MAXVALUE))`,
	`CREATE TRIGGER trg_audit_logs_bu BEFORE UPDATE ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'audit_logs is immutable'`,
	`CREATE TABLE wallet_entries (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, uid CHAR(26) NOT NULL, tenant_id BIGINT UNSIGNED NOT NULL,
  amount BIGINT NOT NULL, created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3), UNIQUE KEY uk_wallet_entries_uid (uid)) ENGINE=InnoDB`,
	`CREATE TABLE plain_notes (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, note VARCHAR(64) NOT NULL) ENGINE=InnoDB`,
}

// env 是一个测试用例的独立环境：临时实例上的随机库 + 三类真实账号（IMP-19，database/README §5.1）。
type env struct {
	inst   *mysqltest.Instance
	dbName string

	appUser, maintUser, migrateUser string
	appPW, maintPW, migratePW       string

	app     *db.AppPool
	maint   *db.MaintPool
	migrate *db.MigratePool
}

func (e *env) cfg(user, pw string) db.Config {
	return db.Config{Host: e.inst.Host(), Port: e.inst.Port, Name: e.dbName, User: user, Password: config.NewSecret(pw)}
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newEnv 创建随机库与三类账号，并以 migrate 账号建表、按 README §5.1 授权：
//   - migrate：ALL ON db.* WITH GRANT OPTION + 全局 SET_USER_ID（binlog=ON、trust=0 时建触发器所需）；
//   - app：业务表表级 SELECT/INSERT/UPDATE/DELETE，audit_logs 与 wallet_entries 仅 SELECT、INSERT；
//   - maint：仅分区表 audit_logs 的表级 SELECT、ALTER、DROP。
func newEnv(t *testing.T) *env {
	t.Helper()
	inst := mysqltest.Start(t) // 默认形态：log-bin=ON、log_bin_trust_function_creators=0
	e := &env{inst: inst, dbName: inst.NewDatabase(t)}
	e.appUser, e.maintUser, e.migrateUser = mysqltest.RandomName("app"), mysqltest.RandomName("maint"), mysqltest.RandomName("mig")
	e.appPW, e.maintPW, e.migratePW = mysqltest.RandomSecret(t), mysqltest.RandomSecret(t), mysqltest.RandomSecret(t)
	for _, a := range []struct{ u, p string }{{e.appUser, e.appPW}, {e.maintUser, e.maintPW}, {e.migrateUser, e.migratePW}} {
		// 口令为运行期生成的十六进制串（无引号/转义风险）；CREATE USER 不支持参数占位，故用格式化。
		inst.Exec(t, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", a.u, a.p))
		inst.DropUserOnCleanup(t, a.u)
	}
	inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+e.migrateUser+"'@'%' WITH GRANT OPTION")
	inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+e.migrateUser+"'@'%'")

	ctx := ctxT(t)
	var err error
	if e.migrate, err = db.OpenMigrate(ctx, e.cfg(e.migrateUser, e.migratePW)); err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = e.migrate.Close() })
	for _, ddl := range schemaDDL {
		if _, err := e.migrate.SQLDB().ExecContext(ctx, ddl); err != nil {
			t.Fatalf("migrate 账号建表失败: %v\n%.80s", err, ddl)
		}
	}
	// 迁移内授权（迁移账号持有 GRANT OPTION，README §5.1/§9 第 15 行）。
	for _, tbl := range []string{"tenants", "sites", "site_domains", "plain_notes"} {
		e.migrateExec(t, "GRANT SELECT, INSERT, UPDATE, DELETE ON `"+e.dbName+"`.`"+tbl+"` TO '"+e.appUser+"'@'%'")
	}
	for _, tbl := range []string{"audit_logs", "wallet_entries"} {
		e.migrateExec(t, "GRANT SELECT, INSERT ON `"+e.dbName+"`.`"+tbl+"` TO '"+e.appUser+"'@'%'")
	}
	e.migrateExec(t, "GRANT SELECT, ALTER, DROP ON `"+e.dbName+"`.`audit_logs` TO '"+e.maintUser+"'@'%'")

	if e.app, err = db.OpenApp(ctx, e.cfg(e.appUser, e.appPW)); err != nil {
		t.Fatalf("OpenApp: %v", err)
	}
	t.Cleanup(func() { _ = e.app.Close() })
	if e.maint, err = db.OpenMaint(ctx, e.cfg(e.maintUser, e.maintPW)); err != nil {
		t.Fatalf("OpenMaint: %v", err)
	}
	t.Cleanup(func() { _ = e.maint.Close() })
	return e
}

func (e *env) migrateExec(t *testing.T, stmt string) {
	t.Helper()
	if _, err := e.migrate.SQLDB().ExecContext(ctxT(t), stmt); err != nil {
		t.Fatalf("migrate 账号执行失败: %v\n%s", err, stmt)
	}
}

// adminScalar 以 root 直接统计行数（绕过被测的 Repository 路径，用来核对「越权没有改动数据」）。
func (e *env) adminScalar(t *testing.T, query string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := e.inst.Admin(t).QueryRowContext(ctxT(t), query, args...).Scan(&s); err != nil {
		t.Fatalf("admin 查询失败: %v\n%s", err, query)
	}
	return s.String
}

// seedTenants 以 root 写入两个租户并返回其 id。
func (e *env) seedTenants(t *testing.T) (a, b uint64) {
	t.Helper()
	admin := e.inst.Admin(t)
	ins := func(uid, name string) uint64 {
		res, err := admin.ExecContext(ctxT(t), "INSERT INTO `"+e.dbName+"`.tenants (uid, name) VALUES (?, ?)", uid, name)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return uint64(id)
	}
	return ins("01J00000000000000000000TA1", "tenant-a"), ins("01J00000000000000000000TB1", "tenant-b")
}
