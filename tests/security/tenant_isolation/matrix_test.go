package tenantisolation

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/httpx"
	"github.com/wuekevin/lingedge/internal/platform/leakguard"
	"github.com/wuekevin/lingedge/tests/apistack"
)

// 矩阵驱动记录它实际发出的 (operation, 身份组)：Driven/FailClosed 的路由必须覆盖四类身份，不能只声明不覆盖。
const (
	grpTenantA = "tenantA"
	grpTenantB = "tenantB"
	grpToken   = "token"
	grpActing  = "platformActing"
)

func groupOf(ident, sit string) string {
	switch ident {
	case IdTenantA:
		return grpTenantA
	case IdTenantB:
		return grpTenantB
	case IdTokenA, IdTokenB:
		return grpToken
	case IdPlatformAdmin, IdPlatformToken:
		if sit != SitNone {
			return grpActing
		}
	}
	return ""
}

type matrix struct {
	t       *testing.T
	f       *fixture
	touched map[string]map[string]bool // operationId → 身份组
	tasks   *taskFx
	notFnd  []nf // 全部「不泄漏存在性」的 404 响应体（去掉 request_id）
}

type nf struct{ what, body string }

func (m *matrix) record404(what string, r apistack.Resp) {
	m.t.Helper()
	if r.Status != 404 || r.ErrCode(m.t) != "resource.not_found" {
		m.t.Errorf("%s: 期望 404 resource.not_found，得到 %d %s", what, r.Status, r.Body)
		return
	}
	m.notFnd = append(m.notFnd, nf{what, r.ErrBodyKey(m.t)})
}

// req 发一个矩阵请求并登记 (operation, 身份组)。
func (m *matrix) req(op, ident, sit, method, path string, body any) apistack.Resp {
	m.t.Helper()
	if g := groupOf(ident, sit); g != "" {
		if m.touched[op] == nil {
			m.touched[op] = map[string]bool{}
		}
		m.touched[op][g] = true
	}
	r := m.f.as(ident)(method, path, body, m.f.opts(ident, sit)...)
	if r.Status/100 == 2 && tenantFacing(ident, sit) { // 租户可见的成功响应一律做 R-T1 断言（驱动无需逐处调用）
		m.leakOp(op, fmt.Sprintf("%s %s × %s", op, ident, sit), r)
	}
	return r
}

// tenantFacing 报告该 (身份, 情形) 的成功响应是否「租户可见」：租户主体、令牌，以及平台主体 acting-as（Scope 是目标租户）。
// 平台主体不带 X-Tenant-ID 的跨租户只读是平台视角，不属租户可见响应（见 docs/testing/tenant-isolation-matrix.md）。
func tenantFacing(ident, sit string) bool {
	return isTenantIdent(ident) || ((ident == IdPlatformAdmin || ident == IdPlatformToken) && sit != SitNone)
}

// leakOp 对租户可见响应做 R-T1 断言（键名、已知内部取值、IP、内部错误全文）。

// ipAllowedOps 是响应里合法携带 IP 字面量的 operation：审计记录的 ip 是操作者（租户自己的用户）的来源地址，是 AuditLog schema 的字段，
// 不是节点/集群地址；节点/出口 IP 仍由 Internal 取值表（SeedInternals 的 NodeIPv4/EgressIP）逐一检查。
var ipAllowedOps = map[string]bool{"listAuditLogs": true, "getAuditLog": true}

func (m *matrix) leakOp(op, what string, r apistack.Resp) {
	m.t.Helper()
	if vs := leakguard.Scan(r.Body, leakguard.Options{Internal: m.f.in.Strings(), ForbidIPValues: !ipAllowedOps[op]}); len(vs) > 0 {
		m.t.Errorf("%s 的响应含节点/集群内部标识（R-T1）: %v\n%s", what, vs, r.Body)
	}
}

func (m *matrix) contract(op, method, path string, r apistack.Resp) {
	m.t.Helper()
	o, ok := m.f.w.Spec.Operation(op)
	if !ok {
		m.t.Fatalf("openapi 没有 %s", op)
	}
	if o.Operation.Responses.Status(r.Status) == nil { // 契约未声明的状态码（如 getCurrentTenant 的 404/400）见 CC-8：此处只校验已声明的
		return
	}
	m.f.w.Contract(op, method, path, r)
}

// ---- 入口 ----

