package setupit

import (
	"strings"
	"testing"
)

func (w *wiz) stepStatuses(t *testing.T) map[string]string {
	t.Helper()
	r := w.mustOK(w.get("status"), "status")
	out := map[string]string{}
	for _, s := range r.JSON["steps"].([]any) {
		m := s.(map[string]any)
		out[m["key"].(string)] = m["status"].(string)
	}
	return out
}

func (w *wiz) wantStatuses(t *testing.T, when string, done ...string) {
	t.Helper()
	got := w.stepStatuses(t)
	isDone := map[string]bool{}
	for _, d := range done {
		isDone[d] = true
	}
	for step, st := range got {
		want := "pending"
		if isDone[step] {
			want = "done"
		}
		if st != want {
			t.Errorf("%s：步骤 %s 状态 = %s，期望 %s（全部：%v）", when, step, st, want, got)
		}
	}
}

// AC5：每一步之后「杀进程」（内存中的口令、缓冲、连接全部丢失，只剩磁盘与数据库）再续做：状态由持久事实重建，各步幂等。
func TestResumeAfterEveryStep(t *testing.T) {
	w := newWiz(t)
	w.mustOK(w.post("checks", nil), "checks")
	w.start()
	w.wantStatuses(t, "环境检查后重启（环境检查只在内存，需要重做不影响续做）")

	w.mustOK(w.put("database", w.dbBody()), "database")
	w.start()
	w.wantStatuses(t, "MySQL 步骤后重启", "database")
	// Redis 之前不能初始化；Redis 步骤可以续做（凭据文件已有 database 段）。
	if r := w.post("initialize", nil); r.Status != 409 || !strings.Contains(r.details(), "redis") {
		t.Fatalf("Redis 步骤之前 initialize 应 409: %d %s", r.Status, r.Raw)
	}
	w.mustOK(w.put("redis", w.redisBody()), "redis")
	w.start()
	w.wantStatuses(t, "Redis 步骤后重启", "database", "redis")
	// ADR-026 §6：migrate 口令不落盘 ⇒ 续做重新提供，绝不「悄悄取回」。
	r := w.post("initialize", nil)
	if r.Status != 409 || !strings.Contains(r.details(), "migrate_password") {
		t.Fatalf("重启后 initialize 应 409 并要求重新提供迁移凭据: %d %s", r.Status, r.Raw)
	}
	w.assertBodyHasNoSecrets(r)
	// (i) 重新输入迁移口令（预建式续做：同一目标，app/maint 由凭据文件读回，口令不会被改写）。
	before := w.readCred(t)
	w.mustOK(w.put("database", w.dbBody()), "database(重新提供迁移口令)")
	if after := w.readCred(t); !strings.Contains(after, `"app"`) || before == "" {
		t.Errorf("凭据文件应保持有效")
	}
	w.mustOK(w.post("initialize", nil), "initialize")
	w.start()
	w.wantStatuses(t, "数据库初始化后重启", "database", "redis", "init_db")
	if n := w.dbInt("SELECT COUNT(*) FROM §.schema_migrations WHERE is_applied = 1 AND version_id > 0"); n != migrationCount {
		t.Errorf("迁移数 %d", n)
	}
	// 重复 initialize：幂等（无待执行迁移）——但此时 migrate 口令已丢，应要求重新提供而不是静默成功/失败。
	if r := w.post("initialize", nil); r.Status != 409 {
		t.Errorf("重启后再次 initialize 应要求重新提供迁移凭据: %d", r.Status)
	}
	// 此时库非空，但属于本向导中断的进行中安装：重新提供迁移口令（建账号模式重置）必须被允许，而不是 database_not_empty。
	w.mustOK(w.put("database", w.dbBody()), "进行中安装的库重新提供凭据")
	r = w.mustOK(w.post("initialize", nil), "initialize(幂等)")
	if !strings.Contains(r.msg("message"), "无待执行迁移") {
		t.Errorf("已完成的迁移再次 initialize 应报告无待执行: %s", r.msg("message"))
	}

	w.mustOK(w.post("admin", w.adminBody()), "admin")
	w.start()
	w.wantStatuses(t, "管理员后重启", "database", "redis", "init_db", "admin")
	r = w.mustOK(w.post("admin", w.adminBody()), "admin(幂等)")
	if !strings.Contains(r.msg("message"), "已存在") {
		t.Errorf("重复创建同一管理员应幂等: %s", r.msg("message"))
	}
	// 另一个邮箱：向导只创建首个管理员。
	other := w.adminBody()
	other["email"] = "other" + rnd(3) + "@example.test"
	if r := w.post("admin", other); r.Status != 409 {
		t.Errorf("已存在管理员时创建另一个应 409: %d %s", r.Status, r.Raw)
	}

	w.mustOK(w.put("system", w.systemBody()), "system")
	w.start()
	w.wantStatuses(t, "系统配置后重启", "database", "redis", "init_db", "admin", "system")

	w.mustOK(w.post("keys", nil), "keys")
	r = w.mustOK(w.post("keys", nil), "keys(幂等)")
	w.start()
	w.wantStatuses(t, "密钥后重启", "database", "redis", "init_db", "admin", "system", "keys")
	if n := w.dbInt("SELECT COUNT(*) FROM §.signing_keys WHERE status = 'active'"); n != 3 {
		t.Errorf("幂等重复初始化不得产生新密钥: %d", n)
	}

	r = w.mustOK(w.post("complete", nil), "complete")
	if r.secrets() != nil {
		t.Errorf("迁移口令由安装者提供且已重新提供时不应一次性返回: %s", r.Raw)
	}
	w.assertNoSecrets()
}

