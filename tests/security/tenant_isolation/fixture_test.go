package tenantisolation

import (
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// fixture 是矩阵的数据：租户 A/B（各有 tenant_owner 会话与 API Token）、状态不可用的租户、平台管理员（含 impersonate）、平台审计员（无 impersonate）、
// 平台令牌（带 impersonate），以及种入数据库的节点/集群内部标识（R-T1 的反例数据）。全部经 root 连接写库或真实 API 创建，口令/令牌运行期生成。
type fixture struct {
	w  *apistack.World
	in apistack.Internals

	A, B, S, Closed, Pending apistack.Tenant

	sa, sb, sAdmin, sAud apistack.Session
	tokA, tokB, tokP     string
	tokPNoImp            string
}

func newFixture(t *testing.T, opts ...apistack.Options) *fixture {
	t.Helper()
	w := apistack.New(t, opts...)
	f := &fixture{w: w, in: w.SeedInternals()}
	f.A, f.B = w.SeedTenant("A"), w.SeedTenant("B")
	f.S, f.Closed, f.Pending = w.SeedTenant("suspended"), w.SeedTenant("closed"), w.SeedTenant("pending")
	w.SetTenantStatus(f.S, "suspended")
	w.SetTenantStatus(f.Closed, "closed")
	w.SetTenantStatus(f.Pending, "pending")

	tenantSession := func(tn apistack.Tenant) apistack.Session {
		u := w.SeedUser("tenant", &tn)
		w.GrantTenantRole(u, "tenant_owner")
		return w.Login(u)
	}
	f.sa, f.sb = tenantSession(f.A), tenantSession(f.B)
	pu := w.SeedUser("platform", nil)
	w.GrantPlatformRole(pu, "platform_admin")
	f.sAdmin = w.Login(pu)
	au := w.SeedUser("platform", nil)
	w.GrantPlatformRole(au, "platform_auditor")
	f.sAud = w.Login(au)

	// 令牌的作用域：已注册的 tenant|either 路由所需权限的并集（排除令牌管理自身）；平台令牌另加 tenant:impersonate。
	need := map[string]bool{}
	for _, m := range w.Router.Routes() {
		if m.Auth == httpx.AuthRequired && isTenantFacing(m.TenantScope) {
			for _, p := range m.Permissions {
				if p != "token:manage" {
					need[p] = true
				}
			}
		}
	}
	scopes := make([]string, 0, len(need))
	for p := range need {
		scopes = append(scopes, p)
	}
	sort.Strings(scopes)
	f.tokA, f.tokB = f.sa.IssueToken("matrix-a", scopes...), f.sb.IssueToken("matrix-b", scopes...)
	f.tokP = f.sAdmin.IssueToken("matrix-platform", append([]string{"tenant:impersonate"}, scopes...)...)
	f.tokPNoImp = f.sAdmin.IssueToken("matrix-platform-noimp", scopes...)
	return f
}

type doer func(method, path string, body any, opts ...apistack.ReqOpt) apistack.Resp

// as 返回某身份的请求函数。
func (f *fixture) as(ident string) doer {
	switch ident {
	case IdTenantA:
		return f.sa.Do
	case IdTenantB:
		return f.sb.Do
	case IdTokenA:
		return func(m, p string, b any, o ...apistack.ReqOpt) apistack.Resp { return f.w.Bearer(f.tokA, m, p, b, o...) }
	case IdTokenB:
		return func(m, p string, b any, o ...apistack.ReqOpt) apistack.Resp { return f.w.Bearer(f.tokB, m, p, b, o...) }
	case IdPlatformAdmin:
		return f.sAdmin.Do
	case IdPlatformNoImp:
		return f.sAud.Do
	case IdPlatformToken:
		return func(m, p string, b any, o ...apistack.ReqOpt) apistack.Resp { return f.w.Bearer(f.tokP, m, p, b, o...) }
	case IdPlatformTokenNoImp:
		return func(m, p string, b any, o ...apistack.ReqOpt) apistack.Resp {
			return f.w.Bearer(f.tokPNoImp, m, p, b, o...)
		}
	case IdAnonymous:
		return f.w.Do
	}
	panic("未知身份 " + ident)
}

// isTenantIdent 报告身份是否是租户主体（会话或令牌）。
func isTenantIdent(ident string) bool {
	return ident == IdTenantA || ident == IdTenantB || ident == IdTokenA || ident == IdTokenB
}

// ownTenant 返回租户主体所属租户；平台主体的「自己的租户」情形固定取租户 A。
func (f *fixture) ownTenant(ident string) apistack.Tenant {
	if ident == IdTenantB || ident == IdTokenB {
		return f.B
	}
	return f.A
}

// target 返回某身份在某情形下的 X-Tenant-ID（空串 = 不带）。
func (f *fixture) target(ident, sit string) string {
	switch sit {
	case SitNone:
		return ""
	case SitOwn:
		return f.ownTenant(ident).UID
	case SitOther:
		if f.ownTenant(ident).UID == f.A.UID {
			return f.B.UID
		}
		return f.A.UID
	case SitMissing:
		return apistack.IDs.MustNew()
	case SitSuspended:
		return f.S.UID
	}
	panic("未知情形 " + sit)
}

func (f *fixture) opts(ident, sit string) []apistack.ReqOpt {
	if tu := f.target(ident, sit); tu != "" {
		return []apistack.ReqOpt{apistack.ActingAs(tu)}
	}
	return nil
}

// errCode 是行期望状态码对应的统一错误码。
func errCode(status int) string {
	switch status {
	case 400:
		return "validation.failed"
	case 401:
		return "auth.unauthenticated"
	case 403:
		return "auth.forbidden"
	case 404:
		return "resource.not_found"
	}
	return ""
}

// findSecrets 返回日志里出现的敏感值（G-5：运行期生成的口令/令牌/会话/CSRF 不得出现在日志里）。
func findSecrets(logs string, secrets []string) []string {
	var hits []string
	for _, s := range secrets {
		if len(s) >= 8 && strings.Contains(logs, s) {
			hits = append(hits, s[:4]+"…")
		}
	}
	return hits
}

func tenantScopeFrom(c *gin.Context) (tenantscope.Scope, bool) {
	return tenantscope.FromContext(c.Request.Context())
}
