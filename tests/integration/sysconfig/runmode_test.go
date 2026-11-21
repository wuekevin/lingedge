package sysconfigit

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/tests/apistack"
)

// Story 1.22 AC1（FR-24）：运行模式经 /admin/system/run-mode 切换——需再认证、写审计、权限按 openapi、无需迁移、无需重启立即生效。
func TestRunModeSwitch(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	migBefore := w.RootInt("SELECT COUNT(*) FROM §.schema_migrations")

	r := adm.Do("GET", "/admin/system/run-mode", nil)
	w.Contract("getRunMode", "GET", "/admin/system/run-mode", r)
	if r.Status != 200 || r.JSON(t)["run_mode"] != "self_hosted" {
		t.Fatalf("默认运行模式应为 self_hosted: %d %s", r.Status, r.Body)
	}

	// 未再认证：403 auth.reauth_required，且什么都没改、没审计。
	r = adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"})
	w.Contract("putRunMode", "PUT", "/admin/system/run-mode", r)
	if r.Status != 403 || r.ErrCode(t) != "auth.reauth_required" {
		t.Fatalf("切换运行模式须再认证: %d %s", r.Status, r.Body)
	}
	if settingRow(w, "system.run_mode") != 0 || auditCount(w, "action = 'system.run_mode.update'") != 0 {
		t.Fatal("未再认证的请求不得产生任何写入")
	}

	adm.Reauth()
	// 非法取值：400，无写入。
	for _, bad := range []any{map[string]any{"run_mode": "enterprise"}, map[string]any{"run_mode": ""}, map[string]any{}, map[string]any{"run_mode": 1}} {
		if r := adm.Do("PUT", "/admin/system/run-mode", bad); r.Status != 400 {
			t.Errorf("非法请求体 %v 应 400: %d %s", bad, r.Status, r.Body)
		}
	}
	if r := adm.Do("PUT", "/admin/system/run-mode", map[string]any{"run_mode": "commercial", "extra": 1}); r.Status != 400 {
		t.Errorf("未知字段应 400: %d", r.Status)
	}

	r = adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"})
	w.Contract("putRunMode", "PUT", "/admin/system/run-mode", r)
	if r.Status != 200 || r.JSON(t)["run_mode"] != "commercial" {
		t.Fatalf("再认证后切换应成功: %d %s", r.Status, r.Body)
	}
	// 立即生效（无需重启）：管理员读取、/auth/me、公开接口都已是 commercial。
	if g := adm.Do("GET", "/admin/system/run-mode", nil); g.JSON(t)["run_mode"] != "commercial" {
		t.Fatalf("GET 应立即读到新模式: %s", g.Body)
	}
	if me := adm.Do("GET", "/auth/me", nil); nested(t, me.JSON(t), "features", "run_mode") != "commercial" || nested(t, me.JSON(t), "capabilities", "run_mode") != "commercial" {
		t.Fatalf("/auth/me 应立即反映新模式: %s", me.Body)
	}
	if pv := w.Do("GET", "/public/version", nil); pv.JSON(t)["run_mode"] != "commercial" {
		t.Fatalf("/public/version 应立即反映新模式: %s", pv.Body)
	}
	if w.RootInt("SELECT COUNT(*) FROM §.schema_migrations") != migBefore {
		t.Fatal("切换运行模式不得需要（也不得产生）迁移（FR-24）")
	}

	// 审计：同事务写入，记录 from/to；操作者为该管理员。
	var changes string
	if n := auditCount(w, "action = 'system.run_mode.update'"); n != 1 {
		t.Fatalf("应恰有一条运行模式审计: %d", n)
	}
	changes = w.RootStr("SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE action = 'system.run_mode.update'")
	var ch map[string]any
	if err := json.Unmarshal([]byte(changes), &ch); err != nil {
		t.Fatal(err)
	}
	if nested(t, ch, "run_mode") != "commercial" || nested(t, ch, "settings", "system.run_mode", "to") != "commercial" {
		t.Errorf("审计摘要应含 to=commercial: %s", changes)
	}
	if w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.run_mode.update' AND actor_user_id = ? AND resource_type = 'system' AND result = 'success'", adm.U.ID) != 1 {
		t.Error("审计应记录操作者、对象类型与结果")
	}

	// 回切：from 衔接到 to。
	if r := adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "self_hosted"}); r.Status != 200 {
		t.Fatalf("回切: %d %s", r.Status, r.Body)
	}
	if me := adm.Do("GET", "/auth/me", nil); nested(t, me.JSON(t), "features", "run_mode") != "self_hosted" {
		t.Fatalf("回切后应立即生效: %s", me.Body)
	}
	if got := w.RootStr("SELECT CAST(JSON_EXTRACT(changes_json, '$.settings.\"system.run_mode\".from') AS CHAR) FROM §.audit_logs WHERE action = 'system.run_mode.update' ORDER BY id DESC LIMIT 1"); got != `"commercial"` {
		t.Errorf("第二条审计的 from 应为 commercial: %s", got)
	}

	// 权限：无 setting:write 的平台用户 403；租户 Console 会话 401；租户令牌 403。
	reader := w.SeedUser("platform", nil)
	w.GrantPlatformRole(reader, w.CustomRole("platform", "setting:read"))
	rs := w.Login(reader)
	rs.Reauth()
	if r := rs.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"}); r.Status != 403 || r.ErrCode(t) != "auth.forbidden" {
		t.Errorf("只读用户不得切换（权限先于再认证）: %d %s", r.Status, r.Body)
	}
	if r := rs.Do("GET", "/admin/system/run-mode", nil); r.Status != 200 {
		t.Errorf("只读用户可读取运行模式: %d", r.Status)
	}
	tn := w.SeedTenant("rm")
	tu := w.SeedUser("tenant", &tn)
	w.GrantTenantRole(tu, "tenant_owner")
	ts := w.Login(tu)
	if r := ts.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": "commercial"}); r.Status != 401 {
		t.Errorf("租户 Console 会话访问平台路由应 401: %d", r.Status)
	}
	if got := w.RootStr("SELECT JSON_UNQUOTE(value_json) FROM §.system_settings WHERE setting_key = 'system.run_mode'"); got != "self_hosted" {
		t.Errorf("越权请求不得改变运行模式: %s", got)
	}
	if hits := findSecrets(w.Logs.String(), w.Secrets()); len(hits) > 0 {
		t.Errorf("日志含明文敏感值（G-5）: %v", hits)
	}
}

