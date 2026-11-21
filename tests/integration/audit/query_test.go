package auditit

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/tests/apistack"
)

type qfix struct {
	w      *apistack.World
	adm    apistack.Session
	A, B   apistack.Tenant
	sa, sb apistack.Session
	noRead apistack.Session // 租户用户：自定义角色不含 audit:read
	seeded map[string]string
}

func newQ(t *testing.T) *qfix {
	t.Helper()
	w := apistack.New(t)
	f := &qfix{w: w, adm: admin(w), A: w.SeedTenant("qa"), B: w.SeedTenant("qb"), seeded: map[string]string{}}
	mk := func(tn apistack.Tenant) apistack.Session {
		u := w.SeedUser("tenant", &tn)
		w.GrantTenantRole(u, "tenant_owner")
		return w.Login(u)
	}
	f.sa, f.sb = mk(f.A), mk(f.B)
	nu := w.SeedUser("tenant", &f.A)
	w.GrantTenantRole(nu, w.CustomRole("tenant", "task:read"))
	f.noRead = w.Login(nu)
	return f
}

// seed 经 root 连接写一条带固定时间戳与结果的审计行；返回 uid。
func (f *qfix) seed(tenantID *uint64, actorUserID uint64, action, rtype, ruid, result, reqID string, at time.Time) string {
	u := apistack.IDs.MustNew()
	f.w.Root("INSERT INTO §.audit_logs (uid, created_at, actor_type, actor_user_id, actor_label, tenant_id, action, resource_type, resource_uid, result, request_id) VALUES (?, ?, 'user', ?, 'seed', ?, ?, ?, ?, ?, ?)",
		u, at.UTC().Format("2006-01-02 15:04:05.000"), actorUserID, tenantID, action, rtype, ruid, result, reqID)
	return u
}

func ids(r apistack.Resp, t *testing.T) []string { return uidsOf(t, r) }

