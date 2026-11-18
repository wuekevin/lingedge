package migrationstest

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/tests/mysqltest"
)

// ddl-rules.sql（docs/database/verification 的逐字节副本，用例与期望错误码不变）：
// 逐例新建空库执行，取第一个错误号与期望比对（与 run_ddl_rules.py 同一判定口径：mysql -f 继续执行、取第一个错误）。

type ruleCase struct {
	id, expect string // expect: "OK" 或 "ERR <code>"
	body       string
}

var (
	caseRe    = regexp.MustCompile(`(?m)^-- CASE (\S+) \| EXPECT (OK|ERR \d+)[ \t]*$`)
	preludeRe = regexp.MustCompile(`(?s)-- PRELUDE\n(.*?)-- END PRELUDE`)
)

func loadRuleCases(t testing.TB) (prelude string, cases []ruleCase) {
	t.Helper()
	b, err := os.ReadFile("ddl-rules.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if m := preludeRe.FindStringSubmatch(text); m != nil {
		prelude = m[1]
	}
	idx := caseRe.FindAllStringSubmatchIndex(text, -1)
	for i, m := range idx {
		end := len(text)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		cases = append(cases, ruleCase{id: text[m[2]:m[3]], expect: text[m[4]:m[5]], body: text[m[1]:end]})
	}
	return prelude, cases
}

func expectCode(c ruleCase) int {
	if c.expect == "OK" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(c.expect, "ERR "))
	return n
}

// 静态预检覆盖的 MySQL 错误号 → 规则。
var staticRuleOf = map[int]string{
	1215: migrate.RuleGeneratedFK, 3823: migrate.RuleCheckFK, 1824: migrate.RuleForwardFK,
	1506: migrate.RulePartitionFK, 1526: migrate.RulePartitionPmax,
}

// staticVerdict 用静态 DDL 预检检查整个用例（序言 + 用例体），返回触发的规则集合。
func staticVerdict(prelude string, c ruleCase) map[string]bool {
	ck := migrate.NewChecker()
	got := map[string]bool{}
	for _, s := range append(splitSQL(prelude), splitSQL(c.body)...) {
		for _, v := range ck.Check(s) {
			got[v.Rule] = true
		}
	}
	return got
}

// 静态预检与 MySQL 8.0.46 的实测逐例一致：期望错误号属于静态规则集合的，静态必须恰好报告该规则；其余用例（OK 或其他错误号）静态必须无报告。
// 本测试不连库，随 `make test` 常驻；真实库侧见 TestDDLRulesOnRealMySQL。
func TestStaticDDLCheckAgreesWithExpectedMySQLResults(t *testing.T) {
	prelude, cases := loadRuleCases(t)
	if len(cases) != 26 {
		t.Fatalf("ddl-rules.sql 应有 26 个用例，实际 %d（用例不得被改动）", len(cases))
	}
	covered := map[string]bool{}
	for _, c := range cases {
		got := staticVerdict(prelude, c)
		want := map[string]bool{}
		if rule, ok := staticRuleOf[expectCode(c)]; ok {
			want[rule] = true
			covered[rule] = true
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("用例 %s（期望 %s）：静态判定 %v，期望 %v", c.id, c.expect, got, want)
		}
	}
	for code, rule := range staticRuleOf {
		if !covered[rule] {
			t.Errorf("规则 %s（ERROR %d）没有任何 ddl-rules.sql 用例覆盖", rule, code)
		}
	}
}

func execAll(ctx context.Context, conn *sql.Conn, stmts []string) (firstErr uint16) {
	for _, s := range stmts {
		rows, err := conn.QueryContext(ctx, s)
		if err != nil {
			if me, ok := err.(*mysql.MySQLError); ok && firstErr == 0 {
				firstErr = me.Number
			} else if !ok && firstErr == 0 {
				firstErr = 65535
			}
			continue
		}
		_ = rows.Close()
	}
	return firstErr
}

