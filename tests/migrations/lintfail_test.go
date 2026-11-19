package migrationstest

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/wuekevin/lingedge/internal/platform/migrate"
)

// 迁移 lint 的负向用例（AC2、AC4、AC6）：tests/fixtures/migrations/lint-fail/<用例>/ 下每个目录是一组迁移文件，
// EXPECT 列出该用例应当触发的规则——实际触发的规则集合必须与之完全相等（既不能漏报，也不能因为夹具写得不干净而夹带其他规则）。

func lintFailCases(t testing.TB) map[string]fstest.MapFS {
	t.Helper()
	root := filepath.Join(fixturesDir, "lint-fail")
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]fstest.MapFS{}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		m := fstest.MapFS{}
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(root, e.Name(), f.Name()))
			if err != nil {
				t.Fatal(err)
			}
			m[f.Name()] = &fstest.MapFile{Data: b}
		}
		out[e.Name()] = m
	}
	return out
}

func TestLintNegativeFixturesTriggerExactlyTheirRules(t *testing.T) {
	cases := lintFailCases(t)
	if len(cases) < 56 {
		t.Fatalf("lint 负向夹具应不少于 56 个，实际 %d", len(cases))
	}
	covered := map[string]bool{}
	for name, fsys := range cases {
		expectRaw, ok := fsys["EXPECT"]
		if !ok {
			t.Errorf("%s: 缺少 EXPECT 文件", name)
			continue
		}
		want := strings.Fields(string(expectRaw.Data))
		sort.Strings(want)
		delete(fsys, "EXPECT")
		set, findings, err := migrate.Load(fsys)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		_ = set
		gotSet := map[string]bool{}
		for _, f := range findings {
			gotSet[f.Rule] = true
		}
		var got []string
		for r := range gotSet {
			got = append(got, r)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: 触发的规则 = %v，期望 %v\n%v", name, got, want, findings)
		}
		for _, w := range want {
			covered[w] = true
		}
	}
	for _, rule := range []string{"header", "name", "sequence", "goose-annotation", "syntax", "class-expand", "class-reversible", "class-irreversible",
		"idempotency", "ddl-dml-split", "compound-statement", "grant-app", "grant-maint", "forbidden-statement", "executable-comment",
		migrate.RuleGeneratedFK, migrate.RuleCheckFK, migrate.RuleForwardFK, migrate.RulePartitionFK, migrate.RulePartitionPmax, migrate.RuleFKChecks,
		migrate.RuleSchemaEnum, migrate.RuleSchemaLOBDefault, migrate.RuleSchemaTime, migrate.RuleSchemaFloat, migrate.RuleSchemaTenantColumn, migrate.RuleSchemaTenantIndex} {
		if !covered[rule] {
			t.Errorf("规则 %s 没有任何负向夹具覆盖", rule)
		}
	}
}

// 合法夹具必须零发现（否则负向用例的「恰好」断言没有意义）。
func TestGoodFixturesAreLintClean(t *testing.T) {
	for _, layers := range [][]string{
		{"good"}, {"good", "extend/expand"}, {"good", "extend/contract"}, {"good", "extend/tampered"}, {"fault"}, {"crash"}, {"dml-atomic"},
	} {
		_, findings, err := migrate.Load(fixtureFS(t, layers...))
		if err != nil || len(findings) != 0 {
			t.Errorf("%v: err=%v findings=%v", layers, err, findings)
		}
	}
}
