package setupit

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/setup"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

func (r resp) details() string {
	e, _ := r.JSON["error"].(map[string]any)
	b, _ := e["details"].([]any)
	var parts []string
	for _, d := range b {
		m := d.(map[string]any)
		parts = append(parts, fmt.Sprint(m["field"], ":", m["reason"]))
	}
	return strings.Join(parts, " | ")
}

// 预建账号模式的 DBA 侧准备：创建库与三类账号。migrate 账号的授权由 grants 参数决定。
func (w *wiz) dbaPrepare(withSetUserID, withGrantOption bool) {
	w.t.Helper()
	w.exec("CREATE DATABASE `" + w.dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	for _, a := range []struct{ u, p string }{{"lingedge_app", w.appPW}, {"lingedge_maint", w.maintPW}, {"lingedge_migrate", w.migPW}} {
		w.exec(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", a.u, a.p))
	}
	g := "GRANT ALL ON `" + w.dbName + "`.* TO 'lingedge_migrate'@'%'"
	if withGrantOption {
		g += " WITH GRANT OPTION"
	}
	w.exec(g)
	if withSetUserID {
		w.exec("GRANT SET_USER_ID ON *.* TO 'lingedge_migrate'@'%'")
	}
}

func (w *wiz) prebuiltBody() map[string]any {
	return map[string]any{"host": w.inst.Host(), "port": w.inst.Port, "database": w.dbName, "username": "lingedge_app", "password": w.appPW,
		"migrate_password": w.migPW, "maint_password": w.maintPW}
}

// AC3：目标库必须为空，否则 400 setup.database_not_empty（建账号模式与预建账号模式各一遍），且不创建任何账号、不写凭据文件。
func TestDatabaseMustBeEmpty(t *testing.T) {
	t.Run("建账号模式", func(t *testing.T) {
		w := newWiz(t)
		w.exec("CREATE DATABASE `" + w.dbName + "` CHARACTER SET utf8mb4")
		w.exec("CREATE TABLE `" + w.dbName + "`.precious (id INT PRIMARY KEY)")
		w.exec("INSERT INTO `" + w.dbName + "`.precious VALUES (1)")
		r := w.put("database", w.dbBody())
		if r.Status != 400 || r.errCode() != "setup.database_not_empty" {
			t.Fatalf("非空库应 400 setup.database_not_empty: %d %s", r.Status, r.Raw)
		}
		if n := w.dbInt("SELECT COUNT(*) FROM mysql.user WHERE User LIKE 'lingedge\\_%'"); n != 0 {
			t.Errorf("非空库被拒绝时不得创建任何账号，实际 %d", n)
		}
		if _, err := w.credFileStat(); err == nil {
			t.Error("被拒绝时不得写运行期凭据文件")
		}
		if n := w.dbInt("SELECT COUNT(*) FROM §.precious"); n != 1 {
			t.Error("已有数据必须保持不变（FR-6：不静默覆盖）")
		}
		w.assertNoSecrets()
	})
	t.Run("预建账号模式", func(t *testing.T) {
		w := newWiz(t)
		w.dbaPrepare(true, true)
		w.exec("CREATE TABLE `" + w.dbName + "`.precious (id INT PRIMARY KEY)")
		r := w.put("database", w.prebuiltBody())
		if r.Status != 400 || r.errCode() != "setup.database_not_empty" {
			t.Fatalf("非空库应 400 setup.database_not_empty: %d %s", r.Status, r.Raw)
		}
		w.assertNoSecrets()
	})
}

// AC3/AC7：MySQL 预检——字符集、严格 sql_mode、binlog 与触发器权限（ERROR 1419 阻断并给两种处置）、GRANT OPTION、账号登录。
func TestMySQLPrechecks(t *testing.T) {
	t.Run("库字符集必须是utf8mb4", func(t *testing.T) {
		w := newWiz(t)
		w.exec("CREATE DATABASE `" + w.dbName + "` CHARACTER SET latin1")
		r := w.put("database", w.dbBody())
		if r.Status != 400 || r.errCode() != "setup.precheck_failed" || !strings.Contains(r.details(), "utf8mb4") {
			t.Fatalf("latin1 库应被预检拒绝: %d %s", r.Status, r.Raw)
		}
	})
	t.Run("全局sql_mode必须是严格模式", func(t *testing.T) {
		w := newWiz(t)
		var old string
		if err := w.adm.QueryRow("SELECT @@GLOBAL.sql_mode").Scan(&old); err != nil {
			t.Fatal(err)
		}
		w.exec("SET GLOBAL sql_mode = ''")
		t.Cleanup(func() { _, _ = w.adm.Exec("SET GLOBAL sql_mode = ?", old) })
		r := w.put("database", w.dbBody())
		if r.Status != 400 || r.errCode() != "setup.precheck_failed" || !strings.Contains(r.details(), "严格模式") {
			t.Fatalf("非严格 sql_mode 应被预检拒绝: %d %s", r.Status, r.Raw)
		}
		w.exec("SET GLOBAL sql_mode = ?", old)
		w.mustOK(w.put("database", w.dbBody()), "恢复严格模式后应通过")
	})
	t.Run("binlog开启且无SET_USER_ID_阻断并给出两种处置", func(t *testing.T) {
		w := newWiz(t) // 默认实例：log_bin=ON、log_bin_trust_function_creators=0
		if n := w.dbInt("SELECT @@log_bin * 10 + @@log_bin_trust_function_creators"); n != 10 {
			t.Fatalf("测试实例应为 log_bin=1、trust=0（生产形态），实际 %d", n)
		}
		w.dbaPrepare(false, true)
		r := w.put("database", w.prebuiltBody())
		if r.Status != 400 || r.errCode() != "setup.precheck_failed" {
			t.Fatalf("ERROR 1419 应阻断: %d %s", r.Status, r.Raw)
		}
		d := r.details()
		for _, must := range []string{"1419", "SET_USER_ID", "log_bin_trust_function_creators=1"} {
			if !strings.Contains(d, must) {
				t.Errorf("阻断说明应含 %q（两种处置）: %s", must, d)
			}
		}
		if _, err := w.credFileStat(); err == nil {
			t.Error("预检失败时不得写运行期凭据文件")
		}
		// 向导不自行改 MySQL 全局变量：仍是 trust=0。
		if n := w.dbInt("SELECT @@log_bin_trust_function_creators"); n != 0 {
			t.Error("向导不得修改 log_bin_trust_function_creators")
		}
		// 无任何探针残留（表/触发器）。
		if n := w.dbInt("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?", w.dbName); n != 0 {
			t.Errorf("探针表应已清理，实际 %d", n)
		}
		// 按处置①授予 SET_USER_ID 后通过。
		w.exec("GRANT SET_USER_ID ON *.* TO 'lingedge_migrate'@'%'")
		w.mustOK(w.put("database", w.prebuiltBody()), "授予 SET_USER_ID 后应通过")
		w.assertNoSecrets()
	})
	t.Run("log_bin关闭无需SET_USER_ID", func(t *testing.T) {
		w := newWizWith(t, mysqltest.Options{NoBinlog: true})
		w.dbaPrepare(false, true)
		w.mustOK(w.put("database", w.prebuiltBody()), "log_bin=OFF")
	})
	t.Run("trust_function_creators开启无需SET_USER_ID", func(t *testing.T) {
		w := newWizWith(t, mysqltest.Options{TrustFunctionCreators: true})
		w.dbaPrepare(false, true)
		w.mustOK(w.put("database", w.prebuiltBody()), "trust=1")
	})
	t.Run("迁移账号缺少GRANT_OPTION", func(t *testing.T) {
		w := newWiz(t)
		w.dbaPrepare(true, false)
		r := w.put("database", w.prebuiltBody())
		if r.Status != 400 || !strings.Contains(r.details(), "GRANT OPTION") {
			t.Fatalf("缺少 GRANT OPTION 应被预检拒绝并说明: %d %s", r.Status, r.Raw)
		}
	})
	t.Run("建账号模式自带SET_USER_ID与GRANT_OPTION", func(t *testing.T) {
		w := newWiz(t)
		w.mustOK(w.put("database", w.dbBody()), "建账号模式")
		grants := w.dbStr("SELECT GROUP_CONCAT(PRIVILEGE_TYPE) FROM information_schema.USER_PRIVILEGES WHERE GRANTEE = \"'lingedge_migrate'@'%'\"")
		if !strings.Contains(grants, "SET_USER_ID") {
			t.Errorf("lingedge_migrate 应持有全局 SET_USER_ID: %s", grants)
		}
		if n := w.dbInt("SELECT COUNT(*) FROM information_schema.SCHEMA_PRIVILEGES WHERE GRANTEE = \"'lingedge_migrate'@'%'\" AND IS_GRANTABLE = 'YES'"); n == 0 {
			t.Error("lingedge_migrate 应持有 WITH GRANT OPTION")
		}
		// app/maint 此刻没有任何库级授权（逐表授权由迁移完成）。
		if n := w.dbInt("SELECT COUNT(*) FROM information_schema.SCHEMA_PRIVILEGES WHERE GRANTEE IN (\"'lingedge_app'@'%'\", \"'lingedge_maint'@'%'\")"); n != 0 {
			t.Errorf("app/maint 不得有库级授权（库级授权无法对 audit_logs 收窄）: %d", n)
		}
	})
	t.Run("管理员口令错误与app账号口令错误都不泄漏口令", func(t *testing.T) {
		w := newWiz(t)
		b := w.dbBody()
		wrong := "wrong-" + rnd(6)
		w.secrets = append(w.secrets, wrong)
		b["admin_password"] = wrong
		r := w.put("database", b)
		if r.Status != 400 || r.errCode() != "setup.precheck_failed" || !strings.Contains(r.details(), "1045") {
			t.Fatalf("管理员口令错误应 400 且说明 MySQL 1045: %d %s", r.Status, r.Raw)
		}
		w.assertBodyHasNoSecrets(r)
		w.dbaPrepare(true, true)
		bad := w.prebuiltBody()
		badApp := "badapp-" + rnd(6)
		w.secrets = append(w.secrets, badApp)
		bad["password"] = badApp
		r = w.put("database", bad)
		if r.Status != 400 || !strings.Contains(r.details(), "1045") {
			t.Fatalf("app 口令错误应被预检拒绝: %d %s", r.Status, r.Raw)
		}
		w.assertNoSecrets()
	})
	t.Run("请求校验", func(t *testing.T) {
		w := newWiz(t)
		cases := map[string]func(m map[string]any){
			"username":         func(m map[string]any) { m["username"] = "someone_else" },
			"port":             func(m map[string]any) { m["port"] = 70000 },
			"database":         func(m map[string]any) { m["database"] = "bad`name" },
			"database(前缀)":     func(m map[string]any) { m["database"] = "mysql" },
			"admin_username":   func(m map[string]any) { delete(m, "admin_username") },
			"migrate_username": func(m map[string]any) { m["migrate_username"] = "lingedge_app" },
			"tls":              func(m map[string]any) { m["host"] = "db.example.invalid" },
			"password":         func(m map[string]any) { m["password"] = "short" },
		}
		for field, mut := range cases {
			b := w.dbBody()
			mut(b)
			r := w.put("database", b)
			if r.Status != 400 || r.errCode() != "validation.failed" || !strings.Contains(r.details(), strings.SplitN(field, "(", 2)[0]+":") {
				t.Errorf("%s 校验: %d %s", field, r.Status, r.Raw)
			}
		}
		if r := w.do("PUT", api+"database", w.token, map[string]any{"host": "x", "unknown_field": 1}); r.Status != 400 {
			t.Errorf("未知字段应 400: %d", r.Status)
		}
	})
	t.Run("主密钥不可用时拒绝涉及口令落盘的步骤", func(t *testing.T) {
		w := newWiz(t, func(o *setup.Options) { o.Box = nil })
		r := w.post("checks", nil)
		var bad bool
		for _, it := range r.JSON["items"].([]any) {
			m := it.(map[string]any)
			if m["key"] == "master_key.readable" && m["ok"] == false {
				bad = true
			}
		}
		if !bad {
			t.Errorf("环境检查应报告主密钥不可读: %s", r.Raw)
		}
		r = w.put("database", w.dbBody())
		if r.Status != 400 || r.errCode() != "setup.precheck_failed" || !strings.Contains(r.details(), "主密钥") {
			t.Errorf("无主密钥应拒绝: %d %s", r.Status, r.Raw)
		}
		if n := w.dbInt("SELECT COUNT(*) FROM mysql.user WHERE User LIKE 'lingedge\\_%'"); n != 0 {
			t.Error("无主密钥时不得创建账号")
		}
	})
}

// AC3：Redis 步骤——INFO server 主版本 ≥8（本机 Redis 8.x 通过；<8 的拒绝分支由 redisx 的构造响应单测覆盖，真实 <8 实例 **等待环境验证**）、
// 不可达/口令错误被拒、口令加密落盘、不使用 FLUSHDB（不清空任何已有键）。
func TestRedisStep(t *testing.T) {
	w := newWiz(t)
	// 顺序：未完成 MySQL 步骤不能配置 Redis。
	if r := w.put("redis", w.redisBody()); r.Status != 409 {
		t.Errorf("MySQL 步骤之前配置 Redis 应 409: %d %s", r.Status, r.Raw)
	}
	w.mustOK(w.put("database", w.dbBody()), "database")
	if r := w.put("redis", map[string]any{"address": "127.0.0.1:1"}); r.Status != 400 || r.errCode() != "setup.precheck_failed" {
		t.Errorf("不可达应 400 precheck_failed: %d %s", r.Status, r.Raw)
	}
	if r := w.put("redis", map[string]any{"address": "no-port"}); r.Status != 400 || r.errCode() != "validation.failed" {
		t.Errorf("地址格式应校验: %d %s", r.Status, r.Raw)
	}
	// 带口令的 Redis：错误口令被拒且不泄漏；正确口令通过并加密落盘；不使用 FLUSHDB（预置键保持不变）。
	rp := rnd(10)
	w.redisPW = rp
	w.secrets = append(w.secrets, rp)
	rds := redistest.Start(t, redistest.Options{Password: rp})
	seed := redisRaw(t, rds, rp, "SET", "le:keep:me", "1")
	_ = seed
	wrong := "wrong-" + rnd(6)
	w.secrets = append(w.secrets, wrong)
	if r := w.put("redis", map[string]any{"address": rds.Addr(), "password": wrong}); r.Status != 400 || r.errCode() != "setup.precheck_failed" {
		t.Errorf("口令错误应被拒绝: %d %s", r.Status, r.Raw)
	} else {
		w.assertBodyHasNoSecrets(r)
	}
	r := w.mustOK(w.put("redis", map[string]any{"address": rds.Addr(), "password": rp, "db": 0}), "redis")
	if !strings.Contains(r.msg("message"), "主版本 ≥8") {
		t.Errorf("消息: %s", r.msg("message"))
	}
	if got := redisRaw(t, rds, rp, "GET", "le:keep:me"); got != "1" {
		t.Errorf("预置键应保持不变（不使用 FLUSHDB），实际 %q", got)
	}
	raw := w.readCred(t)
	if strings.Contains(raw, rp) || !strings.Contains(raw, `"redis"`) {
		t.Errorf("Redis 口令应加密落盘: %s", raw)
	}
	w.assertNoSecrets()
}
