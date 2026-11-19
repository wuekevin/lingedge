package migrate

import (
	"fmt"
	"sort"
	"strings"
)

// DDL 预检（Story 1.6 AC6、ADR-016 §11、database/README §2.2/§8.2/§9）：在不连接数据库的前提下，
// 按迁移顺序静态检查 MySQL 8.0 已实测会报错的 DDL 组合。每条规则带实测的 MySQL 错误号，
// `tests/migrations` 用 ddl-rules.sql 的真实 MySQL 结果核对本检查的判定（静态判定与真实库逐例一致）。
//
// 规则（rule → MySQL 错误）：
//   ddl-generated-fk    STORED 生成列的基列上，外键不得为 CASCADE/SET NULL/SET DEFAULT       → ERROR 1215
//   ddl-check-fk        被 CHECK 引用的外键列只许 RESTRICT/NO ACTION/ON DELETE CASCADE        → ERROR 3823
//   ddl-forward-fk      外键目标表须出现在更早的迁移或同一迁移更早位置（循环外键用 ALTER ADD）  → ERROR 1824
//   ddl-partition-fk    分区表不得出现在任何外键的任一端                                     → ERROR 1506
//   ddl-partition-pmax  RANGE 分区表必须含 `PARTITION pmax VALUES LESS THAN (MAXVALUE)`      → ERROR 1526（越界 INSERT）
//   ddl-fk-checks       迁移不得 SET foreign_key_checks（绕过前向外键检查）
//
// 局限：只理解 CREATE TABLE / ALTER TABLE ... ADD|DROP|MODIFY|CHANGE / DROP TABLE / RENAME TABLE 的常规写法；
// 经 PREPARE 执行的动态 SQL 不可见（须靠真实 MySQL 的空库全量 up 兜底，见 make migrate-test）。

// DDL 规则 ID 与对应 MySQL 错误号。
const (
	RuleGeneratedFK   = "ddl-generated-fk"
	RuleCheckFK       = "ddl-check-fk"
	RuleForwardFK     = "ddl-forward-fk"
	RulePartitionFK   = "ddl-partition-fk"
	RulePartitionPmax = "ddl-partition-pmax"
	RuleFKChecks      = "ddl-fk-checks"
)

// RuleMySQLError 返回规则对应的 MySQL 错误号（实测，database/README §9）；无对应时为 0。
func RuleMySQLError(rule string) int {
	switch rule {
	case RuleGeneratedFK:
		return 1215
	case RuleCheckFK:
		return 3823
	case RuleForwardFK:
		return 1824
	case RulePartitionFK:
		return 1506
	case RulePartitionPmax:
		return 1526
	}
	return 0
}

// Violation 是一条 DDL 预检违规。
type Violation struct {
	Rule    string
	Message string
}

func (v Violation) key() string { return v.Rule + "|" + v.Message }

type colModel struct {
	name      string
	generated bool
	stored    bool
	expr      []token
}

type fkModel struct {
	name     string
	cols     []string
	refTable string
	onDelete string
	onUpdate string
}

type checkModel struct {
	name string
	expr []token
}

type tableModel struct {
	name        string
	cols        map[string]*colModel
	fks         []fkModel
	checks      []checkModel
	partitioned bool
	rangePart   bool
	hasPmax     bool
}

// Checker 按迁移顺序累积「已知表」模型，逐语句检查并报告新出现的违规。零值不可用，用 NewChecker 创建。
type Checker struct {
	tables map[string]*tableModel
	schema *schemaState // schema lint（schemalint.go）的租户索引模型
}

// NewChecker 创建检查器；known 为迁移之外已存在的表（如 goose 版本表），仅用于前向外键判定。
func NewChecker(known ...string) *Checker {
	c := &Checker{tables: map[string]*tableModel{}, schema: newSchemaState()}
	for _, k := range known {
		c.tables[strings.ToLower(k)] = &tableModel{name: strings.ToLower(k), cols: map[string]*colModel{}}
	}
	return c
}

