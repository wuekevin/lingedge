package auditit

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wuekevin/lingedge/internal/platform/audit"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/schedrt/builtin"
	schedrepo "github.com/wuekevin/lingedge/internal/platform/schedrt/repository"
	"github.com/wuekevin/lingedge/internal/platform/tenantscope"
	"github.com/wuekevin/lingedge/tests/apistack"
)

func fixedAt(s string) clock.Clock {
	ts, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return clock.Func(func() time.Time { return ts })
}

func partitionsOf(w *apistack.World) []string {
	w.T.Helper()
	rows, err := w.Adm.QueryContext(apistack.Ctx(w.T), "SELECT PARTITION_NAME FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = 'audit_logs' ORDER BY PARTITION_ORDINAL_POSITION", w.DBName)
	if err != nil {
		w.T.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			w.T.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// AC3/AC6：partition.maintain 对 audit_logs 的真实联调——lingedge_maint 账号（非应用账号）把 pmax REORGANIZE 出未来月份分区并保留 pmax；
// 真实应用账号写入的审计行（含落在 pmax 里的未来行）搬迁后不丢、不重；维护之后应用账号的写入立刻落入新分区；越界永远写得进 pmax（不报 ERROR 1526）。
// 保留期到期的 DROP PARTITION 属 Story 3.48，本 Story 不实现（maint 账号的表级 DROP 授权已在迁移 000003 就位）。
func TestPartitionMaintainRealAuditTable(t *testing.T) {
	w := apistack.New(t)
	adm := admin(w)
	tn := w.SeedTenant("part")
	for i := 0; i < 3; i++ { // 真实 API 写入（当前月份分区）
		if r := adm.Do("PATCH", "/tenants/"+tn.UID, map[string]any{"name": "分区联调 " + string(rune('A'+i))}); r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	}
	real := auditCount(w, "action = 'tenant.update'")
	if real != 3 {
		t.Fatalf("前置：应有 3 条真实审计: %d", real)
	}
	if got := partitionsOf(w); got[len(got)-1] != "pmax" || got[len(got)-2] != "p202712" {
		t.Fatalf("前置分区不符（应到 p202712 + pmax）: %v", got)
	}

	// 以固定时钟的真实 Writer（同一个应用账号连接池）写入「未来」的审计行：2028-01-15 与 2029-06-01 都在最后一个分区之外 → 落入 pmax。
	writeAt := func(day, tag string) {
		t.Helper()
		aw, err := audit.New(audit.Options{Pool: w.App, IDs: apistack.IDs, Clock: fixedAt(day)})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.App.WithTx(apistack.Ctx(t), func(ctx context.Context) error {
			return aw.Record(ctx, audit.ActionTaskCancel, audit.Event{ResourceType: "part-" + tag, ResourceUID: day, Actor: &audit.Actor{Type: audit.ActorSystem, Label: "partition-test"}})
		}); err != nil {
			t.Fatalf("应用账号写入 %s 失败（pmax 兜底分区应使越界写入永不失败）: %v", day, err)
		}
	}
	writeAt("2028-01-15", "jan")
	writeAt("2029-06-01", "far")
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs PARTITION (pmax)"); n != 2 {
		t.Fatalf("前置：两条未来审计应在 pmax: %d", n)
	}

	// 真实 maint 账号 + 内置作业（时钟 2027-12-20、预建 3 个月 → 到 2028-03）。
	parts, err := schedrepo.NewPartitions(w.MaintPool())
	if err != nil {
		t.Fatal(err)
	}
	job := builtin.PartitionMaintain(builtin.Deps{Scope: tenantscope.PlatformForJob(0), // [tenantscope-mint] 测试：Scheduler 是平台进程
		Clock: fixedAt("2027-12-20"), Logger: zap.NewNop(), Partitions: parts, Policy: builtin.PartitionPolicy{AheadMonths: 3, AheadDays: 2, MaxNewPerRun: 120}})
	if err := job(apistack.Ctx(t), time.Now()); err != nil {
		t.Fatalf("partition.maintain: %v", err)
	}
	got := partitionsOf(w)
	if strings.Join(got[len(got)-5:], ",") != "p202712,p202801,p202802,p202803,pmax" {
		t.Fatalf("预建分区后: %v", got)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs PARTITION (p202801)"); n != 1 {
		t.Fatalf("pmax 里的 2028-01 审计应被搬入 p202801: %d", n)
	}
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs PARTITION (pmax)"); n != 1 {
		t.Fatalf("2029 年的审计应仍在 pmax（pmax 被保留）: %d", n)
	}
	if n := auditCount(w, "action = 'tenant.update'"); n != real {
		t.Fatalf("真实审计行不得因分区重组而丢失/重复: %d → %d", real, n)
	}
	// 维护后应用账号立即可写：2028-03-05 落入新分区 p202803。
	writeAt("2028-03-05", "mar")
	if n := w.RootInt("SELECT COUNT(*) FROM §.audit_logs PARTITION (p202803)"); n != 1 {
		t.Fatalf("维护后的写入应落入新分区 p202803: %d", n)
	}
	// 重复触发幂等：已追平则分区不变。
	before := strings.Join(partitionsOf(w), ",")
	if err := job(apistack.Ctx(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	if after := strings.Join(partitionsOf(w), ","); after != before {
		t.Fatalf("重复触发不应改变分区:\n%s\n%s", before, after)
	}
	// 跨分区的读取（审计 API 游标分页）：搬迁后的行与新行都可读。
	for _, rt := range []string{"part-jan", "part-far", "part-mar"} {
		r := adm.Do("GET", "/audit-logs?resource_type="+rt, nil)
		if r.Status != 200 || len(uidsOf(t, r)) != 1 {
			t.Errorf("%s 应可经 API 读到: %d %s", rt, r.Status, r.Body)
		}
	}

	// 账号分工：maint 账号只有 SELECT/ALTER/DROP——不能 INSERT/UPDATE/DELETE 审计行（保留期清理走 DROP PARTITION，不走行级 DELETE；实现在 3.48）。
	mp := w.MaintPool()
	ctx := apistack.Ctx(t)
	for name, q := range map[string]db.Query{
		"DELETE": db.Q("DELETE FROM audit_logs WHERE 1 = 1"),
		"UPDATE": db.Q("UPDATE audit_logs SET action = 'x' WHERE 1 = 1"),
		"INSERT": db.Q(`INSERT INTO audit_logs (uid, actor_type, actor_label, action, resource_type, result, request_id) VALUES ('x', 'system', 's', 'a', 'r', 'success', 'x')`),
	} {
		_, err := mp.Executor(ctx).Exec(ctx, q)
		if n, ok := db.MySQLErrorNumber(err); !ok || n != db.MySQLErrTableAccessDenied {
			t.Errorf("maint 账号 %s 应因权限被拒（1142）: %v", name, err)
		}
	}
}
