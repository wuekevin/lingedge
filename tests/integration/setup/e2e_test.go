package setupit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/wuekevin/lingedge/cmd/lingedge-server/assembly"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/server"
)

// 完整向导端到端（建账号模式，迁移与维护口令由向导自动生成，管理员绑定 TOTP），逐项断言 AC1–AC12 的主体：
// 18 个迁移全部应用并逐个显示分类、三类账号的真实权限（含负向）、运行期凭据文件（权限/加密/无明文）、完成后 410 与 token 删除、
// 一次性机密、日志/响应/审计/state 全量无机密、migrate 窗口关闭。
func TestFullWizardCreateAccountsMode(t *testing.T) {
	w := newWiz(t)

	// 状态：全部 pending，installed=false。
	st := w.mustOK(w.get("status"), "status")
	if st.JSON["installed"] != false {
		t.Fatalf("installed 应为 false: %s", st.Raw)
	}
	steps := st.JSON["steps"].([]any)
	if len(steps) != 8 {
		t.Fatalf("应有 8 个步骤: %s", st.Raw)
	}
	for _, s := range steps {
		if s.(map[string]any)["status"] != "pending" {
			t.Fatalf("初始应全部 pending: %s", st.Raw)
		}
	}

	// 1 环境检查。
	ch := w.mustOK(w.post("checks", nil), "checks")
	for _, it := range ch.JSON["items"].([]any) {
		m := it.(map[string]any)
		if m["ok"] == false && m["severity"] == "error" {
			t.Errorf("环境检查不应有 error: %v", m)
		}
	}

	// 2 MySQL（建账号模式；不给 migrate/maint 口令 → 向导生成）。
	body := w.dbBody()
	delete(body, "migrate_password")
	delete(body, "maint_password")
	r := w.mustOK(w.put("database", body), "database")
	if !strings.Contains(r.msg("message"), "预检通过") {
		t.Errorf("database 消息: %s", r.Raw)
	}
	// 三类账号都已创建，库为 utf8mb4。
	for _, u := range []string{"lingedge_app", "lingedge_maint", "lingedge_migrate"} {
		if n := w.dbInt("SELECT COUNT(*) FROM mysql.user WHERE User = ? AND Host = '%'", u); n != 1 {
			t.Errorf("账号 %s 应已创建", u)
		}
	}
	if cs := w.dbStr("SELECT DEFAULT_CHARACTER_SET_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", w.dbName); cs != "utf8mb4" {
		t.Errorf("库字符集 = %q", cs)
	}
	// 运行期凭据文件：database 段已写入（权限 0600、目录 0700、无明文）。
	w.assertCredFile(t)
	// 此时 audit_logs 尚不存在：步骤事件被缓冲（不丢）。
	if n := w.dbInt("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'audit_logs'", w.dbName); n != 0 {
		t.Fatalf("迁移之前 audit_logs 不应存在")
	}

	// 3 Redis。
	w.mustOK(w.put("redis", w.redisBody()), "redis")

	// 4 数据库初始化：18 个迁移全部应用，逐个显示分类。
	r = w.mustOK(w.post("initialize", nil), "initialize")
	msg := r.msg("message")
	if !strings.Contains(msg, "本次应用 18 个") || !strings.Contains(msg, "000001_") || !strings.Contains(msg, "000018_") || !strings.Contains(msg, "expand") {
		t.Errorf("initialize 应逐个显示迁移与分类: %s", msg)
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.schema_migrations WHERE is_applied = 1 AND version_id > 0"); n != migrationCount {
		t.Errorf("应用的迁移数 = %d，期望 %d", n, migrationCount)
	}
	// 迁移完成后缓冲的审计事件已补写（checks/database/redis/initialize）。
	for _, a := range []string{"setup.checks", "setup.database", "setup.redis", "setup.initialize"} {
		if n := w.dbInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = ?", a); n != 1 {
			t.Errorf("审计 %s 应恰有 1 行，实际 %d", a, n)
		}
	}
	// 三类账号的真实权限（本步骤之后迁移已授予逐表授权）。
	w.assertAccountPrivileges(t)

	// 5 管理员：先取得 TOTP 密钥，再确认启用。
	ab := w.adminBody()
	ab["enable_totp"] = true
	r = w.mustOK(w.post("admin", ab, "__none__"), "admin")
	sec := r.secrets()
	if sec == nil || sec["totp_secret"] == "" || !strings.HasPrefix(fmt.Sprint(sec["totp_otpauth_uri"]), "otpauth://totp/") {
		t.Fatalf("应返回 TOTP 密钥与 otpauth URI: %s", r.Raw)
	}
	totpSecret := sec["totp_secret"].(string)
	w.secrets = append(w.secrets, totpSecret) // 响应体里按设计出现一次，之后任何地方都不得再出现
	w.bodies[len(w.bodies)-1] = strings.ReplaceAll(w.bodies[len(w.bodies)-1], totpSecret, "<ONE_TIME>")
	code, err := totp.GenerateCode(totpSecret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ab["totp_code"] = code
	r = w.mustOK(w.post("admin", ab), "admin(confirm)")
	codes, _ := r.secrets()["recovery_codes"].([]any)
	if len(codes) != 10 {
		t.Fatalf("应一次性发放 10 个恢复码: %s", r.Raw)
	}
	for _, c := range codes {
		w.secrets = append(w.secrets, c.(string))
	}
	w.bodies[len(w.bodies)-1] = "<recovery codes redacted from record>"
	if n := w.dbInt("SELECT COUNT(*) FROM §.user_roles ur JOIN §.roles r ON r.id = ur.role_id JOIN §.users u ON u.id = ur.user_id WHERE r.code = 'platform_admin' AND u.email = ? AND u.must_change_password = 0 AND u.totp_enabled = 1", w.admEmail); n != 1 {
		t.Errorf("管理员应持有 platform_admin、无需强制改密、TOTP 已启用")
	}
	if h := w.dbStr("SELECT password_hash FROM §.users WHERE email = ?", w.admEmail); !strings.HasPrefix(h, "$argon2id$") || strings.Contains(h, w.admPW) {
		t.Errorf("口令应以 argon2id 哈希存储")
	}

	// 6 系统基础配置。
	w.mustOK(w.put("system", w.systemBody()), "system")
	for k, want := range map[string]string{"system.run_mode": `"self_hosted"`, "general.base_url": `"https://edge.example.test"`, "general.timezone": `"Asia/Shanghai"`, "brand.name": `"测试云"`} {
		if got := w.dbStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = ?", k); got != want {
			t.Errorf("设置 %s = %s，期望 %s", k, got, want)
		}
	}

	// 7 密钥。
	r = w.mustOK(w.post("keys", nil), "keys")
	if !strings.Contains(r.msg("message"), "CA 指纹") || !strings.Contains(r.msg("message"), "master.key") {
		t.Errorf("keys 应给出 CA 指纹并提示单独备份主密钥: %s", r.msg("message"))
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.signing_keys WHERE status = 'active'"); n != 3 {
		t.Errorf("应有 3 个 active 密钥（config_signing/mtls_ca/grpc_server），实际 %d", n)
	}
	if v := w.dbStr("SELECT CAST(value_json AS CHAR) FROM §.system_settings WHERE setting_key = 'deploy.lineage'"); v != "1" {
		t.Errorf("deploy.lineage = %s", v)
	}

	// 8 完成：migrate 口令自动生成 → 一次性返回；响应 no-store。
	r = w.post("complete", nil)
	w.mustOK(r, "complete")
	if cc := r.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("complete 响应必须 Cache-Control: no-store，实际 %q", cc)
	}
	migPW, _ := r.secrets()["migrate_password"].(string)
	if len(migPW) < 32 {
		t.Fatalf("应一次性返回自动生成的迁移口令: %s", r.Raw)
	}
	w.secrets = append(w.secrets, migPW)
	w.bodies[len(w.bodies)-1] = strings.ReplaceAll(w.bodies[len(w.bodies)-1], migPW, "<ONE_TIME>")
	if !strings.Contains(r.msg("message"), "migrate credential init") {
		t.Errorf("complete 应给出 credential init 提示: %s", r.msg("message"))
	}
	// 该口令确为 lingedge_migrate 的真实口令（可连接、可管理授权），且只存在于这一个响应里。
	mig := w.userDB("lingedge_migrate", migPW)
	if err := mig.Ping(); err != nil {
		t.Errorf("返回的迁移口令应可登录: %v", err)
	}

	// ---- 完成后的不变量 ----
	if _, err := os.Stat(w.lockFile); err != nil {
		t.Errorf("install.lock 应已写入: %v", err)
	}
	if _, err := os.Stat(w.hashFile); !os.IsNotExist(err) {
		t.Errorf("setup token 哈希文件应已删除: %v", err)
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.system_installations WHERE install_locked_at IS NOT NULL AND status = 'installed'"); n != 1 {
		t.Errorf("system_installations 应有 1 个已完成记录，实际 %d", n)
	}
	if !w.svc.Window().Closed() {
		t.Error("complete 后 migrate 窗口必须关闭")
	}
	if _, _, err := w.svc.Window().OpenRunner(context.Background(), dbConfigFor(w, migPW), migrationsFS()); err == nil {
		t.Error("migrate 窗口关闭后不得再构造 migrate 池")
	} else if err != migrate.ErrWindowClosed {
		t.Errorf("期望 ErrWindowClosed，实际 %v", err)
	}
	select {
	case <-w.svc.Done():
	case <-time.After(5 * time.Second):
		t.Error("complete 后 Done 应已关闭")
	}
	// 永久 410：全部 9 个 operation，带/不带 token 都是 410（不泄漏任何信息）。
	for _, op := range w.allOps() {
		for _, tok := range []string{w.token, "", "wrong"} {
			rr := w.do(op.method, api+op.path, tok, op.body)
			if rr.Status != 410 || rr.errCode() != "setup.already_installed" {
				t.Errorf("%s %s（token=%q）应 410 setup.already_installed，实际 %d %s", op.method, op.path, tok, rr.Status, rr.Raw)
			}
		}
	}
	// 审计：每个向导步骤一行；Actor 是系统组件 setup；其余业务动作（用户创建/TOTP 等）也在。
	for a, want := range map[string]int64{"setup.checks": 1, "setup.database": 1, "setup.redis": 1, "setup.initialize": 1, "setup.system": 1, "setup.keys": 1, "setup.complete": 1, "system.signing_keys.init": 1} {
		if n := w.dbInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = ? AND actor_type = 'system' AND actor_label = 'setup'", a); n != want {
			t.Errorf("审计 %s 应 %d 行，实际 %d", a, want, n)
		}
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'setup.admin'"); n < 3 {
		t.Errorf("setup.admin 应至少 3 行（创建、TOTP 待确认、TOTP 启用），实际 %d", n)
	}
	// 审计里的数据库步骤只记「模式/账号已创建/口令是否自动生成」。
	cs := w.dbStr("SELECT changes_json FROM §.audit_logs WHERE action = 'setup.database'")
	for _, must := range []string{"create_accounts", "accounts_created", "migrate_generated"} {
		if !strings.Contains(cs, must) {
			t.Errorf("setup.database 审计应含 %s: %s", must, cs)
		}
	}
	// 向导状态不含任何机密（只有步骤布尔/版本号/公开配置）。
	if st := w.stateJSON(); !strings.Contains(st, `"init_db"`) || strings.Contains(st, "assword") {
		t.Errorf("state 内容异常: %s", st)
	}
	w.assertCredFile(t)
	w.assertNoSecrets()

	// 运行期凭据文件可被解密，且口令正确。
	f, err := dbcred.Load(w.credFile)
	if err != nil {
		t.Fatal(err)
	}
	if pw, err := f.AppPassword(context.Background(), w.box); err != nil || pw.Reveal() != w.appPW {
		t.Errorf("app 口令往返失败: %v", err)
	}
	if pw, ok, err := f.MaintPassword(context.Background(), w.box); err != nil || !ok || len(pw.Reveal()) < 32 {
		t.Errorf("maint 口令往返失败（向导自动生成的 maint 口令应被写入）: %v", err)
	}

	// 安装完成后正常模式可用：同一份配置（无任何显式 DB/Redis 口令）经运行期凭据文件合并，登录刚创建的管理员（含 TOTP）。
	w.normalModeLogin(t, totpSecret)
}