// AC4：GET /audit-logs 的游标分页（无重复、无遗漏、稳定倒序、末页 next_cursor 为 null）、过滤、校验与 kin-openapi 响应契约。
func TestListAuditLogsCursorPaginationAndFilters(t *testing.T) {
	f := newQ(t)
	w := f.w
	a := f.A.ID
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	var all []string
	for i := 0; i < 7; i++ { // 7 行同一资源类型，时间递增
		all = append(all, f.seed(&a, f.sa.U.ID, "q.write", "qpage", fmt.Sprintf("res-%d", i), "success", apistack.IDs.MustNew(), base.Add(time.Duration(i)*time.Hour)))
	}
	// 游标分页：limit=3 → 3,3,1；全集无重复无遗漏；id 倒序（先写的最后出现）。
	var got []string
	cursor := ""
	pages := 0
	for {
		path := "/audit-logs?resource_type=qpage&limit=3"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := f.adm.Do("GET", path, nil)
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		w.Contract("listAuditLogs", "GET", path, r)
		got = append(got, ids(r, t)...)
		pages++
		nc, _ := r.JSON(t)["next_cursor"].(string)
		if nc == "" {
			if r.JSON(t)["next_cursor"] != nil {
				t.Fatalf("末页 next_cursor 应为 null: %s", r.Body)
			}
			break
		}
		cursor = nc
		if pages > 5 {
			t.Fatal("分页没有终止")
		}
	}
	rev := make([]string, len(all))
	for i, u := range all {
		rev[len(all)-1-i] = u
	}
	if pages != 3 || strings.Join(got, ",") != strings.Join(rev, ",") {
		t.Fatalf("分页结果应为 7 条倒序且无重复无遗漏（页数 %d）:\n got %v\nwant %v", pages, got, rev)
	}

	// 过滤。
	other := f.seed(&a, f.sa.U.ID, "q.denied", "qfilter", "res-x", "denied", "01M7QP5V346STC0AT56NME5NR9", base.Add(48*time.Hour))
	fail := f.seed(&f.B.ID, f.sb.U.ID, "q.fail", "qfilter", "res-y", "failure", apistack.IDs.MustNew(), base.Add(72*time.Hour))
	nt := f.seed(nil, f.adm.U.ID, "q.platform", "qfilter", "res-z", "success", apistack.IDs.MustNew(), base.Add(96*time.Hour))
	q := func(qs string) []string {
		t.Helper()
		path := "/audit-logs?limit=200&" + qs
		r := f.adm.Do("GET", path, nil)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", qs, r.Status, r.Body)
		}
		w.Contract("listAuditLogs", "GET", path, r)
		out := ids(r, t)
		sort.Strings(out)
		return out
	}
	eq := func(name string, got []string, want ...string) {
		t.Helper()
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: 得到 %v，期望 %v", name, got, want)
		}
	}
	eq("action", q("action=q.denied"), other)
	eq("resource_type", q("resource_type=qfilter"), other, fail, nt)
	eq("resource_uid", q("resource_type=qfilter&resource_uid=res-y"), fail)
	eq("result", q("resource_type=qfilter&result=failure"), fail)
	eq("result=denied", q("resource_type=qfilter&result=denied"), other)
	eq("request_id", q("request_id=01M7QP5V346STC0AT56NME5NR9"), other)
	eq("tenant_uid", q("resource_type=qfilter&tenant_uid="+f.B.UID), fail)
	eq("actor_uid", q("resource_type=qfilter&actor_uid="+f.adm.U.UID), nt)
	eq("from", q("resource_type=qfilter&from="+base.Add(72*time.Hour).Format(time.RFC3339)), fail, nt)
	eq("to", q("resource_type=qfilter&to="+base.Add(48*time.Hour).Format(time.RFC3339)), other)
	eq("from+to", q("resource_type=qfilter&from="+base.Add(50*time.Hour).Format(time.RFC3339)+"&to="+base.Add(80*time.Hour).Format(time.RFC3339)), fail)
	eq("无匹配", q("resource_type=nothing"))

	// 校验：非法游标/limit/uid/result/时间 → 400 validation.failed（契约声明 400）。
	for name, qs := range map[string]string{
		"非法游标": "cursor=!!!", "limit=0": "limit=0", "limit 过大": "limit=201", "非法 actor_uid": "actor_uid=zzz", "非法 tenant_uid": "tenant_uid=1",
		"非法 result": "result=maybe", "非法 from": "from=yesterday", "非法 to": "to=2026-13-40",
	} {
		path := "/audit-logs?" + qs
		r := f.adm.Do("GET", path, nil)
		if r.Status != 400 || r.ErrCode(t) != "validation.failed" {
			t.Errorf("%s: 应 400 validation.failed: %d %s", name, r.Status, r.Body)
		}
		w.Contract("listAuditLogs", "GET", path, r)
	}
}

