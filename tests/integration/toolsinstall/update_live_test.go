package toolsinstall

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// Story 1.43 联调：真实 lingedge update / rollback / recover（cli.Execute → installer → backup/dbmigrate/rollback）+ 真实 lingedge-server 进程（旧版本与带真实 SQL 迁移的新版本）+
// 本机临时 MySQL 8.0 / Redis + 真实向导产生的数据。伪的只有「操作系统」：伪 systemctl（start/stop 时由 sysd 真实地拉起/终止进程）、useradd、chown。

func (s *stack) dbEnv() map[string]string {
	return map[string]string{backup.EnvHost: s.be.inst.Host(), backup.EnvPort: strconv.Itoa(s.be.inst.Port), backup.EnvName: s.be.dbName}
}

func (s *stack) opt(wrap func(service.Runner) service.Runner) runOpt {
	return runOpt{wrapRunnerFn: wrap, env: s.dbEnv()}
}

// upgradeOK 运行一次升级并要求退出码 0，返回其 data 的 components[0]。
func (s *stack) upgradeOK(opt runOpt, bundle, timeout string, extra ...string) map[string]any {
	s.t.Helper()
	code, so, se := s.runB(0, opt, s.updArgs(bundle, timeout, extra...)...)
	if code != 0 {
		s.t.Fatalf("update 退出码 %d\nstdout=%s\nstderr=%s\nserver 日志:\n%s", code, so, se, s.spec.logs.String())
	}
	d := dataOf(envelopeOf(s.t, so))
	return d["components"].([]any)[0].(map[string]any)
}

func (s *stack) backupVerifyOK(path string) {
	s.t.Helper()
	code, so, se := s.runB(0, runOpt{}, "backup", "verify", path, "--json", "--non-interactive")
	if code != 0 {
		s.t.Fatalf("backup verify %s: %d\n%s\n%s", path, code, so, se)
	}
}

func (s *stack) backupCount() int { return len(s.backupFiles()) }

// scenario 是一类迁移（决策表一行）。
type scenario struct {
	name    string
	bin     func(testing.TB) string
	classes []string
	path    string // 升级后记录的回滚路径
	col     string // 迁移新增的列（空 = 无）
	schema  int64  // 升级后的 schema 版本
}

var scenarios = []scenario{
	{"none", func(t testing.TB) string { return serverBin(t) }, nil, "binary", "", 18},
	{"reversible", serverWithRev, []string{"reversible"}, "migrate_down", "t43_rev", 19},
	{"expand", serverWithExp, []string{"expand"}, "binary_expand", "t43_exp", 19},
	{"irreversible", serverWithIrr, []string{"irreversible"}, "restore_backup", "", 19},
}

// 迁移决策表（AC3）：四类 × 升级成功。每类的备份、确认、回滚路径与对用户的承诺；升级后真实 schema、真实进程健康。
func TestUpdateServerDecisionTableSuccess(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			s := startStack(t)
			opt := s.opt(s.useSysd())
			lineageBefore := s.lineage()
			b := s.serverBundle("0.2.0", sc.bin(t), 11, noticeOf(sc.classes...))
			var extra []string
			if sc.name == "irreversible" {
				extra = []string{"--accept-irreversible"}
			}
			it := s.upgradeOK(opt, b, "60s", extra...)
			if it["outcome"] != "updated" || it["path"] != sc.path || it["backup_verified"] != true || it["backup"] == "" {
				t.Fatalf("%v", it)
			}
			if sc.name == "irreversible" && (it["has_irreversible"] != true || !strings.Contains(it["promise"].(string), "备份点之后产生的数据将丢失")) {
				t.Fatalf("不可逆的承诺应明示数据丢失: %v", it)
			}
			if s.currentTarget() != "releases/0.2.0" || s.schemaVersion() != sc.schema || (sc.col != "" && !s.hasColumn(sc.col)) {
				t.Fatalf("升级后 current=%s schema=%d", s.currentTarget(), s.schemaVersion())
			}
			s.waitReady()
			v, prev, path, bk, irr := s.stateComp()
			if v != "0.2.0" || prev != "0.1.0" || path != sc.path || bk == "" || irr != (sc.name == "irreversible") {
				t.Fatalf("state: %s %s %s %s %v", v, prev, path, bk, irr)
			}
			s.backupVerifyOK(bk)
			if journalsLeft(s) != 0 {
				t.Fatal("不应有活动 journal")
			}
			s.assertNoCredentialResidue()
			// 升级不触碰 lineage（只有恢复备份路径才 bump）。
			if got := s.lineage(); got != lineageBefore {
				t.Errorf("升级成功不应改变 deploy.lineage: %d → %d", lineageBefore, got)
			}
		})
	}
}

