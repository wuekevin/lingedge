package auditit

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// AC2（FR-23）：audit_logs 只 INSERT——真实 lingedge_app 账号对该表没有 UPDATE/DELETE（也没有 TRUNCATE/DROP/ALTER）权限；
// 即使有人以高权限账号尝试，BEFORE UPDATE/DELETE 触发器也拒绝（纵深防御）。
func TestAuditLogsAreAppendOnlyForRealAppAccount(t *testing.T) {
	w := apistack.New(t)
	a := admin(w)
	tn := w.SeedTenant("imm")
	if r := a.Do("PATCH", "/tenants/"+tn.UID, map[string]any{"name": "留痕"}); r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if n := auditCount(w, "action = 'tenant.update'"); n != 1 {
		t.Fatalf("前置：应有 1 条审计: %d", n)
	}
	ctx := apistack.Ctx(t)
	ex := w.App.Executor(ctx) // 真实 lingedge_app 账号（与请求路径同一个池）

	// 对照：INSERT 与 SELECT 是允许的（否则下面的拒绝可能只是连接问题）。
	var cnt int64
	if err := ex.Get(ctx, &cnt, db.Q("SELECT COUNT(*) FROM audit_logs")); err != nil || cnt < 1 {
		t.Fatalf("app 账号应能 SELECT audit_logs: %v %d", err, cnt)
	}
	if _, err := ex.Exec(ctx, db.Q(`INSERT INTO audit_logs (uid, actor_type, actor_label, action, resource_type, result, request_id) VALUES (?, 'system', 'imm-test', 'test.insert', 'test', 'success', ?)`),
		apistack.IDs.MustNew(), apistack.IDs.MustNew()); err != nil {
		t.Fatalf("app 账号应能 INSERT audit_logs: %v", err)
	}

	for name, q := range map[string]db.Query{
		"UPDATE":   db.Q("UPDATE audit_logs SET action = 'tampered' WHERE 1 = 1"),
		"DELETE":   db.Q("DELETE FROM audit_logs WHERE 1 = 1"),
		"TRUNCATE": db.Q("TRUNCATE TABLE audit_logs"),
		"DROP":     db.Q("DROP TABLE audit_logs"),
		"ALTER":    db.Q("ALTER TABLE audit_logs ADD COLUMN x INT"),
	} {
		_, err := ex.Exec(ctx, q)
		if err == nil {
			t.Errorf("app 账号执行 %s 应被拒绝", name)
			continue
		}
		if n, ok := db.MySQLErrorNumber(err); !ok || n != db.MySQLErrTableAccessDenied {
			t.Errorf("app 账号执行 %s 应因权限被拒（1142），得到 %v", name, err)
		}
	}
	if n := auditCount(w, "action = 'tampered'"); n != 0 {
		t.Fatal("审计行不得被篡改")
	}

	// 纵深防御：即便是拥有全部权限的 root，UPDATE/DELETE 也被触发器拒绝（SQLSTATE 45000）。
	for name, q := range map[string]string{
		"UPDATE": "UPDATE §.audit_logs SET action = 'tampered' LIMIT 1",
		"DELETE": "DELETE FROM §.audit_logs LIMIT 1",
	} {
		_, err := w.Adm.ExecContext(ctx, strings.ReplaceAll(q, "§", "`"+w.DBName+"`"))
		if err == nil || !strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "forbidden") {
			t.Errorf("root 执行 %s 应被触发器拒绝: %v", name, err)
		}
	}
	if n := auditCount(w, "action = 'tampered'"); n != 0 {
		t.Fatal("审计行不得被篡改（root）")
	}
}

// AC2：租户主体与管理员经 API 都没有任何修改/删除审计的路径：没有写路由；对 /audit-logs 的写方法一律 404（不存在的路由，不泄漏）。
func TestNoWritePathForAuditLogsViaAPI(t *testing.T) {
	w := apistack.New(t)
	a := admin(w)
	tn := w.SeedTenant("nowrite")
	owner := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(owner, "tenant_owner")
	s := w.Login(owner)
	if r := a.Do("PATCH", "/tenants/"+tn.UID, map[string]any{"name": "x"}); r.Status != 200 {
		t.Fatalf("%d", r.Status)
	}
	id := uidsOf(t, a.Do("GET", "/audit-logs?limit=1", nil))[0]

	for _, rt := range w.Router.Routes() {
		if strings.HasPrefix(rt.Path, "/audit-logs") && rt.Method != "GET" {
			t.Errorf("审计日志不得有写路由: %s %s", rt.Method, rt.Path)
		}
	}
	for _, ses := range []apistack.Session{a, s} {
		for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			for _, p := range []string{"/audit-logs", "/audit-logs/" + id} {
				if r := ses.Do(m, p, map[string]any{"action": "tampered"}); r.Status != 404 {
					t.Errorf("%s %s 应 404（无此路由）: %d %s", m, p, r.Status, r.Body)
				}
			}
		}
	}
	if n := auditCount(w, "action = 'tampered'"); n != 0 {
		t.Fatal("审计行不得被篡改")
	}
}
