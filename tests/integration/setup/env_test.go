package setupit

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/cmd/lingedge-server/assembly"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/setup"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/pkg/setuptoken"
	"github.com/wuekevin/lingedge/tests/apistack"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

const migrationCount = 18

func rnd(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func ctxT(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// wiz 是一个用例的独立向导环境。
type wiz struct {
	t        *testing.T
	inst     *mysqltest.Instance
	rds      *redistest.Instance
	dir      string
	dataDir  string
	keyFile  string
	hashFile string
	lockFile string
	credFile string
	cfg      *config.Config
	box      *secretbox.Box
	kp       *secretbox.FileKeyProvider
	token    string
	logs     *apistack.SyncBuf
	adm      *sql.DB

	adminUser, adminPW string // 临时数据库管理员
	dbName             string
	appPW              string
	maintPW            string
	migPW              string
	redisPW            string // 本环境 Redis 无口令；个别用例单独起带口令的实例
	admEmail, admPW    string // 向导创建的 Web 管理员

	secrets []string // 本用例里出现过的全部已知机密（日志/响应/审计/state/凭据文件扫描用）
	bodies  []string // 记录的全部 HTTP 响应体（已去掉「按设计只展示一次」的值）

	svc *setup.Service
	srv *httptest.Server
	// baseURL 是向导 HTTP 服务的根（进程内 httptest 或真实二进制）。
	baseURL string
	opts    []func(*setup.Options)
}

func newWiz(t *testing.T, opts ...func(*setup.Options)) *wiz {
	t.Helper()
	return newWizWith(t, mysqltest.Options{}, opts...)
}

// newWizWith 允许选择临时 mysqld 的形态（binlog/trust 组合；每种形态一个独立实例，用后清理）。
func newWizWith(t *testing.T, mo mysqltest.Options, opts ...func(*setup.Options)) *wiz {
	t.Helper()
	inst := mysqltest.Start(t, mo)
	w := &wiz{t: t, inst: inst, rds: redistest.Start(t), logs: &apistack.SyncBuf{}, opts: opts}
	w.adm = inst.Admin(t)
	w.cleanAccounts()
	t.Cleanup(w.cleanAccounts)
	var err error
	w.dir, err = os.MkdirTemp("", "lesetup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(w.dir) })
	w.dataDir = filepath.Join(w.dir, "data")
	if err := os.MkdirAll(w.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	w.keyFile = filepath.Join(w.dir, "master.key")
	w.writeKey(0o400)
	w.hashFile = filepath.Join(w.dir, "setup-token.hash")
	w.lockFile = filepath.Join(w.dataDir, setup.LockFileName)
	w.credFile = filepath.Join(w.dataDir, "secrets", "runtime-credentials.json")
	tok, hash, err := setuptoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	w.token = tok
	if err := setuptoken.WriteHashFile(w.hashFile, hash, 0o600); err != nil {
		t.Fatal(err)
	}
	w.secrets = append(w.secrets, tok)

	w.dbName = "lingedge_w_" + rnd(4)
	t.Cleanup(func() { _, _ = w.adm.Exec("DROP DATABASE IF EXISTS `" + w.dbName + "`") })
	w.adminUser, w.adminPW = "le_adm_"+rnd(4), rnd(12)
	w.exec(fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", w.adminUser, w.adminPW))
	w.exec(fmt.Sprintf("GRANT ALL ON *.* TO '%s'@'%%' WITH GRANT OPTION", w.adminUser))
	t.Cleanup(func() { _, _ = w.adm.Exec("DROP USER IF EXISTS '" + w.adminUser + "'@'%'") })
	w.appPW, w.maintPW, w.migPW = rnd(16), rnd(16), rnd(16)
	w.admEmail, w.admPW = "admin"+rnd(3)+"@example.test", "Zq"+rnd(10)+"!x"
	w.secrets = append(w.secrets, w.adminPW, w.appPW, w.maintPW, w.migPW, w.admPW)

	cfg := config.Defaults()
	cfg.Server.Env, cfg.Server.DataDir = "test", w.dataDir
	cfg.HTTP.Listen = "127.0.0.1:8080"
	cfg.Secrets.MasterKeyFile = w.keyFile
	cfg.Auth.Argon2 = config.Argon2{MemoryKiB: config.MinArgon2MemoryKiB, Iterations: config.MinArgon2Iterations, Parallelism: 1}
	w.cfg = &cfg
	w.openBox()
	t.Cleanup(w.kp.Close)
	w.start()
	t.Cleanup(w.stop) // 先于临时 Redis/MySQL 的清理执行（LIFO）
	return w
}

func (w *wiz) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.adm.Exec(q, args...); err != nil {
		w.t.Fatalf("root 执行失败: %v\n%s", err, strings.SplitN(q, " IDENTIFIED", 2)[0])
	}
}

func (w *wiz) cleanAccounts() {
	for _, u := range []string{"lingedge_app", "lingedge_maint", "lingedge_migrate"} {
		_, _ = w.adm.Exec("DROP USER IF EXISTS '" + u + "'@'%'")
	}
}

func (w *wiz) writeKey(mode os.FileMode) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		w.t.Fatal(err)
	}
	_ = os.Remove(w.keyFile)
	if err := os.WriteFile(w.keyFile, []byte("k1:"+base64.RawURLEncoding.EncodeToString(b)+"\n"), mode); err != nil {
		w.t.Fatal(err)
	}
	if err := os.Chmod(w.keyFile, mode); err != nil {
		w.t.Fatal(err)
	}
}

func (w *wiz) openBox() {
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: w.keyFile})
	if err != nil {
		w.t.Fatalf("主密钥: %v", err)
	}
	w.kp = kp
	if w.box, err = secretbox.New(kp); err != nil {
		w.t.Fatal(err)
	}
}

