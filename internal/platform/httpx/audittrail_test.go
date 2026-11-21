package httpx_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
)

// recAuditor 记录审计钩子 end 回调收到的结果。
type recAuditor struct{ got []httpx.AuditOutcome }

func (a *recAuditor) Begin(_ *http.Request, meta httpx.RouteMeta) (func(httpx.AuditOutcome), error) {
	if !meta.Audit {
		return nil, nil
	}
	return func(o httpx.AuditOutcome) { a.got = append(a.got, o) }, nil
}

// NoteAudited 登记的次数经 AuditOutcome.Recorded 交给审计钩子（Story 1.21：运行期兜底「声明了审计却没有记录」的依据）。
func TestNoteAuditedReachesAuditorOutcome(t *testing.T) {
	ra := &recAuditor{}
	r, _ := newRouter(t, httpx.Options{Hooks: httpx.Hooks{Auditor: ra}})
	reg := func(id, path string, n int, status int) {
		t.Helper()
		m := httpx.RouteMeta{OperationID: id, Method: "POST", Path: path, Auth: httpx.AuthPublic, Permissions: httpx.NoPermissions(), TenantScope: httpx.ScopePublic, Audit: true, Phase: "M1"}
		if err := r.Register(m, func(c *gin.Context) {
			for i := 0; i < n; i++ {
				httpx.NoteAudited(c.Request.Context())
			}
			c.Status(status)
		}); err != nil {
			t.Fatal(err)
		}
	}
	reg("recordedOnce", "/t/once", 1, 204)
	reg("recordedTwice", "/t/twice", 2, 204)
	reg("neverRecorded", "/t/none", 0, 204)
	reg("failedNothing", "/t/failed", 0, 409)
	h := handler(t, r)
	for _, p := range []string{"/t/once", "/t/twice", "/t/none", "/t/failed"} {
		do(h, "POST", "/api/v1"+p, "")
	}
	want := []struct{ rec, status int }{{1, 204}, {2, 204}, {0, 204}, {0, 409}}
	if len(ra.got) != len(want) {
		t.Fatalf("应收到 %d 次回调: %d", len(want), len(ra.got))
	}
	for i, w := range want {
		if ra.got[i].Recorded != w.rec || ra.got[i].Status != w.status {
			t.Errorf("第 %d 次: %+v，期望 Recorded=%d Status=%d", i, ra.got[i], w.rec, w.status)
		}
	}
	// 不经审计中间件的上下文（后台作业、单元测试）：空操作，不 panic。
	httpx.NoteAudited(context.Background())
}
