package tenantisolation

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// acting-as 审计上下文（Story 1.20 AC1）：Admin 代租户操作时，钩子链上下文携带操作者与目标租户（httpx.ActingAsFrom，供审计模块 Story 1.21 直接消费），
// 并有结构化日志；audit_logs 属 Story 1.21，这里不写也不假装已写。
//
// 用测试专用的探针路由（经真实 Router.Register，走真实钩子链）观察上下文：它们只在本测试的装配里存在。
func TestActingAsCarriesAuditContext(t *testing.T) {
	probe := func(r *httpx.Router) {
		reg := func(id, method string, audit bool) {
			m := httpx.RouteMeta{OperationID: id, Method: method, Path: "/probe/acting", Auth: httpx.AuthRequired, Permissions: []string{"task:read"}, TenantScope: httpx.ScopeEither, Audit: audit, Phase: "M1"}
			if err := r.Register(m, func(c *gin.Context) {
				sc, _ := tenantScopeFrom(c)
				a, acting := httpx.ActingAsFrom(c.Request.Context())
				httpx.OK(c, map[string]any{"platform": sc.IsPlatform(), "tenant_id": sc.TenantID(), "scope_acting": sc.ActingAs(), "actor": sc.ActorID(),
					"acting": acting, "ctx_actor": a.ActorID, "ctx_tenant_id": a.TenantID, "ctx_tenant_uid": a.TenantUID, "ctx_op": a.OperationID})
			}); err != nil {
				t.Fatal(err)
			}
		}
		reg("probeActingRead", "GET", false)
		reg("probeActingWrite", "POST", true)
	}
	f := newFixture(t, apistack.Options{Routes: probe, ExtraAudited: []string{"probeActingWrite"}})
	w := f.w

	// 平台管理员 acting-as 租户 A：上下文同时有操作者与目标租户。
	r := f.sAdmin.Do("GET", "/probe/acting", nil, apistack.ActingAs(f.A.UID))
	j := r.JSON(t)
	if r.Status != 200 || j["acting"] != true || j["scope_acting"] != true || j["ctx_tenant_uid"] != f.A.UID || j["ctx_op"] != "probeActingRead" ||
		uint64(j["ctx_actor"].(float64)) != f.sAdmin.U.ID || uint64(j["ctx_tenant_id"].(float64)) != f.A.ID || uint64(j["tenant_id"].(float64)) != f.A.ID || j["platform"] != false {
		t.Fatalf("acting-as 上下文: %d %s", r.Status, r.Body)
	}
	// 平台令牌 acting-as：操作者是令牌的创建者，目标租户同上。
	r = f.w.Bearer(f.tokP, "GET", "/probe/acting", nil, apistack.ActingAs(f.B.UID))
	j = r.JSON(t)
	if r.Status != 200 || j["acting"] != true || j["ctx_tenant_uid"] != f.B.UID || uint64(j["ctx_actor"].(float64)) != f.sAdmin.U.ID {
		t.Fatalf("令牌 acting-as 上下文: %d %s", r.Status, r.Body)
	}
	// 非代租户的请求没有 acting-as 上下文：租户主体、令牌、平台跨租户只读。
	for name, rr := range map[string]apistack.Resp{
		"租户会话":    f.sa.Do("GET", "/probe/acting", nil),
		"租户会话+自己": f.sa.Do("GET", "/probe/acting", nil, apistack.ActingAs(f.A.UID)),
		"租户令牌":    w.Bearer(f.tokA, "GET", "/probe/acting", nil),
		"平台跨租户只读": f.sAdmin.Do("GET", "/probe/acting", nil),
	} {
		if jj := rr.JSON(t); rr.Status != 200 || jj["acting"] != false || jj["scope_acting"] != false {
			t.Errorf("%s 不应有 acting-as 上下文: %d %s", name, rr.Status, rr.Body)
		}
	}
	// acting-as 目标不可用：404 且处理函数不执行。
	for _, tu := range []string{apistack.IDs.MustNew(), f.S.UID, f.Closed.UID, f.Pending.UID} {
		if rr := f.sAdmin.Do("GET", "/probe/acting", nil, apistack.ActingAs(tu)); rr.Status != 404 {
			t.Errorf("acting-as 不可用租户 %s 应 404: %d %s", tu, rr.Status, rr.Body)
		}
	}
	// 无 impersonate 权限的平台主体被拒（403），且不会写出 acting-as 日志。
	before := strings.Count(w.Logs.String(), "管理员代租户操作")
	if rr := f.sAud.Do("GET", "/probe/acting", nil, apistack.ActingAs(f.A.UID)); rr.Status != 403 {
		t.Errorf("无 tenant:impersonate 应 403: %d", rr.Status)
	}
	if after := strings.Count(w.Logs.String(), "管理员代租户操作"); after != before {
		t.Error("被拒绝的 acting-as 不应产生「代租户操作」日志")
	}

	// 写操作（x-audit: true）经过渡审计钩子：结构化日志同时记录操作者与目标租户；audit_logs 不写。
	wr := f.sAdmin.Do("POST", "/probe/acting", nil, apistack.ActingAs(f.A.UID))
	if wr.Status != 200 {
		t.Fatalf("acting-as 写探针: %d %s", wr.Status, wr.Body)
	}
	logs := w.Logs.String()
	var auditLine string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "管理写操作") && strings.Contains(l, "probeActingWrite") {
			auditLine = l
		}
	}
	if auditLine == "" {
		t.Fatal("应有 probeActingWrite 的过渡审计结构化日志")
	}
	for _, want := range []string{`"acting_as":true`, `"acting_as_tenant_uid":"` + f.A.UID + `"`, fmt.Sprintf(`"actor_id":%d`, f.sAdmin.U.ID), `"audit_logs_written":false`} {
		if !strings.Contains(auditLine, want) {
			t.Errorf("审计日志应含 %s:\n%s", want, auditLine)
		}
	}
	if strings.Count(logs, `"acting_as_tenant_uid":"`+f.A.UID+`"`) < 2 {
		t.Error("Scope 钩子与审计钩子都应记录目标租户")
	}
	// 同一写操作由租户主体自己发起：日志里 acting_as=false。
	if rr := f.sa.Do("POST", "/probe/acting", nil); rr.Status != 200 {
		t.Fatalf("租户写探针: %d %s", rr.Status, rr.Body)
	}
	if !strings.Contains(logs+w.Logs.String(), `"acting_as":false`) {
		t.Error("非代租户的写操作日志应带 acting_as=false")
	}
	// 审计模块（Story 1.21）未实现：audit_logs 为空——不假装已写。
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs"); n != 0 {
		t.Fatalf("audit_logs 属 Story 1.21，不得假装已写: %d 行", n)
	}
	if hits := findSecrets(w.Logs.String(), w.Secrets()); len(hits) > 0 {
		t.Fatalf("日志含明文敏感值: %v", hits)
	}
}