// 26 个断言用例在真实 MySQL 上逐例执行（database/README §8.2 第 3 点），并与静态预检的判定逐例核对。
func TestDDLRulesOnRealMySQL(t *testing.T) {
	inst := mysqltest.Start(t)
	prelude, cases := loadRuleCases(t)
	ctx := ctxT(t)
	admin := inst.Admin(t)
	conn, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	dbName := mysqltest.RandomName("le_ddl_rules")
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`") })

	var failed int
	for _, c := range cases {
		for _, s := range []string{
			"DROP DATABASE IF EXISTS `" + dbName + "`",
			"CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci",
			"USE `" + dbName + "`",
		} {
			if _, err := conn.ExecContext(ctx, s); err != nil {
				t.Fatalf("准备用例库: %v", err)
			}
		}
		got := int(execAll(ctx, conn, append(splitSQL(prelude), splitSQL(c.body)...)))
		want := expectCode(c)
		status := "PASS"
		if got != want {
			status = "FAIL"
			failed++
			t.Errorf("用例 %s：期望 %s，实际错误号 %d", c.id, c.expect, got)
		}
		static := staticVerdict(prelude, c)
		wantStatic := map[string]bool{}
		if rule, ok := staticRuleOf[got]; ok {
			wantStatic[rule] = true
		}
		if fmt.Sprint(static) != fmt.Sprint(wantStatic) {
			t.Errorf("用例 %s：真实 MySQL 错误号 %d，但静态预检判定为 %v（应为 %v）", c.id, got, static, wantStatic)
		}
		t.Logf("%-34s 期望 %-9s 实际 %-5d %s", c.id, c.expect, got, status)
	}
	t.Logf("合计 %d 个用例，失败 %d", len(cases), failed)
	if len(cases) != 26 {
		t.Fatalf("用例数 = %d，期望 26", len(cases))
	}
}

// 原样运行迁入的 run_ddl_rules.py（需要 python3 与 mysql 客户端；缺失时跳过），证明脚本在新位置仍然可用且 26/26 通过。
func TestPythonDDLRulesRunnerStillPasses(t *testing.T) {
	inst := mysqltest.Start(t)
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(mysqltest.SkipPrefix + "未找到 python3；run_ddl_rules.py 未执行，不代表通过（Go 版逐例断言 TestDDLRulesOnRealMySQL 已覆盖同一批用例）")
	}
	client := ""
	for _, c := range []string{"/opt/homebrew/opt/mysql@8.0/bin/mysql", "/usr/local/opt/mysql@8.0/bin/mysql", "/usr/bin/mysql"} {
		if _, err := os.Stat(c); err == nil {
			client = c
			break
		}
	}
	if client == "" {
		if p, err := exec.LookPath("mysql"); err == nil {
			client = p
		}
	}
	if client == "" {
		t.Skip(mysqltest.SkipPrefix + "未找到 mysql 客户端；run_ddl_rules.py 未执行，不代表通过")
	}
	cmd := exec.Command(py, "-I", "run_ddl_rules.py", "--host", inst.Host(), "--port", strconv.Itoa(inst.Port), "--user", "root", "--mysql", client)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("run_ddl_rules.py 失败: %v", err)
	}
	if !strings.Contains(string(out), "合计 26 个用例，失败 0") {
		t.Fatalf("期望 26/26 通过:\n%s", out)
	}
}

// 回归用的三份资产是 docs/database/verification 的逐字节副本：原件被 CF-1 冻结复核（freeze-check 读取 m1-fk-skeleton.sql）引用，
// 不能搬走；这里守住两份内容一致，任何一边单独修改都会失败（改动须走 docs/bmad/contract-changes 流程并同步两处）。
func TestRegressionAssetsAreIdenticalToFrozenDocs(t *testing.T) {
	for _, f := range []string{"ddl-rules.sql", "m1-fk-skeleton.sql", "run_ddl_rules.py"} {
		a, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile("../../docs/database/verification/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s 与 docs/database/verification/%s 不一致", f, f)
		}
	}
}
