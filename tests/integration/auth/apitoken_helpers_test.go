package authit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	goredis "github.com/redis/go-redis/v9"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/redisx"
)

// Story 1.19（API Token）集成测试的共用助手。被测系统是真实认证模块 + httpx 固定中间件链 + 真实钩子装配（auth.Module.Hooks），
// 跑在自拉起的临时 MySQL/Redis 上；令牌/口令全部在运行期生成（G-6）。

// tokenRe 是令牌的形态：le_<8 位前缀>_<43 位 base64url>。
var tokenRe = regexp.MustCompile(`^le_([a-z2-7]{8})_([A-Za-z0-9_-]{43})$`)

// probeRoutes 注册三条真实的业务探针路由（经真实 Router.Register，走真实钩子链），代表「平台资源」与「租户资源」：
// /probe/nodes（platform，node:read —— 代表 /nodes 类平台资源）、/probe/sites（tenant，site:read）、POST /probe/sites（tenant，site:write）。
func probeRoutes(r *httpx.Router) {
	reg := func(id, method, path string, scope httpx.TenantScope, perm string) {
		m := httpx.RouteMeta{OperationID: id, Method: method, Path: path, Auth: httpx.AuthRequired, Permissions: []string{perm}, TenantScope: scope, Phase: "M1"}
		if err := r.Register(m, func(c *gin.Context) { httpx.OK(c, map[string]bool{"reached": true}) }); err != nil {
			panic(err)
		}
	}
	reg("probeNodes", "GET", "/probe/nodes", httpx.ScopePlatform, "node:read")
	reg("probeSitesRead", "GET", "/probe/sites", httpx.ScopeTenant, "site:read")
	reg("probeSitesWrite", "POST", "/probe/sites", httpx.ScopeTenant, "site:write")
}

// newTokStack 创建带探针路由与可推进时钟的被测系统。
func newTokStack(t *testing.T, o ...stackOpts) *stack {
	t.Helper()
	opt := stackOpts{fakeClock: true}
	if len(o) > 0 {
		opt = o[0]
		opt.fakeClock = true
	}
	opt.routes = probeRoutes
	return newStack(t, opt)
}

// as 以会话主体（Admin 或 Console，按用户类型）发请求。
func (s *stack) as(ss session, method, path string, body any, opts ...reqOpt) resp {
	s.t.Helper()
	return s.authed(realmOf(ss.u.Type), ss.token, ss.csrf, method, path, body, opts...)
}

// bearer 以 Bearer 令牌发请求（不带 Cookie 与 CSRF）。
func (s *stack) bearer(tok, method, path string, body any, opts ...reqOpt) resp {
	s.t.Helper()
	return s.do(method, path, body, append([]reqOpt{withHeader("Authorization", "Bearer "+tok)}, opts...)...)
}

// tenantSession 创建租户用户（含租户与成员关系）、授予租户内内置角色、登录 Console。
func (s *stack) tenantSession(roleCode string) session {
	s.t.Helper()
	u := s.seedUser("tenant")
	if roleCode != "" {
		s.grantTenantRole(u, roleCode)
	}
	_, token, csrf := s.login(u, freshIP())
	return session{u: u, token: token, csrf: csrf}
}

// issued 是一次创建/轮换的结果。
type issued struct {
	Token  string
	UID    string
	Prefix string
	Secret string
	JSON   map[string]any
}

func tokBody(name string, scopes ...string) map[string]any {
	return map[string]any{"name": name, "scopes": scopes}
}

// issue 创建令牌（要求 201 与契约相符），校验令牌形态。
func (s *stack) issue(ss session, body map[string]any, opts ...reqOpt) issued {
	s.t.Helper()
	r := s.as(ss, "POST", "/auth/api-tokens", body, opts...)
	if r.Status != 201 {
		s.t.Fatalf("创建令牌应 201: %d %s", r.Status, r.Body)
	}
	s.contract("createApiToken", "POST", "/auth/api-tokens", r)
	return s.parseIssued(r)
}

func (s *stack) parseIssued(r resp) issued {
	s.t.Helper()
	j := r.JSON(s.t)
	tok, _ := j["token"].(string)
	m := tokenRe.FindStringSubmatch(tok)
	if m == nil {
		s.t.Fatalf("令牌形态应为 le_<8位前缀>_<随机>: %q", tok)
	}
	uid, _ := j["uid"].(string)
	if j["token_prefix"] != m[1] {
		s.t.Fatalf("token_prefix 应等于令牌前缀: %v vs %s", j["token_prefix"], m[1])
	}
	return issued{Token: tok, UID: uid, Prefix: m[1], Secret: m[2], JSON: j}
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// tokenKey 是「哈希 → 令牌 id」缓存键（与 service.tokenKey 同一格式）。
func (s *stack) tokenKey(tok string) redisx.Key {
	s.t.Helper()
	k, err := s.rc.Keyspace().Key("auth", "apitoken", sha256hex(tok))
	if err != nil {
		s.t.Fatal(err)
	}
	return k
}

// requireStatus 断言 Bearer 请求的状态码。
func (s *stack) requireBearer(tok, method, path string, want int, what string, opts ...reqOpt) resp {
	s.t.Helper()
	r := s.bearer(tok, method, path, nil, opts...)
	if r.Status != want {
		s.t.Fatalf("%s: 期望 %d，得到 %d %s", what, want, r.Status, r.Body)
	}
	return r
}

// tokenRow 经 root 连接读令牌行（按 uid）。
func (s *stack) tokenRow(uid string, col string) string {
	s.t.Helper()
	return s.e.rootStr(s.t, "SELECT CAST("+col+" AS CHAR) FROM §.api_tokens WHERE uid = ?", uid)
}

// opCustomerWith 创建持有指定权限（含 token:manage）的自定义平台角色的 Admin 会话。
func (s *stack) opWithTokenManage(perms ...string) (session, string) {
	s.t.Helper()
	code := s.customRole("platform", append([]string{"token:manage"}, perms...)...)
	return s.adminSession(code), code
}

// redisDump 返回 Redis 里全部键与值（明文扫描用）。
func (s *stack) redisDump() map[string]string {
	s.t.Helper()
	rdb := goredis.NewClient(&goredis.Options{Addr: s.redis.Addr()})
	defer func() { _ = rdb.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	keys, err := rdb.Keys(ctx, "*").Result()
	if err != nil {
		s.t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range keys {
		v, _ := rdb.Get(ctx, k).Result()
		out[k] = v
	}
	return out
}

// errorBodyKey 取错误响应里与请求无关的部分（去掉 request_id），用于比较「不同原因的 401 是否逐字节相同」。
func errorBodyKey(t testing.TB, r resp) string {
	t.Helper()
	e, _ := r.JSON(t)["error"].(map[string]any)
	delete(e, "request_id")
	return toJSON(t, e)
}

func containsAny(hay string, needles ...string) string {
	for _, n := range needles {
		if n != "" && strings.Contains(hay, n) {
			return n
		}
	}
	return ""
}
