package migrationstest

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// 故障注入一：迁移中途失败（真实 MySQL 错误 1146：`CREATE TABLE ... LIKE` 一个运维尚未创建的表）。
// 断言：失败的迁移不被记为已应用（goose 版本表无记录）、被标记 dirty、dirty 时拒绝继续并给出恢复指引；
// 排除故障原因并 recover 后重跑，结果与「一次性执行」逐表逐授权等价（AC4）。
func TestMidMigrationFailureThenRerunIsEquivalentToOneShot(t *testing.T) {
	ctx := ctxT(t)
	fsys := fixtureFS(t, "fault")
	const gate = "CREATE TABLE fault_gate (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY, extra VARCHAR(8) NULL) ENGINE=InnoDB"

	e := newEnv(t)
	r := e.runner(t, fsys)
	_, err := r.Up(ctx, migrate.UpOptions{})
	if err == nil {
		t.Fatal("fault_gate 不存在时迁移必须失败")
	}
	if !strings.Contains(err.Error(), "1146") {
		t.Fatalf("期望真实 MySQL 错误 1146（表不存在），实际: %v", err)
	}
	// 半成品是真实存在的（MySQL DDL 隐式提交），但没有被记为已应用，且被标记 dirty。
	if !e.hasTable(t, "fault_a") || e.hasTable(t, "fault_b") || e.hasTable(t, "fault_c") {
		t.Fatalf("中途失败应留下第一张表、不含后两张: %v", e.tables(t))
	}
	wantVersions(t, e)
	row := e.scalar(t, "SELECT CONCAT(status, '|', direction, '|', IFNULL(error, '')) FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 1")
	if !strings.HasPrefix(row, "failed|up|") || !strings.Contains(row, "1146") {
		t.Fatalf("运行记录应为 failed 并带 MySQL 错误: %q", row)
	}
	if g := e.grantsOf(t, migrate.AppUser); len(g) != 0 {
		t.Fatalf("授权语句在失败点之后，不应已执行: %v", g)
	}

	// dirty：拒绝继续（含重复执行 Up、Down），并给出恢复指引。
	for name, run := range map[string]func() error{
		"up":   func() error { _, err := r.Up(ctx, migrate.UpOptions{}); return err },
		"down": func() error { _, err := r.Down(ctx, migrate.DownOptions{}); return err },
	} {
		err := run()
		var d *migrate.ErrDirty
		if !errors.As(err, &d) || d.Version != 1 || d.Status != "failed" || d.Direction != "up" {
			t.Fatalf("%s: dirty 时必须拒绝继续: %v", name, err)
		}
		if migrate.ExitCode(err) != migrate.ExitDirty {
			t.Fatalf("%s: 退出码 = %d", name, migrate.ExitCode(err))
		}
		for _, want := range []string{"recover --version 1", "幂等", "备份"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: 恢复指引缺少 %q:\n%v", name, want, err)
			}
		}
	}
	if e.hasTable(t, "fault_c") {
		t.Fatal("dirty 时不得继续执行")
	}
	st, err := r.Status(ctx)
	if err != nil || st.Dirty == nil || st.Dirty.Version != 1 || st.Err() == nil || migrate.ExitCode(st.Err()) != migrate.ExitDirty {
		t.Fatalf("status 应报告 dirty: %+v %v", st, err)
	}
	if st.Migrations[0].State != "dirty" {
		t.Fatalf("status 项应为 dirty: %+v", st.Migrations[0])
	}

	// recover 的防呆：版本不符、无 dirty 时都拒绝。
	if err := r.Recover(ctx, 7); err == nil || !strings.Contains(err.Error(), "--version 1") {
		t.Fatalf("recover 指定错误的版本应被拒绝并提示正确版本: %v", err)
	}
	// 不排除故障就 recover + 重跑：仍然失败、仍然 dirty，不会假装成功。
	if err := r.Recover(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{}); err == nil {
		t.Fatal("故障未排除，重跑必须仍然失败")
	}
	if _, err := r.Up(ctx, migrate.UpOptions{}); !errors.As(err, new(*migrate.ErrDirty)) {
		t.Fatalf("再次失败后仍应 dirty: %v", err)
	}

	// 排除故障（运维创建 fault_gate）→ recover → 重跑成功。
	if _, err := e.admin(t).ExecContext(ctx, strings.Replace(gate, "fault_gate", "`"+e.dbName+"`.fault_gate", 1)); err != nil {
		t.Fatal(err)
	}
	if err := r.Recover(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatalf("排除故障后重跑: %v", err)
	}
	wantVersions(t, e, "1")

	// 参照：全新库、故障条件预先满足、一次性执行。
	one := newEnv(t)
	if _, err := one.admin(t).ExecContext(ctx, strings.Replace(gate, "fault_gate", "`"+one.dbName+"`.fault_gate", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := one.runner(t, fsys).Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatalf("一次性执行: %v", err)
	}
	got, want := e.schemaSignature(t), one.schemaSignature(t)
	if got != want {
		t.Fatalf("中断后重跑的结果必须与一次性执行等价:\n--- 重跑\n%s\n--- 一次性\n%s", got, want)
	}
	for _, tbl := range []string{"fault_a", "fault_b", "fault_c"} {
		if !strings.Contains(got, "== "+tbl) {
			t.Errorf("缺少 %s", tbl)
		}
	}
	if g := e.grantsOf(t, migrate.AppUser); len(g) != 3 {
		t.Errorf("重跑后三张新表都应有逐表授权: %v", g)
	}
}

