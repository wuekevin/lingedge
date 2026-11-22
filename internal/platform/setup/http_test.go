package setup

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 正常模式（已安装）：全部向导 operation 永久 410 setup.already_installed（MIN-04），带不带 token 一样，不泄漏任何信息。
func TestInstalledModeReturns410ForEveryOperation(t *testing.T) {
	svc, err := New(Options{Installed: true})
	if err != nil {
		t.Fatal(err)
	}
	r, err := httpx.NewRouter(httpx.Options{IDs: uid.NewGenerator(clock.System()), Hooks: httpx.Hooks{Auditor: allowAuditor{}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Register(r); err != nil {
		t.Fatal(err)
	}
	h, err := r.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.Routes()); got != 9 {
		t.Fatalf("应注册 9 个向导 operation，实际 %d", got)
	}
	for _, m := range r.Routes() {
		for _, tok := range []string{"", "bad"} {
			req := httptest.NewRequest(m.Method, httpx.APIPrefix+m.Path, nil)
			if tok != "" {
				req.Header.Set("X-Setup-Token", tok)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusGone {
				t.Errorf("%s %s（token=%q）应 410，实际 %d: %s", m.Method, m.Path, tok, w.Code, w.Body.String())
			}
		}
	}
}

// 路由元数据：全部公开、无权限、public scope、M1；写 operation 声明审计（getSetupStatus 不审计），与 openapi 的一致性由 make openapi-check 校验。
func TestMetasShape(t *testing.T) {
	for _, m := range Metas() {
		if m.Auth != httpx.AuthPublic || m.TenantScope != httpx.ScopePublic || m.Phase != "M1" || m.Permissions == nil || len(m.Permissions) != 0 || m.Idempotent || m.Reauth {
			t.Errorf("%s 元数据异常: %+v", m.OperationID, m)
		}
		if got := m.Audit; got != (m.OperationID != "getSetupStatus") {
			t.Errorf("%s 的 Audit=%v", m.OperationID, got)
		}
		if p := m.Validate(); len(p) > 0 {
			t.Errorf("%s 校验: %v", m.OperationID, p)
		}
	}
}