func TestTenantIsolationMatrix(t *testing.T) {
	f := newFixture(t)
	m := &matrix{t: t, f: f, touched: map[string]map[string]bool{}}
	routes := f.w.Router.Routes()

	t.Run("coverage registry", func(t *testing.T) {
		if p := AuditCoverage(routes, Registry, testFuncExists(repoRoot(t))); len(p) > 0 {
			for _, s := range p {
				t.Error(s)
			}
		}
	})
	t.Run("skeleton rows", func(t *testing.T) { m.t = t; m.skeletonRows(routes) })

	t.Run("driven tenants/current", func(t *testing.T) { m.t = t; m.driveTenantsCurrent() })
	t.Run("driven tasks", func(t *testing.T) { m.t = t; m.driveTasks() })
	t.Run("driven cancelTask", func(t *testing.T) { m.t = t; m.driveCancelTask() })
	t.Run("driven audit-logs", func(t *testing.T) { m.t = t; m.driveAuditLogs() })

	t.Run("driven coverage is complete", func(t *testing.T) {
		for _, r := range routes {
			c, ok := Registry[r.OperationID]
			if !ok || (c.Kind != Driven && c.Kind != FailClosed) {
				continue
			}
			for _, g := range []string{grpTenantA, grpTenantB, grpToken, grpActing} {
				if !m.touched[r.OperationID][g] {
					t.Errorf("%s 登记为 %s，但驱动没有用「%s」身份组调用它——登记不能只声明不覆盖", r.OperationID, c.Kind, g)
				}
			}
		}
	})
	t.Run("not-found responses are byte-identical", func(t *testing.T) {
		if len(m.notFnd) < 20 {
			t.Fatalf("404 样本过少（%d），矩阵可能没有执行", len(m.notFnd))
		}
		for _, n := range m.notFnd[1:] {
			if n.body != m.notFnd[0].body {
				t.Errorf("404 响应体应逐字节一致（不泄漏存在性）:\n%s: %s\n%s: %s", m.notFnd[0].what, m.notFnd[0].body, n.what, n.body)
			}
		}
		t.Logf("%d 个 404 响应体逐字节一致（身份×操作×情形）", len(m.notFnd))
	})
	t.Run("logs contain no secrets (G-5)", func(t *testing.T) {
		secrets := f.w.Secrets()
		if len(secrets) < 8 {
			t.Fatalf("登记的运行期敏感值过少（%d）", len(secrets))
		}
		if hits := findSecrets(f.w.Logs.String(), secrets); len(hits) > 0 {
			t.Fatalf("日志含明文敏感值: %v", hits)
		}
	})
}

// skeletonRows 执行由 openapi.yaml 自动生成的骨架行（只执行已注册的 operation）。
func (m *matrix) skeletonRows(routes []httpx.RouteMeta) {
	registered := map[string]bool{}
	for _, r := range routes {
		registered[r.OperationID] = true
	}
	rows := Skeleton(m.f.w.Spec)
	run, pending, skipped := 0, map[string]bool{}, 0
	for _, row := range rows {
		if !registered[row.OperationID] {
			pending[row.OperationID] = true
			continue
		}
		if c, ok := Registry[row.OperationID]; ok && c.Kind == Exempt && row.Identity != IdAnonymous {
			skipped++ // 自助/令牌管理：由 Registry 引用的专项测试覆盖
			continue
		}
		run++
		path := samplePath(row.Path)
		r := m.f.as(row.Identity)(row.Method, path, nil, m.f.opts(row.Identity, row.Situation)...)
		if r.Status != row.Want {
			m.t.Errorf("%s：期望 %d，得到 %d %s\n依据：%s", row, row.Want, r.Status, r.Body, row.Why)
			continue
		}
		if c := errCode(row.Want); c != "" && r.ErrCode(m.t) != c {
			m.t.Errorf("%s：期望错误码 %s，得到 %s", row, c, r.ErrCode(m.t))
		}
		if row.NotFoundSet {
			m.record404(row.String(), r)
		}
	}
	if run < 200 {
		m.t.Fatalf("骨架行执行过少（%d），矩阵可能没有执行", run)
	}
	m.t.Logf("骨架 %d 行：执行 %d 行（已注册 operation），%d 行由专项测试覆盖（Exempt），%d 个 operation 尚未实现（待各自 Story 注册后自动纳入）", len(rows), run, skipped, len(pending))
}

// ---- Driven：/tenants/current ----