// 含不可逆迁移（AC4）：未确认 → 退出码 9 且尚未改动任何东西；--accept-irreversible 才执行，升级前强制备份并验证可读；
// 清单预告低估时以 migrate plan 的实际结果为准（取更严格者）；简单回退被拒绝并提示 --from-backup；--from-backup 恢复后 lineage 严格增大（IMP-07）。
func TestUpdateIrreversibleConfirmationAndRecoveryPath(t *testing.T) {
	s := startStack(t)
	opt := s.opt(s.useSysd())
	bin := serverWithIrr(t)

	// (1) 清单预告含 irreversible，非交互未给 --accept-irreversible：9，且什么都没动（连备份与 staging 都没有）。
	declared := s.serverBundle("0.2.0", bin, 11, noticeOf("irreversible"))
	before := tree(t, s.w)
	code, so, se := s.runB(0, opt, s.updArgs(declared, "30s")...)
	env := envelopeOf(t, so)
	if code != 9 || errCodeOf(env) != "confirm.irreversible" || env["exit_name"] != "CONFIRM_REQUIRED" {
		t.Fatalf("应为 9/confirm.irreversible: %d\n%s\n%s", code, so, se)
	}
	if d := diffTrees(before, tree(t, s.w)); d != "" {
		t.Fatalf("退出码 9 时尚未改动任何东西:\n%s", d)
	}
	if s.backupCount() != 0 || s.schemaVersion() != 18 || s.currentTarget() != "releases/0.1.0" || !s.serverRunning() {
		t.Fatalf("状态被改动: backups=%d schema=%d", s.backupCount(), s.schemaVersion())
	}
	// (2) 清单预告低估（只写 reversible）但二进制里真有不可逆迁移：migrate plan（步骤 7）是权威，同样 9；此时只留下备份（证据），系统其余不变。
	understated := s.serverBundle("0.2.0", bin, 12, noticeOf("reversible"))
	code, so, se = s.runB(0, opt, s.updArgs(understated, "30s")...)
	if code != 9 || errCodeOf(envelopeOf(t, so)) != "confirm.irreversible" {
		t.Fatalf("清单低估时应以 migrate plan 为准（9）: %d\n%s\n%s", code, so, se)
	}
	if s.schemaVersion() != 18 || s.currentTarget() != "releases/0.1.0" || !s.serverRunning() || s.hasColumn("t43_irr") {
		t.Fatal("被拒绝的升级不得改动数据库/current/进程")
	}
	if journalsLeft(s) != 0 || s.w.Has("opt/lingedge/server/releases/0.2.0") {
		t.Fatal("被拒绝的升级不得留下 journal 与新 release")
	}
	// (3) 确认后执行。
	lineageBefore := s.lineage()
	it := s.upgradeOK(opt, declared, "60s", "--accept-irreversible")
	bk, _ := it["backup"].(string)
	if it["path"] != "restore_backup" || it["backup_verified"] != true || bk == "" {
		t.Fatalf("%v", it)
	}
	s.backupVerifyOK(bk)
	s.waitReady()
	// 新版本在备份点之后建出的表（备份里没有）：恢复必须清空整库再灌入（Wipe），否则这些表会残留。
	if _, err := s.be.adm.Exec("CREATE TABLE `" + s.be.dbName + "`.t43_after_backup (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	// (4) 简单回退被拒绝（3），提示 --from-backup 与备份路径；系统不变。
	code, so, _ = s.runB(0, opt, s.rbArgs("--yes")...)
	env = envelopeOf(t, so)
	if code != 3 || errCodeOf(env) != "precondition.irreversible_rollback" {
		t.Fatalf("应拒绝简单回退: %d\n%s", code, so)
	}
	hint := env["errors"].([]any)[0].(map[string]any)["hint"].(string)
	if !strings.Contains(hint, "--from-backup") || !strings.Contains(hint, bk) {
		t.Errorf("提示应含 --from-backup 与备份文件: %s", hint)
	}
	if s.currentTarget() != "releases/0.2.0" {
		t.Fatal("被拒绝的回滚不得改动 current")
	}
	// (5) --yes 不足以确认覆盖数据库：须主机名（9）。
	code, so, _ = s.runB(0, opt, s.rbArgs("--from-backup", bk, "--yes")...)
	if code != 9 {
		t.Fatalf("from-backup 缺主机名确认应 9: %d\n%s", code, so)
	}
	// (6) 恢复升级前备份 + 旧版本二进制：数据库回到升级前、lineage 严格增大（节点仍可接收新修订）、服务健康。
	code, so, se = s.runB(0, opt, s.rbArgs("--from-backup", bk, "--yes", "--confirm-name", migHost)...)
	if code != 0 {
		t.Fatalf("rollback --from-backup: %d\n%s\n%s\n%s", code, so, se, s.spec.logs.String())
	}
	s.waitReady()
	if s.currentTarget() != "releases/0.1.0" || s.schemaVersion() != 18 {
		t.Fatalf("恢复后 current=%s schema=%d", s.currentTarget(), s.schemaVersion())
	}
	var extra int
	if err := s.be.adm.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = 't43_after_backup'", s.be.dbName).Scan(&extra); err != nil || extra != 0 {
		t.Fatalf("恢复备份必须清空整库：备份点之后建的表不应残留（%d, %v）", extra, err)
	}
	if after := s.lineage(); after <= lineageBefore {
		t.Fatalf("恢复备份后 deploy.lineage 必须严格增大（IMP-07）：%d → %d", lineageBefore, after)
	}
	rep := dataOf(envelopeOf(t, so))["report"].(map[string]any)
	if rep["version"] != "0.1.0" || rep["health"] != "healthy" || rep["schema_version"] != float64(18) {
		t.Fatalf("%v", rep)
	}
	if v, prev, _, _, _ := s.stateComp(); v != "0.1.0" || prev != "" {
		t.Fatalf("state: %s %s", v, prev)
	}
	// 回滚前安全备份保留（purpose=pre-rollback）。
	if !strings.Contains(strings.Join(s.backupFiles(), " "), "server") || s.backupCount() < 2 {
		t.Errorf("应有升级前备份与回滚前安全备份: %v", s.backupFiles())
	}
}

