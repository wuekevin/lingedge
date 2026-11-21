package migrationstest

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// ADR-026 §4（Story 1.24 AC12）：lingedge_migrate 的 root 侧凭据——来源优先级、`migrate credential init|reset`、连接目标的信任规则。真实 MySQL，无 Mock。

func mainRun(env []string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := migrate.Main(args, &out, &errb, env, migrations.FS)
	return code, out.String(), errb.String()
}

func writeMigrateYAML(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	_ = os.Remove(path)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func (e *env) yaml(pw string) string {
	return fmt.Sprintf("host: %s\nport: %d\ndatabase: %s\nuser: %s\npassword: %s\n", e.inst.Host(), e.inst.Port, e.dbName, e.user, pw)
}

// 口令来源优先级：环境变量 ＞ 口令文件 ＞ migrate.yaml；migrate.yaml 同时是连接目标来源（无需任何命令行选项）。
func TestMigrateCredentialSourcePriority(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	yml := filepath.Join(dir, "migrate.yaml")
	wrong := "wrong-" + mysqltest.RandomSecret(t)
	base := []string{migrate.EnvMigrateYAML + "=" + yml}

	// 只有 migrate.yaml（正确）：status 无需任何连接选项即可工作（目标与口令都取自该文件）。
	writeMigrateYAML(t, yml, e.yaml(e.pw), 0o400)
	if code, out, errs := mainRun(base, "status"); code != 0 || !strings.Contains(out, "已知最新") {
		t.Fatalf("migrate.yaml 应同时提供目标与口令: %d %s %s", code, out, errs)
	}
	// 环境变量口令优先于 migrate.yaml：文件里是错误口令、环境变量正确 → 成功；反过来文件正确、环境变量错误 → 失败（证明环境变量确实被使用）。
	writeMigrateYAML(t, yml, e.yaml(wrong), 0o400)
	if code, _, errs := mainRun(append(base, migrate.EnvDBPass+"="+e.pw), "status"); code != 0 {
		t.Fatalf("环境变量应优先于 migrate.yaml: %d %s", code, errs)
	}
	writeMigrateYAML(t, yml, e.yaml(e.pw), 0o400)
	if code, _, _ := mainRun(append(base, migrate.EnvDBPass+"="+wrong), "status"); code == 0 {
		t.Fatal("环境变量（错误口令）应覆盖文件里的正确口令并导致失败")
	}
	// 口令文件优先于 migrate.yaml。
	pwf := filepath.Join(dir, "mig.pw")
	if err := os.WriteFile(pwf, []byte(e.pw+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	writeMigrateYAML(t, yml, e.yaml(wrong), 0o400)
	if code, _, errs := mainRun(append(base, migrate.EnvDBPassFile+"="+pwf), "status"); code != 0 {
		t.Fatalf("口令文件应优先于 migrate.yaml: %d %s", code, errs)
	}
	// 命令行显式目标优先于 migrate.yaml 的目标（口令仍按来源优先级）。
	writeMigrateYAML(t, yml, strings.Replace(e.yaml(e.pw), e.dbName, "no_such_db", 1), 0o400)
	if code, _, errs := mainRun(base, "status", "--database", e.dbName); code != 0 {
		t.Fatalf("--database 应覆盖 migrate.yaml: %d %s", code, errs)
	}
	// 非交互且没有任何口令来源：退出码 3（前置条件）并说明如何提供。
	if code, _, errs := mainRun(nil, "status", "--host", e.inst.Host(), "--port", fmt.Sprint(e.inst.Port), "--database", e.dbName); code != migrate.ExitPrecondition ||
		!strings.Contains(errs, "credential init") || !strings.Contains(errs, migrate.EnvDBPass) {
		t.Fatalf("无口令来源应退出码 3 并说明: %d %s", code, errs)
	}
}

// migrate.yaml 的安全校验：权限过宽、符号链接一律拒绝；含未知键拒绝；口令不出现在任何输出里。
func TestMigrateYAMLValidation(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	yml := filepath.Join(dir, "migrate.yaml")
	base := []string{migrate.EnvMigrateYAML + "=" + yml}
	writeMigrateYAML(t, yml, e.yaml(e.pw), 0o644)
	if code, out, errs := mainRun(base, "status"); code == 0 || !strings.Contains(errs, "权限过宽") || strings.Contains(out+errs, e.pw) {
		t.Fatalf("0644 应被拒绝且不泄漏口令: %d %s", code, errs)
	}
	writeMigrateYAML(t, yml, e.yaml(e.pw)+"extra_key: 1\n", 0o400)
	if code, _, errs := mainRun(base, "status"); code == 0 || !strings.Contains(errs, "未知字段") {
		t.Fatalf("未知键应被拒绝: %d %s", code, errs)
	}
	real := filepath.Join(dir, "real.yaml")
	writeMigrateYAML(t, real, e.yaml(e.pw), 0o400)
	_ = os.Remove(yml)
	if err := os.Symlink(real, yml); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := mainRun(base, "status"); code == 0 || strings.Contains(errs, e.pw) {
		t.Fatalf("符号链接应被拒绝: %d %s", code, errs)
	}
	// 显式指定的文件不存在是错误（而不是静默回退）。
	if code, _, _ := mainRun([]string{migrate.EnvMigrateYAML + "=" + filepath.Join(dir, "absent.yaml")}, "status"); code == 0 {
		t.Fatal("显式 $LINGEDGE_MIGRATE_CONFIG 指向不存在的文件应失败")
	}
}

// `migrate credential init`：真实连接并预检，成功才写 0400 文件；失败不写；已存在拒绝覆盖（--force 才覆盖）；口令不输出。
func TestCredentialInit(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "migrate.yaml")
	args := []string{"credential", "init", "--file", out, "--host", e.inst.Host(), "--port", fmt.Sprint(e.inst.Port), "--database", e.dbName, "--user", e.user}
	// 错误口令：不写文件。
	wrong := "wrong-" + mysqltest.RandomSecret(t)
	if code, o, errs := mainRun([]string{migrate.EnvDBPass + "=" + wrong}, args...); code == 0 || strings.Contains(o+errs, wrong) {
		t.Fatalf("错误口令应失败且不回显: %d %s", code, errs)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("连接失败不得写文件")
	}
	// 迁移账号缺少全局 SET_USER_ID（binlog=ON、trust=0）：预检失败（退出码 8），不写文件。
	noSUID := newEnvWith(t, false)
	args2 := []string{"credential", "init", "--file", out, "--host", noSUID.inst.Host(), "--port", fmt.Sprint(noSUID.inst.Port), "--database", noSUID.dbName, "--user", noSUID.user}
	if code, _, errs := mainRun([]string{migrate.EnvDBPass + "=" + noSUID.pw}, args2...); code != migrate.ExitPreflight || !strings.Contains(errs, "1419") {
		t.Fatalf("预检失败应退出码 8 且说明 1419: %d %s", code, errs)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("预检失败不得写文件")
	}
	// 成功：0400、内容可被 migrate status 使用，口令未输出。
	code, o, errs := mainRun([]string{migrate.EnvDBPass + "=" + e.pw}, args...)
	if code != 0 || strings.Contains(o+errs, e.pw) {
		t.Fatalf("init 应成功且不输出口令: %d %s %s", code, o, errs)
	}
	st, err := os.Stat(out)
	if err != nil || st.Mode().Perm() != 0o400 {
		t.Fatalf("凭据文件应为 0400: %v %v", st, err)
	}
	if code, _, errs := mainRun([]string{migrate.EnvMigrateYAML + "=" + out}, "status"); code != 0 {
		t.Fatalf("写出的文件应可被 migrate 使用: %d %s", code, errs)
	}
	// 已存在：拒绝覆盖；--force 覆盖。
	if code, _, errs := mainRun([]string{migrate.EnvDBPass + "=" + e.pw}, args...); code == 0 || !strings.Contains(errs, "已存在") {
		t.Fatalf("已存在应拒绝覆盖: %d %s", code, errs)
	}
	if code, _, errs := mainRun([]string{migrate.EnvDBPass + "=" + e.pw}, append(args, "--force")...); code != 0 {
		t.Fatalf("--force 应覆盖: %d %s", code, errs)
	}
}

// `migrate credential reset --admin-user`：ALTER USER 生成新口令并写文件；旧口令失效；口令不输出；管理员口令错误不改任何东西。
func TestCredentialReset(t *testing.T) {
	e := newEnv(t)
	adm, admPW := mysqltest.RandomName("adm"), mysqltest.RandomSecret(t)
	e.inst.Exec(t, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", adm, admPW))
	e.inst.Exec(t, fmt.Sprintf("GRANT ALL ON *.* TO '%s'@'%%' WITH GRANT OPTION", adm))
	e.inst.DropUserOnCleanup(t, adm)
	dir := t.TempDir()
	out := filepath.Join(dir, "migrate.yaml")
	args := []string{"credential", "reset", "--file", out, "--admin-user", adm, "--host", e.inst.Host(), "--port", fmt.Sprint(e.inst.Port), "--database", e.dbName, "--user", e.user}
	if code, _, errs := mainRun([]string{migrate.EnvAdminPass + "=wrong-" + admPW}, args...); code == 0 {
		t.Fatalf("管理员口令错误应失败: %s", errs)
	}
	if err := e.pingUser(t, e.user, e.pw); err != nil {
		t.Fatalf("管理员口令错误时迁移账号不应被改动: %v", err)
	}
	code, o, errs := mainRun([]string{migrate.EnvAdminPass + "=" + admPW}, args...)
	if code != 0 || strings.Contains(o+errs, admPW) || strings.Contains(o+errs, e.pw) {
		t.Fatalf("reset 应成功且不输出口令: %d %s %s", code, o, errs)
	}
	if err := e.pingUser(t, e.user, e.pw); err == nil {
		t.Fatal("reset 后旧口令应失效")
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o400 {
		t.Errorf("凭据文件应为 0400")
	}
	// 新口令写在文件里，且文件可直接驱动 migrate（目标与口令）。
	if !strings.Contains(string(b), "password: ") {
		t.Fatalf("文件应含新口令: %s", b)
	}
	if code, _, errs := mainRun([]string{migrate.EnvMigrateYAML + "=" + out}, "status"); code != 0 {
		t.Fatalf("reset 写出的文件应可使用: %d %s", code, errs)
	}
	// 已存在文件时 reset 直接覆盖（它的用途就是替换丢失/过期的口令）。
	if code, _, errs := mainRun([]string{migrate.EnvAdminPass + "=" + admPW}, append(args, "--force")...); code != 0 {
		t.Fatalf("reset --force: %d %s", code, errs)
	}
}

func (e *env) pingUser(t *testing.T, user, pw string) error {
	t.Helper()
	d, err := sql.Open("mysql", fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", user, pw, e.inst.Host(), e.inst.Port, e.dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	return d.Ping()
}

// ADR-026 §4 信任规则：root 侧 migrate 的连接目标绝不取自 lingedge 可写的 runtime-credentials.json。
// 把该文件的 host 指向「攻击者」监听器，运行各种 migrate 命令：攻击者必须收不到任何连接。
func TestMigrateNeverUsesRuntimeCredentialsFile(t *testing.T) {
	e := newEnv(t)
	// 「攻击者的 MySQL」：只数连接数。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			_ = c.Close()
		}
	}()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "master.key")
	kb := make([]byte, 32)
	if _, err := rand.Read(kb); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("k1:"+base64.RawURLEncoding.EncodeToString(kb)+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	defer kp.Close()
	box, _ := secretbox.New(kp)
	credPath := filepath.Join(dir, "data", "secrets", "runtime-credentials.json")
	host, portS, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	_, _ = fmt.Sscan(portS, &port)
	if err := dbcred.WriteDatabase(context.Background(), credPath, box, dbcred.DatabaseInput{Host: host, Port: port, Name: e.dbName, AppUser: "lingedge_app", AppPass: config.NewSecret("x-" + mysqltest.RandomSecret(t))}); err != nil {
		t.Fatal(err)
	}
	// server.yaml 指向该数据目录，但不含 database.host（连接目标缺失）。
	cfgPath := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  env: test\n  data_dir: "+filepath.Join(dir, "data")+"\nsecrets:\n  master_key_file: "+keyFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{migrate.EnvDBPass + "=" + e.pw}
	for _, args := range [][]string{{"status", "--config", cfgPath}, {"up", "--config", cfgPath}, {"plan", "--config", cfgPath}, {"credential", "init", "--file", filepath.Join(dir, "m.yaml"), "--config", cfgPath}} {
		code, out, errs := mainRun(env, args...)
		if args[0] != "plan" && code == 0 {
			t.Errorf("%v: 缺少连接目标时不应成功（不得退回到 runtime-credentials.json）: %s", args, out)
		}
		_ = errs
	}
	if n := conns.Load(); n != 0 {
		t.Fatalf("migrate 向 runtime-credentials.json 指向的主机发起了 %d 次连接（root 侧口令可能被套取）", n)
	}
	// 依赖面：migrate 包（含子包）不 import dbcred。
	for _, d := range []string{"../../internal/platform/migrate", "../../internal/platform/migrate/provision"} {
		ents, _ := os.ReadDir(d)
		for _, f := range ents {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			af, err := parser.ParseFile(token.NewFileSet(), filepath.Join(d, f.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, im := range af.Imports {
				if strings.Contains(im.Path.Value, "platform/dbcred") {
					t.Errorf("%s 不得 import dbcred（root 侧 migrate 不读 lingedge 可写的文件）", f.Name())
				}
			}
		}
	}
}