// 并发切换：行锁串行化，全部成功；最终状态 = 最后一条审计的 to；审计链的 from 依次衔接；每次切换恰好一条审计。
func TestRunModeConcurrentSwitch(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	adm.Reauth()
	const n = 16
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mode := "commercial"
			if i%2 == 1 {
				mode = "self_hosted"
			}
			codes[i] = adm.Do("PUT", "/admin/system/run-mode", map[string]string{"run_mode": mode}).Status
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("并发切换 #%d 应成功（死锁/重复键由服务端重试），得到 %d", i, c)
		}
	}
	if got := auditCount(w, "action = 'system.run_mode.update'"); got != n {
		t.Fatalf("每次切换恰好一条审计: %d != %d", got, n)
	}
	rows, err := w.Adm.Query(fmt.Sprintf("SELECT COALESCE(JSON_UNQUOTE(JSON_EXTRACT(changes_json, '$.settings.\"system.run_mode\".from')), ''), JSON_UNQUOTE(JSON_EXTRACT(changes_json, '$.settings.\"system.run_mode\".to')) FROM `%s`.audit_logs WHERE action = 'system.run_mode.update' ORDER BY id", w.DBName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	prevTo, last := "", ""
	i := 0
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			t.Fatal(err)
		}
		if from == "null" {
			from = ""
		}
		if from != prevTo {
			t.Errorf("审计链第 %d 条 from=%q 应衔接上一条 to=%q（并发写入未被串行化）", i, from, prevTo)
		}
		prevTo, last = to, to
		i++
	}
	if g := adm.Do("GET", "/admin/system/run-mode", nil); g.JSON(t)["run_mode"] != last {
		t.Errorf("最终运行模式 %v 应等于最后一条审计的 to=%s", g.JSON(t)["run_mode"], last)
	}
	if got := w.RootInt("SELECT COUNT(*) FROM §.system_settings WHERE setting_key = 'system.run_mode'"); got != 1 {
		t.Errorf("运行模式只应有一行: %d", got)
	}
}

func findSecrets(logs string, secrets []string) []string {
	var hits []string
	for _, s := range secrets {
		if len(s) >= 8 && contains(logs, s) {
			hits = append(hits, s[:4]+"…")
		}
	}
	return hits
}
