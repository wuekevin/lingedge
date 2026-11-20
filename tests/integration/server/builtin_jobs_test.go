package serverintegration

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/idempotency"
	idemrepo "github.com/wuekevin/lingedge/internal/platform/idempotency/repository"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/schedrt/builtin"
	schedrepo "github.com/wuekevin/lingedge/internal/platform/schedrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/server"
	taskrepo "github.com/wuekevin/lingedge/internal/platform/taskrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

// 内置内部周期作业的真实效果（Story 1.15 AC2）：真实 MySQL、真实 lingedge_app / lingedge_maint 账号，无 Mock。
// 注意：向 root 会话写时间一律用 UTC_TIMESTAMP(3)（临时实例的服务器时区可能不是 UTC，而 app/maint 连接固定 UTC）。

func (e *env) builtinDeps(t *testing.T, clk clock.Clock, parts builtin.PartitionStore) builtin.Deps {
	t.Helper()
	ids := uid.NewGenerator(clock.System())
	tasks, err := taskrepo.New(e.app, ids)
	if err != nil {
		t.Fatal(err)
	}
	idem, err := idemrepo.New(e.app, idemrepo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := schedrepo.NewSessions(e.app)
	if err != nil {
		t.Fatal(err)
	}
	return builtin.Deps{Scope: tenantscope.PlatformForJob(0), // [tenantscope-mint] 测试：Scheduler 是平台进程
		Clock: clk, Logger: discardLogger(t), Tasks: tasks, Idempotency: idem, Sessions: sessions, Partitions: parts}
}

// ---- idempotency.cleanup：24h 边界 ----

func TestIdempotencyCleanupBoundary(t *testing.T) {
	e := newEnv(t, all)
	hash := strings.Repeat("a", 64)
	begin := func(key string, created time.Time) {
		st, err := idemrepo.New(e.app, idemrepo.Options{Clock: clock.Func(func() time.Time { return created })})
		if err != nil {
			t.Fatal(err)
		}
		out, err := st.Begin(ctxT(t), idempotency.Request{PrincipalKey: "platform:1:0", Key: key, Method: "POST", Path: "/api/v1/x", Hash: hash})
		if err != nil || out.Result != idempotency.Acquired {
			t.Fatalf("Begin %s: %v %v", key, out.Result, err)
		}
	}
	now := time.Now().UTC()
	begin("expired-long", now.Add(-48*time.Hour))
	begin("expired-2m", now.Add(-24*time.Hour-2*time.Minute)) // 创建于 24h2m 前 → 已过期 2 分钟
	begin("alive-2m", now.Add(-24*time.Hour+2*time.Minute))   // 创建于 23h58m 前 → 再过 2 分钟才到期
	begin("fresh", now)
	if ttl := e.rootInt(t, "SELECT TIMESTAMPDIFF(HOUR, created_at, expires_at) FROM §.idempotency_keys WHERE idem_key = 'fresh'"); ttl != 24 {
		t.Fatalf("记录保留 %dh，应为 24h", ttl)
	}
	job := builtin.IdempotencyCleanup(e.builtinDeps(t, clock.System(), nil))
	for i := 0; i < 2; i++ { // 第二次：重复触发不产生任何额外副作用
		if err := job(ctxT(t), now); err != nil {
			t.Fatal(err)
		}
		left := e.rootString(t, "SELECT GROUP_CONCAT(idem_key ORDER BY idem_key) FROM §.idempotency_keys")
		if left != "alive-2m,fresh" {
			t.Fatalf("第 %d 次清理后剩余 %q，应只剩未到 24h 的记录", i+1, left)
		}
	}
}

// ---- sessions.cleanup：过期/吊销后 30 天边界 ----

func TestSessionsCleanupBoundary(t *testing.T) {
	e := newEnv(t, all)
	uidv := e.ids2().MustNew()
	e.root(t, "INSERT INTO §.users (uid, user_type, email, display_name, password_hash) VALUES (?, 'platform', 'sess@example.test', 'x', 'x')", uidv)
	userID := e.rootInt(t, "SELECT id FROM §.users WHERE uid = ?", uidv)
	// 每行 (标签, expires_at, absolute_expires_at, revoked_at)，相对 UTC 现在；期望：是否被删除。
	type c struct {
		label             string
		exp, abs, revoked string
		wantDeleted       bool
	}
	ago := func(h int) string { return fmt.Sprintf("UTC_TIMESTAMP(3) - INTERVAL %d HOUR", h) }
	ahead := func(h int) string { return fmt.Sprintf("UTC_TIMESTAMP(3) + INTERVAL %d HOUR", h) }
	cases := []c{
		{"idle-expired-31d", ago(31 * 24), ahead(24), "NULL", true},
		{"idle-expired-30d-1h", ago(30*24 + 1), ahead(24), "NULL", true},
		{"idle-expired-30d+1h", ago(30*24 - 1), ahead(24), "NULL", false}, // 过期未满 30 天
		{"idle-expired-29d", ago(29 * 24), ahead(24), "NULL", false},
		{"abs-expired-31d", ahead(24), ago(31 * 24), "NULL", true}, // 绝对过期也算结束
		{"abs-expired-29d", ahead(24), ago(29 * 24), "NULL", false},
		{"revoked-31d", ahead(24), ahead(48), ago(31 * 24), true},
		{"revoked-29d", ahead(24), ahead(48), ago(29 * 24), false},
		{"active", ahead(24), ahead(48), "NULL", false},
		{"active-revoked-just-now", ahead(24), ahead(48), ago(0), false},
	}
	for i, x := range cases {
		e.root(t, fmt.Sprintf("INSERT INTO §.sessions (token_hash, csrf_hash, user_id, realm, last_seen_at, expires_at, absolute_expires_at, revoked_at) "+
			"VALUES (?, ?, ?, 'admin', UTC_TIMESTAMP(3), %s, %s, %s)", x.exp, x.abs, x.revoked), strconv.Itoa(1000+i)+strings.Repeat("0", 60), strings.Repeat("c", 64), userID)
	}
	job := builtin.SessionsCleanup(e.builtinDeps(t, clock.System(), nil))
	for i := 0; i < 2; i++ {
		if err := job(ctxT(t), time.Now()); err != nil {
			t.Fatal(err)
		}
		for j, x := range cases {
			n := e.rootInt(t, "SELECT COUNT(*) FROM §.sessions WHERE token_hash = ?", strconv.Itoa(1000+j)+strings.Repeat("0", 60))
			if (n == 0) != x.wantDeleted {
				t.Errorf("第 %d 次清理后 %s：存在=%d，期望被删除=%v", i+1, x.label, n, x.wantDeleted)
			}
		}
	}
	// 他人不得借非平台 Scope 触发清理。
	repo, _ := schedrepo.NewSessions(e.app)
	ts, err := tenantscope.FromTask(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DeleteEnded(ctxT(t), ts, time.Hour, 10); err == nil {
		t.Error("租户 Scope 不得执行跨租户的会话清理")
	}
}

func (e *env) ids2() *uid.Generator { return uid.NewGenerator(clock.System()) }

// ---- partition.maintain ----

func (e *env) maintPool(t *testing.T) *db.MaintPool {
	t.Helper()
	p, err := db.OpenMaint(ctxT(t), db.Config{Host: e.inst.Host(), Port: e.inst.Port, Name: e.dbName, User: migrate.MaintUser, Password: config.NewSecret(e.maintPW), MaxOpenConns: 2})
	if err != nil {
		t.Fatalf("OpenMaint: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// partitions 返回表的分区名（按序，root 视角）。
func (e *env) partitions(t *testing.T, table string) []string {
	t.Helper()
	rows, err := e.adm.QueryContext(ctxT(t), "SELECT PARTITION_NAME FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY PARTITION_ORDINAL_POSITION", e.dbName, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n sql.NullString
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n.String)
	}
	return out
}

func fixedClock(s string) clock.Clock {
	ts, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return clock.Func(func() time.Time { return ts })
}

func TestPartitionMaintainRollsPartitionsAndKeepsPmax(t *testing.T) {
	e := newEnv(t, all)
	parts, err := schedrepo.NewPartitions(e.maintPool(t))
	if err != nil {
		t.Fatal(err)
	}
	// 前置：audit_logs 到 p202712 + pmax；一行 2028-01-15 的审计记录落在 pmax。
	if got := e.partitions(t, "audit_logs"); got[len(got)-1] != "pmax" || got[len(got)-2] != "p202712" {
		t.Fatalf("前置分区不符: %v", got)
	}
	e.root(t, "INSERT INTO §.audit_logs (uid, created_at, actor_type, actor_label, action, resource_type, result, request_id) VALUES (?, '2028-01-15 10:00:00.000', 'system', 'sys', 'test.x', 'test', 'success', ?)",
		e.ids2().MustNew(), e.ids2().MustNew())
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs PARTITION (pmax)"); n != 1 {
		t.Fatalf("前置：行应在 pmax: %d", n)
	}

	// 时钟 2027-12-20、预建 3 个月：到 2028-03（含）。
	d := e.builtinDeps(t, fixedClock("2027-12-20"), parts)
	d.Policy = builtin.PartitionPolicy{AheadMonths: 3, AheadDays: 2, MaxNewPerRun: 120}
	job := builtin.PartitionMaintain(d)
	if err := job(ctxT(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	got := e.partitions(t, "audit_logs")
	if strings.Join(got[len(got)-5:], ",") != "p202712,p202801,p202802,p202803,pmax" {
		t.Fatalf("按月预建后分区: %v", got)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs PARTITION (p202801)"); n != 1 {
		t.Fatalf("pmax 里的 2028-01 行应被搬入 p202801: %d", n)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs PARTITION (pmax)"); n != 0 {
		t.Fatalf("pmax 应已清空: %d", n)
	}
	// 新分区立刻可用：2028-03-05 的写入落入 p202803 而不是 pmax；越界的 2029 年仍写得进 pmax（永远不报 ERROR 1526）。
	for _, ts := range []string{"2028-03-05 00:00:00.000", "2029-06-01 00:00:00.000"} {
		e.root(t, "INSERT INTO §.audit_logs (uid, created_at, actor_type, actor_label, action, resource_type, result, request_id) VALUES (?, ?, 'system', 'sys', 'test.y', 'test', 'success', ?)",
			e.ids2().MustNew(), ts, e.ids2().MustNew())
	}
	if a, b := e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs PARTITION (p202803)"), e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs PARTITION (pmax)"); a != 1 || b != 1 {
		t.Fatalf("p202803=%d pmax=%d，应各 1", a, b)
	}
	// node_heartbeats 同时被按日预建（2027-12-20 距最后分区 p20261127 已久：单次最多新建 MaxNewPerRun 个，分多次追上）。
	hb := e.partitions(t, "node_heartbeats")
	if len(hb) != 8+120+1 || hb[len(hb)-1] != "pmax" || hb[8] != "p20261128" {
		t.Fatalf("node_heartbeats 分区数=%d（应 8+120+pmax），首个新分区 %v", len(hb), hb[8])
	}
	// 重复触发：已追平的表（audit_logs）无变化（幂等）；积压表继续每次追 MaxNewPerRun 个。
	before := strings.Join(e.partitions(t, "audit_logs"), ",")
	if err := job(ctxT(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	if after := strings.Join(e.partitions(t, "audit_logs"), ","); after != before {
		t.Fatalf("重复触发改变了分区:\n%s\n%s", before, after)
	}
}

func TestPartitionMaintainOnlyTouchesExistingAuthorizedTables(t *testing.T) {
	e := newEnv(t, all)
	parts, err := schedrepo.NewPartitions(e.maintPool(t))
	if err != nil {
		t.Fatal(err)
	}
	// ext_part：形态合规，但 maint 未被授权 → 看不到、不处理；
	// bad_part：maint 被授权，但没有 pmax 兜底分区 → 不受支持：拒绝改动并报错（告警），不影响其他表。
	e.root(t, "CREATE TABLE §.ext_part (id BIGINT NOT NULL, created_at DATETIME(3) NOT NULL, PRIMARY KEY (id, created_at)) PARTITION BY RANGE COLUMNS(created_at) "+
		"(PARTITION p202611 VALUES LESS THAN ('2026-12-01 00:00:00'), PARTITION pmax VALUES LESS THAN (MAXVALUE))")
	e.root(t, "CREATE TABLE §.bad_part (id BIGINT NOT NULL, created_at DATETIME(3) NOT NULL, PRIMARY KEY (id, created_at)) PARTITION BY RANGE COLUMNS(created_at) "+
		"(PARTITION p202611 VALUES LESS THAN ('2026-12-01 00:00:00'), PARTITION p202612 VALUES LESS THAN ('2027-01-01 00:00:00'))")
	e.root(t, "GRANT SELECT, ALTER, DROP ON §.bad_part TO '"+migrate.MaintUser+"'@'%'")
	extBefore, badBefore := strings.Join(e.partitions(t, "ext_part"), ","), strings.Join(e.partitions(t, "bad_part"), ",")

	d := e.builtinDeps(t, fixedClock("2026-11-25"), parts)
	d.Policy = builtin.PartitionPolicy{AheadMonths: 14, AheadDays: 3, MaxNewPerRun: 120}
	err = builtin.PartitionMaintain(d)(ctxT(t), time.Now())
	if err == nil || !strings.Contains(err.Error(), "bad_part") || !strings.Contains(err.Error(), "pmax") {
		t.Fatalf("不受支持的分区表应使作业失败（告警）并指明表与原因: %v", err)
	}
	if strings.Contains(err.Error(), "audit_logs") || strings.Contains(err.Error(), "node_heartbeats") {
		t.Errorf("合规表不应出错: %v", err)
	}
	if a := strings.Join(e.partitions(t, "ext_part"), ","); a != extBefore {
		t.Errorf("未授权的表被改动: %s", a)
	}
	if a := strings.Join(e.partitions(t, "bad_part"), ","); a != badBefore {
		t.Errorf("不受支持的表被改动: %s", a)
	}
	// 出错的表不阻止其余表：audit_logs 预建到 2027-12 之后（14 个月：2026-11 + 14 → 到 2028-01），node_heartbeats 预建 3 天。
	if got := e.partitions(t, "audit_logs"); got[len(got)-2] != "p202801" {
		t.Errorf("audit_logs 应继续被维护: %v", got[len(got)-3:])
	}
	if got := e.partitions(t, "node_heartbeats"); got[len(got)-2] != "p20261128" {
		t.Errorf("node_heartbeats 应继续被维护: %v", got[len(got)-3:])
	}
	// 失败不影响写入：应用账号照常写 audit 与心跳表。
	if _, err := e.app.Executor(ctxT(t)).Exec(ctxT(t), db.Q("INSERT INTO node_heartbeats (node_id, received_at, agent_version, openresty_state) VALUES (1, '2026-11-25 01:00:00.000', 'v', 'running')")); err != nil {
		t.Fatalf("分区维护失败期间写入应不受影响: %v", err)
	}
}

// 以进程内真实 server.Run 装配 scheduler 角色，验证 maint 账号的三种配置形态：缺失（明确跳过告警）、凭据错误（失败告警，不影响写入）、正确（真实预建）。
type schedServer struct {
	logs   *syncBuf
	cancel context.CancelFunc
	done   chan error
}

func (e *env) runSchedServer(t *testing.T, extraEnv []string, openMaint func(context.Context, config.Database) (*db.MaintPool, error)) *schedServer {
	t.Helper()
	logs := &syncBuf{}
	lg, err := logx.New(logx.Options{Level: "info", Output: logs})
	if err != nil {
		t.Fatal(err)
	}
	e.ports.metrics = freePort(t)
	e.writeConfig(t)
	cfg, err := config.Load(e.cfgPath, config.Options{Environ: e.environ(extraEnv...), RequireRuntime: true})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := server.ParseRoles("scheduler")
	ctx, cancel := context.WithCancel(context.Background())
	s := &schedServer{logs: logs, cancel: cancel, done: make(chan error, 1)}
	ready := make(chan struct{})
	go func() {
		s.done <- server.Run(ctx, server.Options{Config: cfg, Roles: rs, Logger: lg, OpenMaint: openMaint, OnReady: func(server.Info) { close(ready) }})
	}()
	t.Cleanup(cancel)
	select {
	case <-ready:
	case err := <-s.done:
		t.Fatalf("启动失败: %v\n%s", err, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatal("启动超时")
	}
	return s
}

func (s *schedServer) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("停机应无错误: %v\n%s", err, s.logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("停机超时")
	}
}

func openMaintFromConfig(ctx context.Context, d config.Database) (*db.MaintPool, error) {
	cfg, ok := db.MaintConfigFrom(d)
	if !ok {
		return nil, nil
	}
	return db.OpenMaint(ctx, cfg)
}

func (e *env) maintEnv(pw string) []string {
	return []string{"LINGEDGE_DATABASE__MAINT__USER=" + migrate.MaintUser, "LINGEDGE_DATABASE__MAINT__PASSWORD=" + pw}
}

func TestSchedulerPartitionMaintainMaintConfigurations(t *testing.T) {
	e := newEnv(t, all)
	hbBefore := e.partitions(t, "node_heartbeats")

	// 1) maint 缺失：明确跳过并告警；分区原样；不是成功。
	s := e.runSchedServer(t, nil, openMaintFromConfig)
	eventually(t, "partition.maintain 被跳过并告警", 15*time.Second, func() bool {
		return strings.Contains(s.logs.String(), "周期作业已跳过") && strings.Contains(s.logs.String(), `"job":"partition.maintain"`)
	})
	s.stop(t)
	if strings.Contains(s.logs.String(), "已滚动预建分区") {
		t.Error("maint 缺失时不得预建分区或报成功")
	}
	if got := e.partitions(t, "node_heartbeats"); strings.Join(got, ",") != strings.Join(hbBefore, ",") {
		t.Fatalf("maint 缺失时分区不得变化: %v", got)
	}

	// 2) maint 凭据错误：作业失败（告警、窗口内退避重试），服务照常运行，写入不受影响。
	s = e.runSchedServer(t, e.maintEnv("wrong-"+e.maintPW), openMaintFromConfig)
	eventually(t, "partition.maintain 失败告警", 15*time.Second, func() bool {
		return strings.Contains(s.logs.String(), "周期作业失败（告警）") && strings.Contains(s.logs.String(), "无法以 maint 账号连接数据库")
	})
	if _, err := e.app.Executor(ctxT(t)).Exec(ctxT(t), db.Q("INSERT INTO node_heartbeats (node_id, received_at, agent_version, openresty_state) VALUES (1, '2026-11-20 01:00:00.000', 'v', 'running')")); err != nil {
		t.Fatalf("maint 失败期间写入应不受影响: %v", err)
	}
	s.stop(t)
	if got := e.partitions(t, "node_heartbeats"); strings.Join(got, ",") != strings.Join(hbBefore, ",") {
		t.Fatalf("maint 凭据错误时分区不得变化: %v", got)
	}

	// 失败的窗口租约保持到重试时刻（窗口 1/4，上限 5 分钟）：这里模拟重试延迟已过，下一次进程/窗口可重试。
	e.root(t, "UPDATE §.job_locks SET lease_until = UTC_TIMESTAMP(3) WHERE name = 'job.partition.maintain'")

	// 3) maint 正确：真实预建（node_heartbeats 按日滚动，pmax 保留）。
	s = e.runSchedServer(t, e.maintEnv(e.maintPW), openMaintFromConfig)
	eventually(t, "partition.maintain 预建分区", 20*time.Second, func() bool { return strings.Contains(s.logs.String(), "已滚动预建分区") })
	s.stop(t)
	got := e.partitions(t, "node_heartbeats")
	if got[len(got)-1] != "pmax" || len(got) <= len(hbBefore) {
		t.Fatalf("应新增分区且保留 pmax: %v", got)
	}
	if strings.Contains(s.logs.String(), "周期作业失败（告警）") {
		t.Errorf("正确配置下不应有失败告警:\n%s", s.logs.String())
	}
}
