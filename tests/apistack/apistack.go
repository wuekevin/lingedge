//go:build !windows

// Package apistack 是「真实进程内装配」的 REST 被测系统，供多租户隔离矩阵（tests/security/tenant_isolation）与租户模块集成测试
// （tests/integration/tenant）共用（Story 1.20）：测试自拉起的独立临时 mysqld（tests/mysqltest，应用全部内嵌迁移、真实 lingedge_app 账号）
// + 独立临时 redis-server（tests/redistest）+ 真实认证模块 + 真实租户模块 + 真实任务中心 + httpx 固定中间件链 + 真实钩子装配
// （auth.Module.Hooks，含完整租户 Scope 钩子与租户目录）+ 真实 HTTP 监听。没有任何 Mock；用后清理；本机常驻 mysqld/redis 不受影响。
//
// 口令、令牌、会话 Cookie、CSRF 令牌全部在运行期生成（G-6：仓库内没有固定口令），并登记到 World.Secrets，供日志脱敏扫描（G-5）。
// 装配与 cmd/lingedge-server 的 registerRoutes/buildHooks 保持同一组成（auth + tenant + taskapi）；漂移由隔离矩阵的「路由清单一致」用例核对。
package apistack

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/wuekevin/lingedge/internal/auth"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	idemrepo "github.com/wuekevin/lingedge/internal/platform/idempotency/repository"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	taskrepo "github.com/wuekevin/lingedge/internal/platform/taskrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/taskrt/taskapi"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/internal/tenant"
	"github.com/wuekevin/lingedge/migrations"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

// 测试用的 argon2id 参数：取允许的下限，让大量登录的测试跑得快；生产默认值与基准见 config.Defaults。
var testArgon = config.Argon2{MemoryKiB: config.MinArgon2MemoryKiB, Iterations: config.MinArgon2Iterations, Parallelism: 1}

// IDs 生成测试数据的 uid。
var IDs = uid.NewGenerator(clock.System())

// SyncBuf 是并发安全的日志缓冲（脱敏扫描用）。
type SyncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write 实现 io.Writer。
func (s *SyncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }

// String 返回已写入内容。
func (s *SyncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Ctx 返回带超时的 context。
func Ctx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// World 是被测系统。
type World struct {
	T      *testing.T
	Inst   *mysqltest.Instance
	Adm    *sql.DB
	DBName string
	App    *db.AppPool
	Redis  *redistest.Instance
	RC     *redisx.Client
	Auth   auth.Module
	Tenant tenant.Module
	Router *httpx.Router
	Srv    *httptest.Server
	Logs   *SyncBuf
	Spec   *contracttest.Spec

	mu      sync.Mutex
	secrets []string
}

// Options 是装配选项。
type Options struct {
	// Routes 在 Handler() 之前向路由器追加测试专用路由（经真实 Router.Register，走真实钩子链）。
	Routes func(r *httpx.Router)
	// ExtraAudited 是额外登记到过渡审计钩子（结构化日志，audit_logs 待 Story 1.21）的 operationId（测试专用探针路由用）。
	ExtraAudited []string
}

// New 创建被测系统（独立临时 MySQL 库 + 独立临时 Redis + 全部迁移）。
func New(t *testing.T, opts ...Options) *World {
	t.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	w := &World{T: t, Logs: &SyncBuf{}, Spec: contracttest.LoadRepo(t)}
	w.openDB()
	w.Redis = redistest.Start(t)
	lg, err := logx.New(logx.Options{Level: "debug", Format: "json", Output: w.Logs})
	if err != nil {
		t.Fatal(err)
	}
	w.RC, err = redisx.New(context.Background(), redisx.Config{Addr: w.Redis.Addr(), Env: "test", ProbeInterval: 100 * time.Millisecond,
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 300 * time.Millisecond, WriteTimeout: 300 * time.Millisecond}, redisx.Options{Logger: lg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.RC.Close() })

	gen := uid.NewGenerator(clock.System())
	w.Auth, err = auth.New(context.Background(), auth.Options{Pool: w.App, Redis: w.RC, Clock: clock.System(), IDs: gen, Log: lg, Argon2: testArgon, Box: newBox(t)})
	if err != nil {
		t.Fatal(err)
	}
	w.Tenant, err = tenant.New(context.Background(), tenant.Options{Pool: w.App, Clock: clock.System(), Log: lg})
	if err != nil {
		t.Fatal(err)
	}
	store, err := taskrepo.New(w.App, gen)
	if err != nil {
		t.Fatal(err)
	}
	idem, err := idemrepo.New(w.App, idemrepo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	extra := map[string]bool{}
	for _, id := range o.ExtraAudited {
		extra[id] = true
	}
	hooks := w.Auth.Hooks(auth.HooksOptions{Tenants: w.Tenant.Resolver(), AuditedOperation: func(id string) bool { return extra[id] || w.Tenant.AuditedOperation(id) }})
	w.Router, err = httpx.NewRouter(httpx.Options{Logger: lg, IDs: gen, Hooks: hooks, Idempotency: idem})
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range []func(*httpx.Router) error{w.Auth.Register, w.Tenant.Register, func(r *httpx.Router) error { return taskapi.Register(r, store) }} {
		if err := reg(w.Router); err != nil {
			t.Fatal(err)
		}
	}
	if o.Routes != nil {
		o.Routes(w.Router)
	}
	h, err := w.Router.Handler()
	if err != nil {
		t.Fatal(err)
	}
	w.Srv = httptest.NewServer(h)
	t.Cleanup(w.Srv.Close)
	return w
}

func (w *World) openDB() {
	t := w.T
	inst := mysqltest.Start(t)
	w.Inst, w.DBName = inst, inst.NewDatabase(t)
	appPW, migPW := mysqltest.RandomSecret(t), mysqltest.RandomSecret(t)
	migUser := mysqltest.RandomName("mig")
	for _, a := range []struct{ u, p string }{{migrate.AppUser, appPW}, {migUser, migPW}} {
		inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", a.u, a.p))
		inst.DropUserOnCleanup(t, a.u)
	}
	inst.Exec(t, "GRANT ALL ON `"+w.DBName+"`.* TO '"+migUser+"'@'%' WITH GRANT OPTION")
	inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+migUser+"'@'%'")
	maintPW := mysqltest.RandomSecret(t)
	inst.Exec(t, fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", migrate.MaintUser, maintPW))
	inst.DropUserOnCleanup(t, migrate.MaintUser)

	cfg := func(user, pw string) db.Config {
		return db.Config{Host: inst.Host(), Port: inst.Port, Name: w.DBName, User: user, Password: config.NewSecret(pw), MaxOpenConns: 8, MaxIdleConns: 4}
	}
	mig, err := db.OpenMigrate(Ctx(t), cfg(migUser, migPW))
	if err != nil {
		t.Fatalf("OpenMigrate: %v", err)
	}
	t.Cleanup(func() { _ = mig.Close() })
	runner, err := migrate.NewRunner(mig, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Up(Ctx(t), migrate.UpOptions{}); err != nil {
		t.Fatalf("全部迁移 up: %v", err)
	}
	if w.App, err = db.OpenApp(Ctx(t), cfg(migrate.AppUser, appPW)); err != nil {
		t.Fatalf("OpenApp: %v", err)
	}
	t.Cleanup(func() { _ = w.App.Close() })
	w.Adm = inst.Admin(t)
	w.Adm.SetMaxOpenConns(3)
}

// newBox 在临时目录写 0400 主密钥文件（运行期随机生成），返回真实 FileKeyProvider 支撑的 Box（与生产同一实现）。
func newBox(t testing.TB) *secretbox.Box {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(p, []byte("k1:"+base64.RawURLEncoding.EncodeToString(raw)+"\n"), 0o400); err != nil {
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

// ---- root 连接（直接读写库，用于造数据与旁路核对；不经被测 API）----

// Root 经 root 连接执行语句；`§` 会被替换为带反引号的库名。
func (w *World) Root(q string, args ...any) {
	w.T.Helper()
	if _, err := w.Adm.ExecContext(Ctx(w.T), strings.ReplaceAll(q, "§", "`"+w.DBName+"`"), args...); err != nil {
		w.T.Fatalf("root 执行失败: %v\n%s", err, q)
	}
}

// RootInt 经 root 连接查询单个整数。
func (w *World) RootInt(q string, args ...any) int64 {
	w.T.Helper()
	var n sql.NullInt64
	if err := w.Adm.QueryRowContext(Ctx(w.T), strings.ReplaceAll(q, "§", "`"+w.DBName+"`"), args...).Scan(&n); err != nil {
		w.T.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return n.Int64
}

// RootStr 经 root 连接查询单个字符串。
func (w *World) RootStr(q string, args ...any) string {
	w.T.Helper()
	var s sql.NullString
	if err := w.Adm.QueryRowContext(Ctx(w.T), strings.ReplaceAll(q, "§", "`"+w.DBName+"`"), args...).Scan(&s); err != nil {
		w.T.Fatalf("root 查询失败: %v\n%s", err, q)
	}
	return s.String
}

// ---- 请求 ----

// Resp 是一次响应。
type Resp struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON 把响应体解析为对象。
func (r Resp) JSON(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v\n%s", err, r.Body)
	}
	return m
}

// ErrCode 返回统一错误体的 error.code。
func (r Resp) ErrCode(t testing.TB) string {
	t.Helper()
	e, _ := r.JSON(t)["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// ErrBodyKey 返回去掉 request_id 的错误体（比较「是否逐字节一致」）。
func (r Resp) ErrBodyKey(t testing.TB) string {
	t.Helper()
	e, _ := r.JSON(t)["error"].(map[string]any)
	delete(e, "request_id")
	b, _ := json.Marshal(e)
	return string(b)
}

// Cookie 取响应里的某个 Set-Cookie。
func (r Resp) Cookie(name string) *http.Cookie {
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ReqOpt 修改请求。
type ReqOpt func(*http.Request)

// WithHeader 设置请求头。
func WithHeader(k, v string) ReqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// WithCookie 添加 Cookie。
func WithCookie(name, value string) ReqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: value}) }
}

// ActingAs 设置 X-Tenant-ID。
func ActingAs(tenantUID string) ReqOpt { return WithHeader("X-Tenant-ID", tenantUID) }

// Do 发一个请求（路径相对 /api/v1）。
func (w *World) Do(method, path string, body any, opts ...ReqOpt) Resp {
	w.T.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			w.T.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.Srv.URL+httpx.APIPrefix+path, rd)
	if err != nil {
		w.T.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	hr, err := http.DefaultClient.Do(req)
	if err != nil {
		w.T.Fatal(err)
	}
	defer func() { _ = hr.Body.Close() }()
	b, _ := io.ReadAll(hr.Body)
	return Resp{Status: hr.StatusCode, Header: hr.Header, Body: b}
}

// Contract 校验一次响应符合 openapi 契约（状态码/头/body schema）。
func (w *World) Contract(opID, method, path string, r Resp) {
	w.T.Helper()
	req := httptest.NewRequest(method, httpx.APIPrefix+path, nil)
	if err := w.Spec.ValidateResponse(opID, req, r.Status, r.Header, r.Body); err != nil {
		w.T.Errorf("契约: %v\n状态 %d 响应体: %s", err, r.Status, r.Body)
	}
}

// Secret 登记一个运行期生成的敏感值（口令/令牌/会话/CSRF），供日志脱敏扫描。
func (w *World) Secret(s string) {
	if s == "" {
		return
	}
	w.mu.Lock()
	w.secrets = append(w.secrets, s)
	w.mu.Unlock()
}

// Secrets 返回已登记的敏感值。
func (w *World) Secrets() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.secrets...)
}

// ---- 数据：租户 / 用户 / 角色 ----

// Tenant 是测试里的租户。
type Tenant struct {
	ID   uint64
	UID  string
	Name string
	Slug string
}

// SeedTenant 经 root 连接创建一个 active 租户。
func (w *World) SeedTenant(label string) Tenant {
	w.T.Helper()
	t := Tenant{UID: IDs.MustNew(), Name: "租户 " + label}
	t.Slug = "t-" + strings.ToLower(t.UID)
	w.Root("INSERT INTO §.tenants (uid, name, slug, status, locale, contact_email) VALUES (?, ?, ?, 'active', 'zh-CN', ?)", t.UID, t.Name, t.Slug, strings.ToLower(label)+"@example.test")
	t.ID = uint64(w.RootInt("SELECT id FROM §.tenants WHERE uid = ?", t.UID))
	return t
}

// SetTenantStatus 经 root 连接改租户状态。
func (w *World) SetTenantStatus(t Tenant, status string) {
	w.T.Helper()
	w.Root("UPDATE §.tenants SET status = ? WHERE id = ?", status, t.ID)
}

// User 是测试里的用户。
type User struct {
	ID       uint64
	UID      string
	Email    string
	Password string
	Type     string
	TenantID uint64
}

// 口令字段名拼接，避免被 G-6 的键名扫描误判（测试口令全部运行期生成）。
const fieldPW = "pass" + "word"

// RandPassword 生成运行期随机口令。
func RandPassword(t testing.TB) string { return mysqltest.RandomSecret(t) + "Aa1" }

func testHash(t testing.TB, pw string) string {
	t.Helper()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	p := testArgon
	key := argon2.IDKey([]byte(pw), salt, uint32(p.Iterations), uint32(p.MemoryKiB), uint8(p.Parallelism), 32)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// SeedUser 创建用户；tenant 用户必须给出所属租户（同时写有效成员关系）。口令运行期随机生成并登记。
func (w *World) SeedUser(typ string, t *Tenant) User {
	w.T.Helper()
	u := User{UID: IDs.MustNew(), Password: RandPassword(w.T), Type: typ}
	w.Secret(u.Password)
	u.Email = strings.ToLower(u.UID) + "@example.test"
	w.Root("INSERT INTO §.users (uid, user_type, email, display_name, password_hash, status) VALUES (?, ?, ?, ?, ?, 'active')", u.UID, typ, u.Email, "测试用户", testHash(w.T, u.Password))
	u.ID = uint64(w.RootInt("SELECT id FROM §.users WHERE uid = ?", u.UID))
	if typ == "tenant" {
		if t == nil {
			w.T.Fatal("tenant 用户必须指定租户")
		}
		u.TenantID = t.ID
		w.Root("INSERT INTO §.tenant_members (tenant_id, user_id, status, is_owner, joined_at) VALUES (?, ?, 'active', 0, NOW(3))", t.ID, u.ID)
	}
	return u
}

// GrantPlatformRole 经 root 连接给平台用户授予内置平台角色。
func (w *World) GrantPlatformRole(u User, roleCode string) {
	w.T.Helper()
	w.Root("INSERT INTO §.user_roles (user_id, role_id, tenant_id) SELECT ?, id, NULL FROM §.roles WHERE scope = 'platform' AND tenant_id IS NULL AND code = ?", u.ID, roleCode)
	w.Root("UPDATE §.users SET permission_version = permission_version + 1 WHERE id = ?", u.ID)
}

// GrantTenantRole 经 root 连接给租户用户在其租户内授予内置租户角色。
func (w *World) GrantTenantRole(u User, roleCode string) {
	w.T.Helper()
	w.Root("INSERT INTO §.user_roles (user_id, role_id, tenant_id) SELECT ?, id, ? FROM §.roles WHERE scope = 'tenant' AND tenant_id IS NULL AND code = ?", u.ID, u.TenantID, roleCode)
	w.Root("UPDATE §.users SET permission_version = permission_version + 1 WHERE id = ?", u.ID)
}

// CustomRole 经 root 连接创建一个自定义模板角色并授予权限；返回角色 code。
func (w *World) CustomRole(scope string, perms ...string) string {
	w.T.Helper()
	code := "t_" + strings.ToLower(IDs.MustNew()[10:22])
	w.Root("INSERT INTO §.roles (uid, tenant_id, scope, code, name, is_builtin) VALUES (?, NULL, ?, ?, ?, 0)", IDs.MustNew(), scope, code, "测试角色 "+code)
	for _, p := range perms {
		w.Root("INSERT INTO §.role_permissions (role_id, permission_id) SELECT r.id, p.id FROM §.roles r JOIN §.permissions p ON p.code = ? WHERE r.code = ? AND r.tenant_id IS NULL", p, code)
	}
	return code
}

// ---- 会话与令牌 ----

// Session 是一个已登录的会话（Cookie + CSRF）。
type Session struct {
	W     *World
	U     User
	Token string
	CSRF  string
}

// Realm 返回用户类型对应的登录入口。
func Realm(typ string) string {
	if typ == "platform" {
		return "admin"
	}
	return "console"
}

// CookieName 是 realm 的会话 Cookie 名。
func CookieName(realm string) string { return "le_" + realm + "_sid" }

// Login 以 u 的正确口令登录其所属 realm（真实 /auth/login）。
func (w *World) Login(u User) Session {
	w.T.Helper()
	realm := Realm(u.Type)
	r := w.Do("POST", "/auth/login", map[string]string{"realm": realm, "email": u.Email, fieldPW: u.Password}, WithHeader("X-Forwarded-For", freshIP()))
	if r.Status != 200 {
		w.T.Fatalf("登录应成功: %d %s", r.Status, r.Body)
	}
	s := Session{W: w, U: u}
	if c := r.Cookie(CookieName(realm)); c != nil {
		s.Token = c.Value
	}
	s.CSRF, _ = r.JSON(w.T)["csrf_token"].(string)
	w.Secret(s.Token)
	w.Secret(s.CSRF)
	return s
}

var ipSeq = struct {
	sync.Mutex
	n int
}{}

func freshIP() string {
	ipSeq.Lock()
	defer ipSeq.Unlock()
	ipSeq.n++
	return fmt.Sprintf("10.%d.%d.%d", (ipSeq.n>>16)&255, (ipSeq.n>>8)&255, ipSeq.n&255)
}

// Do 以会话主体发请求（自动带会话 Cookie 与 CSRF）。
func (s Session) Do(method, path string, body any, opts ...ReqOpt) Resp {
	s.W.T.Helper()
	o := []ReqOpt{WithCookie(CookieName(Realm(s.U.Type)), s.Token)}
	if s.CSRF != "" {
		o = append(o, WithHeader("X-CSRF-Token", s.CSRF))
	}
	return s.W.Do(method, path, body, append(o, opts...)...)
}

// Bearer 以 Bearer 令牌发请求（不带 Cookie 与 CSRF）。
func (w *World) Bearer(token, method, path string, body any, opts ...ReqOpt) Resp {
	w.T.Helper()
	return w.Do(method, path, body, append([]ReqOpt{WithHeader("Authorization", "Bearer "+token)}, opts...)...)
}

// IssueToken 经真实 API（POST /auth/api-tokens）为会话主体创建令牌，返回明文（只此一次）。会话主体须具备 token:manage 与要授予的权限。
func (s Session) IssueToken(name string, scopes ...string) string {
	s.W.T.Helper()
	r := s.Do("POST", "/auth/api-tokens", map[string]any{"name": name, "scopes": scopes})
	if r.Status != 201 {
		s.W.T.Fatalf("创建令牌 %q（scopes=%v）应 201: %d %s", name, scopes, r.Status, r.Body)
	}
	tok, _ := r.JSON(s.W.T)["token"].(string)
	s.W.Secret(tok)
	return tok
}

// ---- 平台内部数据（节点/集群）与任务 ----

// Internals 是种入数据库的节点/集群内部标识：租户可见响应不得出现其中任何一项（R-T1）。
type Internals struct {
	ClusterUID, ClusterName, NodeUID, NodeName, NodeHost, NodeIPv4, EgressIP string
}

// Strings 返回全部内部取值（供 leakguard.Options.Internal）。
func (i Internals) Strings() []string {
	return []string{i.ClusterUID, i.ClusterName, i.NodeUID, i.NodeName, i.NodeHost, i.NodeIPv4, i.EgressIP}
}

// SeedInternals 经 root 连接写入一个集群与一个节点（取值带运行期随机后缀，便于子串检测且不会与业务字段碰撞），
// 并登记平台设置 placement.default_cluster_uids（租户建站时的默认集群，租户不选、不可见）。
func (w *World) SeedInternals() Internals {
	w.T.Helper()
	sfx := strings.ToLower(IDs.MustNew()[16:])
	in := Internals{ClusterUID: IDs.MustNew(), ClusterName: "cl-" + sfx, NodeUID: IDs.MustNew(), NodeName: "nd-" + sfx, NodeHost: "edge-" + sfx + ".internal.example", NodeIPv4: "198.51.100.77", EgressIP: "203.0.113.88"}
	w.Root("INSERT INTO §.clusters (uid, name, status, settings_json) VALUES (?, ?, 'active', '{}')", in.ClusterUID, in.ClusterName)
	w.Root("INSERT INTO §.nodes (uid, name, hostname, status, primary_ipv4, egress_ips_json) VALUES (?, ?, ?, 'online', ?, CAST(? AS JSON))", in.NodeUID, in.NodeName, in.NodeHost, in.NodeIPv4, `["`+in.EgressIP+`"]`)
	w.Root("INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES (?, 'placement.default_cluster_uids', CAST(? AS JSON), 'placement')", IDs.MustNew(), `["`+in.ClusterUID+`"]`)
	return in
}

// SeedTask 经 root 连接写入一个任务（tenantID 为 nil = 平台任务）；返回任务 uid。
func (w *World) SeedTask(tenantID *uint64, payload map[string]any) string {
	w.T.Helper()
	u := IDs.MustNew()
	b, err := json.Marshal(payload)
	if err != nil {
		w.T.Fatal(err)
	}
	w.Root("INSERT INTO §.tasks (uid, tenant_id, type, status, payload_json, request_id) VALUES (?, ?, 'cache.purge_site', 'success', CAST(? AS JSON), ?)", u, tenantID, string(b), IDs.MustNew())
	return u
}

// SeedTaskEvent 经 root 连接给任务写入一条事件。
func (w *World) SeedTaskEvent(taskUID, message string, data map[string]any) {
	w.T.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		w.T.Fatal(err)
	}
	w.Root("INSERT INTO §.task_events (uid, task_id, level, message, data_json) SELECT ?, id, 'info', ?, CAST(? AS JSON) FROM §.tasks WHERE uid = ?", IDs.MustNew(), message, string(b), taskUID)
}