func (m *matrix) driveTenantsCurrent() {
	f := m.f
	const op, path = "getCurrentTenant", "/tenants/current"
	for _, c := range []struct {
		ident string
		want  apistack.Tenant
	}{{IdTenantA, f.A}, {IdTenantB, f.B}, {IdTokenA, f.A}, {IdTokenB, f.B}} {
		for _, sit := range []string{SitNone, SitOwn} {
			r := m.req(op, c.ident, sit, "GET", path, nil)
			if r.Status != 200 || r.JSON(m.t)["uid"] != c.want.UID || r.JSON(m.t)["name"] != c.want.Name {
				m.t.Errorf("%s × %s：应得到自己的租户 %s，得到 %d %s", c.ident, sit, c.want.UID, r.Status, r.Body)
				continue
			}
			m.contract(op, "GET", path, r)
		}
		for _, sit := range []string{SitOther, SitMissing, SitSuspended} { // 越权/不存在：404，响应体与「不存在」逐字节一致
			x := m.req(op, c.ident, sit, "GET", path, nil)
			m.record404(c.ident+" × "+sit, x)
			m.contract(op, "GET", path, x) // CC-8：getCurrentTenant 声明了 404
		}
	}
	// 平台 acting-as（管理员与令牌）：目标租户就是 Scope；可代任一真实且 active 的租户。
	for _, ident := range []string{IdPlatformAdmin, IdPlatformToken} {
		for _, c := range []struct {
			sit  string
			want apistack.Tenant
		}{{SitOwn, f.A}, {SitOther, f.B}} {
			r := m.req(op, ident, c.sit, "GET", path, nil)
			if r.Status != 200 || r.JSON(m.t)["uid"] != c.want.UID {
				m.t.Errorf("%s acting-as %s：应得到该租户，得到 %d %s", ident, c.sit, r.Status, r.Body)
				continue
			}
			m.contract(op, "GET", path, r)
		}
		for _, sit := range []string{SitMissing, SitSuspended} {
			m.record404(ident+" acting-as "+sit, m.req(op, ident, sit, "GET", path, nil))
		}
		for _, tn := range []apistack.Tenant{f.Closed, f.Pending} { // 状态不可用的租户一律 404
			r := f.as(ident)("GET", path, nil, apistack.ActingAs(tn.UID))
			m.record404(ident+" acting-as 不可用租户", r)
		}
	}
	// 平台令牌 acting-as 同样受 impersonate 约束：scopes 不含 tenant:impersonate → 403（目标租户真实存在也一样）。
	if r := m.req(op, IdPlatformTokenNoImp, SitOwn, "GET", path, nil); r.Status != 403 {
		m.t.Errorf("无 tenant:impersonate 的平台令牌 acting-as 应 403: %d %s", r.Status, r.Body)
	}
	// 平台无 impersonate：403；平台不带 X-Tenant-ID 访问 tenant 路由：400。
	if r := m.req(op, IdPlatformNoImp, SitOwn, "GET", path, nil); r.Status != 403 {
		m.t.Errorf("无 tenant:impersonate 的平台主体 acting-as 应 403: %d %s", r.Status, r.Body)
	}
	if r := m.req(op, IdPlatformAdmin, SitNone, "GET", path, nil); r.Status != 400 {
		m.t.Errorf("平台主体不带 X-Tenant-ID 访问 tenant 路由应 400: %d %s", r.Status, r.Body)
	} else {
		m.contract(op, "GET", path, r) // CC-8：getCurrentTenant 声明了 400
	}
	if r := f.sAdmin.Do("GET", path, nil, apistack.WithHeader("X-Tenant-ID", "not-a-uid")); r.Status != 400 {
		m.t.Errorf("非法 X-Tenant-ID 应 400: %d %s", r.Status, r.Body)
	}
	// 一个无关的 canonical 404（不存在的任务）与上述全部 404 逐字节一致。
	m.record404("canonical: 不存在的任务", m.req("getTask", IdTenantA, SitNone, "GET", "/tasks/"+apistack.IDs.MustNew(), nil))
}

// ---- Driven：任务中心 ----

type taskFx struct{ a1, a2, b1, plat string }

func (m *matrix) seedTasks() taskFx {
	f := m.f
	dirty := func(tn *apistack.Tenant, extra string) map[string]any {
		return map[string]any{
			"resource_type": "deployment", "resource_uid": apistack.IDs.MustNew(), "note": extra,
			"cluster_uid": f.in.ClusterUID, "cluster_name": f.in.ClusterName, "node_id": f.in.NodeUID, "node_name": f.in.NodeName,
			"egress_node_ip": f.in.EgressIP, "targets": []string{f.in.NodeHost}, "nodes": []string{f.in.NodeHost, f.in.NodeIPv4},
			"summary": map[string]any{"healthy": 3, "total": 4, "offline": 1},
		}
	}
	tx := taskFx{}
	a, b := f.A.ID, f.B.ID
	tx.a1, tx.a2 = f.w.SeedTask(&a, dirty(&f.A, "a1")), f.w.SeedTask(&a, dirty(&f.A, "a2"))
	tx.b1 = f.w.SeedTask(&b, dirty(&f.B, "b1"))
	tx.plat = f.w.SeedTask(nil, dirty(nil, "platform"))
	f.w.SeedTaskEvent(tx.a1, "发布中", map[string]any{"cluster_uid": f.in.ClusterUID, "node_id": f.in.NodeUID, "egress_node_ip": f.in.EgressIP, "summary": map[string]any{"healthy": 3, "total": 4, "offline": 1}})
	return tx
}

