package migrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestSplitStatementsIgnoresSemicolonsInStringsAndComments(t *testing.T) {
	src := "-- 注释; 里的分号\n" +
		"INSERT IGNORE INTO t (a) VALUES ('x;y'), (\"p;q\"), ('it''s');\n" +
		"/* 块注释; */ CREATE TABLE IF NOT EXISTS `a;b` (id INT);\n" +
		"SELECT 1 # 行尾注释;\n;\n"
	stmts, rest := splitStatements(src, 1)
	if rest != "" {
		t.Fatalf("不应有未结束语句: %q", rest)
	}
	if len(stmts) != 3 {
		t.Fatalf("语句数 = %d，期望 3: %+v", len(stmts), stmts)
	}
	if stmts[0].Line != 2 || !strings.Contains(stmts[0].Lead, "注释") {
		t.Errorf("首条语句应保留前导注释与行号: %+v", stmts[0])
	}
	if stmts[1].Line != 3 {
		t.Errorf("第二条语句行号 = %d，期望 3", stmts[1].Line)
	}
}

func TestSplitStatementsReportsUnfinished(t *testing.T) {
	_, rest := splitStatements("CREATE TABLE IF NOT EXISTS t (id INT)", 1)
	if !strings.Contains(rest, "CREATE TABLE") {
		t.Fatalf("缺少分号的末尾语句必须被报告: %q", rest)
	}
	if _, rest := splitStatements("SELECT 1;\n-- 仅注释\n", 1); rest != "" {
		t.Fatalf("仅注释不是未结束语句: %q", rest)
	}
}

func TestTokenizeHandlesQuotesAndComments(t *testing.T) {
	toks := tokenize("ALTER TABLE `my``tbl` ADD COLUMN c VARCHAR(8) DEFAULT 'a''b' -- DROP\n/* MODIFY */")
	var words []string
	for _, k := range toks {
		words = append(words, k.text)
	}
	got := strings.Join(words, "|")
	want := "ALTER|TABLE|my`tbl|ADD|COLUMN|c|VARCHAR|(|8|)|DEFAULT|a'b"
	if got != want {
		t.Fatalf("tokenize = %q，期望 %q", got, want)
	}
}

const goodExpand = "-- lingedge:class=expand\n-- lingedge:summary=创建 t 表\n-- +goose Up\n" +
	"CREATE TABLE IF NOT EXISTS t (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY) ENGINE=InnoDB;\n" +
	"GRANT SELECT, INSERT, UPDATE, DELETE ON `t` TO lingedge_app;\n"

func TestParseFileGooseSections(t *testing.T) {
	m, fs := ParseFile("000007_things.sql", []byte("-- lingedge:class=reversible\n-- lingedge:summary=加列\n-- lingedge:contract-after=1.4.0\n-- +goose Up\n"+
		"ALTER TABLE t ADD COLUMN c INT NULL;\n"+
		"-- +goose StatementBegin\nCREATE TRIGGER trg BEFORE UPDATE ON t FOR EACH ROW BEGIN SET NEW.c = 1; END;\n-- +goose StatementEnd\n"+
		"-- +goose Down\nALTER TABLE t DROP COLUMN c;\n"))
	if len(fs) != 0 {
		t.Fatalf("不应有发现: %v", fs)
	}
	if m.Version != 7 || m.Name != "things" || m.Class != ClassReversible || m.Summary != "加列" || m.ContractAfter != "1.4.0" {
		t.Errorf("头部解析错误: %+v", m)
	}
	if len(m.Up) != 2 || !m.Up[1].Block || !strings.Contains(m.Up[1].Text, "BEGIN SET NEW.c = 1; END") {
		t.Errorf("StatementBegin/End 应整体成一条语句: %+v", m.Up)
	}
	if len(m.Down) != 1 || !m.HasDown || m.DownMarker {
		t.Errorf("Down 解析错误: %+v", m)
	}
	if len(m.Checksum) != 64 {
		t.Errorf("校验和应为 SHA-256 十六进制: %q", m.Checksum)
	}
}

func TestParseFileIrreversibleMarker(t *testing.T) {
	m, fs := ParseFile("000002_drop_old.sql", []byte("-- lingedge:class=irreversible\n-- +goose Up\nALTER TABLE t DROP COLUMN old;\n-- +goose Down\n-- lingedge:reason=列数据已被删除，无法还原\nSELECT 'irreversible';\n"))
	if len(fs) != 0 {
		t.Fatalf("不应有发现: %v", fs)
	}
	if m.HasDown || !m.DownMarker || !strings.Contains(m.Reason, "无法还原") {
		t.Errorf("irreversible 标记解析错误: HasDown=%v marker=%v reason=%q", m.HasDown, m.DownMarker, m.Reason)
	}
}