// 取得向导里 maint 口令：自动生成时测试并不知道它；此处以凭据文件为准更新 w.maintPW，使后续扫描覆盖它。
func (w *wiz) learnGeneratedSecrets(t *testing.T) {
	t.Helper()
	f, err := dbcred.Load(w.credFile)
	if err != nil {
		t.Fatal(err)
	}
	if pw, ok, err := f.MaintPassword(context.Background(), w.box); err == nil && ok {
		w.secrets = append(w.secrets, pw.Reveal())
	}
}

func (w *wiz) assertCredFile(t *testing.T) {
	t.Helper()
	st, err := os.Lstat(w.credFile)
	if err != nil {
		t.Fatalf("runtime-credentials.json 应已写入: %v", err)
	}
	if st.Mode().Perm() != 0o600 || !st.Mode().IsRegular() {
		t.Errorf("runtime-credentials.json 权限 = %v，应为普通文件 0600", st.Mode())
	}
	if ds, err := os.Stat(filepath.Dir(w.credFile)); err != nil || ds.Mode().Perm() != 0o700 {
		t.Errorf("secrets 目录应为 0700: %v %v", ds, err)
	}
	raw, _ := os.ReadFile(w.credFile)
	for _, s := range []string{w.appPW, w.maintPW, w.migPW, w.adminPW, w.admPW, w.token} {
		if s != "" && bytes.Contains(raw, []byte(s)) {
			t.Errorf("runtime-credentials.json 含明文机密")
		}
	}
	if !bytes.Contains(raw, []byte(`"password_enc": "v1:`)) {
		t.Errorf("口令字段应为 secretbox 五段密文: %s", raw)
	}
	w.learnGeneratedSecrets(t)
}