func uids(t *testing.T, r apistack.Resp) []string {
	t.Helper()
	var out []string
	for _, it := range r.JSON(t)["items"].([]any) {
		out = append(out, it.(map[string]any)["uid"].(string))
	}
	sort.Strings(out)
	return out
}

func sorted(in ...string) []string { sort.Strings(in); return in }

func (m *matrix) driveTasks() {
	f, tx := m.f, m.seedTasks()
	const list = "/tasks"
	type view struct {
		ident, sit string
		want       []string
		own, other string // 自己租户的任务 / 他租户的任务
	}
	views := []view{
		{IdTenantA, SitNone, sorted(tx.a1, tx.a2), tx.a1, tx.b1},
		{IdTokenA, SitNone, sorted(tx.a1, tx.a2), tx.a1, tx.b1},
		{IdPlatformAdmin, SitOwn, sorted(tx.a1, tx.a2), tx.a1, tx.b1},
		{IdPlatformToken, SitOwn, sorted(tx.a1, tx.a2), tx.a1, tx.b1},
		{IdTenantB, SitNone, sorted(tx.b1), tx.b1, tx.a1},
		{IdTokenB, SitNone, sorted(tx.b1), tx.b1, tx.a1},
		{IdPlatformAdmin, SitOther, sorted(tx.b1), tx.b1, tx.a1}, // 平台 acting-as 租户 B
	}
	for _, v := range views {
		what := v.ident + " × " + v.sit
		// listTasks：只含本租户的任务（不含他租户、不含平台任务）。
		r := m.req("listTasks", v.ident, v.sit, "GET", list, nil)
		if r.Status != 200 {
			m.t.Errorf("%s listTasks: %d %s", what, r.Status, r.Body)
			continue
		}
		if got := uids(m.t, r); fmt.Sprint(got) != fmt.Sprint(v.want) || r.JSON(m.t)["total"].(float64) != float64(len(v.want)) {
			m.t.Errorf("%s listTasks 应只含本租户的任务 %v，得到 %v（total=%v）", what, v.want, got, r.JSON(m.t)["total"])
		}
		m.contract("listTasks", "GET", list, r)
		// getTask：自己的任务 200（含 R-T1）；他租户的任务、平台任务、不存在的任务 → 404 逐字节一致。
		g := m.req("getTask", v.ident, v.sit, "GET", "/tasks/"+v.own, nil)
		if g.Status != 200 || g.JSON(m.t)["uid"] != v.own {
			m.t.Errorf("%s getTask 自己的任务: %d %s", what, g.Status, g.Body)
		} else {
			m.contract("getTask", "GET", "/tasks/"+v.own, g)
			if s := string(g.Body); !strings.Contains(s, `"summary":{"healthy":3,"offline":1,"total":4}`) {
				m.t.Errorf("%s getTask：聚合计数 summary 应保留（允许）: %s", what, s)
			}
		}
		for _, other := range []string{v.other, tx.plat, apistack.IDs.MustNew()} {
			x := m.req("getTask", v.ident, v.sit, "GET", "/tasks/"+other, nil)
			m.record404(what+" getTask 他租户/平台/不存在", x)
			m.contract("getTask", "GET", "/tasks/"+other, x)
			e := m.req("listTaskEvents", v.ident, v.sit, "GET", "/tasks/"+other+"/events", nil)
			m.record404(what+" listTaskEvents 他租户/平台/不存在", e)
		}
		ev := m.req("listTaskEvents", v.ident, v.sit, "GET", "/tasks/"+v.own+"/events", nil)
		if ev.Status != 200 {
			m.t.Errorf("%s listTaskEvents 自己的任务: %d %s", what, ev.Status, ev.Body)
			continue
		}
		m.contract("listTaskEvents", "GET", "/tasks/"+v.own+"/events", ev)
	}
	// 事件：租户 A 的任务确有 1 条事件，且内部标识键已剔除、聚合计数保留。
	if ev := f.sa.Do("GET", "/tasks/"+tx.a1+"/events", nil); ev.Status != 200 || ev.JSON(m.t)["total"].(float64) != 1 || !strings.Contains(string(ev.Body), `"summary":{"healthy":3,"offline":1,"total":4}`) {
		m.t.Errorf("事件视图: %d %s", ev.Status, ev.Body)
	}
	// 平台跨租户只读（不带 X-Tenant-ID）：平台视角，看得到全部任务与内部标识——它不是租户可见响应，R-T1 不适用；
	// 同时证明断言器对真实数据有区分力：同一批任务，租户视角零违规、平台视角有违规。
	pr := m.req("listTasks", IdPlatformAdmin, SitNone, "GET", list, nil)
	if pr.Status != 200 || len(uids(m.t, pr)) != 4 {
		m.t.Fatalf("平台跨租户只读应看到全部 4 个任务: %d %s", pr.Status, pr.Body)
	}
	if vs := leakguard.Scan(pr.Body, leakguard.Options{Internal: f.in.Strings()}); len(vs) == 0 {
		m.t.Error("平台视角含内部标识，R-T1 断言器应能发现（否则断言器对真实数据没有区分力）")
	}
	if pg := m.req("getTask", IdPlatformAdmin, SitNone, "GET", "/tasks/"+tx.plat, nil); pg.Status != 200 {
		m.t.Errorf("平台跨租户只读应可读平台任务: %d", pg.Status)
	}
	// 租户视角的原始响应体里不得出现任何已种入的内部取值（整体子串扫描，独立于键名规则）。
	for _, ident := range []string{IdTenantA, IdTokenB} {
		body := string(m.req("listTasks", ident, SitNone, "GET", list, nil).Body)
		for _, s := range f.in.Strings() {
			if strings.Contains(strings.ToLower(body), strings.ToLower(s)) {
				m.t.Errorf("%s 的任务列表含内部取值 %q", ident, s)
			}
		}
	}
	m.tasks = &tx
}

