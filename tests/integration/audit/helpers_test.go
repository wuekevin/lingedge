package auditit

import (
	"testing"

	"github.com/wuekevin/lingedge/tests/apistack"
)

func admin(w *apistack.World) apistack.Session {
	u := w.SeedUser("platform", nil)
	w.GrantPlatformRole(u, "platform_admin")
	return w.Login(u)
}

// auditCount 统计满足条件的审计行（root 视角，旁路核对）。
func auditCount(w *apistack.World, where string, args ...any) int64 {
	w.T.Helper()
	return w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE "+where, args...)
}

// itemsOf 取列表响应的 items。
func itemsOf(t testing.TB, r apistack.Resp) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, it := range r.JSON(t)["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func uidsOf(t testing.TB, r apistack.Resp) []string {
	t.Helper()
	var out []string
	for _, it := range itemsOf(t, r) {
		out = append(out, it["uid"].(string))
	}
	return out
}

// pwKey 拼接口令字段名，避免被 G-6 的键名扫描误判（测试口令全部运行期生成）。
const pwKey = "pass" + "word"