func TestParseFileFindings(t *testing.T) {
	cases := []struct {
		name, file, body, rule string
	}{
		{"缺少 class", "000001_a.sql", "-- +goose Up\nSELECT 1;\n", "header"},
		{"class 不在第一行", "000001_a.sql", "-- 说明\n-- lingedge:class=expand\n-- +goose Up\nSELECT 1;\n", "header"},
		{"未知 class", "000001_a.sql", "-- lingedge:class=contract\n-- +goose Up\nSELECT 1;\n", "header"},
		{"缺 Up", "000001_a.sql", "-- lingedge:class=expand\nSELECT 1;\n", "goose-annotation"},
		{"语句缺分号", "000001_a.sql", "-- lingedge:class=expand\n-- +goose Up\nSELECT 1\n", "syntax"},
		{"ENVSUB", "000001_a.sql", "-- lingedge:class=expand\n-- +goose ENVSUB on\n-- +goose Up\nSELECT 1;\n", "goose-annotation"},
		{"StatementBegin 未结束", "000001_a.sql", "-- lingedge:class=expand\n-- +goose Up\n-- +goose StatementBegin\nSELECT 1;\n", "goose-annotation"},
		{"文件名：时间戳", "20261117010101_a.sql", "-- lingedge:class=expand\n-- +goose Up\nSELECT 1;\n", "name"},
		{"文件名：大写", "000001_Add.sql", "-- lingedge:class=expand\n-- +goose Up\nSELECT 1;\n", "name"},
		{"class 写成无等号的旧形态", "000001_a.sql", "-- lingedge:class expand\n-- +goose Up\nSELECT 1;\n", "header"},
		{"class 行有前导空白", "000001_a.sql", " -- lingedge:class=expand\n-- +goose Up\nSELECT 1;\n", "header"},
		{"class 键写错", "000001_a.sql", "-- lingedge:classy=expand\n-- +goose Up\nSELECT 1;\n", "header"},
		{"文件名：五位序号", "00001_add.sql", "-- lingedge:class=expand\n-- +goose Up\nSELECT 1;\n", "name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, fs := ParseFile(c.file, []byte(c.body))
			for _, f := range fs {
				if f.Rule == c.rule {
					return
				}
			}
			t.Fatalf("期望出现规则 %q，实际发现: %v", c.rule, fs)
		})
	}
}

func TestLoadSortsAndChecksSequence(t *testing.T) {
	fsys := fstest.MapFS{
		"000002_b.sql": {Data: []byte("-- lingedge:class=expand\n-- +goose Up\nCREATE TABLE IF NOT EXISTS u (id INT) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON u TO lingedge_app;\n")},
		"000001_a.sql": {Data: []byte(goodExpand)},
		"CHECKSUMS":    {Data: []byte("# 非迁移文件被忽略\n")},
		"embed.go":     {Data: []byte("package migrations\n")},
	}
	set, findings, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("不应有发现: %v", findings)
	}
	if len(set.Migrations) != 2 || set.Migrations[0].Version != 1 || set.Migrations[1].Version != 2 {
		t.Fatalf("应按序号排序: %+v", set.Migrations)
	}
	// 跳号。
	delete(fsys, "000002_b.sql")
	fsys["000003_c.sql"] = &fstest.MapFile{Data: []byte("-- lingedge:class=expand\n-- +goose Up\nCREATE TABLE IF NOT EXISTS w (id INT) ENGINE=InnoDB;\nGRANT SELECT, INSERT, UPDATE, DELETE ON w TO lingedge_app;\n")}
	_, findings, _ = Load(fsys)
	if !hasRule(findings, "sequence") {
		t.Fatalf("跳号必须被报告: %v", findings)
	}
	// 重复序号。
	fsys["000001_dup.sql"] = &fstest.MapFile{Data: []byte(goodExpand)}
	_, findings, _ = Load(fsys)
	if !hasRule(findings, "sequence") {
		t.Fatalf("重复序号必须被报告: %v", findings)
	}
}

func hasRule(fs []Finding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func TestSplitStatementsFlagsGooseMysqlDivergences(t *testing.T) {
	stmts, _ := splitStatements("DROP TABLE IF EXISTS a; DROP TABLE IF EXISTS b;\nSELECT 1;\n", 1)
	if len(stmts) != 3 || stmts[0].SameLine || !stmts[1].SameLine || stmts[2].SameLine {
		t.Fatalf("同一行的第二条语句必须被标出（goose 按行尾分号切分）: %+v", stmts)
	}
	stmts, _ = splitStatements("/*!40014 SET FOREIGN_KEY_CHECKS=0 */;\nCREATE TABLE IF NOT EXISTS t (id INT);\n", 1)
	if len(stmts) != 2 || !stmts[0].ExecComment || stmts[1].ExecComment {
		t.Fatalf("可执行注释必须被标出: %+v", stmts)
	}
	stmts, _ = splitStatements("/* 普通块注释 */ SELECT 1;\n", 1)
	if len(stmts) != 1 || stmts[0].ExecComment {
		t.Fatalf("普通块注释不是可执行注释: %+v", stmts)
	}
	stmts, _ = splitStatements("INSERT IGNORE INTO t (a) VALUES ('x;\ny');\n", 1)
	if len(stmts) != 1 || !stmts[0].SplitRisk {
		t.Fatalf("字符串内以分号结尾的行会被 goose 切断，必须被标出: %+v", stmts)
	}
	stmts, _ = splitStatements("INSERT IGNORE INTO t (a) VALUES ('x;y');\n", 1)
	if len(stmts) != 1 || stmts[0].SplitRisk {
		t.Fatalf("行内分号不是风险: %+v", stmts)
	}
}