// ---- Driven：cancelTask（Story 1.21：取消与审计同一事务；此前是 FailClosed）----

// actorOf 返回矩阵身份对应的操作者用户 id 与是否经令牌。
func (f *fixture) actorOf(ident string) (userID uint64, viaToken bool) {
	switch ident {
	case IdTenantA:
		return f.sa.U.ID, false
	case IdTenantB:
		return f.sb.U.ID, false
	case IdTokenA:
		return f.sa.U.ID, true
	case IdTokenB:
		return f.sb.U.ID, true
	case IdPlatformAdmin:
		return f.sAdmin.U.ID, false
	case IdPlatformToken:
		return f.sAdmin.U.ID, true
	}
	panic("未知身份 " + ident)
}

func (m *matrix) driveCancelTask() {
	f := m.f
	if m.tasks == nil {
		m.t.Fatal("driveTasks 应先于 driveCancelTask 执行")
	}
	a, b := f.A.ID, f.B.ID
	type view struct {
		ident, sit string
		own, other apistack.Tenant
	}
	views := []view{
		{IdTenantA, SitNone, f.A, f.B}, {IdTokenA, SitNone, f.A, f.B}, {IdPlatformAdmin, SitOwn, f.A, f.B}, {IdPlatformToken, SitOwn, f.A, f.B},
		{IdTenantB, SitNone, f.B, f.A}, {IdTokenB, SitNone, f.B, f.A}, {IdPlatformAdmin, SitOther, f.B, f.A},
	}
	pending := func(t apistack.Tenant) string {
		id := t.ID
		return f.w.SeedTaskStatus(&id, "pending", map[string]any{"resource_type": "deployment", "resource_uid": apistack.IDs.MustNew(), "cluster_uid": f.in.ClusterUID, "node_name": f.in.NodeName})
	}
	_, _ = a, b
	plat := f.w.SeedTaskStatus(nil, "pending", map[string]any{"note": "platform"})
	status := func(uid string) string { return f.w.RootStr("SELECT status FROM §.tasks WHERE uid = ?", uid) }
	for _, v := range views {
		what := v.ident + " × " + v.sit
		own, other := pending(v.own), pending(v.other)
		// 他租户的任务、平台任务、不存在的任务：404 逐字节一致，任务状态与审计均不变。
		before := f.w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'task.cancel'")
		for _, tgt := range []string{other, plat, apistack.IDs.MustNew()} {
			r := m.req("cancelTask", v.ident, v.sit, "POST", "/tasks/"+tgt+"/cancel", nil)
			m.record404(what+" cancelTask 他租户/平台/不存在", r)
		}
		if status(other) != "pending" || status(plat) != "pending" {
			m.t.Errorf("%s：越权取消不得改变他租户/平台任务", what)
		}
		if after := f.w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'task.cancel'"); after != before {
			m.t.Errorf("%s：被拒绝（404）的取消不得写审计: %d → %d", what, before, after)
		}
		// 自己的任务：200 canceled（含 R-T1 与契约），并恰好写一条审计：操作者、所属租户、acting_as、令牌。
		r := m.req("cancelTask", v.ident, v.sit, "POST", "/tasks/"+own+"/cancel", nil)
		if r.Status != 200 || r.JSON(m.t)["status"] != "canceled" {
			m.t.Errorf("%s cancelTask 自己的任务应 200 canceled: %d %s", what, r.Status, r.Body)
			continue
		}
		m.contract("cancelTask", "POST", "/tasks/"+own+"/cancel", r)
		actor, viaToken := f.actorOf(v.ident)
		acting := 0
		if v.ident == IdPlatformAdmin || v.ident == IdPlatformToken {
			acting = 1
		}
		tokenCond := "actor_token_id IS NULL"
		if viaToken {
			tokenCond = "actor_token_id IS NOT NULL AND actor_type = 'api_token'"
		}
		q := "SELECT COUNT(*) FROM §.audit_logs WHERE action = 'task.cancel' AND resource_type = 'task' AND resource_uid = ? AND request_id = ? AND actor_user_id = ? AND tenant_id = ? AND acting_as = ? AND result = 'success' AND " + tokenCond
		if n := f.w.RootInt(q, own, r.Header.Get("X-Request-ID"), actor, v.own.ID, acting); n != 1 {
			m.t.Errorf("%s：取消应恰好写一条审计（操作者=%d 租户=%d acting_as=%d token=%v）: %d", what, actor, v.own.ID, acting, viaToken, n)
		}
		// 已结束的任务再取消：409，没有变更就没有审计。
		before = f.w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'task.cancel'")
		if r2 := m.req("cancelTask", v.ident, v.sit, "POST", "/tasks/"+own+"/cancel", nil); r2.Status != 409 {
			m.t.Errorf("%s 终态任务再取消应 409: %d %s", what, r2.Status, r2.Body)
		}
		if after := f.w.RootInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'task.cancel'"); after != before {
			m.t.Errorf("%s：409 不得写审计", what)
		}
	}
}