// AC4：越权与可见范围——未认证 401、无 audit:read 的租户用户 403、租户主体仅见自己租户、平台主体不带 X-Tenant-ID 跨租户 / 带则代租户。
func TestAuditLogsAuthorizationAndScope(t *testing.T) {
	f := newQ(t)
	w := f.w
	a, b := f.A.ID, f.B.ID
	at := time.Now().UTC().Add(-time.Hour)
	ra := f.seed(&a, f.sa.U.ID, "q.scope", "qscope", "a", "success", apistack.IDs.MustNew(), at)
	rb := f.seed(&b, f.sb.U.ID, "q.scope", "qscope", "b", "success", apistack.IDs.MustNew(), at)
	rp := f.seed(nil, f.adm.U.ID, "q.scope", "qscope", "p", "success", apistack.IDs.MustNew(), at)
	path := "/audit-logs?resource_type=qscope"

	if r := w.Do("GET", path, nil); r.Status != 401 {
		t.Errorf("未认证应 401: %d", r.Status)
	} else {
		w.Contract("listAuditLogs", "GET", path, r)
	}
	if r := f.noRead.Do("GET", path, nil); r.Status != 403 || r.ErrCode(t) != "auth.forbidden" {
		t.Errorf("无 audit:read 应 403: %d %s", r.Status, r.Body)
	} else {
		w.Contract("listAuditLogs", "GET", path, r)
	}
	if r := f.noRead.Do("GET", "/audit-logs/"+ra, nil); r.Status != 403 {
		t.Errorf("无 audit:read 读详情应 403: %d", r.Status)
	}
	sorted := func(in ...string) string { sort.Strings(in); return strings.Join(in, ",") }
	check := func(name string, r apistack.Resp, want ...string) {
		t.Helper()
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", name, r.Status, r.Body)
		}
		w.Contract("listAuditLogs", "GET", path, r)
		got := ids(r, t)
		if sorted(got...) != sorted(want...) {
			t.Errorf("%s: 得到 %v，期望 %v", name, got, want)
		}
	}
	check("租户 A", f.sa.Do("GET", path, nil), ra)
	check("租户 B", f.sb.Do("GET", path, nil), rb)
	check("租户 A 用 tenant_uid 过滤 B：取交集为空", f.sa.Do("GET", path+"&tenant_uid="+f.B.UID, nil))
	check("租户 A 带自己的 X-Tenant-ID", f.sa.Do("GET", path, nil, apistack.ActingAs(f.A.UID)), ra)
	if r := f.sa.Do("GET", path, nil, apistack.ActingAs(f.B.UID)); r.Status != 404 {
		t.Errorf("租户 A 指定他租户 X-Tenant-ID 应 404（不泄漏存在性）: %d %s", r.Status, r.Body)
	}
	check("平台不带 X-Tenant-ID：跨租户（含平台级）", f.adm.Do("GET", path, nil), ra, rb, rp)
	check("平台带 X-Tenant-ID：代租户 A", f.adm.Do("GET", path, nil, apistack.ActingAs(f.A.UID)), ra)
	check("平台带 X-Tenant-ID：代租户 B", f.adm.Do("GET", path, nil, apistack.ActingAs(f.B.UID)), rb)
	check("平台跨租户按 tenant_uid 过滤", f.adm.Do("GET", path+"&tenant_uid="+f.A.UID, nil), ra)
	if r := f.adm.Do("GET", path, nil, apistack.ActingAs(apistack.IDs.MustNew())); r.Status != 404 {
		t.Errorf("acting-as 不存在的租户应 404: %d", r.Status)
	}

	// 详情：本租户 200（契约）；他租户/平台级/不存在 404 且逐字节一致；平台跨租户可读平台级。
	g := f.sa.Do("GET", "/audit-logs/"+ra, nil)
	if g.Status != 200 || g.JSON(t)["uid"] != ra {
		t.Fatalf("%d %s", g.Status, g.Body)
	}
	w.Contract("getAuditLog", "GET", "/audit-logs/"+ra, g)
	n1, n2, n3 := f.sa.Do("GET", "/audit-logs/"+rb, nil), f.sa.Do("GET", "/audit-logs/"+rp, nil), f.sa.Do("GET", "/audit-logs/"+apistack.IDs.MustNew(), nil)
	n4 := f.sa.Do("GET", "/audit-logs/not-a-uid", nil)
	for i, n := range []apistack.Resp{n1, n2, n3, n4} {
		if n.Status != 404 || n.ErrBodyKey(t) != n1.ErrBodyKey(t) {
			t.Errorf("第 %d 个 404 应逐字节一致: %d %s", i+1, n.Status, n.Body)
		}
		w.Contract("getAuditLog", "GET", "/audit-logs/"+ra, n)
	}
	if r := f.adm.Do("GET", "/audit-logs/"+rp, nil); r.Status != 200 {
		t.Errorf("平台跨租户可读平台级审计: %d", r.Status)
	}
	// 令牌：租户令牌同样只见自己租户。
	tok := f.sa.IssueToken("audit-read", "audit:read")
	check("租户令牌", w.Bearer(tok, "GET", path, nil), ra)
	// 响应字段（平台视角保留操作者标签，契约 required 字段齐全由 Contract 校验）。
	if r := f.adm.Do("GET", "/audit-logs/"+ra, nil); r.Status != 200 || r.JSON(t)["actor_label"] != "seed" {
		t.Errorf("平台视角应保留操作者标签: %s", r.Body)
	}
}

