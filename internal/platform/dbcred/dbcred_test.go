package dbcred_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

func key(t testing.TB) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newBox(t testing.TB, content string) *secretbox.Box {
	t.Helper()
	p := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
		t.Fatal(err)
	}
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kp.Close)
	b, err := secretbox.New(kp)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rnd(t testing.TB) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeFull(t *testing.T) (path string, box *secretbox.Box, app, maint, redis string) {
	t.Helper()
	box = newBox(t, "k1:"+key(t)+"\n")
	path = filepath.Join(t.TempDir(), "data", "secrets", "runtime-credentials.json")
	app, maint, redis = "app-"+rnd(t), "maint-"+rnd(t), "redis-"+rnd(t)
	ctx := context.Background()
	if err := dbcred.WriteDatabase(ctx, path, box, dbcred.DatabaseInput{Host: "127.0.0.1", Port: 3306, Name: "lingedge", TLS: "preferred", AppUser: "lingedge_app",
		AppPass: config.NewSecret(app), MaintUser: "lingedge_maint", MaintPass: config.NewSecret(maint)}); err != nil {
		t.Fatal(err)
	}
	if err := dbcred.WriteRedis(ctx, path, box, dbcred.RedisInput{Addr: "127.0.0.1:6379", DB: 1, Pass: config.NewSecret(redis)}); err != nil {
		t.Fatal(err)
	}
	return
}