// ---- Driven：审计日志（Story 1.21）----

type auditFx struct{ a1, a2, b1, plat string }

// seededDisplayName 是租户 A 用户在审计矩阵里的展示名（租户视角的 actor_label）。
const seededDisplayName = "租户A-张三"

// seedAudit 经 root 连接写入四条带 resource_type=matrixres 的审计行（便于用过滤器隔离出本驱动的数据）：租户 A 两条（a2 是平台管理员代租户操作，
// 带管理员邮箱标签与来源 IP/UA）、租户 B 一条、平台级（tenant_id NULL）一条；changes 里种入节点/集群内部标识。
func (m *matrix) seedAudit() auditFx {
	f := m.f
	changes := func(note string) string {
		b, _ := json.Marshal(map[string]any{"note": note, "cluster_uid": f.in.ClusterUID, "node_name": f.in.NodeName, "egress_node_ip": f.in.EgressIP,
			"nodes": []string{f.in.NodeHost}, "summary": map[string]any{"healthy": 3, "total": 4, "offline": 1}})
		return string(b)
	}
	// actorTenant 与真实写入一致：租户用户的审计行带其租户，平台操作者为 NULL（面向租户的裁剪据此判断操作者是否平台身份，Story 1.22b）。
	ins := func(tenantID, actorTenant *uint64, actorUserID uint64, label string, acting bool, ip any, ua any, note string) string {
		u := apistack.IDs.MustNew()
		f.w.Root("INSERT INTO §.audit_logs (uid, actor_type, actor_user_id, actor_label, actor_tenant_id, tenant_id, acting_as, action, resource_type, resource_uid, result, changes_json, request_id, ip, user_agent) "+
			"VALUES (?, 'user', ?, ?, ?, ?, ?, 'matrix.write', 'matrixres', ?, 'success', CAST(? AS JSON), ?, ?, ?)",
			u, actorUserID, label, actorTenant, tenantID, acting, apistack.IDs.MustNew(), changes(note), apistack.IDs.MustNew(), ip, ua)
		return u
	}
	a, b := f.A.ID, f.B.ID
	// 租户用户的展示名（租户视角下 actor_label 显示它而不是邮箱）。
	f.w.Root("UPDATE §.users SET display_name = ? WHERE id = ?", seededDisplayName, f.sa.U.ID)
	return auditFx{
		a1:   ins(&a, &a, f.sa.U.ID, f.sa.U.Email, false, []byte{192, 0, 2, 10}, "tenant-ua", "a1"),
		a2:   ins(&a, nil, f.sAdmin.U.ID, f.sAdmin.U.Email, true, []byte{198, 18, 0, 9}, "admin-console-ua", "a2"),
		b1:   ins(&b, &b, f.sb.U.ID, f.sb.U.Email, false, []byte{192, 0, 2, 20}, "tenant-ub", "b1"),
		plat: ins(nil, nil, f.sAdmin.U.ID, f.sAdmin.U.Email, false, []byte{198, 18, 0, 9}, "admin-console-ua", "platform"),
	}
}