// 管理员代租户操作经真实 API 写入的审计，租户自己能看到（字段裁剪：不暴露平台管理员的身份与来源）。
func TestTenantSeesActingAsAuditWithoutPlatformIdentity(t *testing.T) {
	f := newQ(t)
	w := f.w
	pending := w.SeedTaskStatus(&f.A.ID, "pending", map[string]any{"note": "acting"})
	r := f.adm.Do("POST", "/tasks/"+pending+"/cancel", nil, apistack.ActingAs(f.A.UID))
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	rid := r.Header.Get("X-Request-ID")
	path := "/audit-logs?request_id=" + rid
	// 平台视角：看到管理员本人。
	pv := f.adm.Do("GET", path, nil)
	if len(ids(pv, t)) != 1 || itemsOf(t, pv)[0]["actor_label"] != f.adm.U.Email || itemsOf(t, pv)[0]["actor_uid"] != f.adm.U.UID || itemsOf(t, pv)[0]["acting_as"] != true {
		t.Fatalf("平台视角应含操作者与 acting_as: %s", pv.Body)
	}
	// 租户视角：看到「平台做了什么」，看不到是谁、从哪来。
	tv := f.sa.Do("GET", path, nil)
	w.Contract("listAuditLogs", "GET", path, tv)
	if len(ids(tv, t)) != 1 {
		t.Fatalf("租户应能看到代其操作的审计: %s", tv.Body)
	}
	it := itemsOf(t, tv)[0]
	if it["acting_as"] != true || it["actor_label"] != "platform" || it["actor_uid"] != nil || it["ip"] != nil || it["user_agent"] != nil || it["action"] != "task.cancel" || it["tenant_uid"] != f.A.UID {
		t.Fatalf("租户视角应裁剪平台操作者: %v", it)
	}
	if strings.Contains(string(tv.Body), f.adm.U.Email) || strings.Contains(string(tv.Body), f.adm.U.UID) {
		t.Fatalf("租户视角泄露了平台管理员身份: %s", tv.Body)
	}
}

