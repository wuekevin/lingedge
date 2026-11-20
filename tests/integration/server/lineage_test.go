package serverintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/lineage"
	"github.com/wuekevin/lingedge/internal/platform/uid"
)

func (e *env) seedLineage(t testing.TB, v int64) {
	t.Helper()
	e.root(t, "INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES ('01HZZZZZZZZZZZZZZZZZZZZZZZ', 'deploy.lineage', CAST(? AS JSON), 'deploy')", strconv.FormatInt(v, 10))
}

func (e *env) lineageValue(t testing.TB) int64 {
	t.Helper()
	return e.rootInt(t, "SELECT CAST(value_json AS UNSIGNED) FROM §.system_settings WHERE setting_key = 'deploy.lineage'")
}

type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// AC2：bump = max(当前值+1, 当前 UNIX 秒)，写审计，重复执行严格单调递增（真实 MySQL）。
func TestLineageBumpRule(t *testing.T) {
	e := newEnv(t, all)
	clk := &stepClock{t: time.Unix(1_900_000_000, 0).UTC()}
	svc, err := lineage.New(e.app, clk, uid.NewGenerator(clock.System()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxT(t)

	// 设置项不存在：拒绝，且不写审计、不创建设置项。
	if _, err := svc.Show(ctx); !errors.Is(err, lineage.ErrNotInstalled) {
		t.Fatalf("show 应报未安装: %v", err)
	}
	if _, err := svc.Bump(ctx); !errors.Is(err, lineage.ErrNotInstalled) {
		t.Fatalf("bump 应拒绝: %v", err)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.system_settings WHERE setting_key = 'deploy.lineage'"); n != 0 {
		t.Fatalf("拒绝后不得创建设置项: %d", n)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.lineage.bump'"); n != 0 {
		t.Fatalf("拒绝时不得写审计: %d", n)
	}

	e.seedLineage(t, 1)
	// 当前 UNIX 秒远大于 当前值+1 → 取 UNIX 秒。
	r, err := svc.Bump(ctx)
	if err != nil || r.Before != 1 || r.After != 1_900_000_000 {
		t.Fatalf("bump #1: %v %+v", err, r)
	}
	// 时钟不动（同一秒内重复执行 / 时钟回拨）→ 取 当前值+1，严格递增。
	prev := r.After
	for i := 0; i < 5; i++ {
		r, err = svc.Bump(ctx)
		if err != nil || r.Before != prev || r.After != prev+1 {
			t.Fatalf("重复 bump #%d: %v %+v（应 %d → %d）", i, err, r, prev, prev+1)
		}
		prev = r.After
	}
	// 时钟回拨到更早：仍然单调。
	clk.set(time.Unix(1_000_000_000, 0).UTC())
	if r, err = svc.Bump(ctx); err != nil || r.After != prev+1 {
		t.Fatalf("时钟回拨后应继续 +1: %v %+v", err, r)
	}
	prev = r.After
	// 时钟前进超过 当前值+1：跳到新的 UNIX 秒。
	clk.set(time.Unix(prev+1000, 0).UTC())
	if r, err = svc.Bump(ctx); err != nil || r.After != prev+1000 {
		t.Fatalf("时钟前进后应取 UNIX 秒: %v %+v", err, r)
	}
	if got := e.lineageValue(t); got != r.After {
		t.Fatalf("库内值 %d != %d", got, r.After)
	}
	if v, err := svc.Show(ctx); err != nil || v != r.After {
		t.Fatalf("show: %v %d", err, v)
	}

	// 审计：8 次成功 bump 各一条，含前后值与 request_id。
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.lineage.bump' AND result = 'success' AND resource_uid = 'deploy.lineage' AND actor_type = 'system'"); n != 8 {
		t.Fatalf("审计条数 = %d，期望 8", n)
	}
	ch := e.rootString(t, "SELECT CAST(changes_json AS CHAR) FROM §.audit_logs WHERE request_id = ? AND action = 'system.lineage.bump'", r.RequestID)
	var changes map[string]int64
	if err := json.Unmarshal([]byte(ch), &changes); err != nil || changes["before"] != r.Before || changes["after"] != r.After {
		t.Fatalf("审计 changes_json 不符: %q %v", ch, err)
	}
}

// 值异常（不是正整数）：拒绝并保持原值。
func TestLineageRejectsInvalidStoredValue(t *testing.T) {
	e := newEnv(t, all)
	svc, _ := lineage.New(e.app, nil, uid.NewGenerator(clock.System()))
	e.root(t, "INSERT INTO §.system_settings (uid, setting_key, value_json, group_name) VALUES ('01HZZZZZZZZZZZZZZZZZZZZZZZ', 'deploy.lineage', CAST('\"abc\"' AS JSON), 'deploy')")
	if _, err := svc.Bump(ctxT(t)); !errors.Is(err, lineage.ErrInvalidValue) {
		t.Fatalf("应拒绝: %v", err)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.lineage.bump'"); n != 0 {
		t.Fatalf("拒绝时不得写审计: %d", n)
	}
}

// 并发 bump：串行化（FOR UPDATE），结果互不相同且恰为连续递增。
func TestLineageConcurrentBumpsAreSerialized(t *testing.T) {
	e := newEnv(t, all)
	clk := &stepClock{t: time.Unix(5, 0).UTC()} // UNIX 秒很小：恒取 当前值+1
	svc, _ := lineage.New(e.app, clk, uid.NewGenerator(clock.System()))
	e.seedLineage(t, 10)
	const n = 12
	var wg sync.WaitGroup
	results := make([]int64, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := svc.Bump(context.Background())
			results[i], errs[i] = r.After, err
		}()
	}
	wg.Wait()
	seen := map[int64]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发 bump 失败: %v", errs[i])
		}
		seen[results[i]] = true
	}
	for v := int64(11); v < 11+n; v++ {
		if !seen[v] {
			t.Fatalf("缺少 %d：并发结果应恰为 11..%d 各一次: %v", v, 10+n, results)
		}
	}
	if got := e.lineageValue(t); got != 10+n {
		t.Fatalf("终值 %d，期望 %d", got, 10+n)
	}
}