// 故障注入二：迁移执行中途进程被杀（SIGKILL，模拟崩溃/断电）。运行记录停在 running（进程没有机会写 failed），同样视为 dirty。
func TestKilledProcessLeavesDirtyThenRecoverAndRerun(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	bin := buildHelper(t, "./tests/migrations/cmd/migratebin")
	crashDir := t.TempDir()
	copyFixtureDir(t, "crash", crashDir)

	cmd := exec.Command(bin, "up", "--host", e.inst.Host(), "--port", fmt.Sprint(e.inst.Port), "--database", e.dbName, "--user", e.user)
	cmd.Env = append(os.Environ(), "LINGEDGE_TEST_MIGRATIONS_DIR="+crashDir, migrate.EnvDBPass+"="+e.pw)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// 等到迁移真的执行到休眠语句（crash_a 已建、第二条语句在服务端运行），再杀进程。
	var sleepID string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		sleepID = e.scalar(t, "SELECT ID FROM information_schema.PROCESSLIST WHERE DB = ? AND INFO LIKE 'DO SLEEP%' LIMIT 1", e.dbName)
		if sleepID != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if sleepID == "" {
		t.Fatalf("迁移进程没有执行到休眠语句；输出:\n%s", out.String())
	}
	if !e.hasTable(t, "crash_a") || e.hasTable(t, "crash_b") {
		t.Fatalf("杀进程前应已建 crash_a、尚未建 crash_b: %v", e.tables(t))
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	t.Logf("迁移进程已被 SIGKILL，退出状态: %v", <-waited)
	// 客户端死亡后，服务端要等休眠语句结束才会发现连接已断；测试用 KILL 代替「服务端发现连接断开」，随后互斥与会话一并释放。
	e.inst.Exec(t, "KILL "+sleepID)
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && e.scalar(t, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER = ?", e.user) != "0" {
		time.Sleep(100 * time.Millisecond)
	}

	if got := e.scalar(t, "SELECT CONCAT(status, '|', direction) FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 1"); got != "running|up" {
		t.Fatalf("进程被杀后运行记录应停在 running|up: %q", got)
	}
	wantVersions(t, e)
	crashFS := fstest.MapFS{}
	for _, n := range []string{"000001_slow_tables.sql"} {
		b, err := os.ReadFile(filepath.Join(crashDir, n))
		if err != nil {
			t.Fatal(err)
		}
		crashFS[n] = &fstest.MapFile{Data: b}
	}
	rc := e.runner(t, crashFS)
	_, err := rc.Up(ctx, migrate.UpOptions{})
	var d *migrate.ErrDirty
	if !errors.As(err, &d) || d.Status != "running" || d.Version != 1 {
		t.Fatalf("崩溃后必须 dirty（running）并拒绝继续: %v", err)
	}
	if !strings.Contains(err.Error(), "中断") {
		t.Errorf("running 状态应表述为「执行被中断」: %v", err)
	}

	// 操作者确认后恢复：recover 清除标记；故障原因（休眠语句）已不存在 → 重跑成功，等价于一次性执行。
	fixed := fstest.MapFS{"000001_slow_tables.sql": {Data: []byte(strings.Replace(string(crashFS["000001_slow_tables.sql"].Data), "DO SLEEP(120);", "DO SLEEP(0);", 1))}}
	rf := e.runner(t, fixed)
	if err := rf.Recover(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := rf.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatalf("恢复后重跑: %v", err)
	}
	wantVersions(t, e, "1")

	one := newEnv(t)
	if _, err := one.runner(t, fixed).Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.schemaSignature(t) != one.schemaSignature(t) {
		t.Fatalf("崩溃后重跑必须与一次性执行等价:\n%s\n---\n%s", e.schemaSignature(t), one.schemaSignature(t))
	}
}

// DML 迁移在事务里执行：中途失败整体回滚，不留下半份种子数据；同样标记 dirty。
func TestDMLMigrationFailureRollsBackCompletely(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	r := e.runner(t, fixtureFS(t, "dml-atomic"))
	_, err := r.Up(ctx, migrate.UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "1048") {
		t.Fatalf("第二条 DML 违反 NOT NULL，应报真实错误 1048: %v", err)
	}
	wantVersions(t, e, "1")
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.seed_items"); got != "0" {
		t.Fatalf("失败的 DML 迁移不得留下半份数据（第一条 INSERT 必须随事务回滚），实际 %s 行", got)
	}
	var d *migrate.ErrDirty
	if _, err := r.Up(ctx, migrate.UpOptions{}); !errors.As(err, &d) || d.Version != 2 {
		t.Fatalf("应 dirty 于 000002: %v", err)
	}
	// DML 迁移回滚是干净的：recover 后重跑仍失败（同一条语句），数据仍为 0 行。
	if err := r.Recover(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{}); err == nil {
		t.Fatal("故障语句未变，重跑必须仍然失败")
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`.seed_items"); got != "0" {
		t.Fatalf("重跑失败后仍不得有数据: %s", got)
	}
}

// 迁移互斥（AC3）：另一个 migrate 在运行时，第二个在超时后拒绝；两个同时启动的 Up 只会应用一次。
func TestMigrationMutex(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	slow := fstest.MapFS{"000001_slow.sql": {Data: []byte("-- lingedge:class=expand\n-- +goose Up\nDO SLEEP(4);\nCREATE TABLE IF NOT EXISTS slow_t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON `slow_t` TO lingedge_app;\n")}}
	a, b := e.runner(t, slow), e.runner(t, slow)
	b.LockTimeout = 1 * time.Second

	var wg sync.WaitGroup
	var aErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, aErr = a.Up(ctx, migrate.UpOptions{})
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && e.scalar(t, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE DB = ? AND INFO LIKE 'DO SLEEP%'", e.dbName) == "0" {
		time.Sleep(50 * time.Millisecond)
	}
	_, err := b.Up(ctx, migrate.UpOptions{})
	var lt *migrate.ErrLockTimeout
	if !errors.As(err, &lt) || migrate.ExitCode(err) != migrate.ExitLocked {
		t.Fatalf("另一个 migrate 在运行时必须因互斥超时而拒绝: %v", err)
	}
	// 只读命令不被互斥阻塞，也不会把「进行中」误判为 dirty：status 可用并标出进行中；plan 明确拒绝（计划此刻不稳定）。
	st, err := b.Status(ctx)
	if err != nil || !st.InProgress || st.Dirty != nil || st.Err() != nil {
		t.Fatalf("迁移进行中 status 应标出 in_progress 且不是 dirty: %+v %v", st, err)
	}
	if _, err := b.Plan(ctx); !errors.As(err, new(*migrate.ErrMigrationRunning)) || migrate.ExitCode(err) != migrate.ExitLocked {
		t.Fatalf("迁移进行中 plan 应拒绝并说明正在运行: %v", err)
	}
	wg.Wait()
	if aErr != nil {
		t.Fatalf("持锁方应成功: %v", aErr)
	}
	wantVersions(t, e, "1")

	// 两个同时启动、互相等待的 Up：恰好一次应用，另一个在拿到锁后发现无事可做。
	e2 := newEnv(t)
	good := fixtureFS(t, "good")
	r1, r2 := e2.runner(t, good), e2.runner(t, good)
	results := make(chan *migrate.UpResult, 2)
	errs := make(chan error, 2)
	for _, r := range []*migrate.Runner{r1, r2} {
		go func(r *migrate.Runner) {
			res, err := r.Up(ctx, migrate.UpOptions{})
			results <- res
			errs <- err
		}(r)
	}
	var applied int
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发 Up: %v", err)
		}
		applied += len((<-results).Applied)
	}
	if applied != 5 {
		t.Fatalf("两个并发 Up 合计应恰好应用 5 个迁移，实际 %d", applied)
	}
	wantVersions(t, e2, "1", "2", "3", "4", "5")
}

// 账号预检（AC6 ⑤）：binlog=ON 且 log_bin_trust_function_creators=0 时，缺少 SET_USER_ID 的迁移账号在迁移开始之前即被拒绝（ERROR 1419），
// 库保持为空；授予后通过。binlog 关闭 / trust=1 的实例上不需要该权限。
func TestPreflightDetectsMissingSetUserID(t *testing.T) {
	ctx := ctxT(t)
	e := newEnvWith(t, false) // 默认实例：log_bin=ON，trust=0
	if got := e.scalar(t, "SELECT CONCAT(@@log_bin, '|', @@log_bin_trust_function_creators)"); got != "1|0" {
		t.Fatalf("测试前提不成立（需 log_bin=1、trust=0）: %s", got)
	}
	r := e.runner(t, fixtureFS(t, "good"))
	_, err := r.Up(ctx, migrate.UpOptions{})
	var pf *migrate.ErrPreflight
	if !errors.As(err, &pf) || migrate.ExitCode(err) != migrate.ExitPreflight {
		t.Fatalf("缺 SET_USER_ID 必须在预检阶段被拒绝: %v", err)
	}
	if n, ok := mysqlErr(pf.Err); !ok || n != 1419 {
		t.Fatalf("预检应由真实的 ERROR 1419 触发: %v", pf.Err)
	}
	for _, want := range []string{"SET_USER_ID", "log_bin_trust_function_creators=1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("预检错误应给出两种处置，缺少 %q: %v", want, err)
		}
	}
	if len(e.tables(t)) != 0 {
		t.Fatalf("预检失败时库必须保持为空（探针表/触发器已清理）: %v", e.tables(t))
	}

	e.inst.Exec(t, "GRANT SET_USER_ID ON *.* TO '"+e.user+"'@'%'")
	r2 := e.runner(t, fixtureFS(t, "good")) // 新连接才会读到新权限
	if _, err := r2.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatalf("授予 SET_USER_ID 后应通过: %v", err)
	}

	// trust=1 的实例：不需要 SET_USER_ID，含触发器的迁移照样成功。
	et := newEnvWith(t, false, mysqltest.Options{TrustFunctionCreators: true})
	if got := et.scalar(t, "SELECT CONCAT(@@log_bin, '|', @@log_bin_trust_function_creators)"); got != "1|1" {
		t.Fatalf("trust 实例形态不对: %s", got)
	}
	if _, err := et.runner(t, fixtureFS(t, "good")).Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatalf("log_bin_trust_function_creators=1 时无需 SET_USER_ID: %v", err)
	}
}

// expand 兼容性：上一版本二进制（只认识旧列）在 expand 之后的新库结构上照常工作；contract（irreversible 删列）则不然，
// 所以 contract 必须晚一个版本发布并标 irreversible（ADR-013 §1）。
func TestExpandIsCompatibleWithPreviousBinaryButContractIsNot(t *testing.T) {
	ctx := ctxT(t)
	// 「旧二进制」的 SQL：只用 v5 结构里的列。
	oldBinary := func(e *env) error {
		for _, q := range []string{
			"INSERT INTO `%s`.widgets (tenant_id, name, legacy, note) VALUES (1, 'compat', 'old', 'n')",
			"SELECT id, tenant_id, name, legacy, note FROM `%s`.widgets WHERE tenant_id = 1",
			"UPDATE `%s`.widgets SET name = 'compat2', legacy = 'x' WHERE tenant_id = 1",
			"DELETE FROM `%s`.widgets WHERE name = 'compat2'",
		} {
			rows, err := e.admin(t).QueryContext(ctx, fmt.Sprintf(q, e.dbName))
			if err != nil {
				return err
			}
			_ = rows.Close()
		}
		return nil
	}

	// expand：新增可空列。
	e := newEnv(t)
	if _, err := e.runner(t, fixtureFS(t, "good")).Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := oldBinary(e); err != nil {
		t.Fatalf("前提：旧二进制在 v5 上应可用: %v", err)
	}
	r := e.runner(t, fixtureFS(t, "good", "extend/expand"))
	if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.createStmt(t, "widgets"), "`flags`") {
		t.Fatal("expand 迁移应已加列")
	}
	if err := oldBinary(e); err != nil {
		t.Fatalf("expand 之后，上一版本二进制必须仍可读写新库结构: %v", err)
	}

	// contract：删除旧列 —— 旧二进制必然出错（这正是它不能与 expand 同版本发布、且必须标 irreversible 的原因）。
	c := newEnv(t)
	if _, err := c.runner(t, fixtureFS(t, "good")).Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.runner(t, fixtureFS(t, "good", "extend/contract")).Up(ctx, migrate.UpOptions{AcceptIrreversible: true}); err != nil {
		t.Fatal(err)
	}
	if err := oldBinary(c); err == nil {
		t.Fatal("contract 删列之后旧二进制应当出错——否则本兼容性测试没有区分力")
	}
}

// 在独立目录构建辅助二进制（`go build`）；返回可执行文件路径。
func buildHelper(t testing.TB, pkg string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
	return out
}

func copyFixtureDir(t testing.TB, layer, dst string) {
	t.Helper()
	for name, f := range fixtureFS(t, layer) {
		if err := os.WriteFile(filepath.Join(dst, name), f.Data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Down 本身失败（真实 MySQL 错误 1091）：标记 failed|down 并 dirty；recover 把迁移恢复为已应用，可重试 down。
func TestFailedDownIsDirtyAndRecoverRestoresApplied(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	r := e.runner(t, fixtureFS(t, "down-fail"))
	if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Down(ctx, migrate.DownOptions{})
	if err == nil || !strings.Contains(err.Error(), "1091") {
		t.Fatalf("Down 应因真实的 ERROR 1091 失败: %v", err)
	}
	wantVersions(t, e, "1") // 回退失败：仍记为已应用
	if got := e.scalar(t, "SELECT CONCAT(status, '|', direction) FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 1"); got != "failed|down" {
		t.Fatalf("运行记录 = %q，期望 failed|down", got)
	}
	var d *migrate.ErrDirty
	if _, err := r.Up(ctx, migrate.UpOptions{}); !errors.As(err, &d) || d.Direction != "down" {
		t.Fatalf("dirty(down) 时 up 必须拒绝: %v", err)
	}
	if !strings.Contains(d.Error(), "migrate down") {
		t.Errorf("down 方向的恢复指引应指向重跑 down: %v", d)
	}
	if err := r.Recover(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := e.scalar(t, "SELECT CONCAT(status, '|', direction) FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 1"); got != "applied|up" {
		t.Fatalf("recover 后应恢复为 applied|up: %q", got)
	}
	if st, err := r.Status(ctx); err != nil || st.Err() != nil || st.Dirty != nil {
		t.Fatalf("recover 后应健康: %+v %v", st, err)
	}
	if _, err := r.Down(ctx, migrate.DownOptions{}); err == nil { // 可重试（故障未排除，仍失败）
		t.Fatal("重试 down 应当再次失败")
	}
}

// recover 的另外两种边界：回退已完成而运行记录没来得及清理；up 已落库而最后的 applied 标记丢失。
func TestRecoverHandlesLostMarkers(t *testing.T) {
	ctx := ctxT(t)
	good := fixtureFS(t, "good")

	// (1) 回退 SQL 与版本表删除都已完成，运行记录停在 running|down：recover 只清理运行记录，之后可重新 up。
	e := newEnv(t)
	r := e.runner(t, good)
	if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatal(err)
	}
	e.inst.Exec(t, "ALTER TABLE `"+e.dbName+"`.widgets DROP COLUMN note")
	e.inst.Exec(t, "DELETE FROM `"+e.dbName+"`."+migrate.VersionTable+" WHERE version_id = 5")
	e.inst.Exec(t, "UPDATE `"+e.dbName+"`."+migrate.RunsTable+" SET direction = 'down', status = 'running' WHERE version = 5")
	if err := r.Recover(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 5"); got != "0" {
		t.Fatalf("回退已完成时 recover 应删除运行记录: %s", got)
	}
	if _, err := r.Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatalf("recover 之后应能重新应用 000005（不得撞上主键冲突）: %v", err)
	}
	wantVersions(t, e, "1", "2", "3", "4", "5")

	// (2) up 的 SQL 已完整执行且版本表已记录，仅 applied 标记丢失（运行记录停在 running）：recover 补记 applied，且保持原校验和。
	e.inst.Exec(t, "UPDATE `"+e.dbName+"`."+migrate.RunsTable+" SET status = 'running' WHERE version = 2")
	sumBefore := e.scalar(t, "SELECT checksum FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 2")
	// 文件在此期间被改动：recover 不得借机把被篡改文件的哈希洗白成已应用的校验和。
	if err := e.runner(t, fixtureFS(t, "good", "extend/tampered")).Recover(ctx, 2); !errors.As(err, new(*migrate.ErrChecksumMismatch)) {
		t.Fatalf("recover 必须核对校验和，拒绝被篡改的文件: %v", err)
	}
	if got := e.scalar(t, "SELECT status FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 2"); got != "running" {
		t.Fatalf("被拒绝的 recover 不得改动运行记录: %s", got)
	}
	if err := r.Recover(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if got := e.scalar(t, "SELECT CONCAT(status, '|', checksum) FROM `"+e.dbName+"`."+migrate.RunsTable+" WHERE version = 2"); got != "applied|"+sumBefore {
		t.Fatalf("recover 应补记 applied 并保持原校验和: %q", got)
	}
	if st, err := r.Status(ctx); err != nil || st.Err() != nil {
		t.Fatalf("recover 后应健康: %+v %v", st, err)
	}
}

// 持锁连接的心跳：一个长迁移期间互斥与 job_locks 租约持续被续约（fence 递增），而不是只在每个迁移之前续约一次。
func TestHeartbeatRenewsLeaseDuringLongMigration(t *testing.T) {
	ctx := ctxT(t)
	e := newEnv(t)
	fsys := fstest.MapFS{
		"000001_job_locks.sql": {Data: []byte("-- lingedge:class=expand\n-- +goose Up\nCREATE TABLE IF NOT EXISTS job_locks (name VARCHAR(128) NOT NULL PRIMARY KEY, holder VARCHAR(128) NOT NULL, fence BIGINT UNSIGNED NOT NULL DEFAULT 0, lease_until DATETIME(3) NOT NULL) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON `job_locks` TO lingedge_app;\n")},
		"000002_slow.sql":      {Data: []byte("-- lingedge:class=expand\n-- +goose Up\nDO SLEEP(4);\n")},
	}
	r := e.runner(t, fsys)
	r.HeartbeatInterval = 300 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := r.Up(ctx, migrate.UpOptions{})
		done <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && e.scalar(t, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE DB = ? AND INFO LIKE 'DO SLEEP%'", e.dbName) == "0" {
		time.Sleep(50 * time.Millisecond)
	}
	fence := func() int {
		var n int
		if _, err := fmt.Sscanf(e.scalar(t, "SELECT fence FROM `"+e.dbName+"`.job_locks WHERE name = 'migrate'"), "%d", &n); err != nil {
			t.Fatalf("读取 fence: %v", err)
		}
		return n
	}
	first := fence()
	time.Sleep(1500 * time.Millisecond)
	second := fence()
	if first == 0 || second <= first {
		t.Fatalf("长迁移期间租约应被心跳续约（fence 递增）：%d → %d", first, second)
	}
	if got := e.scalar(t, "SELECT IF(lease_until > UTC_TIMESTAMP(3), 'held', 'expired') FROM `"+e.dbName+"`.job_locks WHERE name = 'migrate'"); got != "held" {
		t.Fatalf("迁移进行中租约应有效: %s", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := e.scalar(t, "SELECT IF(lease_until <= UTC_TIMESTAMP(3), 'released', 'held') FROM `"+e.dbName+"`.job_locks WHERE name = 'migrate'"); got != "released" {
		t.Fatalf("迁移结束后租约应立即过期: %s", got)
	}
}

// 迁移账号必须有 GRANT OPTION（ADR-016 §6）：缺少时在迁移开始之前拒绝，而不是执行到第一条 GRANT 才失败并留下 dirty。
func TestPreflightRequiresGrantOption(t *testing.T) {
	ctx := ctxT(t)
	e := newEnvWith(t, true)
	e.inst.Exec(t, "REVOKE GRANT OPTION ON `"+e.dbName+"`.* FROM '"+e.user+"'@'%'")
	r := e.runner(t, fixtureFS(t, "good"))
	_, err := r.Up(ctx, migrate.UpOptions{})
	var pf *migrate.ErrPreflight
	if !errors.As(err, &pf) || migrate.ExitCode(err) != migrate.ExitPreflight || !strings.Contains(err.Error(), "GRANT OPTION") {
		t.Fatalf("缺 GRANT OPTION 必须在预检阶段被拒绝: %v", err)
	}
	if len(e.tables(t)) != 0 {
		t.Fatalf("预检失败时库必须保持为空（探针表已清理）: %v", e.tables(t))
	}
	if got := e.scalar(t, "SELECT COUNT(*) FROM mysql.tables_priv WHERE Db = ? AND Table_name LIKE '\\_lingedge\\_probe\\_%'", e.dbName); got != "0" {
		t.Fatalf("探针授权应已清理: %s", got)
	}
	e.inst.Exec(t, "GRANT ALL ON `"+e.dbName+"`.* TO '"+e.user+"'@'%' WITH GRANT OPTION")
	if _, err := e.runner(t, fixtureFS(t, "good")).Up(ctx, migrate.UpOptions{}); err != nil {
		t.Fatalf("恢复 GRANT OPTION 后应通过: %v", err)
	}
}
