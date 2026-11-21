package auditit

import (
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/tests/apistack"
)

// injectAuditFailure 在真实 MySQL 上用 root 建一个 BEFORE INSERT 触发器：写入指定动作的审计行时报错（SQLSTATE 45000）。
// 这是对「审计写入失败」的真实注入（没有 Mock）：应用账号的 INSERT 真的失败，由真实的审计记录器、事务与回滚路径处理。
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

// AC6（IMP-19）：审计写入与业务变更同一事务——审计写入失败 = 该业务操作整体回滚（fail-closed）：响应 500（统一错误体，不泄漏内部原因）、
// 业务数据不得残留、审计无残留；清除故障后重试同一请求成功，且恰好留下一条审计与一次业务变更。
func TestAuditFailureRollsBackTheBusinessOperation(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	tn := w.SeedTenant("atomic")
	owner := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(owner, "tenant_owner")
	ownerSes := w.Login(owner)
	pendingTask := w.SeedTaskStatus(&tn.ID, "pending", map[string]any{"note": "atomic"})
	newEmail := strings.ToLower(apistack.IDs.MustNew()) + "@example.test"
	loginUser := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(loginUser, "tenant_owner")
	pwUser := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(pwUser, "tenant_owner")
	pwSes := w.Login(pwUser)
	outUser := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(outUser, "tenant_owner")
	outSes := w.Login(outUser)
	totpUser := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(totpUser, "tenant_owner")
	totpSes := w.Login(totpUser)
	adm.Reauth()
	newPW := apistack.RandPassword(t)
	w.Secret(newPW)
	idemKey := "atomic-" + apistack.IDs.MustNew()

	type tc struct {
		name, action string
		do           func() apistack.Resp
		wantOK       int
		unchanged    func() bool // 故障期间：业务数据没有变化
		applied      func() bool // 重试成功后：业务变更已生效
	}
	hash := func(id uint64) string { return w.RootStr("SELECT password_hash FROM §.users WHERE id = ?", id) }
	pwHash0 := hash(pwUser.ID)
	cases := []tc{
		{"createUser", "user.create", func() apistack.Resp {
			return adm.Do("POST", "/admin/users", map[string]any{"email": newEmail, "display_name": "原子性", "user_type": "platform", "role_codes": []string{}}, apistack.WithHeader("Idempotency-Key", idemKey))
		}, 201,
			func() bool { return w.RootInt("SELECT COUNT(*) FROM §.users WHERE email = ?", newEmail) == 0 },
			func() bool { return w.RootInt("SELECT COUNT(*) FROM §.users WHERE email = ?", newEmail) == 1 }},
		{"updateTenant", "tenant.update", func() apistack.Resp {
			return adm.Do("PATCH", "/tenants/"+tn.UID, map[string]any{"name": "改后名称"})
		}, 200,
			func() bool { return w.RootStr("SELECT name FROM §.tenants WHERE id = ?", tn.ID) == tn.Name },
			func() bool { return w.RootStr("SELECT name FROM §.tenants WHERE id = ?", tn.ID) == "改后名称" }},
		{"putTenantQuotas", "tenant.quotas.put", func() apistack.Resp {
			return adm.Do("PUT", "/tenants/"+tn.UID+"/quotas", map[string]any{"limits": map[string]any{"sites": 3}})
		}, 200,
			func() bool { return w.RootInt("SELECT COUNT(*) FROM §.tenant_quotas WHERE tenant_id = ?", tn.ID) == 0 },
			func() bool { return w.RootInt("SELECT COUNT(*) FROM §.tenant_quotas WHERE tenant_id = ?", tn.ID) == 1 }},
		{"cancelTask", "task.cancel", func() apistack.Resp { return ownerSes.Do("POST", "/tasks/"+pendingTask+"/cancel", nil) }, 200,
			func() bool { return w.RootStr("SELECT status FROM §.tasks WHERE uid = ?", pendingTask) == "pending" },
			func() bool { return w.RootStr("SELECT status FROM §.tasks WHERE uid = ?", pendingTask) == "canceled" }},
		{"createApiToken", "api_token.create", func() apistack.Resp {
			return ownerSes.Do("POST", "/auth/api-tokens", map[string]any{"name": "原子令牌", "scopes": []string{"task:read"}})
		}, 201,
			func() bool { return w.RootInt("SELECT COUNT(*) FROM §.api_tokens WHERE name = '原子令牌'") == 0 },
			func() bool { return w.RootInt("SELECT COUNT(*) FROM §.api_tokens WHERE name = '原子令牌'") == 1 }},
		{"login", "auth.login", func() apistack.Resp {
			return w.Do("POST", "/auth/login", map[string]string{"realm": "console", "email": loginUser.Email, pwKey: loginUser.Password})
		}, 200,
			func() bool {
				return w.RootInt("SELECT COUNT(*) FROM §.sessions WHERE user_id = ?", loginUser.ID) == 0 &&
					w.RootInt("SELECT COUNT(*) FROM §.login_logs WHERE user_id = ? AND event = 'login_success'", loginUser.ID) == 0
			},
			func() bool { return w.RootInt("SELECT COUNT(*) FROM §.sessions WHERE user_id = ?", loginUser.ID) == 1 }},
		{"changePassword", "auth.password.change", func() apistack.Resp {
			return pwSes.Do("POST", "/auth/password/change", map[string]string{"current_password": pwUser.Password, "new_password": newPW})
		}, 204,
			func() bool {
				return hash(pwUser.ID) == pwHash0 && pwSes.Do("GET", "/auth/me", nil).Status == 200
			},
			func() bool { return hash(pwUser.ID) != pwHash0 && pwSes.Do("GET", "/auth/me", nil).Status == 401 }},
		{"logout", "auth.logout", func() apistack.Resp { return outSes.Do("POST", "/auth/logout", nil) }, 204,
			func() bool { return outSes.Do("GET", "/auth/me", nil).Status == 200 },
			func() bool { return outSes.Do("GET", "/auth/me", nil).Status == 401 }},
		{"setupTotp", "auth.totp.setup", func() apistack.Resp { return totpSes.Do("POST", "/auth/totp/setup", nil) }, 200,
			func() bool {
				return w.RootInt("SELECT COUNT(*) FROM §.users WHERE id = ? AND totp_secret_enc IS NOT NULL", totpUser.ID) == 0
			},
			func() bool {
				return w.RootInt("SELECT COUNT(*) FROM §.users WHERE id = ? AND totp_secret_enc IS NOT NULL", totpUser.ID) == 1
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			injectAuditFailure(w, c.action)
			defer clearAuditFailure(w)
			before := auditCount(w, "action = ?", c.action)
			r := c.do()
			if r.Status != 500 || r.ErrCode(t) != "internal.error" {
				t.Fatalf("审计写入失败应使操作整体失败（500 internal.error）: %d %s", r.Status, r.Body)
			}
			if strings.Contains(string(r.Body), "injected") || strings.Contains(string(r.Body), "45000") {
				t.Errorf("500 响应不得泄漏内部原因: %s", r.Body)
			}
			if !c.unchanged() {
				t.Fatal("审计失败后业务数据不得残留（应整体回滚）")
			}
			if n := auditCount(w, "action = ?", c.action); n != before {
				t.Fatalf("失败的事务不得留下审计行: %d → %d", before, n)
			}
			// 清除故障后重试：成功，业务变更生效，恰好一条审计。
			clearAuditFailure(w)
			r = c.do()
			if r.Status != c.wantOK {
				t.Fatalf("清除故障后重试应 %d: %d %s", c.wantOK, r.Status, r.Body)
			}
			if !c.applied() {
				t.Fatal("重试成功后业务变更应已生效")
			}
			if n := auditCount(w, "action = ?", c.action); n != before+1 {
				t.Fatalf("重试成功应恰好留下一条审计: %d → %d", before, n)
			}
		})
	}
	if n := w.Audit.Missing(); n != 0 {
		t.Errorf("运行期兜底不应报「声明了审计却没有记录」: %d", n)
	}
}

// Record 不在事务内调用必须被拒绝（不存在「业务已提交、审计没写」的尽力而为写法）。
func TestRecordRefusesToRunOutsideATransaction(t *testing.T) {
	w := apistack.New(t)
	err := recordOutsideTx(w)
	if err == nil || !strings.Contains(err.Error(), "事务内") {
		t.Fatalf("事务外的 Record 必须失败（ErrNotInTx）: %v", err)
	}
}
