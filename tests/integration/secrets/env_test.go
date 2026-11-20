package secretsintegration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

func ctxT(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// env 是一个用例的独立环境：随机库 + 全部迁移 + 真实 lingedge_app/lingedge_maint 账号。
type env struct {
	inst   *mysqltest.Instance
	adm    *sql.DB
	dbName string
	app    *db.AppPool
	maint  *db.MaintPool
	ids    *uid.Generator
	tenant string // 默认租户 id（种子）
}

func newEnv(t *testing.T) *env {
	t.Helper()
	inst := mysqltest.Start(t)
	e := &env{inst: inst, dbName: inst.NewDatabase(t), ids: uid.NewGenerator(clock.System())}
	appPW, maintPW, migPW := mysqltest.RandomSecret(t), mysqltest.RandomSecret(t), mysqltest.RandomSecret(t)
	migUser := mysqltest.RandomName("mig")
	// 迁移内 GRANT 的对象 lingedge_app/lingedge_maint 必须先存在（Story 1.7 待办③）；账号是实例级对象，用后删除。
	for _, a := range []struct{ u, p string }{{migrate.AppUser, appPW}, {migrate.MaintUser, maintPW}, {migUser, migPW}} {
		inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", a.u, a.p))
		inst.DropUserOnCleanup(t, a.u)
	}
	inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+migUser+"'@'%' WITH GRANT OPTION")
	inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+migUser+"'@'%'")

	cfg := func(user, pw string) db.Config {
		return db.Config{Host: inst.Host(), Port: inst.Port, Name: e.dbName, User: user, Password: config.NewSecret(pw), MaxOpenConns: 4, MaxIdleConns: 2}
	}
	mp, err := db.OpenMigrate(ctxT(t), cfg(migUser, migPW))
	if err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = mp.Close() })
	r, err := migrate.NewRunner(mp, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if f := r.Findings(); len(f) != 0 {
		t.Fatalf("真实迁移必须通过 lint: %v", f)
	}
	if _, err := r.Up(ctxT(t), migrate.UpOptions{}); err != nil {
		t.Fatalf("全部迁移 up: %v", err)
	}
	if e.app, err = db.OpenApp(ctxT(t), cfg(migrate.AppUser, appPW)); err != nil {
		t.Fatalf("OpenApp: %v", err)
	}
	t.Cleanup(func() { _ = e.app.Close() })
	if e.maint, err = db.OpenMaint(ctxT(t), cfg(migrate.MaintUser, maintPW)); err != nil {
		t.Fatalf("OpenMaint: %v", err)
	}
	t.Cleanup(func() { _ = e.maint.Close() })

	e.adm = inst.Admin(t)
	e.adm.SetMaxOpenConns(3)
	if err := e.adm.QueryRowContext(ctxT(t), "SELECT id FROM `"+e.dbName+"`.tenants ORDER BY id LIMIT 1").Scan(&e.tenant); err != nil {
		t.Fatalf("取默认租户: %v", err)
	}
	return e
}

// root 以 root 执行语句（造数/旁路篡改/核对，绕过被测路径）。
func (e *env) root(t testing.TB, q string, args ...any) {
	t.Helper()
	if _, err := e.adm.ExecContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...); err != nil {
		t.Fatalf("root 执行失败: %v\n%s", err, q)
	}
}

func (e *env) rootStr(t testing.TB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := e.adm.QueryRowContext(ctxT(t), strings.ReplaceAll(q, "§", "`"+e.dbName+"`"), args...).Scan(&s); err != nil {
		t.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return s.String
}

// seed 在四张目标表各写入记录：n 条/表，密文由 box 真实加密，AAD 绑定（表,列,uid）；明文 = "pt|<表>|<uid>"。
// 返回 uid→明文 的映射键 [表,列,uid]。
func (e *env) seed(t *testing.T, box *secretbox.Box, n int) map[[3]string]string {
	t.Helper()
	ctx := ctxT(t)
	want := map[[3]string]string{}
	kek := box.CurrentKeyID()
	enc := func(table, col string) (string, string) {
		u := e.ids.MustNew()
		pt := "pt|" + table + "|" + u
		c, err := box.Seal(ctx, []byte(pt), secretbox.RecordContext{Table: table, Column: col, UID: u})
		if err != nil {
			t.Fatal(err)
		}
		want[[3]string{table, col, u}] = pt
		return u, c
	}
	for i := 0; i < n; i++ {
		u, c := enc("users", "totp_secret_enc")
		e.root(t, "INSERT INTO §.users (uid, user_type, email, display_name, password_hash, totp_enabled, totp_secret_enc) VALUES (?, 'tenant', ?, 'u', 'x', 1, ?)", u, strings.ToLower(u)+"@example.test", c)

		u, c = enc("system_settings", "value_enc")
		e.root(t, "INSERT INTO §.system_settings (uid, setting_key, value_enc, group_name) VALUES (?, ?, ?, 'test')", u, "k."+strings.ToLower(u), c)

		u, c = enc("signing_keys", "private_key_enc")
		e.root(t, "INSERT INTO §.signing_keys (uid, purpose, key_id, algorithm, public_key, private_key_enc, enc_key_id) VALUES (?, 'config_signing', ?, 'ed25519', ?, ?, ?)", u, "kid-"+strings.ToLower(u[10:]), []byte("pub"), c, kek)

		u, c = enc("certificates", "private_key_enc")
		e.root(t, "INSERT INTO §.certificates (uid, tenant_id, name, source, primary_domain, domains_json, serial, issuer, not_before, not_after, fingerprint_sha256, key_algorithm, chain_pem, private_key_enc, enc_key_id) "+
			"VALUES (?, ?, 'c', 'upload', 'example.test', '[]', '01', 'ca', '2026-01-01 00:00:00', '2027-01-01 00:00:00', ?, 'ecdsa_p256', 'pem', ?, ?)", u, e.tenant, strings.Repeat("a", 64), c, kek)
	}
	return want
}

// readEnc 以 root 读取某记录的密文（不经被测路径）。
func (e *env) readEnc(t testing.TB, table, col, u string) string {
	t.Helper()
	return e.rootStr(t, "SELECT "+"`"+col+"`"+" FROM §.`"+table+"` WHERE uid = ?", u)
}

// provider 在临时目录写主密钥文件（0400）并返回真实 FileKeyProvider 与 Box。keys: kek_id→32B base64url，current 指定当前 KEK。
func newBox(t testing.TB, keys map[string]string, current string) *secretbox.Box {
	t.Helper()
	var ids []string
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("@current " + current + "\n")
	for _, id := range ids {
		b.WriteString(id + ":" + keys[id] + "\n")
	}
	p := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kp.Close)
	box, err := secretbox.New(kp)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func newKey(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