// 审计写失败 → 整个事务回滚，lineage 不变（单事务、与审计同成败）。
func TestLineageBumpRollsBackWhenAuditFails(t *testing.T) {
	e := newEnv(t, all)
	e.seedLineage(t, 5)
	// Bump 先取 request_id 再取审计 uid：failingIDs 令 request_id 超过 CHAR(26)，使审计 INSERT 失败。
	svc, _ := lineage.New(e.app, nil, &failingIDs{})
	if _, err := svc.Bump(ctxT(t)); err == nil {
		t.Fatal("审计写入失败时 bump 必须失败")
	}
	if got := e.lineageValue(t); got != 5 {
		t.Fatalf("回滚后 lineage 应仍为 5，实际 %d", got)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.audit_logs WHERE action = 'system.lineage.bump'"); n != 0 {
		t.Fatalf("回滚后不得残留审计: %d", n)
	}
}

type failingIDs struct{ n int }

func (f *failingIDs) New() (string, error) {
	f.n++
	if f.n == 1 { // request_id（Bump 第一个取）：超长，使 INSERT 失败
		return strings.Repeat("X", 40), nil
	}
	return "01HZZZZZZZZZZZZZZZZZZZZZZZ", nil
}

// 真实子命令：`lingedge-server lineage show|bump`（退出码、输出、未安装拒绝）。
func TestLineageSubcommandRealBinary(t *testing.T) {
	e := newEnv(t, all)
	bin := buildServer(t)
	run := func(args ...string) (string, string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Env = e.environ()
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return so.String(), se.String(), code
	}
	// 安装未完成：拒绝并提示。
	_, se, code := run("lineage", "bump", "--config", e.cfgPath)
	if code != 1 || !strings.Contains(se, "安装尚未完成") {
		t.Fatalf("未安装时 bump 应退出 1 并提示: code=%d stderr=%q", code, se)
	}
	_, se, code = run("lineage", "show", "--config", e.cfgPath)
	if code != 1 || !strings.Contains(se, "安装尚未完成") {
		t.Fatalf("未安装时 show 应退出 1: code=%d stderr=%q", code, se)
	}
	e.seedLineage(t, 7)
	so, _, code := run("lineage", "show", "--config", e.cfgPath)
	if code != 0 || strings.TrimSpace(so) != "7" {
		t.Fatalf("show: code=%d out=%q", code, so)
	}
	now := time.Now().Unix()
	so, se, code = run("lineage", "bump", "--config", e.cfgPath, "--json")
	if code != 0 {
		t.Fatalf("bump: code=%d stderr=%s", code, se)
	}
	var r struct {
		Before, After int64
		RequestID     string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(so), &r); err != nil || r.Before != 7 || r.After < now || r.RequestID == "" {
		t.Fatalf("bump 输出: %q %v %+v（期望 after >= %d）", so, err, r, now)
	}
	// 重复执行严格递增。
	so2, _, code := run("lineage", "bump", "--config", e.cfgPath, "--json")
	var r2 struct{ Before, After int64 }
	if code != 0 || json.Unmarshal([]byte(so2), &r2) != nil || r2.Before != r.After || r2.After <= r.After {
		t.Fatalf("重复 bump: code=%d out=%q", code, so2)
	}
	// 参数错误：退出码 2。
	if _, _, code := run("lineage"); code != 2 {
		t.Fatalf("缺子命令应退出 2: %d", code)
	}
	if _, _, code := run("lineage", "bump", "--bogus"); code != 2 {
		t.Fatalf("未知 flag 应退出 2: %d", code)
	}
}