// start（重新）创建 Service 与 HTTP 服务：模拟「杀进程再续」——内存中的凭据与缓冲全部丢失，只剩磁盘与数据库里的状态。
func (w *wiz) start() {
	w.t.Helper()
	w.stop()
	lg, err := logx.New(logx.Options{Level: "debug", Format: "json", Output: w.logs})
	if err != nil {
		w.t.Fatal(err)
	}
	o := setup.Options{Config: w.cfg, Box: w.box, Logger: lg, Clock: clock.System(), IDs: uid.NewGenerator(clock.System()), Migrations: migrations.FS,
		NewAdmin: assembly.NewSetupAdmin, TokenHashPath: w.hashFile, LockPath: w.lockFile}
	for _, f := range w.opts {
		f(&o)
	}
	w.svc, err = setup.New(o)
	if err != nil {
		w.t.Fatal(err)
	}
	h, err := w.svc.BuildHandler(lg, clock.System(), uid.NewGenerator(clock.System()))
	if err != nil {
		w.t.Fatal(err)
	}
	w.srv = httptest.NewServer(h)
	w.baseURL = w.srv.URL
}

func (w *wiz) stop() {
	if w.srv != nil {
		w.srv.Close()
		w.srv = nil
	}
	if w.svc != nil {
		w.svc.Close()
		w.svc = nil
	}
}

// ---- HTTP ----

type resp struct {
	Status int
	Header http.Header
	Raw    string
	JSON   map[string]any
}