func (m *matrix) driveAuditLogs() {
	f, ax := m.f, m.seedAudit()
	const list = "/audit-logs?resource_type=matrixres&limit=100"
	type view struct {
		ident, sit string
		want       []string
		own, other string
	}
	views := []view{
		{IdTenantA, SitNone, sorted(ax.a1, ax.a2), ax.a1, ax.b1},
		{IdTokenA, SitNone, sorted(ax.a1, ax.a2), ax.a1, ax.b1},
		{IdPlatformAdmin, SitOwn, sorted(ax.a1, ax.a2), ax.a1, ax.b1},
		{IdPlatformToken, SitOwn, sorted(ax.a1, ax.a2), ax.a1, ax.b1},
		{IdTenantB, SitNone, sorted(ax.b1), ax.b1, ax.a1},
		{IdTokenB, SitNone, sorted(ax.b1), ax.b1, ax.a1},
		{IdPlatformAdmin, SitOther, sorted(ax.b1), ax.b1, ax.a1},
	}
	adminEmail := f.sAdmin.U.Email
	for _, v := range views {
		what := v.ident + " × " + v.sit
		r := m.req("listAuditLogs", v.ident, v.sit, "GET", list, nil)
		if r.Status != 200 {
			m.t.Errorf("%s listAuditLogs: %d %s", what, r.Status, r.Body)
			continue
		}
		if got := uids(m.t, r); fmt.Sprint(got) != fmt.Sprint(v.want) {
			m.t.Errorf("%s listAuditLogs 应只含本租户的审计 %v，得到 %v", what, v.want, got)
		}
		m.contract("listAuditLogs", "GET", list, r)
		// 字段裁剪：租户可见的响应不含平台管理员身份/来源（acting_as 记录里只剩「platform」），也不含平台内部字段。
		body := string(r.Body)
		if strings.Contains(body, adminEmail) || strings.Contains(body, "admin-console-ua") || strings.Contains(body, "198.18.0.9") {
			m.t.Errorf("%s：租户可见的审计响应泄露了平台管理员的身份/来源: %s", what, body)
		}
		// 租户用户操作者：同租户成员读到的是展示名，而不是邮箱（Story 1.22b）。
		if strings.Contains(body, f.sa.U.Email) || strings.Contains(body, f.sb.U.Email) {
			m.t.Errorf("%s：租户可见的审计响应含用户邮箱（应为展示名/脱敏）: %s", what, body)
		}
		for _, it := range r.JSON(m.t)["items"].([]any) {
			row := it.(map[string]any)
			if row["uid"] == ax.a1 && row["actor_label"] != seededDisplayName {
				m.t.Errorf("%s：租户用户操作者的 actor_label 应为展示名 %q: %v", what, seededDisplayName, row["actor_label"])
			}
			if row["uid"] == ax.a2 && (row["actor_label"] != "platform" || row["actor_uid"] != nil || row["ip"] != nil || row["user_agent"] != nil || row["acting_as"] != true) {
				m.t.Errorf("%s：acting_as 记录应裁剪平台操作者（actor_label=platform，无 actor_uid/ip/user_agent）: %v", what, row)
			}
			ch, _ := row["changes"].(map[string]any)
			for k := range ch {
				if leakguard.ForbiddenKey(k) {
					m.t.Errorf("%s：changes 含 R-T1 内部键 %s", what, k)
				}
			}
			if sm, _ := ch["summary"].(map[string]any); sm == nil || sm["healthy"] != float64(3) {
				m.t.Errorf("%s：聚合计数 summary 应保留（允许）: %v", what, ch)
			}
		}
		// getAuditLog：自己的记录 200；他租户的、平台级的、不存在的 → 404 逐字节一致。
		g := m.req("getAuditLog", v.ident, v.sit, "GET", "/audit-logs/"+v.own, nil)
		if g.Status != 200 || g.JSON(m.t)["uid"] != v.own {
			m.t.Errorf("%s getAuditLog 自己的记录: %d %s", what, g.Status, g.Body)
		} else {
			m.contract("getAuditLog", "GET", "/audit-logs/"+v.own, g)
		}
		for _, other := range []string{v.other, ax.plat, apistack.IDs.MustNew()} {
			x := m.req("getAuditLog", v.ident, v.sit, "GET", "/audit-logs/"+other, nil)
			m.record404(what+" getAuditLog 他租户/平台/不存在", x)
			m.contract("getAuditLog", "GET", "/audit-logs/"+other, x)
		}
	}
	// 租户主体用 tenant_uid 过滤他租户：不会越权（与 Scope 限定取交集 = 空）。
	for _, ident := range []string{IdTenantA, IdTokenA} {
		r := m.req("listAuditLogs", ident, SitNone, "GET", list+"&tenant_uid="+f.B.UID, nil)
		if r.Status != 200 || len(uids(m.t, r)) != 0 {
			m.t.Errorf("%s 以 tenant_uid 过滤他租户应得到空集（不越权）: %d %s", ident, r.Status, r.Body)
		}
	}
	// 平台跨租户只读（不带 X-Tenant-ID）：平台视角，四条全可见，保留管理员身份与 changes 里的内部标识——它不是租户可见响应；
	// 同一批数据，租户视角零违规、平台视角有违规，证明断言器对真实数据有区分力。
	pr := m.req("listAuditLogs", IdPlatformAdmin, SitNone, "GET", list, nil)
	if pr.Status != 200 || len(uids(m.t, pr)) != 4 {
		m.t.Fatalf("平台跨租户只读应看到全部 4 条: %d %s", pr.Status, pr.Body)
	}
	if !strings.Contains(string(pr.Body), adminEmail) {
		m.t.Error("平台视角应保留操作者标签")
	}
	if vs := leakguard.Scan(pr.Body, leakguard.Options{Internal: f.in.Strings()}); len(vs) == 0 {
		m.t.Error("平台视角 changes 含内部标识，R-T1 断言器应能发现")
	}
	if pg := m.req("getAuditLog", IdPlatformAdmin, SitNone, "GET", "/audit-logs/"+ax.plat, nil); pg.Status != 200 {
		m.t.Errorf("平台跨租户只读应可读平台级审计: %d", pg.Status)
	}
	// 平台带 tenant_uid 过滤：只含该租户（平台视角）。
	if r := m.req("listAuditLogs", IdPlatformAdmin, SitNone, "GET", list+"&tenant_uid="+f.B.UID, nil); r.Status != 200 || fmt.Sprint(uids(m.t, r)) != fmt.Sprint(sorted(ax.b1)) {
		m.t.Errorf("平台按 tenant_uid 过滤: %d %s", r.Status, r.Body)
	}
	// 租户视角原始响应体里不得出现任何已种入的内部取值（整体子串扫描，独立于键名规则）。
	for _, ident := range []string{IdTenantA, IdTokenB} {
		body := strings.ToLower(string(m.req("listAuditLogs", ident, SitNone, "GET", list, nil).Body))
		for _, s := range f.in.Strings() {
			if strings.Contains(body, strings.ToLower(s)) {
				m.t.Errorf("%s 的审计列表含内部取值 %q", ident, s)
			}
		}
	}
}