// 三类账号的真实权限：app 对 audit_logs 仅 INSERT/SELECT；maint 仅分区表 SELECT/ALTER/DROP；两者都不能 DROP 其他表/读其他表（maint）。
func (w *wiz) assertAccountPrivileges(t *testing.T) {
	t.Helper()
	appPW := w.appPW
	app := w.userDB("lingedge_app", appPW)
	denied := func(who, what string, err error, wantNum string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s 不应能执行 %s", who, what)
			return
		}
		if !strings.Contains(err.Error(), wantNum) {
			t.Errorf("%s 执行 %s 应被拒绝（%s），实际错误: %v", who, what, wantNum, err)
		}
	}
	if _, err := app.Exec("SELECT COUNT(*) FROM audit_logs"); err != nil {
		t.Errorf("app 应能 SELECT audit_logs: %v", err)
	}
	_, err := app.Exec("UPDATE audit_logs SET result = 'success' WHERE id = 0")
	denied("lingedge_app", "UPDATE audit_logs", err, "1142")
	_, err = app.Exec("DELETE FROM audit_logs WHERE id = 0")
	denied("lingedge_app", "DELETE audit_logs", err, "1142")
	_, err = app.Exec("DROP TABLE users")
	denied("lingedge_app", "DROP TABLE users", err, "1142")
	_, err = app.Exec("ALTER TABLE audit_logs COMMENT = 'x'")
	denied("lingedge_app", "ALTER audit_logs", err, "1142")
	if _, err := app.Exec("UPDATE system_settings SET updated_at = updated_at WHERE id = 0"); err != nil {
		t.Errorf("app 应能 UPDATE system_settings: %v", err)
	}
	maintPW := w.maintPWFromFile(t)
	maint := w.userDB("lingedge_maint", maintPW)
	if _, err := maint.Exec("SELECT COUNT(*) FROM audit_logs"); err != nil {
		t.Errorf("maint 应能 SELECT 分区表 audit_logs: %v", err)
	}
	if _, err := maint.Exec("ALTER TABLE audit_logs COMMENT = 'maint-ok'"); err != nil {
		t.Errorf("maint 应能 ALTER 分区表 audit_logs: %v", err)
	}
	_, err = maint.Exec("DROP TABLE users")
	denied("lingedge_maint", "DROP TABLE users", err, "1142")
	_, err = maint.Exec("SELECT COUNT(*) FROM users")
	denied("lingedge_maint", "SELECT users", err, "1142")
	_, err = maint.Exec("INSERT INTO audit_logs (id) VALUES (0)")
	denied("lingedge_maint", "INSERT audit_logs", err, "1142")
}