// 失败的升级自动回滚（AC5）：新版本健康检查失败（真实进程因读不到配置而启动失败）→ 逆序撤销 → 退出码 10；回滚后版本号、配置、schema 与升级前一致，自动复核健康与 doctor 子集。
// 四类迁移各走各的回滚路径（down / 仅切回二进制 / 恢复备份）。
func TestUpdateHealthFailureAutoRollbackPerClass(t *testing.T) {
	for _, sc := range scenarios[1:] { // none 的路径在 TestUpdateNoMigrationHealthFailure
		t.Run(sc.name, func(t *testing.T) {
			s := startStack(t)
			opt := s.opt(s.useSysd())
			s.spec.failVersions = map[string]bool{"0.2.0": true}
			files := s.filesState()
			lineageBefore := s.lineage()
			b := s.serverBundle("0.2.0", sc.bin(t), 11, noticeOf(sc.classes...))
			code, so, se := s.runB(0, opt, s.updArgs(b, "3s", "--accept-irreversible")...)
			env := envelopeOf(t, so)
			if code != 10 || env["exit_name"] != "ROLLED_BACK" {
				t.Fatalf("健康失败应退出码 10: %d\n%s\n%s\n%s", code, so, se, s.spec.logs.String())
			}
			e := env["errors"].([]any)[0].(map[string]any)
			rb := e["details"].(map[string]any)["rollback"].(map[string]any)
			wantHealth := "healthy"
			if sc.name == "expand" {
				// 旧二进制对「库里有它不认识的 expand 迁移」报告 /healthz/ready 不就绪（tools-design §17.7）：回滚后只要求存活探针，并如实标明。
				wantHealth = "healthy_live_only"
			}
			if e["system_state"] != "rolled_back" || rb["version"] != "0.1.0" || rb["health"] != wantHealth || rb["path"] != sc.path {
				t.Fatalf("回滚复核报告: %v", e)
			}
			s.spec.failVersions = nil
			if s.currentTarget() != "releases/0.1.0" {
				t.Fatalf("current=%s", s.currentTarget())
			}
			wantSchema := int64(18)
			if sc.name == "expand" {
				wantSchema = 19 // expand：只切回旧二进制，不执行 down（旧二进制与新增结构兼容，§17.2）
			}
			if got := s.schemaVersion(); got != wantSchema {
				t.Fatalf("回滚后 schema=%d，期望 %d", got, wantSchema)
			}
			if sc.name == "reversible" && s.hasColumn("t43_rev") {
				t.Fatal("reversible：down 之后新列应已移除")
			}
			if sc.name == "expand" {
				// 旧二进制 + 已含 expand 迁移的库：进程存活，但 /healthz/ready 如实报告「库里有二进制不认识的迁移」→ 503（tools-design §17.7 的已知缺口）。
				if err := httpGetOK(s.baseURL + "/healthz"); err != nil {
					t.Fatalf("旧版本服务应已恢复存活: %v", err)
				}
				if httpGetOK(s.baseURL+"/healthz/ready") == nil {
					t.Error("若旧二进制对 expand 迁移后的库报告就绪，说明 §17.7 的缺口已被 server 侧修复：请把回滚后的健康检查恢复为 ready")
				}
			} else {
				s.waitReady() // 旧版本服务已恢复
			}
			if v, prev, _, _, _ := s.stateComp(); v != "0.1.0" || prev != "" {
				t.Fatalf("state 应不变: %s %s", v, prev)
			}
			if journalsLeft(s) != 0 || s.w.Has("opt/lingedge/server/releases/0.2.0") {
				t.Fatal("回滚后不应留下 journal 与新 release")
			}
			if got := s.filesState(); sameState(files, got) != "" {
				t.Fatalf("回滚后配置/服务数据与升级前不一致:\n%s", sameState(files, got))
			}
			if sc.name == "irreversible" {
				if s.lineage() <= lineageBefore {
					t.Fatalf("恢复备份的回滚路径必须在启动旧版本前 lineage bump：%d → %d", lineageBefore, s.lineage())
				}
			} else if s.lineage() != lineageBefore {
				t.Errorf("非恢复备份的路径不应改变 lineage")
			}
			if s.backupCount() != 1 {
				t.Errorf("升级前备份应保留: %v", s.backupFiles())
			}
			s.assertNoCredentialResidue()
		})
	}
}