// ---- 路由清单与真实进程的一致性 ----

func repoRoot(t testing.TB) string {
	t.Helper()
	p, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 矩阵使用的进程内装配（tests/apistack）必须与真实 lingedge-server 注册的路由集合一致：否则登记机制会漏掉真实进程里的租户路由。
func TestRegisteredRoutesMatchServerBinary(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("go", "run", "./cmd/lingedge-server", "routes")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("lingedge-server routes: %v", err)
	}
	var manifest []struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(out, &manifest); err != nil {
		t.Fatalf("路由清单不是 JSON: %v", err)
	}
	want := map[string]bool{}
	for _, m := range manifest {
		want[m.OperationID] = true
	}
	// 进程内装配的路由集合只需要路由器：不启动数据库也能得到——但 apistack 需要真实库，所以直接复用矩阵的世界。
	w := apistack.New(t)
	got := map[string]bool{}
	for _, r := range w.Router.Routes() {
		got[r.OperationID] = true
	}
	var diff []string
	for id := range want {
		if !got[id] {
			diff = append(diff, "真实进程有、矩阵装配没有: "+id)
		}
	}
	for id := range got {
		if !want[id] {
			diff = append(diff, "矩阵装配有、真实进程没有: "+id)
		}
	}
	sort.Strings(diff)
	if len(diff) > 0 {
		t.Fatalf("装配漂移:\n%s", strings.Join(diff, "\n"))
	}
	if len(want) < 40 {
		t.Fatalf("路由清单过少（%d）", len(want))
	}
}

// G-5 扫描器自测：日志里出现运行期敏感值必须被发现（反），干净日志不误报（正）。
func TestFindSecretsDetectsLeakAndAllowsClean(t *testing.T) {
	if hits := findSecrets(`{"msg":"login","prefix":"abcd1234"}`, []string{"S3cr3t-Value-xyz", "le_abcd1234_longsecretpart"}); len(hits) != 0 {
		t.Fatalf("干净日志不应命中: %v", hits)
	}
	if hits := findSecrets(`{"msg":"oops le_abcd1234_longsecretpart leaked"}`, []string{"S3cr3t-Value-xyz", "le_abcd1234_longsecretpart"}); len(hits) != 1 {
		t.Fatalf("应发现泄漏: %v", hits)
	}
}