// 文件格式、权限、加密往返；转储里没有任何明文口令；目录 0700、文件 0600。
func TestWriteLoadRoundTripAndDump(t *testing.T) {
	path, box, app, maint, redis := writeFull(t)
	st, err := os.Lstat(path)
	if err != nil || st.Mode().Perm() != 0o600 || !st.Mode().IsRegular() {
		t.Fatalf("文件应为 0600 普通文件: %v %v", st, err)
	}
	if ds, _ := os.Stat(filepath.Dir(path)); ds.Mode().Perm() != 0o700 {
		t.Errorf("目录应为 0700: %v", ds.Mode())
	}
	raw, _ := os.ReadFile(path)
	if dbcred.HasEncryptedPlaintext(raw, app, maint, redis) {
		t.Fatalf("转储不得含明文口令:\n%s", raw)
	}
	for _, want := range []string{`"v": 1`, `"` + "password" + `_enc": "v1:`, `"host": "127.0.0.1"`, `"tls": "preferred"`, `"addr": "127.0.0.1:6379"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("文件缺少 %s:\n%s", want, raw)
		}
	}
	f, err := dbcred.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if pw, err := f.AppPassword(ctx, box); err != nil || pw.Reveal() != app {
		t.Errorf("app 往返: %v", err)
	}
	if pw, ok, err := f.MaintPassword(ctx, box); err != nil || !ok || pw.Reveal() != maint {
		t.Errorf("maint 往返: %v", err)
	}
	if pw, err := f.RedisPassword(ctx, box); err != nil || pw.Reveal() != redis {
		t.Errorf("redis 往返: %v", err)
	}
	// 密文是随机的：同一明文两次写入的密文不同。
	before := string(raw)
	if err := dbcred.WriteRedis(ctx, path, box, dbcred.RedisInput{Addr: "127.0.0.1:6379", DB: 1, Pass: config.NewSecret(redis)}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) == before {
		t.Error("每次加密应使用新的随机 DEK/nonce")
	}
	// 临时文件无残留。
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".runtime-credentials-") {
			t.Errorf("临时文件残留: %s", e.Name())
		}
	}
}

// 启动校验（fail-closed）：符号链接、过宽权限、未知版本、未知字段、过大、多余内容、非 JSON 一律拒绝；不存在返回 ErrNotFound。
func TestLoadRejects(t *testing.T) {
	path, _, _, _, _ := writeFull(t)
	good, _ := os.ReadFile(path)
	dir := filepath.Dir(path)
	write := func(name string, b []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		_ = os.Remove(p)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, mode)
		return p
	}
	cases := map[string]string{
		"过宽权限(0640)": write("a.json", good, 0o640),
		"过宽权限(0604)": write("b.json", good, 0o604),
		"未知版本":       write("c.json", []byte(strings.Replace(string(good), `"v": 1`, `"v": 2`, 1)), 0o600),
		"未知字段":       write("d.json", []byte(strings.Replace(string(good), `"v": 1`, `"v": 1, "extra": true`, 1)), 0o600),
		"多余内容":       write("e.json", append(append([]byte(nil), good...), []byte(`{"v":1}`)...), 0o600),
		"非JSON":      write("f.json", []byte("not json"), 0o600),
		"过大":         write("g.json", append([]byte(`{"v":1,"pad":"`), append(make([]byte, 70<<10), []byte(`"}`)...)...), 0o600),
	}
	for name, p := range cases {
		if _, err := dbcred.Load(p); !errors.Is(err, dbcred.ErrInvalid) {
			t.Errorf("%s 应 ErrInvalid，实际 %v", name, err)
		}
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := dbcred.Load(link); !errors.Is(err, dbcred.ErrInvalid) {
		t.Errorf("符号链接应被拒绝: %v", err)
	}
	if _, err := dbcred.Load(dir); !errors.Is(err, dbcred.ErrInvalid) {
		t.Errorf("目录应被拒绝: %v", err)
	}
	if _, err := dbcred.Load(filepath.Join(dir, "absent.json")); !errors.Is(err, dbcred.ErrNotFound) {
		t.Errorf("不存在应 ErrNotFound: %v", err)
	}
	// 已存在但无效的文件不会被写入操作静默覆盖。
	if err := dbcred.WriteRedis(context.Background(), cases["未知版本"], newBox(t, "k1:"+key(t)+"\n"), dbcred.RedisInput{Addr: "h:1"}); !errors.Is(err, dbcred.ErrInvalid) {
		t.Errorf("无效文件不得被静默覆盖: %v", err)
	}
}

// AAD 绑定字段与账号：把 app 的密文挪到 maint 字段（或换账号名）解密失败。
func TestAADBindsFieldAndAccount(t *testing.T) {
	path, box, _, _, _ := writeFull(t)
	raw, _ := os.ReadFile(path)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	db := m["database"].(map[string]any)
	app, maint := db["app"].(map[string]any), db["maint"].(map[string]any)
	app["password_enc"], maint["password_enc"] = maint["password_enc"], app["password_enc"]
	swapped, _ := json.Marshal(m)
	p := filepath.Join(filepath.Dir(path), "swapped.json")
	if err := os.WriteFile(p, swapped, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := dbcred.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.AppPassword(context.Background(), box); err == nil {
		t.Error("app 与 maint 密文互换后应解密失败（AAD 含字段路径）")
	}
	// 换用户名（密文不变）也失败。
	app["password_enc"], maint["password_enc"] = maint["password_enc"], app["password_enc"]
	app["user"] = "someone_else"
	renamed, _ := json.Marshal(m)
	p2 := filepath.Join(filepath.Dir(path), "renamed.json")
	if err := os.WriteFile(p2, renamed, 0o600); err != nil {
		t.Fatal(err)
	}
	f2, _ := dbcred.Load(p2)
	if _, err := f2.AppPassword(context.Background(), box); err == nil {
		t.Error("换账号名后应解密失败（AAD 含账号名）")
	}
	// 主密钥不匹配：解密失败且错误不含密文。
	other := newBox(t, "k1:"+key(t)+"\n")
	f3, _ := dbcred.Load(path)
	if _, err := f3.AppPassword(context.Background(), other); err == nil {
		t.Error("错误的主密钥应解密失败")
	}
}

// 合并：显式通道（已填入 cfg 的值）优先；缺省才读文件；全部显式时不读文件；文件不存在不报错；maint 仅在需要时解密。
func TestMergePriority(t *testing.T) {
	path, box, app, maint, redis := writeFull(t)
	ctx := context.Background()
	newCfg := func() *config.Config {
		c := config.Defaults()
		c.Server.DataDir = filepath.Dir(filepath.Dir(path))
		return &c
	}
	// 全部缺省 → 全部来自文件。
	c := newCfg()
	m, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{NeedMaint: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.Host != "127.0.0.1" || c.Database.Name != "lingedge" || c.Database.User != "lingedge_app" || c.Database.Password.Reveal() != app || c.Database.TLS != "preferred" ||
		c.Database.Maint.User != "lingedge_maint" || c.Database.Maint.Password.Reveal() != maint || c.Redis.Addr != "127.0.0.1:6379" || c.Redis.DB != 1 || c.Redis.Password.Reveal() != redis {
		t.Fatalf("缺省应来自文件: %+v %+v", c.Database, c.Redis)
	}
	if len(m.Used) == 0 {
		t.Error("应报告用了哪些字段")
	}
	// api 角色不需要 maint：不解密、不填充。
	c = newCfg()
	if _, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{NeedMaint: false}); err != nil || c.Database.Maint.Configured() {
		t.Errorf("NeedMaint=false 不应填充 maint: %v %v", err, c.Database.Maint)
	}
	// 显式通道优先：显式口令/地址不被文件覆盖。
	c = newCfg()
	explicit := "explicit-" + rnd(t)
	c.Database.Host, c.Database.Name, c.Database.User = "db.example.test", "other", "other_user"
	c.Database.Password = config.NewSecret(explicit)
	c.Database.Maint = config.DatabaseMaint{User: "m", Password: config.NewSecret("mp-" + rnd(t))}
	c.Redis.Addr = "r.example.test:6379"
	c.Redis.Password = config.NewSecret("rp-" + rnd(t))
	if m, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{NeedMaint: true}); err != nil || len(m.Used) != 0 {
		t.Fatalf("全部显式时不应读文件: %v %v", m, err)
	}
	if c.Database.Host != "db.example.test" || c.Database.Password.Reveal() != explicit || c.Database.User != "other_user" {
		t.Errorf("显式值被覆盖: %+v", c.Database)
	}
	// 只显式给口令（目标缺省）：目标与账号名来自文件，口令保持显式。
	c = newCfg()
	c.Database.Password = config.NewSecret(explicit)
	c.Redis.Addr = "r:1"
	c.Redis.Password = config.NewSecret(rnd(t))
	if _, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{}); err != nil {
		t.Fatal(err)
	}
	if c.Database.Password.Reveal() != explicit || c.Database.Host != "127.0.0.1" {
		t.Errorf("显式口令应保留、目标取自文件: %+v", c.Database)
	}
	// 显式给了另一个连接目标：文件里的口令不会被发往那个目标（口令与其所属目标配套）。
	c = newCfg()
	c.Database.Host, c.Database.Name = "other.example.test", "otherdb"
	c.Redis.Addr = "other-redis.example.test:6379"
	if _, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{NeedMaint: true}); err != nil {
		t.Fatal(err)
	}
	if c.Database.Password.IsSet() || c.Database.Maint.Configured() || c.Redis.Password.IsSet() {
		t.Errorf("目标不同时不得合并文件里的口令: %+v %+v", c.Database, c.Redis)
	}
	// 文件不存在：不报错、不改动（缺失项交给 ValidateRuntime）。
	c = newCfg()
	c.Server.DataDir = t.TempDir()
	if m, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{}); err != nil || len(m.Used) != 0 || c.Database.Host != "" {
		t.Errorf("无文件应无操作: %v %v", m, err)
	}
	// 文件存在但无效：fail-closed。
	bad := newCfg()
	bad.Secrets.RuntimeCredentialsFile = filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(bad.Secrets.RuntimeCredentialsFile, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dbcred.Merge(ctx, bad, box, dbcred.MergeOptions{}); !errors.Is(err, dbcred.ErrInvalid) {
		t.Errorf("无效文件应拒绝: %v", err)
	}
	// 主密钥缺失：需要解密时报错（不静默忽略）。
	c = newCfg()
	if _, err := dbcred.Merge(ctx, c, nil, dbcred.MergeOptions{}); !errors.Is(err, secretbox.ErrMasterKeyMissing) {
		t.Errorf("无主密钥应报 ErrMasterKeyMissing: %v", err)
	}
	// 显式的 secrets.runtime_credentials_file 优先于缺省路径。
	custom := newCfg()
	custom.Server.DataDir = t.TempDir()
	custom.Secrets.RuntimeCredentialsFile = path
	if _, err := dbcred.Merge(ctx, custom, box, dbcred.MergeOptions{}); err != nil || custom.Database.User != "lingedge_app" {
		t.Errorf("runtime_credentials_file 应生效: %v", err)
	}
	if dbcred.Path(custom) != path {
		t.Errorf("Path 应取配置键")
	}
}

// KEK 轮换：KEKIDs 列出仍被引用的 kek_id；RewrapFile 把全部密文重包装到新 KEK（可重入），之后旧 KEK 不再被引用，可被移除。
func TestRewrapAndKEKIDs(t *testing.T) {
	k1, k2 := key(t), key(t)
	p := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(p, []byte("k1:"+k1+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	kp, _ := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p})
	box, _ := secretbox.New(kp)
	path := filepath.Join(t.TempDir(), "s", "runtime-credentials.json")
	ctx := context.Background()
	if err := dbcred.WriteDatabase(ctx, path, box, dbcred.DatabaseInput{Host: "h", Port: 1, Name: "n", AppUser: "lingedge_app", AppPass: config.NewSecret("a-" + rnd(t)), MaintUser: "lingedge_maint", MaintPass: config.NewSecret("m-" + rnd(t))}); err != nil {
		t.Fatal(err)
	}
	if err := dbcred.WriteRedis(ctx, path, box, dbcred.RedisInput{Addr: "h:1", Pass: config.NewSecret("r-" + rnd(t))}); err != nil {
		t.Fatal(err)
	}
	kp.Close()
	if ids, err := dbcred.KEKIDs(path); err != nil || len(ids) != 1 || ids[0] != "k1" {
		t.Fatalf("KEKIDs = %v %v", ids, err)
	}
	// 新主密钥文件同时含 k1（旧）与 k2（当前）。
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("k1:"+k1+"\nk2:"+k2+"\n@current k2\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	kp2, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	defer kp2.Close()
	box2, _ := secretbox.New(kp2)
	n, err := dbcred.RewrapFile(ctx, box2, path, "k2")
	if err != nil || n != 3 {
		t.Fatalf("应重包装 3 个字段: n=%d err=%v", n, err)
	}
	if ids, _ := dbcred.KEKIDs(path); len(ids) != 1 || ids[0] != "k2" {
		t.Errorf("重包装后应只引用 k2: %v", ids)
	}
	if n, err := dbcred.RewrapFile(ctx, box2, path, "k2"); err != nil || n != 0 {
		t.Errorf("可重入：再次重包装应为 0: %d %v", n, err)
	}
	f, _ := dbcred.Load(path)
	if _, err := f.AppPassword(ctx, box2); err != nil {
		t.Errorf("重包装后应仍可解密: %v", err)
	}
	// 文件不存在：0、nil。
	if n, err := dbcred.RewrapFile(ctx, box2, filepath.Join(t.TempDir(), "none.json"), "k2"); err != nil || n != 0 {
		t.Errorf("无文件: %d %v", n, err)
	}
}

// ReplacePassword 只换一个角色的密文，其余字段（含另一角色的密文）原样保留；账号不存在则拒绝。
func TestReplacePassword(t *testing.T) {
	path, box, app, maint, _ := writeFull(t)
	ctx := context.Background()
	before, _ := dbcred.Load(path)
	newApp := "newapp-" + rnd(t)
	if err := dbcred.ReplacePassword(ctx, path, box, dbcred.RoleApp, config.NewSecret(newApp)); err != nil {
		t.Fatal(err)
	}
	after, _ := dbcred.Load(path)
	if after.Database.Maint.PasswordEnc != before.Database.Maint.PasswordEnc {
		t.Error("另一角色的密文应原样保留")
	}
	if pw, _ := after.AppPassword(ctx, box); pw.Reveal() != newApp || newApp == app {
		t.Error("app 口令应已替换")
	}
	if pw, _, _ := after.MaintPassword(ctx, box); pw.Reveal() != maint {
		t.Error("maint 口令应不变")
	}
	if err := dbcred.ReplacePassword(ctx, path, box, dbcred.Role("migrate"), config.NewSecret("x")); err == nil {
		t.Error("未知角色应拒绝（migrate 口令绝不进入该文件）")
	}
	// 只有 database 无 maint 时拒绝 maint。
	p2 := filepath.Join(t.TempDir(), "x", "c.json")
	if err := dbcred.WriteDatabase(ctx, p2, box, dbcred.DatabaseInput{Host: "h", Port: 1, Name: "n", AppUser: "lingedge_app", AppPass: config.NewSecret("a-" + rnd(t))}); err != nil {
		t.Fatal(err)
	}
	if err := dbcred.ReplacePassword(ctx, p2, box, dbcred.RoleMaint, config.NewSecret("x")); err == nil {
		t.Error("没有 maint 账号时应拒绝")
	}
}

// 写入前往返自检：主密钥不可用（box=nil）拒绝，且不留残文件。
func TestWriteRefusesWithoutMasterKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s", "c.json")
	err := dbcred.WriteDatabase(context.Background(), path, nil, dbcred.DatabaseInput{Host: "h", Port: 1, Name: "n", AppUser: "lingedge_app", AppPass: config.NewSecret("a-" + rnd(t))})
	if !errors.Is(err, secretbox.ErrMasterKeyMissing) {
		t.Fatalf("无主密钥应拒绝: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("不得留下残文件")
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) > 0 {
		t.Errorf("不得留下临时文件: %v", ents)
	}
	if err := dbcred.WriteDatabase(context.Background(), path, newBox(t, "k1:"+key(t)+"\n"), dbcred.DatabaseInput{}); err == nil {
		t.Error("缺少必填字段应拒绝")
	}
}

// CC-13：socket 目标的往返、与 host/port 互斥、Merge 成组取用与 sameTarget 判定（口令不发往不同目标）。
func TestSocketTarget(t *testing.T) {
	box := newBox(t, "k1:"+key(t)+"\n")
	path := filepath.Join(t.TempDir(), "data", "secrets", "runtime-credentials.json")
	ctx := context.Background()
	app := "app-" + rnd(t)
	in := dbcred.DatabaseInput{Socket: "/var/run/mysqld/mysqld.sock", Name: "lingedge", AppUser: "lingedge_app", AppPass: config.NewSecret(app)}
	bad := in
	bad.Host, bad.Port = "127.0.0.1", 3306
	if err := dbcred.WriteDatabase(ctx, path, box, bad); err == nil {
		t.Error("socket 与 host/port 同设应拒绝")
	}
	rel := in
	rel.Socket = "mysql.sock"
	if err := dbcred.WriteDatabase(ctx, path, box, rel); err == nil {
		t.Error("相对路径应拒绝")
	}
	if err := dbcred.WriteDatabase(ctx, path, box, in); err != nil {
		t.Fatal(err)
	}
	f, err := dbcred.Load(path)
	if err != nil || f.Database.Socket != in.Socket || f.Database.Host != "" || f.Database.Port != 0 {
		t.Fatalf("往返: %v %+v", err, f.Database)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), app) || strings.Contains(string(raw), `"host"`) {
		t.Error("文件不应含明文口令或空 host 字段")
	}
	newCfg := func() *config.Config {
		c := config.Defaults()
		c.Server.DataDir = filepath.Dir(filepath.Dir(path))
		return &c
	}
	c := newCfg()
	if _, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{}); err != nil || c.Database.Socket != in.Socket || c.Database.Host != "" || c.Database.Port != 0 || c.Database.Password.Reveal() != app {
		t.Fatalf("缺省应整组取自文件: %v %+v", err, c.Database)
	}
	// 显式给了另一个 socket 目标：文件里的口令不发往它。
	c = newCfg()
	c.Database.Socket, c.Database.Name, c.Database.User = "/tmp/other.sock", "lingedge", "lingedge_app"
	if _, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{}); err != nil || c.Database.Password.IsSet() {
		t.Errorf("不同 socket 目标不得取用文件口令: %v", err)
	}
	// 显式给了 TCP 目标：同样不取。
	c = newCfg()
	c.Database.Host, c.Database.Name, c.Database.User = "127.0.0.1", "lingedge", "lingedge_app"
	if _, err := dbcred.Merge(ctx, c, box, dbcred.MergeOptions{}); err != nil || c.Database.Password.IsSet() {
		t.Errorf("TCP 目标不得取用 socket 文件口令: %v", err)
	}
	// 文件里 socket 与 host 并存 → 拒绝加载。
	raw, _ := os.ReadFile(path)
	tampered := strings.Replace(string(raw), `"socket"`, `"host": "127.0.0.1", "socket"`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dbcred.Load(path); err == nil {
		t.Error("socket 与 host 并存应拒绝加载")
	}
}
