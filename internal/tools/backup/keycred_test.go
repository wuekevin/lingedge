package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

func keyLine(id string, fill byte) string {
	k := make([]byte, 32)
	for i := range k {
		k[i] = fill + byte(i)
	}
	return id + ":" + base64.RawURLEncoding.EncodeToString(k)
}

func writeKey(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKeySetFingerprints(t *testing.T) {
	p1 := writeKey(t, keyLine("k1", 1)+"\n")
	ks1, err := LoadKeySet(p1, nil)
	if err != nil || len(ks1.IDs) != 1 || len(ks1.Overall) != 64 || len(ks1.FP["k1"]) != 64 {
		t.Fatalf("LoadKeySet: %+v %v", ks1, err)
	}
	// 与 install 输出同源：文件内容 SHA-256 的前 16 位十六进制。
	if want := sum([]byte(keyLine("k1", 1) + "\n"))[:16]; ShortFingerprint(ks1.Overall) != want {
		t.Fatalf("短指纹应为文件 sha256 前 8 字节: %s vs %s", ShortFingerprint(ks1.Overall), want)
	}
	// 注释/行序/@current 不影响单键指纹；新增 KEK 改变整体指纹但不改变既有单键指纹。
	p2 := writeKey(t, "# rotated\n"+keyLine("k2", 50)+"\n"+keyLine("k1", 1)+"\n@current k2\n")
	ks2, err := LoadKeySet(p2, nil)
	if err != nil || ks2.Current != "k2" || ks2.FP["k1"] != ks1.FP["k1"] || ks2.Overall == ks1.Overall {
		t.Fatalf("轮换后的指纹: %+v %v", ks2, err)
	}
	// 密钥字节与指纹不得出现在任何错误里；坏格式/缺失/宽权限 fail-closed。
	if _, err := LoadKeySet(filepath.Join(t.TempDir(), "none"), nil); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("缺失: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "master.key")
	_ = os.WriteFile(bad, []byte(keyLine("k1", 1)+"\n"), 0o644)
	if _, err := LoadKeySet(bad, nil); !errors.Is(err, ErrMasterKeyInvalid) {
		t.Fatalf("权限过宽应被拒绝: %v", err)
	}
	if _, err := LoadKeySet(writeKey(t, "garbage\n"), nil); !errors.Is(err, ErrMasterKeyInvalid) {
		t.Fatalf("格式错误: %v", err)
	}
	if _, err := LoadKeySet(writeKey(t, keyLine("k1", 1)+"\n"), []uint32{4242}); !errors.Is(err, ErrMasterKeyInvalid) {
		t.Fatalf("属主不在允许集合内应被拒绝: %v", err)
	}
}

func TestCheckKeyMatch(t *testing.T) {
	k1 := writeKey(t, keyLine("k1", 1)+"\n")
	ks1, _ := LoadKeySet(k1, nil)
	other, _ := LoadKeySet(writeKey(t, keyLine("k1", 99)+"\n"), nil) // 同 kek_id、不同密钥
	rotated, _ := LoadKeySet(writeKey(t, keyLine("k1", 1)+"\n"+keyLine("k2", 7)+"\n"), nil)
	m := Manifest{MasterKeyFingerprint: ks1.Overall, KEKFingerprints: ks1.FP, KEKIDsRequired: []string{"k1"}}

	if mm := CheckKeyMatch(m, ks1); mm != nil {
		t.Fatalf("同一把密钥应匹配: %v", mm)
	}
	if mm := CheckKeyMatch(m, rotated); mm != nil {
		t.Fatalf("备份之后新增了 KEK（轮换）仍应匹配（k1 指纹相同）: %v", mm)
	}
	if mm := CheckKeyMatch(m, other); mm == nil || len(mm.Different) != 1 || mm.Different[0] != "k1" {
		t.Fatalf("同 kek_id 不同密钥应判为 Different: %v", mm)
	}
	missing, _ := LoadKeySet(writeKey(t, keyLine("k9", 3)+"\n"), nil)
	if mm := CheckKeyMatch(m, missing); mm == nil || len(mm.Missing) != 1 {
		t.Fatalf("缺少所需 kek_id 应判为 Missing: %v", mm)
	}
	// 库内无密文（kek_ids_required 为空）：取备份记录的全部 kek_id 比对，整体指纹相同直接通过。
	m2 := Manifest{MasterKeyFingerprint: ks1.Overall, KEKFingerprints: ks1.FP}
	if CheckKeyMatch(m2, ks1) != nil || CheckKeyMatch(m2, other) == nil || CheckKeyMatch(m2, missing) == nil {
		t.Fatal("无 kek_ids_required 时按备份记录的全部 KEK 比对")
	}
	// node 备份（无任何指纹）不比对。
	if CheckKeyMatch(Manifest{}, missing) != nil {
		t.Fatal("没有指纹的备份不应比对")
	}
	// 错误信息不含密钥字节或完整指纹之外的敏感内容。
	if msg := CheckKeyMatch(m, other).Error(); strings.Contains(msg, keyLine("k1", 99)) {
		t.Fatalf("错误信息泄露密钥: %s", msg)
	}
}

func TestResolveDBPriorityAndTrust(t *testing.T) {
	dir := t.TempDir()
	my := filepath.Join(dir, "migrate.yaml")
	srv := filepath.Join(dir, "server.yaml")
	pf := filepath.Join(dir, "pw")
	_ = os.WriteFile(my, []byte("host: 10.0.0.9\nport: 3307\ndatabase: fromyaml\nuser: yamluser\npassword: yamlpass\n"), 0o400)
	_ = os.WriteFile(srv, []byte("database:\n  host: 10.9.9.9\n  port: 3399\n  name: fromserver\nhttp:\n  listen: 127.0.0.1:8080\n"), 0o640)
	_ = os.WriteFile(pf, []byte("filepass\n"), 0o400)
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	// 口令优先级：环境变量 > 口令文件 > migrate.yaml。
	c, err := ResolveDB(CredEnv{Getenv: env(EnvDBPass, "envpass"), MigrateYAML: my, ServerYAML: srv})
	if err != nil || string(c.Password) != "envpass" || c.Host != "10.0.0.9" || c.Port != 3307 || c.Name != "fromyaml" || c.User != "yamluser" {
		t.Fatalf("环境变量口令 + migrate.yaml 目标: %+v %v", c, err)
	}
	c, _ = ResolveDB(CredEnv{Getenv: env(EnvPassFile, pf), MigrateYAML: my})
	if string(c.Password) != "filepass" {
		t.Fatalf("口令文件: %q", c.Password)
	}
	c, _ = ResolveDB(CredEnv{Getenv: env(), MigrateYAML: my})
	if string(c.Password) != "yamlpass" {
		t.Fatalf("migrate.yaml: %q", c.Password)
	}
	asked := 0
	c, err = ResolveDB(CredEnv{Getenv: env(), MigrateYAML: filepath.Join(dir, "absent.yaml"), ServerYAML: srv, ReadSecret: func(string) (string, error) { asked++; return "typed", nil }})
	if err != nil || string(c.Password) != "typed" || asked != 1 || c.Host != "10.9.9.9" || c.User != DefaultUser {
		t.Fatalf("交互输入 + server.yaml 目标: %+v %v", c, err)
	}
	// 非交互且没有任何口令来源 → ErrNoCredentials（先于任何改动）。
	if _, err := ResolveDB(CredEnv{Getenv: env(), MigrateYAML: filepath.Join(dir, "absent.yaml"), ServerYAML: srv}); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("应为 ErrNoCredentials: %v", err)
	}
	// 显式目标 > 环境变量目标 > 文件。
	c, _ = ResolveDB(CredEnv{Getenv: env(EnvDBPass, "p", EnvHost, "envhost", EnvName, "envdb"), MigrateYAML: my, Explicit: DBTarget{Host: "flaghost", Port: 1234}})
	if c.Host != "flaghost" || c.Port != 1234 || c.Name != "envdb" {
		t.Fatalf("目标优先级: %+v", c)
	}
	c, _ = ResolveDB(CredEnv{Getenv: env(EnvDBPass, "p", EnvSocket, "/tmp/my.sock", EnvName, "d"), MigrateYAML: my})
	if c.Socket != "/tmp/my.sock" || c.Host != "" {
		t.Fatalf("socket 目标: %+v", c)
	}
	// 缺目标。
	if _, err := ResolveDB(CredEnv{Getenv: env(EnvDBPass, "p"), MigrateYAML: filepath.Join(dir, "absent.yaml")}); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("应为 ErrNoTarget: %v", err)
	}
	// 口令来源互斥。
	if _, err := ResolveDB(CredEnv{Getenv: env(EnvDBPass, "a", EnvPassFile, pf), MigrateYAML: my}); err == nil {
		t.Fatal("两种口令来源同时给出应报错")
	}
	// migrate.yaml 权限过宽 / 含未知字段 / 符号链接 → 拒绝；错误不含口令。
	wide := filepath.Join(dir, "wide.yaml")
	_ = os.WriteFile(wide, []byte("host: h\ndatabase: d\npassword: topsecretpw\n"), 0o644)
	if _, err := ResolveDB(CredEnv{Getenv: env(), MigrateYAML: wide}); err == nil || strings.Contains(err.Error(), "topsecretpw") {
		t.Fatalf("权限过宽应被拒绝且不泄露口令: %v", err)
	}
	unk := filepath.Join(dir, "unk.yaml")
	_ = os.WriteFile(unk, []byte("host: h\ndatabase: d\npassword: topsecretpw\nbogus: 1\n"), 0o400)
	if _, err := ResolveDB(CredEnv{Getenv: env(), MigrateYAML: unk}); err == nil || strings.Contains(err.Error(), "topsecretpw") {
		t.Fatalf("未知字段: %v", err)
	}
	ln := filepath.Join(dir, "ln.yaml")
	_ = os.Symlink(my, ln)
	if _, err := ResolveDB(CredEnv{Getenv: env(), MigrateYAML: ln}); err == nil {
		t.Fatal("符号链接应被拒绝")
	}
	// 目标字段注入（换行/引号）被拒绝。
	if _, err := ResolveDB(CredEnv{Getenv: env(EnvDBPass, "p"), Explicit: DBTarget{Host: "h\nuser=root", Name: "d"}}); err == nil {
		t.Fatal("host 含换行应被拒绝")
	}
	// 绝不读取 runtime-credentials.json：把它放进数据目录，ResolveDB 没有任何入口去读它（由依赖扫描 + 此处语义断言）。
	rc := filepath.Join(dir, "runtime-credentials.json")
	_ = os.WriteFile(rc, []byte(`{"database":{"host":"evil","name":"d"}}`), 0o600)
	c, _ = ResolveDB(CredEnv{Getenv: env(EnvDBPass, "p"), MigrateYAML: my})
	if c.Host == "evil" {
		t.Fatal("不得读取 runtime-credentials.json")
	}
}