// 迁移中途失败（第 2 条迁移的语句出错，库 dirty）：down 被拒绝 → 升级为「恢复备份」路径（AC3）；回滚后库不再 dirty、schema 回到升级前。
func TestUpdateMigrationFailureEscalatesToRestoreBackup(t *testing.T) {
	s := startStack(t)
	opt := s.opt(s.useSysd())
	lineageBefore := s.lineage()
	b := s.serverBundle("0.2.0", serverWithBad(t), 11, noticeOf("reversible"))
	code, so, se := s.runB(0, opt, s.updArgs(b, "10s")...)
	env := envelopeOf(t, so)
	if code != 10 {
		t.Fatalf("迁移失败应自动回滚（10）: %d\n%s\n%s", code, so, se)
	}
	rb := env["errors"].([]any)[0].(map[string]any)["details"].(map[string]any)["rollback"].(map[string]any)
	detail := strings.Join(anyStrings(rb["detail"]), "；")
	if !strings.Contains(detail, "恢复升级前备份") {
		t.Fatalf("down 失败（dirty）应升级为恢复备份: %s", detail)
	}
	s.waitReady()
	if s.schemaVersion() != 18 || s.hasColumn("t43_rev") || s.hasColumn("t43_bad") || s.currentTarget() != "releases/0.1.0" {
		t.Fatalf("回滚后 schema=%d current=%s", s.schemaVersion(), s.currentTarget())
	}
	var dirty int
	_ = s.be.adm.QueryRow("SELECT COUNT(*) FROM `" + s.be.dbName + "`.schema_migrations WHERE is_applied = 0 AND version_id > 0").Scan(&dirty)
	if dirty != 0 {
		t.Errorf("恢复后不应残留 dirty 记录: %d", dirty)
	}
	if s.lineage() <= lineageBefore {
		t.Errorf("恢复备份路径必须 lineage bump")
	}
	if journalsLeft(s) != 0 {
		t.Fatal("回滚成功后不应有活动 journal")
	}
}

// 无迁移的升级（binary 路径）：健康失败 → 切回旧 release，数据库完全不动；成功后保留策略清理旧 release。
func TestUpdateNoMigrationHealthFailureAndRetention(t *testing.T) {
	s := startStack(t)
	opt := s.opt(s.useSysd())
	s.spec.failVersions = map[string]bool{"0.2.0": true}
	b2 := s.serverBundle("0.2.0", serverBin(t), 11, nil)
	if code, so, se := s.runB(0, opt, s.updArgs(b2, "3s")...); code != 10 {
		t.Fatalf("%d\n%s\n%s", code, so, se)
	}
	s.spec.failVersions = nil
	s.waitReady()
	if s.currentTarget() != "releases/0.1.0" || s.schemaVersion() != 18 {
		t.Fatal("回滚后应在旧版本")
	}
	s.upgradeOK(opt, s.serverBundle("0.2.0", serverBin(t), 12, nil), "60s")
	s.upgradeOK(opt, s.serverBundle("0.3.0", serverBin(t), 13, nil), "60s")
	rel := func(v string) bool { return s.w.Has("opt/lingedge/server/releases/" + v) }
	if rel("0.1.0") || !rel("0.2.0") || !rel("0.3.0") {
		t.Fatalf("保留 2 个 release（成功确认之后才清理）：0.1.0=%v 0.2.0=%v 0.3.0=%v", rel("0.1.0"), rel("0.2.0"), rel("0.3.0"))
	}
	s.waitReady()
}

func anyStrings(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			out = append(out, x.(string))
		}
	}
	return out
}

var _ = os.Remove
var _ = filepath.Join