// AC10：迁移口令未知（自动生成后进程重启）→ 用临时管理员 ALTER USER 重置；旧口令随之失效；重置后的新口令在 complete 时一次性返回。
func TestResumeResetsUnknownMigratePassword(t *testing.T) {
	w := newWiz(t)
	b := w.dbBody() // 安装者最初指定的迁移口令 w.migPW
	w.mustOK(w.put("database", b), "database")
	w.mustOK(w.put("redis", w.redisBody()), "redis")
	w.start() // 内存口令丢失
	b2 := w.dbBody()
	delete(b2, "migrate_password") // 不再提供：由向导重置为新的随机口令
	w.mustOK(w.put("database", b2), "database(重置迁移账号)")
	if err := w.userDB("lingedge_migrate", w.migPW).Ping(); err == nil {
		t.Error("重置后旧迁移口令应失效")
	}
	w.mustOK(w.post("initialize", nil), "initialize")
	w.mustOK(w.post("admin", w.adminBody()), "admin")
	w.mustOK(w.put("system", w.systemBody()), "system")
	w.mustOK(w.post("keys", nil), "keys")
	r := w.post("complete", nil)
	w.mustOK(r, "complete")
	newPW, _ := r.secrets()["migrate_password"].(string)
	if newPW == "" || newPW == w.migPW {
		t.Fatalf("应一次性返回重置后的新迁移口令: %s", r.Raw)
	}
	w.secrets = append(w.secrets, newPW)
	w.bodies[len(w.bodies)-1] = strings.ReplaceAll(w.bodies[len(w.bodies)-1], newPW, "<ONE_TIME>")
	if err := w.userDB("lingedge_migrate", newPW).Ping(); err != nil {
		t.Errorf("返回的新迁移口令应可登录: %v", err)
	}
	w.assertNoSecrets()
}

// 预建账号模式 + 全流程：DBA 预建库与账号；app/migrate 口令由安装者提供；maint 缺省则不配置（分区维护将跳过并告警）。
func TestPrebuiltAccountsModeFullFlow(t *testing.T) {
	w := newWiz(t)
	w.dbaPrepare(true, true)
	body := w.prebuiltBody()
	delete(body, "maint_password")
	r := w.mustOK(w.put("database", body), "database(预建)")
	if !strings.Contains(r.msg("message"), "未配置 maint") {
		t.Errorf("未提供 maint 口令应明确提示分区维护将跳过: %s", r.msg("message"))
	}
	if strings.Contains(w.readCred(t), `"maint"`) {
		t.Error("未配置 maint 时凭据文件不应含 maint 账号")
	}
	w.mustOK(w.put("redis", w.redisBody()), "redis")
	w.mustOK(w.post("initialize", nil), "initialize")
	w.mustOK(w.post("admin", w.adminBody()), "admin")
	w.mustOK(w.put("system", w.systemBody()), "system")
	w.mustOK(w.post("keys", nil), "keys")
	r = w.mustOK(w.post("complete", nil), "complete")
	if r.secrets() != nil {
		t.Errorf("预建账号模式不应一次性返回迁移口令: %s", r.Raw)
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.schema_migrations WHERE is_applied = 1 AND version_id > 0"); n != migrationCount {
		t.Errorf("迁移数 %d", n)
	}
	// 预建账号模式：向导没有改动 DBA 创建的账号口令（四个口令仍然可用）。
	for _, a := range []struct{ u, p string }{{"lingedge_app", w.appPW}, {"lingedge_migrate", w.migPW}} {
		if err := w.userDB(a.u, a.p).Ping(); err != nil {
			t.Errorf("%s 口令应保持不变: %v", a.u, err)
		}
	}
	w.assertNoSecrets()
}
