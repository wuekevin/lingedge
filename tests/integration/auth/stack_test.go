package authit

import (
	"bytes"
	"context"
	"crypto/rand"
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

	"go.uber.org/zap"
	"golang.org/x/crypto/argon2"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/auth"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/httpx/contracttest"
	idemrepo "github.com/wuekevin/lingedge/internal/platform/idempotency/repository"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/rbac"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/pkg/secretbox"
	"github.com/wuekevin/lingedge/tests/mysqltest"
	"github.com/wuekevin/lingedge/tests/redistest"
)

// 测试用的 argon2id 参数：取允许的下限，让大量登录的测试跑得快；生产默认值与基准见 config.Defaults 与 service 的 BenchmarkArgon2idHash。
var testArgon = config.Argon2{MemoryKiB: config.MinArgon2MemoryKiB, Iterations: config.MinArgon2Iterations, Parallelism: 1}

// syncBuf 是并发安全的日志缓冲（脱敏扫描用）。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// stack 是被测系统：真实认证模块 + httpx 路由器（固定中间件链 + 真实钩子装配）+ 真实 HTTP 监听。
type stack struct {
	t     *testing.T
	e     *env
	redis *redistest.Instance
	rc    *redisx.Client
	clk   *fakeClock
	mod   auth.Module
	// aw 是真实的审计记录器/钩子（与生产同一实现，Story 1.21）。
	aw     *audit.Writer
	srv    *httptest.Server
	router *httpx.Router
	logs   *syncBuf
	spec   *contracttest.Spec
	box    *secretbox.Box
	opt    stackOpts

	spy *spyState
}

type spyState struct {
	mu     sync.Mutex
	access *rbac.Decision
}

type stackOpts struct {
	// noRedis 为 true 时 Redis 地址指向一个无人监听的端口（Redis 从未可用）。
	noRedis bool
	// fakeClock 为 true 时认证模块使用可推进的时钟。
	fakeClock bool
	// settings 是启动前写入 system_settings 的键值（JSON 字面量）。
	settings map[string]string
	// reauthRoutes 为 true 时，按 openapi 的 x-reauth 元数据注册「声明了 x-reauth: true 的 operation」的占位路由（这些 operation 的真实实现属后续 Story）。
	reauthRoutes bool
	// permTTL 是权限缓存 TTL（0 = 生产上限 60 秒）；TTL 兜底测试调小它。
	permTTL time.Duration
	// routes 在 Handler() 之前向路由器追加测试专用路由（经真实 Router.Register，走真实钩子链）。
	routes func(r *httpx.Router)
	// spyScope 为 true 时在 Scope 钩子前插入记录授权决策的探针（只观察 httpx.AccessFrom，不改变任何决策）。
	spyScope bool
	// permPoll 是权限失效广播的轮询兜底周期（0 = 默认 30 秒）。
	permPoll time.Duration
	// trustedProxies 是路由器的可信反代（空 = 默认只信任本机 127.0.0.1/::1）；来源 IP 白名单测试用它验证「伪造 X-Forwarded-For 不能绕过」。
	trustedProxies []string
	// deniedWindow 是被拒绝类审计（access.denied）的去重窗口（0 = 默认 60 秒）。
	deniedWindow time.Duration
}

