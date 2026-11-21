package sysconfigit

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/tests/apistack"
)

func admin(w *apistack.World) apistack.Session {
	u := w.SeedUser("platform", nil)
	w.GrantPlatformRole(u, "platform_admin")
	return w.Login(u)
}

func auditCount(w *apistack.World, where string, args ...any) int64 {
	w.T.Helper()
	return w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE "+where, args...)
}

func settingRow(w *apistack.World, key string) int64 {
	w.T.Helper()
	return w.RootInt("SELECT COUNT(*) FROM §.system_settings WHERE setting_key = ?", key)
}

func nested(t testing.TB, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("路径 %v 在 %q 处不是对象: %v", path, p, cur)
		}
		cur = mm[p]
	}
	return cur
}

func patch(s apistack.Session, values map[string]any) apistack.Resp {
	return s.Do("PATCH", "/admin/settings", map[string]any{"values": values})
}

func injectAuditFailure(w *apistack.World, action string) {
	w.T.Helper()
	w.Root("DROP TRIGGER IF EXISTS §.trg_inject_audit_failure")
	w.Root("CREATE TRIGGER §.trg_inject_audit_failure BEFORE INSERT ON §.audit_logs FOR EACH ROW BEGIN IF NEW.action = '" + action +
		"' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected audit write failure'; END IF; END")
}

func clearAuditFailure(w *apistack.World) {
	w.T.Helper()
	w.Root("DROP TRIGGER IF EXISTS §.trg_inject_audit_failure")
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