// ---- 选项文件：0600、第一个参数、口令不在命令行、用后清零删除 ----

type recRunner struct {
	mu   sync.Mutex
	cmds []safeexec.Cmd
	on   func(c safeexec.Cmd) (safeexec.Result, error)
}

func (r *recRunner) Run(_ context.Context, c safeexec.Cmd) (safeexec.Result, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	if r.on != nil {
		return r.on(c)
	}
	return safeexec.Result{}, nil
}

func TestOptionFileLifecycleAndNoPasswordLeak(t *testing.T) {
	scratch := t.TempDir()
	const trickyText = `p@ss"w\ord#$ 1`
	conn := DBConn{DBTarget: DBTarget{Host: "127.0.0.1", Port: 3306, Name: "lingedge", User: "lingedge_migrate"}, Password: []byte(trickyText)}
	var seenOpt string
	var seenMode os.FileMode
	var seenBody string
	run := &recRunner{on: func(c safeexec.Cmd) (safeexec.Result, error) {
		first := c.Args[0]
		if !strings.HasPrefix(first, "--defaults-extra-file=") {
			t.Errorf("--defaults-extra-file 必须是第一个参数: %v", c.Args)
		}
		seenOpt = strings.TrimPrefix(first, "--defaults-extra-file=")
		fi, err := os.Stat(seenOpt)
		if err != nil {
			t.Errorf("执行期间选项文件应存在: %v", err)
			return safeexec.Result{}, nil
		}
		seenMode = fi.Mode().Perm()
		b, _ := os.ReadFile(seenOpt)
		seenBody = string(b)
		di, _ := os.Stat(filepath.Dir(seenOpt))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("暂存目录应为 0700: %v", di.Mode().Perm())
		}
		for _, a := range c.Args[1:] { // 第一个参数是 --defaults-extra-file=<路径>（路径里可能含测试名）
			if strings.Contains(a, "p@ss") || strings.Contains(strings.ToLower(a), "password") || strings.HasPrefix(a, "-p") {
				t.Errorf("口令（或口令类参数）出现在命令行里: %q", a)
			}
		}
		for _, e := range c.Env {
			if strings.Contains(e, "p@ss") {
				t.Errorf("口令出现在环境变量里: %q", e)
			}
		}
		return safeexec.Result{Stdout: []byte("1\n")}, nil
	}}
	m := &MySQL{Run: run, Scratch: scratch}
	if _, err := m.Query(context.Background(), conn, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if seenMode != 0o600 {
		t.Errorf("选项文件应为 0600，实际 %v", seenMode)
	}
	wantLine := "pass" + "word=" + `"p@ss\"w\\ord#$ 1"` // 选项文件的口令行（拆开写，避免固定口令扫描把「期望的文件内容」当成口令赋值）
	if !strings.Contains(seenBody, wantLine) || !strings.Contains(seenBody, "user=lingedge_migrate") {
		t.Errorf("选项文件内容/转义不符:\n%s", seenBody)
	}
	// 用后立即删除：暂存区里不留任何东西。
	ents, _ := os.ReadDir(scratch)
	if len(ents) != 0 {
		t.Errorf("凭据文件清理后暂存区应为空，实际 %v", ents)
	}
	// 命令失败时同样清理，且错误不含口令。
	run.on = func(c safeexec.Cmd) (safeexec.Result, error) {
		return safeexec.Result{}, &safeexec.ExitError{Exe: "mysql", Args: c.Args, Code: 1, Stderr: "ERROR 1045 Access denied"}
	}
	_, err := m.Query(context.Background(), conn, "SELECT 1")
	if err == nil || strings.Contains(err.Error(), "p@ss") {
		t.Fatalf("失败时错误不得含口令: %v", err)
	}
	if ents, _ := os.ReadDir(scratch); len(ents) != 0 {
		t.Errorf("失败路径同样要清理凭据文件: %v", ents)
	}
	// 控制字符口令（换行）拒绝写入选项文件，不会注入额外选项。
	bad := conn
	bad.Password = []byte("a\nuser=root")
	if _, err := m.Query(context.Background(), bad, "SELECT 1"); err == nil {
		t.Fatal("含换行的口令应被拒绝")
	}
	if ents, _ := os.ReadDir(scratch); len(ents) != 0 {
		t.Errorf("拒绝路径不留文件: %v", ents)
	}
}