// Tables 返回模型中已知的表名（排序）。
func (c *Checker) Tables() []string {
	var out []string
	for n := range c.tables {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Check 处理一条语句，返回它引入的新违规，并更新模型。
func (c *Checker) Check(stmt string) []Violation {
	toks := tokenize(stmt)
	if len(toks) == 0 {
		return nil
	}
	before := c.evaluate()
	var imm []Violation
	switch {
	case toks[0].is("SET"):
		imm = c.checkSet(toks)
	case toks[0].is("CREATE"):
		imm = c.createTable(toks)
	case toks[0].is("ALTER"):
		imm = c.alterTable(toks)
	case toks[0].is("DROP"):
		c.dropTable(toks)
	case toks[0].is("RENAME"):
		c.renameTable(toks)
	}
	seen := map[string]bool{}
	for _, v := range before {
		seen[v.key()] = true
	}
	var out []Violation
	for _, v := range append(c.evaluate(), imm...) {
		if !seen[v.key()] {
			seen[v.key()] = true
			out = append(out, v)
		}
	}
	return out
}

func (c *Checker) checkSet(toks []token) []Violation {
	for _, t := range toks {
		if t.kind == tWord && strings.EqualFold(strings.TrimPrefix(t.text, "@@"), "foreign_key_checks") {
			return []Violation{{RuleFKChecks, "迁移不得修改 foreign_key_checks（会绕过前向外键 ERROR 1824 检查）：循环外键请先建表、再 ALTER TABLE ... ADD CONSTRAINT"}}
		}
	}
	return nil
}

// parseTableName 解析 [db.]name，返回小写表名与下一个位置。
func parseTableName(toks []token, i int) (string, int) {
	if i >= len(toks) || (toks[i].kind != tWord && toks[i].kind != tQuoted) {
		return "", i
	}
	name := toks[i].ident()
	i++
	if i+1 < len(toks) && toks[i].isPunct(".") && (toks[i+1].kind == tWord || toks[i+1].kind == tQuoted) {
		name = toks[i+1].ident()
		i += 2
	}
	return name, i
}

func (c *Checker) createTable(toks []token) []Violation {
	i := 1
	// 跳过 CREATE 与 TABLE 之间的修饰（TEMPORARY、DEFINER 不会出现在 CREATE TABLE）。
	temporary := false
	if i < len(toks) && toks[i].is("TEMPORARY") {
		temporary = true
		i++
	}
	if i >= len(toks) || !toks[i].is("TABLE") {
		return nil
	}
	i++
	if i+2 < len(toks) && toks[i].is("IF") && toks[i+1].is("NOT") && toks[i+2].is("EXISTS") {
		i += 3
	}
	name, i := parseTableName(toks, i)
	if name == "" || temporary {
		return nil
	}
	if i < len(toks) && toks[i].is("LIKE") {
		ref, _ := parseTableName(toks, i+1)
		t := &tableModel{name: name, cols: map[string]*colModel{}}
		if src := c.tables[ref]; src != nil {
			t = src.clone(name)
		}
		c.tables[name] = t
		return nil
	}
	t := &tableModel{name: name, cols: map[string]*colModel{}}
	var imm []Violation
	if i < len(toks) && toks[i].isPunct("(") {
		j := matchParen(toks, i)
		if j < 0 {
			j = len(toks)
		}
		for _, def := range splitTopLevel(toks[i+1 : j]) {
			imm = append(imm, c.applyDef(t, def)...)
		}
		if j < len(toks) {
			t.applyPartition(toks[j+1:])
		}
	}
	// IF NOT EXISTS 且表已存在：保持原模型（语句本身不会改变它）。
	if _, exists := c.tables[name]; exists && hasIfNotExists(toks) {
		return nil
	}
	c.tables[name] = t
	// 前向外键在登记本表之后判定「自引用」：自引用外键允许。
	return imm
}

func hasIfNotExists(toks []token) bool {
	for i := 0; i+2 < len(toks) && i < 6; i++ {
		if toks[i].is("IF") && toks[i+1].is("NOT") && toks[i+2].is("EXISTS") {
			return true
		}
	}
	return false
}

// applyDef 把一条表定义（列或约束）应用到表模型。
func (c *Checker) applyDef(t *tableModel, def []token) []Violation {
	if len(def) == 0 {
		return nil
	}
	kw := def[0]
	switch {
	case kw.is("PRIMARY") || kw.is("UNIQUE") || kw.is("KEY") || kw.is("INDEX") || kw.is("FULLTEXT") || kw.is("SPATIAL") || kw.is("LIKE"):
		return nil
	case kw.is("CONSTRAINT"):
		name := ""
		i := 1
		if i < len(def) && !def[i].is("PRIMARY") && !def[i].is("UNIQUE") && !def[i].is("FOREIGN") && !def[i].is("CHECK") {
			name = def[i].ident()
			i++
		}
		if i < len(def) {
			switch {
			case def[i].is("FOREIGN"):
				return c.addFK(t, name, def[i:])
			case def[i].is("CHECK"):
				t.addCheck(name, def[i:])
			}
		}
		return nil
	case kw.is("FOREIGN"):
		return c.addFK(t, "", def)
	case kw.is("CHECK"):
		t.addCheck("", def)
		return nil
	}
	t.addColumn(def)
	return nil
}

// clone 深拷贝表模型（CREATE TABLE ... LIKE）：之后对副本的 ALTER 不得改动源表。
func (t *tableModel) clone(name string) *tableModel {
	cp := *t
	cp.name = name
	cp.cols = make(map[string]*colModel, len(t.cols))
	for k, v := range t.cols {
		c := *v
		cp.cols[k] = &c
	}
	cp.fks = append([]fkModel(nil), t.fks...)
	for i := range cp.fks {
		cp.fks[i].cols = append([]string(nil), cp.fks[i].cols...)
	}
	cp.checks = append([]checkModel(nil), t.checks...)
	return &cp
}

func (t *tableModel) addColumn(def []token) {
	if len(def) == 0 || (def[0].kind != tWord && def[0].kind != tQuoted) {
		return
	}
	col := &colModel{name: def[0].ident()}
	depth := 0
	for i := 1; i < len(def); i++ {
		switch {
		case def[i].isPunct("("):
			// 顶层括号：可能是 AS (...)、CHECK (...)、DEFAULT (...)、类型长度；AS 与 CHECK 由下面的分支先处理。
			depth++
		case def[i].isPunct(")"):
			depth--
		case depth == 0 && def[i].is("AS") && i+1 < len(def) && def[i+1].isPunct("("):
			if j := matchParen(def, i+1); j > 0 {
				col.generated = true
				col.expr = def[i+2 : j]
				for k := j + 1; k < len(def) && k <= j+1; k++ {
					if def[k].is("STORED") {
						col.stored = true
					}
				}
				i = j
			}
		case depth == 0 && def[i].is("CHECK") && i+1 < len(def) && def[i+1].isPunct("("):
			if j := matchParen(def, i+1); j > 0 {
				name := ""
				if i >= 2 && def[i-2].is("CONSTRAINT") {
					name = def[i-1].ident()
				}
				if j+2 >= len(def) || !def[j+1].is("NOT") || !def[j+2].is("ENFORCED") {
					t.checks = append(t.checks, checkModel{name: name, expr: def[i+2 : j]})
				}
				i = j
			}
		}
	}
	t.cols[col.name] = col
}

func (t *tableModel) addCheck(name string, def []token) {
	// def 以 CHECK 开头：CHECK ( expr ) [[NOT] ENFORCED]
	if len(def) < 2 || !def[1].isPunct("(") {
		return
	}
	j := matchParen(def, 1)
	if j < 0 {
		return
	}
	if j+2 < len(def) && def[j+1].is("NOT") && def[j+2].is("ENFORCED") {
		return
	}
	t.checks = append(t.checks, checkModel{name: name, expr: def[2:j]})
}

func identList(toks []token) []string {
	var out []string
	for _, t := range toks {
		if t.kind == tWord || t.kind == tQuoted {
			out = append(out, t.ident())
		}
	}
	return out
}

// addFK 解析 FOREIGN KEY [idx] (cols) REFERENCES tbl [(cols)] [MATCH x] [ON DELETE a] [ON UPDATE b]。
func (c *Checker) addFK(t *tableModel, name string, def []token) []Violation {
	i := 0
	if i+1 < len(def) && def[i].is("FOREIGN") && def[i+1].is("KEY") {
		i += 2
	} else {
		return nil
	}
	if i < len(def) && !def[i].isPunct("(") {
		if name == "" {
			name = def[i].ident()
		}
		i++
	}
	if i >= len(def) || !def[i].isPunct("(") {
		return nil
	}
	j := matchParen(def, i)
	if j < 0 {
		return nil
	}
	fk := fkModel{name: name, cols: identList(def[i+1 : j])}
	i = j + 1
	if i >= len(def) || !def[i].is("REFERENCES") {
		return nil
	}
	ref, k := parseTableName(def, i+1)
	fk.refTable = ref
	i = k
	if i < len(def) && def[i].isPunct("(") {
		if j := matchParen(def, i); j > 0 {
			i = j + 1
		}
	}
	for i < len(def) {
		if def[i].is("ON") && i+1 < len(def) && (def[i+1].is("DELETE") || def[i+1].is("UPDATE")) {
			which := def[i+1].up
			act, n := parseRefAction(def, i+2)
			if which == "DELETE" {
				fk.onDelete = act
			} else {
				fk.onUpdate = act
			}
			i = n
			continue
		}
		i++
	}
	var imm []Violation
	_, known := c.tables[fk.refTable]
	if !known && fk.refTable != t.name { // 自引用外键合法
		imm = append(imm, Violation{RuleForwardFK, fmt.Sprintf("表 %s 的外键 %s 引用了尚未创建的表 %s（前向外键，MySQL ERROR 1824）：调整迁移顺序，循环外键先建表、再 ALTER TABLE ... ADD CONSTRAINT", t.name, fkLabel(fk), fk.refTable)})
	}
	t.fks = append(t.fks, fk)
	return imm
}

func fkLabel(fk fkModel) string {
	if fk.name != "" {
		return fk.name
	}
	return "(" + strings.Join(fk.cols, ",") + ")"
}

func parseRefAction(def []token, i int) (string, int) {
	if i >= len(def) {
		return "", i
	}
	switch {
	case def[i].is("RESTRICT"), def[i].is("CASCADE"):
		return def[i].up, i + 1
	case def[i].is("NO") && i+1 < len(def) && def[i+1].is("ACTION"):
		return "NO ACTION", i + 2
	case def[i].is("SET") && i+1 < len(def) && (def[i+1].is("NULL") || def[i+1].is("DEFAULT")):
		return "SET " + def[i+1].up, i + 2
	}
	return "", i + 1
}

// applyPartition 识别 PARTITION BY ... 子句及 pmax 兜底分区。
func (t *tableModel) applyPartition(rest []token) {
	for i := 0; i+1 < len(rest); i++ {
		if !rest[i].is("PARTITION") || !rest[i+1].is("BY") {
			continue
		}
		t.partitioned = true
		k := i + 2
		if k < len(rest) && rest[k].is("LINEAR") {
			k++
		}
		t.rangePart = k < len(rest) && rest[k].is("RANGE")
		t.hasPmax = false
		for p := i; p+5 < len(rest); p++ {
			if rest[p].is("PARTITION") && rest[p+2].is("VALUES") && rest[p+3].is("LESS") && rest[p+4].is("THAN") {
				q := p + 5
				if rest[q].isPunct("(") {
					q++
				}
				if q < len(rest) && rest[q].is("MAXVALUE") && rest[p+1].ident() == "pmax" {
					t.hasPmax = true
				}
			}
		}
		return
	}
}

func (c *Checker) alterTable(toks []token) []Violation {
	i := 1
	if i < len(toks) && toks[i].is("ONLINE") {
		i++
	}
	if i >= len(toks) || !toks[i].is("TABLE") {
		return nil
	}
	name, i := parseTableName(toks, i+1)
	if name == "" {
		return nil
	}
	t := c.tables[name]
	if t == nil {
		// 迁移之外已存在（或根本不存在）的表：用临时模型承接本语句，不登记为「已知表」——
		// 否则一个拼错的表名会让之后指向它的外键绕过前向外键检查。
		t = &tableModel{name: name, cols: map[string]*colModel{}}
	}
	var imm []Violation
	for _, spec := range splitTopLevel(toks[i:]) {
		if len(spec) == 0 {
			continue
		}
		switch {
		case spec[0].is("PARTITION") && len(spec) > 1 && spec[1].is("BY"):
			t.applyPartition(spec)
		case spec[0].is("REMOVE") && len(spec) > 1 && spec[1].is("PARTITIONING"):
			t.partitioned, t.rangePart, t.hasPmax = false, false, false
		case spec[0].is("RENAME"):
			c.alterRename(t, spec[1:])
		case spec[0].is("ADD"):
			imm = append(imm, c.alterAdd(t, spec[1:])...)
		case spec[0].is("DROP"):
			t.alterDrop(spec[1:])
		case spec[0].is("MODIFY"):
			rest := spec[1:]
			if len(rest) > 0 && rest[0].is("COLUMN") {
				rest = rest[1:]
			}
			t.addColumn(rest)
		case spec[0].is("CHANGE"):
			rest := spec[1:]
			if len(rest) > 0 && rest[0].is("COLUMN") {
				rest = rest[1:]
			}
			if len(rest) >= 2 {
				delete(t.cols, rest[0].ident())
				t.addColumn(rest[1:])
			}
		}
	}
	return imm
}

// alterRename 处理 RENAME [TO|AS] <新表名> 与 RENAME COLUMN <旧> TO <新>（RENAME INDEX/KEY 不影响模型）。
func (c *Checker) alterRename(t *tableModel, spec []token) {
	if len(spec) == 0 {
		return
	}
	switch {
	case spec[0].is("COLUMN") && len(spec) >= 4 && spec[2].is("TO"):
		oldN, newN := spec[1].ident(), spec[3].ident()
		if col := t.cols[oldN]; col != nil {
			delete(t.cols, oldN)
			col.name = newN
			t.cols[newN] = col
		}
		for i := range t.fks {
			for k, cn := range t.fks[i].cols {
				if cn == oldN {
					t.fks[i].cols[k] = newN
				}
			}
		}
	case spec[0].is("INDEX"), spec[0].is("KEY"):
	default:
		rest := spec
		if rest[0].is("TO") || rest[0].is("AS") {
			rest = rest[1:]
		}
		to, _ := parseTableName(rest, 0)
		if to == "" {
			return
		}
		if _, known := c.tables[t.name]; known {
			delete(c.tables, t.name)
			for _, o := range c.tables {
				for k := range o.fks {
					if o.fks[k].refTable == t.name {
						o.fks[k].refTable = to
					}
				}
			}
		}
		t.name = to
		c.tables[to] = t
	}
}

func (c *Checker) alterAdd(t *tableModel, spec []token) []Violation {
	if len(spec) == 0 {
		return nil
	}
	if spec[0].is("COLUMN") {
		spec = spec[1:]
		if len(spec) > 0 && spec[0].isPunct("(") {
			var imm []Violation
			if j := matchParen(spec, 0); j > 0 {
				for _, def := range splitTopLevel(spec[1:j]) {
					imm = append(imm, c.applyDef(t, def)...)
				}
			}
			return imm
		}
		t.addColumn(spec)
		return nil
	}
	if spec[0].isPunct("(") {
		var imm []Violation
		if j := matchParen(spec, 0); j > 0 {
			for _, def := range splitTopLevel(spec[1:j]) {
				imm = append(imm, c.applyDef(t, def)...)
			}
		}
		return imm
	}
	switch {
	case spec[0].is("CONSTRAINT"), spec[0].is("FOREIGN"), spec[0].is("CHECK"), spec[0].is("PRIMARY"), spec[0].is("UNIQUE"),
		spec[0].is("KEY"), spec[0].is("INDEX"), spec[0].is("FULLTEXT"), spec[0].is("SPATIAL"):
		return c.applyDef(t, spec)
	case spec[0].is("PARTITION"):
		return nil
	}
	t.addColumn(spec) // ADD <col_def>（COLUMN 关键字可省）
	return nil
}

func (t *tableModel) alterDrop(spec []token) {
	if len(spec) == 0 {
		return
	}
	switch {
	case spec[0].is("COLUMN") && len(spec) > 1:
		delete(t.cols, spec[1].ident())
	case spec[0].is("FOREIGN") && len(spec) > 2:
		name := spec[2].ident()
		out := t.fks[:0]
		for _, f := range t.fks {
			if f.name != name {
				out = append(out, f)
			}
		}
		t.fks = out
	case spec[0].is("CHECK") || spec[0].is("CONSTRAINT"):
		if len(spec) > 1 {
			name := spec[1].ident()
			outF := t.fks[:0]
			for _, f := range t.fks {
				if f.name != name {
					outF = append(outF, f)
				}
			}
			t.fks = outF
			outC := t.checks[:0]
			for _, ck := range t.checks {
				if ck.name != name {
					outC = append(outC, ck)
				}
			}
			t.checks = outC
		}
	case spec[0].is("PARTITION"):
	case spec[0].kind == tWord && !spec[0].is("INDEX") && !spec[0].is("KEY") && !spec[0].is("PRIMARY"):
		delete(t.cols, spec[0].ident()) // DROP <col>（COLUMN 可省）
	}
}

func (c *Checker) dropTable(toks []token) {
	i := 1
	if i < len(toks) && toks[i].is("TEMPORARY") {
		return
	}
	if i >= len(toks) || !toks[i].is("TABLE") {
		return
	}
	i++
	if i+1 < len(toks) && toks[i].is("IF") && toks[i+1].is("EXISTS") {
		i += 2
	}
	for _, part := range splitTopLevel(toks[i:]) {
		if n, _ := parseTableName(part, 0); n != "" {
			delete(c.tables, n)
		}
	}
}

func (c *Checker) renameTable(toks []token) {
	if len(toks) < 2 || !toks[1].is("TABLE") {
		return
	}
	for _, part := range splitTopLevel(toks[2:]) {
		from, i := parseTableName(part, 0)
		if from == "" || i >= len(part) || !part[i].is("TO") {
			continue
		}
		to, _ := parseTableName(part, i+1)
		if t := c.tables[from]; t != nil && to != "" {
			delete(c.tables, from)
			t.name = to
			c.tables[to] = t
			for _, o := range c.tables { // 其他表外键对它的引用同步改名
				for k := range o.fks {
					if o.fks[k].refTable == from {
						o.fks[k].refTable = to
					}
				}
			}
		}
	}
}

// refsIn 返回表达式里引用的、属于该表的列名（排序去重）。
func refsIn(expr []token, cols map[string]*colModel) []string {
	set := map[string]bool{}
	for _, t := range expr {
		if t.kind == tWord || t.kind == tQuoted {
			if _, ok := cols[t.ident()]; ok {
				set[t.ident()] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func intersects(a, b []string) []string {
	var out []string
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
				break
			}
		}
	}
	return out
}

func isCascadeLike(a string) bool { return a == "CASCADE" || a == "SET NULL" || a == "SET DEFAULT" }

// evaluate 对整个模型求当前违规集合（语句前后各求一次，差集即新增违规）。
func (c *Checker) evaluate() []Violation {
	var out []Violation
	names := c.Tables()
	for _, n := range names {
		t := c.tables[n]
		// 规则 1：STORED 生成列 × 基列外键动作。
		for _, g := range sortedCols(t) {
			if !g.generated || !g.stored {
				continue
			}
			base := refsIn(g.expr, t.cols)
			for _, fk := range t.fks {
				if !isCascadeLike(fk.onDelete) && !isCascadeLike(fk.onUpdate) {
					continue
				}
				if hit := intersects(fk.cols, base); len(hit) > 0 {
					out = append(out, Violation{RuleGeneratedFK, fmt.Sprintf("表 %s：STORED 生成列 %s 的基列 %s 上的外键 %s 使用了 %s（MySQL ERROR 1215）：唯一性生成列改用 VIRTUAL，或把该外键改为 RESTRICT",
						t.name, g.name, strings.Join(hit, ","), fkLabel(fk), fkActions(fk))})
				}
			}
		}
		// 规则 2：CHECK × 外键引用动作。
		for _, ck := range t.checks {
			refs := refsIn(ck.expr, t.cols)
			for _, fk := range t.fks {
				bad := fk.onDelete == "SET NULL" || fk.onDelete == "SET DEFAULT" || isCascadeLike(fk.onUpdate)
				if !bad {
					continue
				}
				if hit := intersects(fk.cols, refs); len(hit) > 0 {
					lbl := ck.name
					if lbl == "" {
						lbl = "(未命名)"
					}
					out = append(out, Violation{RuleCheckFK, fmt.Sprintf("表 %s：CHECK %s 引用的外键列 %s 上的外键 %s 使用了 %s（MySQL ERROR 3823）：只允许 RESTRICT/NO ACTION/ON DELETE CASCADE",
						t.name, lbl, strings.Join(hit, ","), fkLabel(fk), fkActions(fk))})
				}
			}
		}
		// 规则 4/5：分区表。
		if t.partitioned && len(t.fks) > 0 {
			out = append(out, Violation{RulePartitionFK, fmt.Sprintf("分区表 %s 不得带外键（MySQL ERROR 1506）：分区表一律无 FK，一致性由应用/迁移保证", t.name)})
		}
		for _, fk := range t.fks {
			if p := c.tables[fk.refTable]; p != nil && p.partitioned {
				out = append(out, Violation{RulePartitionFK, fmt.Sprintf("表 %s 的外键 %s 引用了分区表 %s（MySQL ERROR 1506）：分区表不得作为外键的父表", t.name, fkLabel(fk), p.name)})
			}
		}
		if t.partitioned && t.rangePart && !t.hasPmax {
			out = append(out, Violation{RulePartitionPmax, fmt.Sprintf("RANGE 分区表 %s 缺少 `PARTITION pmax VALUES LESS THAN (MAXVALUE)` 兜底分区（越界 INSERT 将报 ERROR 1526）", t.name)})
		}
	}
	return out
}

func fkActions(fk fkModel) string {
	var p []string
	if fk.onDelete != "" {
		p = append(p, "ON DELETE "+fk.onDelete)
	}
	if fk.onUpdate != "" {
		p = append(p, "ON UPDATE "+fk.onUpdate)
	}
	return strings.Join(p, " ")
}

func sortedCols(t *tableModel) []*colModel {
	names := make([]string, 0, len(t.cols))
	for n := range t.cols {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*colModel, 0, len(names))
	for _, n := range names {
		out = append(out, t.cols[n])
	}
	return out
}