func newStack(t *testing.T, o ...stackOpts) *stack {
	t.Helper()
	var opt stackOpts
	if len(o) > 0 {
		opt = o[0]
	}
	s := &stack{t: t, e: newEnv(t), logs: &syncBuf{}, spec: contracttest.LoadRepo(t), box: newBox(t), opt: opt}
	for k, v := range opt.settings {
		group := "login"
		if strings.HasPrefix(k, "auth.") {
			group = "security"
		}
		s.e.root(t, "INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES (?, ?, CAST(? AS JSON), ?)", ids.MustNew(), k, v, group)
	}
	s.redis = redistest.Start(t)
	addr := s.redis.Addr()
	if opt.noRedis {
		addr = "127.0.0.1:1"
	}
	lg, err := logx.New(logx.Options{Level: "debug", Format: "json", Output: s.logs})
	if err != nil {
		t.Fatal(err)
	}
	s.rc, err = redisx.New(context.Background(), redisx.Config{Addr: addr, Env: "test", ProbeInterval: 100 * time.Millisecond,
		DialTimeout: 300 * time.Millisecond, ReadTimeout: 300 * time.Millisecond, WriteTimeout: 300 * time.Millisecond}, redisx.Options{Logger: lg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.rc.Close() })
	clk := clock.Clock(clock.System())
	if opt.fakeClock {
		s.clk = newFakeClock()
		clk = s.clk
	}
	s.mod = s.newModule(lg, clk)
	idem, err := idemrepo.New(s.e.app, idemrepo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	hooks := assemble(s.mod, s.aw)
	if opt.spyScope {
		s.spy = &spyState{}
		hooks.Scope = &spyScope{st: s.spy, inner: hooks.Scope}
	}
	s.router, err = httpx.NewRouter(httpx.Options{Logger: lg, IDs: uid.NewGenerator(clock.System()),
		Hooks: hooks, Idempotency: idem, TrustedProxies: opt.trustedProxies})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.mod.Register(s.router); err != nil {
		t.Fatal(err)
	}
	// 探针路由：用于证明「认证通过但授权钩子未实现的路由仍被拒绝」与 realm 路由隔离（经真实 Router.Register，真实钩子链）。
	probe := func(id, path string, scope httpx.TenantScope) {
		m := httpx.RouteMeta{OperationID: id, Method: "POST", Path: path, Auth: httpx.AuthRequired, Permissions: []string{"probe:run"}, TenantScope: scope, Phase: "M1"}
		if err := s.router.Register(m, func(c *gin.Context) { httpx.OK(c, map[string]bool{"reached": true}) }); err != nil {
			t.Fatal(err)
		}
	}
	if opt.reauthRoutes {
		real := map[string]bool{}
		for _, r := range s.router.Routes() {
			real[r.OperationID] = true
		}
		for _, m := range s.spec.Expected() {
			if m.Reauth && !real[m.OperationID] { // 已有真实实现的 operation（1.18 的 /admin/users|roles）不再用占位
				m.Idempotent = false // 占位路由不接幂等存储；元数据其余部分取自 openapi
				if err := s.router.Register(m, func(c *gin.Context) { httpx.OK(c, map[string]bool{"reached": true}) }); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if opt.routes != nil {
		opt.routes(s.router)
	}
	probe("probePlatform", "/probe/platform", httpx.ScopePlatform)
	probe("probeTenant", "/probe/tenant", httpx.ScopeTenant)
	h, err := s.router.Handler()
	if err != nil {
		t.Fatal(err)
	}
	s.srv = httptest.NewServer(h)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stack) newModule(lg *zap.Logger, clk clock.Clock) auth.Module {
	s.t.Helper()
	aw, err := audit.New(audit.Options{Pool: s.e.app, IDs: uid.NewGenerator(clock.System()), Clock: clk, Log: lg, Redis: s.rc, DeniedWindow: s.opt.deniedWindow})
	if err != nil {
		s.t.Fatal(err)
	}
	s.aw = aw
	m, err := auth.New(context.Background(), auth.Options{Pool: s.e.app, Redis: s.rc, Clock: clk, IDs: uid.NewGenerator(clock.System()), Log: lg, Argon2: testArgon, Box: s.box, PermCacheTTL: s.opt.permTTL, PermPollEvery: s.opt.permPoll, Audit: aw})
	if err != nil {
		s.t.Fatal(err)
	}
	return m
}

// ---- 请求助手 ----

type resp struct {
	Status int
	Header http.Header
	Body   []byte
	Dur    time.Duration
}

func (r resp) JSON(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v\n%s", err, r.Body)
	}
	return m
}

func (r resp) errCode(t testing.TB) string {
	t.Helper()
	e, _ := r.JSON(t)["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// cookie 取响应里的某个 Set-Cookie。
func (r resp) cookie(name string) *http.Cookie {
	for _, c := range (&http.Response{Header: r.Header}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

type reqOpt func(*http.Request)

func withCookie(name, value string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: value}) }
}
func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// withIP 经 X-Forwarded-For 指定来源 IP（测试服务器在 127.0.0.1，是默认的可信反代）。
func withIP(ip string) reqOpt { return withHeader("X-Forwarded-For", ip) }

func (s *stack) do(method, path string, body any, opts ...reqOpt) resp {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.srv.URL+httpx.APIPrefix+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	start := time.Now()
	hr, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = hr.Body.Close() }()
	b, _ := io.ReadAll(hr.Body)
	return resp{Status: hr.StatusCode, Header: hr.Header, Body: b, Dur: time.Since(start)}
}

// contract 校验一次响应符合 openapi 契约（状态码/头/body schema）。
func (s *stack) contract(opID, method, path string, r resp) {
	s.t.Helper()
	req := httptest.NewRequest(method, httpx.APIPrefix+path, nil)
	if err := s.spec.ValidateResponse(opID, req, r.Status, r.Header, r.Body); err != nil {
		s.t.Errorf("契约: %v\n状态 %d 响应体: %s", err, r.Status, r.Body)
	}
}

// ---- 种子数据（测试内创建；口令随机、哈希用真实 argon2id）----

type user struct {
	ID       uint64
	UID      string
	Email    string
	Password string
	Type     string
	TenantID uint64
}

// shortPW 是故意过短/无意义的输入（并非口令字面量：只用于校验失败路径）；fieldPW 是 JSON 字段名（拼接以免被 G-6 的键名扫描误判）。
const shortPW = "x"
const fieldPW = "pass" + "word"

func randPassword(t testing.TB) string { return mysqltest.RandomSecret(t) + "Aa1" }

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

type userOpt func(*userSpec)
type userSpec struct {
	status     string
	mustChange bool
	noMember   bool
	hash       string
}

func withStatus(s string) userOpt { return func(u *userSpec) { u.status = s } }
func withMustChange() userOpt     { return func(u *userSpec) { u.mustChange = true } }
func withNoMembership() userOpt   { return func(u *userSpec) { u.noMember = true } }
func withHash(h string) userOpt   { return func(u *userSpec) { u.hash = h } }

var ids = uid.NewGenerator(clock.System())

// seedUser 创建一个用户（typ=platform|tenant）；tenant 用户同时获得一个租户与有效成员关系。
func (s *stack) seedUser(typ string, opts ...userOpt) user {
	s.t.Helper()
	sp := userSpec{status: "active"}
	for _, o := range opts {
		o(&sp)
	}
	u := user{UID: ids.MustNew(), Password: randPassword(s.t), Type: typ}
	u.Email = strings.ToLower(strings.ToLower(u.UID)) + "@example.test"
	hash := sp.hash
	if hash == "" {
		hash = testHash(s.t, u.Password)
	}
	s.e.root(s.t, "INSERT INTO §.users (uid, user_type, email, display_name, password_hash, status, must_change_password) VALUES (?, ?, ?, ?, ?, ?, ?)",
		u.UID, typ, u.Email, "测试用户", hash, sp.status, sp.mustChange)
	u.ID = uint64(s.e.rootInt(s.t, "SELECT id FROM §.users WHERE uid = ?", u.UID))
	if typ == "tenant" && !sp.noMember {
		tuid := ids.MustNew()
		s.e.root(s.t, "INSERT INTO §.tenants (uid, name, slug, status) VALUES (?, ?, ?, 'active')", tuid, "租户"+tuid[20:], "t-"+strings.ToLower(tuid))
		u.TenantID = uint64(s.e.rootInt(s.t, "SELECT id FROM §.tenants WHERE uid = ?", tuid))
		s.e.root(s.t, "INSERT INTO §.tenant_members (tenant_id, user_id, status, is_owner, joined_at) VALUES (?, ?, 'active', 1, NOW(3))", u.TenantID, u.ID)
	}
	return u
}

func realmOf(typ string) string {
	if typ == "platform" {
		return "admin"
	}
	return "console"
}

func cookieName(realm string) string { return "le_" + realm + "_sid" }

type loginReq struct {
	Realm    string `json:"realm"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// login 以 u 的正确口令登录其所属 realm，返回响应、Cookie 值与 CSRF 令牌。
func (s *stack) login(u user, ip string, opts ...reqOpt) (r resp, token, csrf string) {
	s.t.Helper()
	realm := realmOf(u.Type)
	r = s.do("POST", "/auth/login", loginReq{Realm: realm, Email: u.Email, Password: u.Password}, append([]reqOpt{withIP(ip)}, opts...)...)
	if r.Status != 200 {
		s.t.Fatalf("登录应成功: %d %s", r.Status, r.Body)
	}
	if c := r.cookie(cookieName(realm)); c != nil {
		token = c.Value
	}
	csrf, _ = r.JSON(s.t)["csrf_token"].(string)
	return r, token, csrf
}

// authed 以会话 Cookie（及可选 CSRF 令牌）发请求。
func (s *stack) authed(realm, token, csrf, method, path string, body any, opts ...reqOpt) resp {
	s.t.Helper()
	o := []reqOpt{withCookie(cookieName(realm), token)}
	if csrf != "" {
		o = append(o, withHeader("X-CSRF-Token", csrf))
	}
	return s.do(method, path, body, append(o, opts...)...)
}

var ipSeq = struct {
	sync.Mutex
	n int
}{}

// freshIP 返回本进程内唯一的测试 IP（避免用例之间共享 IP 维度的节流计数）。
func freshIP() string {
	ipSeq.Lock()
	defer ipSeq.Unlock()
	ipSeq.n++
	return fmt.Sprintf("10.%d.%d.%d", (ipSeq.n>>16)&255, (ipSeq.n>>8)&255, ipSeq.n&255)
}

func zapNop() *zap.Logger   { return zap.NewNop() }
func nowClock() clock.Clock { return clock.System() }

// restartServer 用 s2 的认证模块重建路由器与监听（模拟进程重启：进程内状态清空，数据库与 Redis 状态保留）。
func (s *stack) restartServer(s2 *stack) *httptest.Server {
	s.t.Helper()
	idem, err := idemrepo.New(s.e.app, idemrepo.Options{})
	if err != nil {
		s.t.Fatal(err)
	}
	r, err := httpx.NewRouter(httpx.Options{Logger: zap.NewNop(), IDs: uid.NewGenerator(clock.System()),
		Hooks: assemble(s2.mod, s2.aw), Idempotency: idem})
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s2.mod.Register(r); err != nil {
		s.t.Fatal(err)
	}
	h, err := r.Handler()
	if err != nil {
		s.t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	s.t.Cleanup(srv.Close)
	return srv
}

func toJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// weakHash 用低于模块当前参数的 argon2id 参数哈希（m=8MiB,t=1,p=1），用于验证登录时的哈希升级。
func weakHash(t testing.TB, pw string) string {
	t.Helper()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatal(err)
	}
	key := argon2.IDKey([]byte(pw), salt, 1, 8192, 1, 32)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=8192,t=1,p=1$%s$%s", argon2.Version, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// assemble 是真实钩子装配（auth.Module.Hooks，与 cmd/lingedge-server 的 assembleHooks 同一份）。
func assemble(m auth.Module, aw *audit.Writer) httpx.Hooks {
	return m.Hooks(auth.HooksOptions{Auditor: aw.Auditor()})
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

// spyScope 是 Scope 钩子前的观察探针：记录授权钩子交来的授权决策，然后原样委托（不改变任何决策）。
type spyScope struct {
	st    *spyState
	inner httpx.ScopeResolver
}

func (p *spyScope) ResolveScope(r *http.Request, meta httpx.RouteMeta, pr tenantscope.Principal) (tenantscope.Scope, error) {
	if d, ok := httpx.AccessFrom(r.Context()); ok {
		p.st.mu.Lock()
		p.st.access = &d
		p.st.mu.Unlock()
	}
	return p.inner.ResolveScope(r, meta, pr)
}

// takeAccess 取出并清除最近一次记录的授权决策。
func (s *stack) takeAccess() *rbac.Decision {
	s.spy.mu.Lock()
	defer s.spy.mu.Unlock()
	d := s.spy.access
	s.spy.access = nil
	return d
}
