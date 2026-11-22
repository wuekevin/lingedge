package setupit

import (
	"context"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
)

// socketBody 把标准请求体的 TCP 目标换成 Unix 套接字（CC-13）。
func (w *wiz) socketBody(body map[string]any) map[string]any {
	delete(body, "host")
	delete(body, "port")
	body["socket"] = w.inst.Socket
	return body
}

// 建账号模式全向导经 Unix socket（CC-13）：迁移明细随 initialize 返回；凭据文件含 socket 且无明文；
// 完成后以「合并后的配置」经 socket 连上 app 与 maint 池；全程无机密泄漏。
func TestFullWizardOverUnixSocket(t *testing.T) {
	w := newWiz(t)
	if len(w.inst.Socket) > 100 {
		t.Skipf("socket 路径过长（%d），超出向导允许的 100 字节", len(w.inst.Socket))
	}
	w.mustOK(w.post("checks", nil), "checks")
	body := w.socketBody(w.dbBody())
	delete(body, "migrate_password")
	delete(body, "maint_password")
	w.mustOK(w.put("database", body), "database(socket)")
	raw := w.readCred(t)
	if !strings.Contains(raw, `"socket": "`+w.inst.Socket+`"`) || strings.Contains(raw, `"host"`) || strings.Contains(raw, `"port"`) {
		t.Errorf("凭据文件应只含 socket 目标: %s", raw)
	}
	w.assertCredFile(t)
	w.mustOK(w.put("redis", w.redisBody()), "redis")
	r := w.mustOK(w.post("initialize", nil), "initialize")
	ms, ok := r.JSON["migrations"].([]any)
	if !ok || len(ms) != migrationCount {
		t.Fatalf("initialize 应返回 %d 条迁移明细: %s", migrationCount, r.Raw)
	}
	first := ms[0].(map[string]any)
	if first["version"].(float64) != 1 || first["name"] == "" || first["category"] == "" {
		t.Errorf("迁移明细字段不全: %v", first)
	}
	for _, m := range ms {
		switch m.(map[string]any)["category"] {
		case "reversible", "expand", "irreversible":
		default:
			t.Errorf("迁移分类非法: %v", m)
		}
	}
	w.mustOK(w.post("admin", w.adminBody()), "admin")
	w.mustOK(w.put("system", w.systemBody()), "system")
	w.mustOK(w.post("keys", nil), "keys")
	r = w.mustOK(w.post("complete", nil, w.migPW), "complete")
	if sec := r.secrets(); sec != nil {
		if mp, _ := sec["migrate_password"].(string); mp != "" {
			w.secrets = append(w.secrets, mp)
			w.bodies[len(w.bodies)-1] = strings.ReplaceAll(w.bodies[len(w.bodies)-1], mp, "<ONE_TIME>")
		}
	}

	// 合并后的配置经 socket 连上 app/maint 池（Server 运行期的真实路径）。
	cfg := *w.cfg
	if _, err := dbcred.Merge(context.Background(), &cfg, w.box, dbcred.MergeOptions{NeedMaint: true}); err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Socket != w.inst.Socket || cfg.Database.Host != "" {
		t.Fatalf("合并结果应取 socket 目标: %+v", cfg.Database)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	app, err := db.OpenApp(ctx, db.AppConfigFrom(cfg.Database))
	if err != nil {
		t.Fatalf("app 池经 socket 连接失败: %v", err)
	}
	defer func() { _ = app.Close() }()
	mc, ok := db.MaintConfigFrom(cfg.Database)
	if !ok {
		t.Fatal("maint 应已配置")
	}
	maint, err := db.OpenMaint(ctx, mc)
	if err != nil {
		t.Fatalf("maint 池经 socket 连接失败: %v", err)
	}
	_ = maint.Close()
	// 合并前后的口令不是明文落盘的（再次确认）。
	if b, _ := os.ReadFile(w.credFile); strings.Contains(string(b), cfg.Database.Password.Reveal()) {
		t.Error("凭据文件含 app 明文口令")
	}
	w.assertNoSecrets()
}

// 预建账号模式经 socket：续做时同目标判定也认 socket（二次提交视为同一目标）。
func TestPrebuiltAccountsOverUnixSocket(t *testing.T) {
	w := newWiz(t)
	w.dbaPrepare(true, true)
	w.mustOK(w.put("database", w.socketBody(w.prebuiltBody())), "database(预建+socket)")
	w.mustOK(w.put("database", w.socketBody(w.prebuiltBody())), "database(重复提交)")
	w.mustOK(w.put("redis", w.redisBody()), "redis")
	r := w.mustOK(w.post("initialize", nil), "initialize")
	if ms, ok := r.JSON["migrations"].([]any); !ok || len(ms) != migrationCount {
		t.Errorf("迁移明细: %s", r.Raw)
	}
	// 数据库已是最新时再次 initialize：migrations 键存在且为空数组（而非省略）。
	// 迁移口令用毕即清零：再次初始化须重新提供（续做语义），已是最新时迁移明细为空数组。
	w.mustOK(w.put("database", w.socketBody(w.prebuiltBody())), "database(重新提供迁移口令)")
	r = w.mustOK(w.post("initialize", nil), "initialize(幂等)")
	ms, ok := r.JSON["migrations"].([]any)
	if !ok || len(ms) != 0 {
		t.Errorf("已是最新时应返回空数组: %s", r.Raw)
	}
	w.assertNoSecrets()
}

// 请求校验：host 与 socket 同设、都不设、相对路径、含 TLS、socket+port 都是 400 validation.failed，且不创建账号、不写凭据文件。
func TestSocketRequestValidation(t *testing.T) {
	w := newWiz(t)
	cases := map[string]func(b map[string]any){
		"host 与 socket 同设": func(b map[string]any) { b["socket"] = w.inst.Socket },
		"socket 与 port 同设": func(b map[string]any) { delete(b, "host"); b["socket"] = w.inst.Socket },
		"都不设":              func(b map[string]any) { delete(b, "host"); delete(b, "port") },
		"相对路径":             func(b map[string]any) { w.socketBody(b); b["socket"] = "mysql.sock" },
		"含 .. 的路径":         func(b map[string]any) { w.socketBody(b); b["socket"] = "/tmp/../tmp/x.sock" },
		"socket 加 tls":     func(b map[string]any) { w.socketBody(b); b["tls"] = true },
	}
	for name, mut := range cases {
		b := w.dbBody()
		mut(b)
		r := w.put("database", b)
		if r.Status != 400 || r.errCode() != "validation.failed" {
			t.Errorf("%s: 应 400 validation.failed，实际 %d %s", name, r.Status, r.Raw)
		}
	}
	if n := w.dbInt("SELECT COUNT(*) FROM mysql.user WHERE User LIKE 'lingedge\\_%'"); n != 0 {
		t.Errorf("校验失败不得创建账号: %d", n)
	}
	if _, err := w.credFileStat(); err == nil {
		t.Error("校验失败不得写凭据文件")
	}
}

// 不存在的 socket：setup.precheck_failed，口令不泄漏，不写凭据文件。
func TestSocketMissingFile(t *testing.T) {
	w := newWiz(t)
	b := w.socketBody(w.dbBody())
	b["socket"] = w.dir + "/nope.sock"
	r := w.put("database", b)
	if r.Status != 400 || r.errCode() != "setup.precheck_failed" {
		t.Fatalf("应 400 setup.precheck_failed: %d %s", r.Status, r.Raw)
	}
	w.assertBodyHasNoSecrets(r)
	if _, err := w.credFileStat(); err == nil {
		t.Error("失败不得写凭据文件")
	}
}

// db credential rotate 的连接目标取自 server.yaml 的 database.socket（无 --host）：阶段一 RETAIN、阶段二 DISCARD 经套接字完成。
func TestDBCredentialRotateOverSocket(t *testing.T) {
	w := newWiz(t)
	body := w.socketBody(w.dbBody())
	w.mustOK(w.put("database", body), "database(socket)")
	cfgPath := filepath.Join(w.dir, "server-sock.yaml")
	y := "server:\n  env: test\n  data_dir: " + w.dataDir + "\nhttp:\n  listen: 127.0.0.1:8080\nlog:\n  level: info\n  format: json\nsecrets:\n  master_key_file: " + w.keyFile + "\ndatabase:\n  socket: " + w.inst.Socket + "\n"
	if err := os.WriteFile(cfgPath, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	env := cleanEnv(migrate.EnvAdminPass + "=" + w.adminPW)
	args := []string{"db", "credential", "rotate", "--role", "app", "--admin-user", w.adminUser, "--config", cfgPath}
	if so, se, code := runBin(t, env, args...); code != 0 {
		t.Fatalf("socket 阶段一失败: %d %s %s", code, so, se)
	}
	f, err := dbcred.Load(w.credFile)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := f.AppPassword(context.Background(), w.box)
	if err != nil || pw.Reveal() == w.appPW {
		t.Fatalf("凭据文件应换成新口令: %v", err)
	}
	w.secrets = append(w.secrets, pw.Reveal())
	if so, se, code := runBin(t, env, append(args, "--finish")...); code != 0 {
		t.Fatalf("socket --finish 失败: %d %s %s", code, so, se)
	}
	if err := w.userDB("lingedge_app", w.appPW).Ping(); err == nil {
		t.Error("--finish 后旧口令应失效")
	}
}