// AC4：GET /admin/login-logs（平台全局）：游标分页、过滤（event/identifier/ip/时间）、契约；租户主体拒绝；GET /auth/login-logs 仍只含自己。
func TestAdminLoginLogsPlatformWide(t *testing.T) {
	f := newQ(t)
	w := f.w
	t0 := time.Now().UTC().Add(-time.Minute)
	ip := "10.77.0.5"
	// 产生真实登录事件：租户用户成功登录（指定来源 IP）、平台用户一次错误口令。
	u := w.SeedUser("tenant", &f.A)
	w.GrantTenantRole(u, "tenant_owner")
	if r := w.Do("POST", "/auth/login", map[string]string{"realm": "console", "email": u.Email, pwKey: u.Password}, apistack.WithHeader("X-Forwarded-For", ip)); r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	pu := w.SeedUser("platform", nil)
	badPW := apistack.RandPassword(t)
	w.Secret(badPW)
	if r := w.Do("POST", "/auth/login", map[string]string{"realm": "admin", "email": pu.Email, pwKey: badPW}, apistack.WithHeader("X-Forwarded-For", "10.77.0.6")); r.Status != 401 {
		t.Fatalf("错误口令应 401: %d %s", r.Status, r.Body)
	}
	t1 := time.Now().UTC().Add(time.Minute)

	path := "/admin/login-logs?limit=100"
	r := f.adm.Do("GET", path, nil)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	w.Contract("listLoginLogs", "GET", path, r)
	body := string(r.Body)
	for _, want := range []string{u.Email, pu.Email, `"realm":"console"`, `"realm":"admin"`, `"event":"login_failed"`, `"event":"login_success"`, ip} {
		if !strings.Contains(body, want) {
			t.Errorf("平台全局登录日志应含 %q", want)
		}
	}
	if strings.Contains(body, badPW) || strings.Contains(body, u.Password) {
		t.Fatal("登录日志不得含口令")
	}
	// 过滤。
	filter := func(name, qs string, wantMin int, mustHave string) {
		t.Helper()
		p := "/admin/login-logs?limit=100&" + qs
		rr := f.adm.Do("GET", p, nil)
		if rr.Status != 200 {
			t.Fatalf("%s: %d %s", name, rr.Status, rr.Body)
		}
		w.Contract("listLoginLogs", "GET", p, rr)
		if n := len(itemsOf(t, rr)); n < wantMin || n == 0 || !strings.Contains(string(rr.Body), mustHave) {
			t.Errorf("%s: 条数 %d，应含 %q: %s", name, n, mustHave, rr.Body)
		}
		for _, it := range itemsOf(t, rr) {
			if strings.Contains(qs, "event=login_failed") && it["event"] != "login_failed" {
				t.Errorf("%s: event 过滤不生效: %v", name, it)
			}
			if strings.Contains(qs, "identifier=") && !strings.EqualFold(fmt.Sprint(it["identifier"]), pu.Email) {
				t.Errorf("%s: identifier 过滤不生效: %v", name, it)
			}
			if strings.Contains(qs, "ip=") && it["ip"] != ip {
				t.Errorf("%s: ip 过滤不生效: %v", name, it)
			}
		}
	}
	filter("event", "event=login_failed", 1, pu.Email)
	filter("identifier", "identifier="+url.QueryEscape(strings.ToUpper(pu.Email)), 1, pu.Email) // 标识符大小写不敏感（与登录一致：邮箱规范化为小写）
	filter("ip", "ip="+ip, 1, u.Email)
	filter("from/to", "from="+url.QueryEscape(t0.Format(time.RFC3339))+"&to="+url.QueryEscape(t1.Format(time.RFC3339)), 2, u.Email)
	if rr := f.adm.Do("GET", "/admin/login-logs?from="+url.QueryEscape(t1.Format(time.RFC3339)), nil); rr.Status != 200 || len(itemsOf(t, rr)) != 0 {
		t.Errorf("from 在未来应无结果: %d %s", rr.Status, rr.Body)
	}
	// 游标分页：limit=1 逐页走完，无重复。
	seen, cur, n := map[string]bool{}, "", 0
	for {
		p := "/admin/login-logs?limit=1"
		if cur != "" {
			p += "&cursor=" + url.QueryEscape(cur)
		}
		rr := f.adm.Do("GET", p, nil)
		if rr.Status != 200 {
			t.Fatalf("%d %s", rr.Status, rr.Body)
		}
		w.Contract("listLoginLogs", "GET", p, rr)
		for _, id := range ids(rr, t) {
			if seen[id] {
				t.Fatalf("游标分页出现重复: %s", id)
			}
			seen[id] = true
		}
		nc, _ := rr.JSON(t)["next_cursor"].(string)
		if nc == "" {
			break
		}
		cur = nc
		if n++; n > 100 {
			t.Fatal("分页没有终止")
		}
	}
	if int64(len(seen)) != w.RootInt("SELECT COUNT(*) FROM §.login_logs") {
		t.Errorf("游标分页应遍历全部登录日志: %d vs %d", len(seen), w.RootInt("SELECT COUNT(*) FROM §.login_logs"))
	}
	// 校验。
	for name, qs := range map[string]string{"非法 ip": "ip=999.1.1.1", "非法 from": "from=x", "非法游标": "cursor=zz", "limit=0": "limit=0"} {
		p := "/admin/login-logs?" + qs
		rr := f.adm.Do("GET", p, nil)
		if rr.Status != 400 {
			t.Errorf("%s 应 400: %d %s", name, rr.Status, rr.Body)
		}
		w.Contract("listLoginLogs", "GET", p, rr)
	}
	// 租户主体不可访问平台路由：Console 会话在认证层 401（realm 隔离）、租户令牌 403。
	if rr := f.sa.Do("GET", path, nil); rr.Status != 401 {
		t.Errorf("租户 Console 会话访问平台登录日志应 401: %d", rr.Status)
	}
	if rr := w.Bearer(f.sa.IssueToken("ll", "audit:read"), "GET", path, nil); rr.Status != 403 {
		t.Errorf("租户令牌访问平台登录日志应 403: %d", rr.Status)
	}
	if rr := w.Do("GET", path, nil); rr.Status != 401 {
		t.Errorf("未认证应 401: %d", rr.Status)
	}
	// GET /auth/login-logs 保持：只含当前用户自己的事件。
	mine := f.sa.Do("GET", "/auth/login-logs?limit=100", nil)
	if mine.Status != 200 || strings.Contains(string(mine.Body), u.Email) || strings.Contains(string(mine.Body), pu.Email) {
		t.Errorf("我的登录历史不得含他人: %d %s", mine.Status, mine.Body)
	}
	w.Contract("listMyLoginLogs", "GET", "/auth/login-logs?limit=100", mine)
}