func (w *wiz) maintPWFromFile(t *testing.T) string {
	t.Helper()
	f, err := dbcred.Load(w.credFile)
	if err != nil {
		t.Fatal(err)
	}
	pw, ok, err := f.MaintPassword(context.Background(), w.box)
	if err != nil || !ok {
		t.Fatalf("maint 口令: %v %v", ok, err)
	}
	return pw.Reveal()
}

type opCase struct {
	method, path string
	body         any
}

func (w *wiz) allOps() []opCase {
	return []opCase{{"GET", "status", nil}, {"POST", "checks", nil}, {"PUT", "database", w.dbBody()}, {"PUT", "redis", w.redisBody()}, {"POST", "initialize", nil},
		{"POST", "admin", w.adminBody()}, {"PUT", "system", w.systemBody()}, {"POST", "keys", nil}, {"POST", "complete", nil}}
}

// normalModeLogin：向导完成后，Server 以同一份 server.yaml 等价配置进入正常模式（运行期凭据合并），刚创建的管理员（含 TOTP）可登录。
func (w *wiz) normalModeLogin(t *testing.T, totpSecret string) {
	t.Helper()
	cfg := *w.cfg
	cfg.HTTP.Listen, cfg.Metrics.Listen = freeAddr(t), freeAddr(t)
	cfg.GRPC.Listen, cfg.GRPC.EnrollListen = freeAddr(t), freeAddr(t)
	if cfg.Database.Password.IsSet() || cfg.Database.Host != "" || cfg.Redis.Addr != "" {
		t.Fatal("测试前提：配置里没有任何显式数据库/Redis 配置（全部来自运行期凭据文件）")
	}
	lg, err := logx.New(logx.Options{Level: "info", Format: "json", Output: w.logs})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := server.ParseRoles("api")
	asm := assembly.New()
	ready := make(chan server.Info, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, server.Options{Config: &cfg, Roles: rs, Logger: lg, Clock: clock.System(), Register: asm.Register, BuildHooks: asm.BuildHooks, OnReady: func(i server.Info) { ready <- i }})
	}()
	var info server.Info
	select {
	case info = <-ready:
	case err := <-done:
		t.Fatalf("正常模式启动失败: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("正常模式启动超时")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("正常模式优雅停机应无错误: %v", err)
		}
	})
	base := "http://" + info.HTTPAddr + "/api/v1"
	post := func(path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		rr, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rr.Body.Close() }()
		var m map[string]any
		_ = json.NewDecoder(rr.Body).Decode(&m)
		return rr.StatusCode, m
	}
	// 错误口令被拒；正确口令 → 需要 TOTP；TOTP 验证（下一个 time-step，规避重放保护）后登录成功。
	if code, _ := post("/auth/login", map[string]string{"realm": "admin", "email": w.admEmail, "password": w.admPW + "x"}); code != 401 {
		t.Errorf("错误口令应 401: %d", code)
	}
	code, m := post("/auth/login", map[string]string{"realm": "admin", "email": w.admEmail, "password": w.admPW})
	if code != 200 || m["status"] != "totp_required" {
		t.Fatalf("应进入 TOTP 第二步: %d %v", code, m)
	}
	otp, err := totp.GenerateCode(totpSecret, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	code, m = post("/auth/totp/verify", map[string]string{"mfa_token": m["mfa_token"].(string), "code": otp})
	if code != 200 {
		t.Fatalf("TOTP 验证后应登录成功: %d %v", code, m)
	}
	// 正常模式（真实认证/审计钩子链）下 9 个向导 operation 永久 410：带/不带 token 一样，写 operation 不会因审计钩子而变成 500。
	for _, op := range w.allOps() {
		for _, tok := range []string{"", w.token} {
			req, _ := http.NewRequest(op.method, base+"/public/setup/"+op.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			if tok != "" {
				req.Header.Set("X-Setup-Token", tok)
			}
			rr, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = rr.Body.Close()
			if rr.StatusCode != 410 {
				t.Errorf("正常模式下 %s /public/setup/%s 应 410，实际 %d", op.method, op.path, rr.StatusCode)
			}
		}
	}
	// 正常模式不再有向导页：/setup 与 /setup/*（含静态资源路径）统一 410 setup.already_installed（Story 1.37、MIN-04），不会 503/404。
	for _, p := range []string{"/setup", "/setup/", "/setup/assets/index.js"} {
		rr, err := http.Get("http://" + info.HTTPAddr + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = rr.Body.Close()
		if rr.StatusCode != 410 {
			t.Errorf("正常模式下 %s 应 410，实际 %d", p, rr.StatusCode)
		}
	}
}
