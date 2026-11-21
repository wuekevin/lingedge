package tenantisolation

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// acting-as 审计（Story 1.20 AC1 → Story 1.21 AC1）：Admin 代租户操作时，钩子链上下文携带操作者与目标租户（httpx.ActingAsFrom），
// 真实审计记录器据此在业务事务内写 audit_logs：操作者（actor_user_id）与目标租户（tenant_id）同时记录，acting_as=1（Admin 代租户「双记」）。
//
// 用测试专用的探针路由（经真实 Router.Register，走真实钩子链）观察上下文：它们只在本测试的装配里存在。
func TestActingAsCarriesAuditContext(t *testing.T) {
	probe := func(w *apistack.World, r *httpx.Router) {
		reg := func(id, method string, audited bool) {
			m := httpx.RouteMeta{OperationID: id, Method: method, Path: "/probe/acting", Auth: httpx.AuthRequired, Permissions: []string{"task:read"}, TenantScope: httpx.ScopeEither, Audit: audited, Phase: "M1"}
			if err := r.Register(m, func(c *gin.Context) {
				if audited {
					// 与生产 Service 同一写法：在业务事务内写审计（探针没有业务写入，事务里只有审计行）。
					if err := w.App.WithTx(c.Request.Context(), func(ctx context.Context) error {
						return w.Audit.Record(ctx, audit.ActionTaskCancel, audit.Event{ResourceType: audit.ResourceTask, ResourceUID: "probe-resource", Changes: map[string]any{"note": "探针"}})
					}); err != nil {
						httpx.Fail(c, apierr.Internal(err))
						return
					}
				}
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
	f := newFixture(t, apistack.Options{Routes: probe, ExtraAuditOps: []audit.Op{{OperationID: "probeActingWrite", Actions: []audit.Action{audit.ActionTaskCancel}}}})
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

	// 写操作（x-audit: true）：真实审计记录器在事务内写 audit_logs，同时记录操作者与目标租户（acting_as=1）。
	wr := f.sAdmin.Do("POST", "/probe/acting", nil, apistack.ActingAs(f.A.UID))
	if wr.Status != 200 {
		t.Fatalf("acting-as 写探针: %d %s", wr.Status, wr.Body)
	}
	reqID := wr.Header.Get("X-Request-ID")
	q := "SELECT COUNT(*) FROM §.audit_logs WHERE request_id = ? AND action = 'task.cancel' AND actor_type = 'user' AND actor_user_id = ? AND tenant_id = ? AND acting_as = 1 AND actor_tenant_id IS NULL AND result = 'success'"
	if n := w.RootInt(q, reqID, f.sAdmin.U.ID, f.A.ID); n != 1 {
		t.Fatalf("acting-as 写操作应恰好写一条审计（操作者 + 目标租户 + acting_as=1）: %d", n)
	}
	// 同一写操作由租户主体自己发起：actor 是租户用户，tenant 是其租户，acting_as=0。
	sw := f.sa.Do("POST", "/probe/acting", nil)
	if sw.Status != 200 {
		t.Fatalf("租户写探针: %d %s", sw.Status, sw.Body)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE request_id = ? AND actor_user_id = ? AND tenant_id = ? AND actor_tenant_id = ? AND acting_as = 0", sw.Header.Get("X-Request-ID"), f.sa.U.ID, f.A.ID, f.A.ID); n != 1 {
		t.Fatalf("租户自己的写操作应写一条 acting_as=0 的审计: %d", n)
	}
	// 被拒绝（无 impersonate 权限 / 目标租户不可用）的 acting-as 写请求：业务与审计都不执行，不写任何审计行。
	total := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'task.cancel'")
	_ = f.sAud.Do("POST", "/probe/acting", nil, apistack.ActingAs(f.A.UID))
	_ = f.sAdmin.Do("POST", "/probe/acting", nil, apistack.ActingAs(f.Closed.UID))
	if after := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'task.cancel'"); after != total {
		t.Fatalf("被拒绝的写请求不得产生审计行: %d → %d", total, after)
	}
	// Scope 钩子仍有「代租户操作」结构化日志（目标租户 + 操作者）。
	if !strings.Contains(w.Logs.String(), fmt.Sprintf(`"actor_id":%d`, f.sAdmin.U.ID)) || strings.Count(w.Logs.String(), `"acting_as_tenant_uid":"`+f.A.UID+`"`) < 2 {
		t.Error("Scope 钩子应记录操作者与目标租户")
	}
	if hits := findSecrets(w.Logs.String(), w.Secrets()); len(hits) > 0 {
		t.Fatalf("日志含明文敏感值: %v", hits)
	}
}