// Story 1.22b：租户用户的审计 actor_label 对外是展示名（不是邮箱）——真实 API 写入、同租户另一位成员读取；平台视角保留邮箱快照。
func TestTenantSeesUserActorByDisplayNameNotEmail(t *testing.T) {
	f := newQ(t)
	w := f.w
	const display = "审计展示名-甲"
	w.Root("UPDATE §.users SET display_name = ? WHERE id = ?", display, f.sa.U.ID)
	pending := w.SeedTaskStatus(&f.A.ID, "pending", map[string]any{"note": "display-name"})
	r := f.sa.Do("POST", "/tasks/"+pending+"/cancel", nil)
	if r.Status != 200 {
		t.Fatalf("租户用户取消任务: %d %s", r.Status, r.Body)
	}
	path := "/audit-logs?request_id=" + r.Header.Get("X-Request-ID")
	// 库里的快照仍是邮箱（取证用）。
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE request_id = ? AND actor_label = ? AND actor_tenant_id = ?", r.Header.Get("X-Request-ID"), f.sa.U.Email, f.A.ID); n != 1 {
		t.Fatalf("审计快照应保存邮箱与操作者租户: %d", n)
	}
	// 同租户的另一位成员读取。
	peerUser := w.SeedUser("tenant", &f.A)
	w.GrantTenantRole(peerUser, "tenant_owner")
	peer := w.Login(peerUser)
	for name, sess := range map[string]apistack.Session{"操作者本人": f.sa, "同租户成员": peer} {
		v := sess.Do("GET", path, nil)
		w.Contract("listAuditLogs", "GET", path, v)
		items := itemsOf(t, v)
		if len(items) != 1 || items[0]["actor_label"] != display || items[0]["actor_type"] != "user" {
			t.Fatalf("%s：actor_label 应为展示名 %q: %s", name, display, v.Body)
		}
		if strings.Contains(string(v.Body), f.sa.U.Email) {
			t.Fatalf("%s：响应泄露了操作者邮箱: %s", name, v.Body)
		}
	}
	// 平台视角：保留邮箱快照。
	if pv := f.adm.Do("GET", path, nil); len(ids(pv, t)) != 1 || itemsOf(t, pv)[0]["actor_label"] != f.sa.U.Email {
		t.Fatalf("平台视角应保留邮箱快照: %s", pv.Body)
	}
}