func (r resp) errCode() string {
	if e, ok := r.JSON["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func (r resp) msg(key string) string {
	s, _ := r.JSON[key].(string)
	return s
}

func (r resp) secrets() map[string]any {
	m, _ := r.JSON["one_time_secrets"].(map[string]any)
	return m
}

// do 发起请求并记录响应体；allowed 是该响应按设计允许出现的一次性机密（记录前替换，不参与泄漏扫描）。
func (w *wiz) do(method, path, token string, body any, allowed ...string) resp {
	w.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			w.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.baseURL+path, rd)
	if err != nil {
		w.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-Setup-Token", token)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	b, _ := io.ReadAll(r.Body)
	out := resp{Status: r.StatusCode, Header: r.Header, Raw: string(b)}
	_ = json.Unmarshal(b, &out.JSON)
	rec := out.Raw
	for _, a := range allowed {
		if a != "" {
			rec = strings.ReplaceAll(rec, a, "<ONE_TIME>")
		}
	}
	w.bodies = append(w.bodies, rec)
	return out
}

const api = "/api/v1/public/setup/"

func (w *wiz) get(path string) resp { return w.do("GET", api+path, w.token, nil) }
func (w *wiz) post(path string, body any, allowed ...string) resp {
	return w.do("POST", api+path, w.token, body, allowed...)
}
func (w *wiz) put(path string, body any, allowed ...string) resp {
	return w.do("PUT", api+path, w.token, body, allowed...)
}

func (w *wiz) mustOK(r resp, what string) resp {
	w.t.Helper()
	if r.Status != 200 {
		w.t.Fatalf("%s: 期望 200，实际 %d：%s", what, r.Status, r.Raw)
	}
	return r
}

// ---- 各步骤的标准请求体 ----

func (w *wiz) dbBody() map[string]any {
	return map[string]any{"host": w.inst.Host(), "port": w.inst.Port, "database": w.dbName, "username": "lingedge_app", "password": w.appPW,
		"create_if_missing": true, "admin_username": w.adminUser, "admin_password": w.adminPW, "migrate_password": w.migPW, "maint_password": w.maintPW}
}

func (w *wiz) redisBody() map[string]any {
	return map[string]any{"address": w.rds.Addr(), "db": 0}
}

func (w *wiz) adminBody() map[string]any {
	return map[string]any{"email": w.admEmail, "display_name": "Root Admin", "password": w.admPW}
}

func (w *wiz) systemBody() map[string]any {
	return map[string]any{"run_mode": "self_hosted", "base_url": "https://edge.example.test", "timezone": "Asia/Shanghai", "default_locale": "zh-CN",
		"brand": map[string]any{"name": "测试云", "name_en": "TestCloud"}}
}

// runThrough 依次执行到（含）某一步；steps 顺序：database redis init admin system keys complete。
func (w *wiz) runThrough(last string) resp {
	w.t.Helper()
	order := []string{"checks", "database", "redis", "init", "admin", "system", "keys", "complete"}
	var r resp
	for _, st := range order {
		switch st {
		case "checks":
			r = w.mustOK(w.post("checks", nil), "checks")
		case "database":
			r = w.mustOK(w.put("database", w.dbBody()), "database")
		case "redis":
			r = w.mustOK(w.put("redis", w.redisBody()), "redis")
		case "init":
			r = w.mustOK(w.post("initialize", nil), "initialize")
		case "admin":
			r = w.mustOK(w.post("admin", w.adminBody()), "admin")
		case "system":
			r = w.mustOK(w.put("system", w.systemBody()), "system")
		case "keys":
			r = w.mustOK(w.post("keys", nil), "keys")
		case "complete":
			r = w.mustOK(w.post("complete", nil, w.migPW), "complete")
		}
		if st == last {
			return r
		}
	}
	return r
}

// ---- 数据库探查（以测试实例 root，仅用于断言） ----

func (w *wiz) dbInt(q string, args ...any) int64 {
	w.t.Helper()
	var n sql.NullInt64
	if err := w.adm.QueryRow(strings.ReplaceAll(q, "§", "`"+w.dbName+"`"), args...).Scan(&n); err != nil {
		w.t.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return n.Int64
}

func (w *wiz) dbStr(q string, args ...any) string {
	w.t.Helper()
	var s sql.NullString
	if err := w.adm.QueryRow(strings.ReplaceAll(q, "§", "`"+w.dbName+"`"), args...).Scan(&s); err != nil {
		w.t.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return s.String
}

// stateJSON 返回向导行的 meta_json。
func (w *wiz) stateJSON() string {
	return w.dbStr("SELECT meta_json FROM §.system_installations WHERE component = 'server' AND instance_name = 'default'")
}

// dumpAudit 返回 audit_logs 全部行的全文（每行把所有列拼成一个字符串），用于泄漏扫描。
func (w *wiz) dumpAudit() string {
	w.t.Helper()
	rows, err := w.adm.Query(strings.ReplaceAll("SELECT CONCAT_WS('|', action, resource_type, IFNULL(resource_uid,''), result, IFNULL(changes_json,''), IFNULL(meta_json,''), actor_label) FROM §.audit_logs", "§", "`"+w.dbName+"`"))
	if err != nil {
		w.t.Fatalf("读取审计: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var sb strings.Builder
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			w.t.Fatal(err)
		}
		sb.WriteString(s.String + "\n")
	}
	return sb.String()
}

// assertNoSecrets 是 G-5/ADR-026 §10 的全量扫描：日志、全部 HTTP 响应体、审计行、向导状态、运行期凭据文件转储里都不得出现任何已知机密。
func (w *wiz) assertNoSecrets(extra ...string) {
	w.t.Helper()
	secrets := append(append([]string(nil), w.secrets...), extra...)
	check := func(where, text string) {
		for _, s := range secrets {
			if s != "" && strings.Contains(text, s) {
				w.t.Errorf("%s 中出现了已知机密（长度 %d，前缀 %q…）", where, len(s), s[:min(3, len(s))])
			}
		}
	}
	if w.logs.String() == "" || len(w.bodies) == 0 {
		w.t.Fatal("扫描对象为空：日志或响应体没有记录，扫描会空转通过")
	}
	check("日志", w.logs.String())
	for i, b := range w.bodies {
		check(fmt.Sprintf("第 %d 个 HTTP 响应体", i+1), b)
	}
	if b, err := os.ReadFile(w.credFile); err == nil {
		check("runtime-credentials.json", string(b))
	}
	if w.dbInt("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'audit_logs'", w.dbName) > 0 {
		check("audit_logs", w.dumpAudit())
		check("system_installations.meta_json", w.stateJSON())
	}
}

// userDB 以某账号连接目标库（真实账号权限断言用）。
func (w *wiz) userDB(user, pw string) *sql.DB {
	w.t.Helper()
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&multiStatements=false", user, pw, w.inst.Host(), w.inst.Port, w.dbName)
	d, err := sql.Open("mysql", dsn)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = d.Close() })
	return d
}

func dbConfigFor(w *wiz, migPW string) db.Config {
	return db.Config{Host: w.inst.Host(), Port: w.inst.Port, Name: w.dbName, User: "lingedge_migrate", Password: config.NewSecret(migPW)}
}

func migrationsFS() fs.FS { return migrations.FS }

func freeAddr(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func redisAddr(t testing.TB) string { return redistest.Start(t).Addr() }

func removeFile(p string) error { return os.Remove(p) }

// assertBodyHasNoSecrets：单个响应不含任何已知机密。
func (w *wiz) assertBodyHasNoSecrets(r resp) {
	w.t.Helper()
	for _, s := range w.secrets {
		if s != "" && strings.Contains(r.Raw, s) {
			w.t.Errorf("响应体含已知机密")
		}
	}
}

func (w *wiz) credFileStat() (os.FileInfo, error) { return os.Lstat(w.credFile) }

func (w *wiz) readCred(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.credFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