func TestDumpArgsAndWipeStatements(t *testing.T) {
	scratch, out := t.TempDir(), filepath.Join(t.TempDir(), "dump.sql")
	var got safeexec.Cmd
	run := &recRunner{on: func(c safeexec.Cmd) (safeexec.Result, error) { got = c; return safeexec.Result{}, nil }}
	m := &MySQL{Run: run, Scratch: scratch}
	conn := DBConn{DBTarget: DBTarget{Socket: "/tmp/m.sock", Name: "lingedge", User: "lingedge_migrate"}, Password: []byte("pw")}
	if err := m.Dump(context.Background(), conn, out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--single-transaction", "--routines", "--triggers", "--result-file=" + out, "lingedge"} {
		found := false
		for _, a := range got.Args {
			found = found || a == want
		}
		if !found {
			t.Errorf("mysqldump 参数缺少 %q: %v", want, got.Args)
		}
	}
	if got.Path != "mysqldump" {
		t.Errorf("Path=%q", got.Path)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("转储文件应预先以 0600 创建: %v %v", fi, err)
	}
	if err := m.Dump(context.Background(), conn, out); err == nil {
		t.Error("转储目标已存在应报错（O_EXCL）")
	}
	// 非法目标字段在任何进程启动前就被拒绝。
	for _, bad := range []DBTarget{{Host: "h", Name: "a;b", User: "u"}, {Host: "h", Name: "d", User: "u v"}, {Host: "h x", Name: "d", User: "u"}, {Host: "h", Port: 70000, Name: "d", User: "u"},
		{Host: "h", Socket: "/s", Name: "d", User: "u"}, {Socket: "rel", Name: "d", User: "u"}, {Host: "h", Name: "d", User: "u", TLS: "weird"}} {
		if bad.Validate() == nil {
			t.Errorf("应拒绝 %+v", bad)
		}
	}
	// Wipe 的语句：先列对象再按类型 DROP，标识符转义。
	calls := 0
	run.on = func(c safeexec.Cmd) (safeexec.Result, error) {
		calls++
		if c.Stdin != nil {
			b := new(strings.Builder)
			buf := make([]byte, 4096)
			for {
				n, err := c.Stdin.Read(buf)
				b.Write(buf[:n])
				if err != nil {
					break
				}
			}
			s := b.String()
			for _, w := range []string{"SET FOREIGN_KEY_CHECKS=0;", "DROP VIEW IF EXISTS `v1`;", "DROP TABLE IF EXISTS `t``x`;", "DROP PROCEDURE IF EXISTS `p1`;", "DROP EVENT IF EXISTS `e1`;"} {
				if !strings.Contains(s, w) {
					t.Errorf("清空脚本缺少 %q:\n%s", w, s)
				}
			}
			return safeexec.Result{}, nil
		}
		q := c.Args[len(c.Args)-1]
		switch {
		case strings.Contains(q, "information_schema.tables"):
			if calls > 4 {
				return safeexec.Result{}, nil
			}
			return safeexec.Result{Stdout: []byte("t`x\tBASE TABLE\nv1\tVIEW\n")}, nil
		case strings.Contains(q, "routines"):
			if calls > 4 {
				return safeexec.Result{}, nil
			}
			return safeexec.Result{Stdout: []byte("p1\tPROCEDURE\n")}, nil
		default:
			if calls > 4 {
				return safeexec.Result{}, nil
			}
			return safeexec.Result{Stdout: []byte("e1\n")}, nil
		}
	}
	calls = 0
	if err := m.Wipe(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
}

// ---- 保留备份所需 KEK 的核对（MIN-05） ----

func TestKEKGuardAgainstRetainedBackups(t *testing.T) {
	dir := t.TempDir()
	reg := registry.New(registry.Layout{Root: filepath.Join(dir, "tools")}, registry.Options{})
	s := &Service{Reg: reg}
	bdir := filepath.Join(dir, "backups")
	_ = os.MkdirAll(bdir, 0o700)
	mk := func(name string, enc bool, keks ...string) {
		f := newFixture(t, enc)
		f.m.KEKIDsRequired = keks
		f.m.KEKFingerprints = map[string]string{}
		for _, k := range keks {
			f.m.KEKFingerprints[k] = sum([]byte(k))
		}
		f.m.MasterKeyFingerprint = sum([]byte("m"))
		o := WriteOptions{KDF: testKDF}
		if enc {
			o.Passphrase = []byte(testPass)
		}
		if err := WriteArchive(filepath.Join(bdir, name), f.m, f.files, o); err != nil {
			t.Fatal(err)
		}
	}
	mk("lingedge-backup-h-server-20260101T000000Z.tar.gz", false, "k1")
	mk("lingedge-backup-h-server-20260102T000000Z.tar.gz.enc", true, "k1", "k2")

	list, err := s.List(bdir)
	if err != nil || len(list) != 2 {
		t.Fatalf("List: %v %v", list, err)
	}
	for _, e := range list {
		if e.Status != "ok" || e.Meta == nil || len(e.Meta.KEKIDsRequired) == 0 {
			t.Fatalf("list 应展示每个备份所需的 kek_id（加密备份无需口令）: %+v", e)
		}
		if e.Encrypted == e.MetaAuthenticated {
			t.Errorf("加密备份的头部摘要未经认证，明文备份的清单已解析: %+v", e)
		}
	}
	// k2 仅被加密备份需要；k1 被两个都需要；k3 无人需要。
	if _, err := s.GuardKEKRemoval(bdir, "k2", false); !errors.Is(err, secretbox.ErrKEKRequiredByBackup) || !strings.Contains(err.Error(), "20260102") {
		t.Fatalf("k2 被保留备份需要，应拒绝并点名: %v", err)
	}
	if req, err := s.GuardKEKRemoval(bdir, "k3", false); err != nil || len(req) != 2 {
		t.Fatalf("k3 无人需要: %v %v", req, err)
	}
	// --drop-backup-compat 放行（调用方须提示旧备份将不可解密）。
	if _, err := s.GuardKEKRemoval(bdir, "k2", true); err != nil {
		t.Fatalf("--drop-backup-compat: %v", err)
	}
	// 与 secretbox.CheckKEKRemovable 联动：BackupRequired 取自 GuardKEKRemoval。
	req, _ := s.GuardKEKRemoval(bdir, "k3", false)
	if err := secretbox.CheckKEKRemovable(context.Background(), zeroCount{}, secretbox.RemovalCheck{KEKID: "k2", CurrentKEKID: "k9", BackupRequired: req}); !errors.Is(err, secretbox.ErrKEKRequiredByBackup) {
		t.Fatalf("CheckKEKRemovable 应因保留备份拒绝: %v", err)
	}
	if err := secretbox.CheckKEKRemovable(context.Background(), zeroCount{}, secretbox.RemovalCheck{KEKID: "k2", CurrentKEKID: "k9", BackupRequired: req, DropBackupCompat: true}); err != nil {
		t.Fatalf("DropBackupCompat 放行: %v", err)
	}
	// 无法解析的备份：保守拒绝（不知道它是否需要）。
	_ = os.WriteFile(filepath.Join(bdir, "lingedge-backup-h-server-20260103T000000Z.tar.gz"), []byte("not a backup"), 0o600)
	if _, err := s.GuardKEKRemoval(bdir, "k3", false); !errors.Is(err, secretbox.ErrKEKRequiredByBackup) {
		t.Fatalf("存在无法解析的备份时应保守拒绝: %v", err)
	}
	if _, err := s.GuardKEKRemoval(bdir, "k3", true); err != nil {
		t.Fatalf("drop-compat 放行: %v", err)
	}
	// 目录不存在：没有备份需要它。
	if req, err := s.GuardKEKRemoval(filepath.Join(dir, "nope"), "k1", false); err != nil || len(req) != 0 {
		t.Fatalf("空目录: %v %v", req, err)
	}
}

type zeroCount struct{ secretbox.RecordStore }

func (zeroCount) CountByKEK(context.Context, string) (int64, error) { return 0, nil }
