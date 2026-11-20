package migrate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Schema lint（Story 1.7 AC2、database/README §2.1/§2.2/§2.6/§6）：在迁移 Up 语句上静态检查列类型与租户索引约定。
// 与 DDL 预检（ddlcheck.go，MySQL 会报错的组合）不同，这里检查的是「MySQL 不会报错、但违反本项目数据库约定」的写法。
//
// 规则（rule → 约定）：
//
//	schema-enum           不得使用 ENUM/SET 类型：用 VARCHAR(n) + CHECK（新增取值走 expand 迁移，§2.2）
//	schema-lob-default    TEXT/BLOB/JSON 列不得带 DEFAULT（字面量与表达式均不允许，§2.2；应用层显式赋值）
//	schema-time           时间列一律 DATETIME(3)（UTC）：不得用 TIMESTAMP（2038、时区）、TIME，DATETIME 必须带 (3)（§2.2）
//	schema-float          不得使用 FLOAT/DOUBLE/REAL/DECIMAL/NUMERIC 列（金额用 BIGINT 最小货币单位，比率用整数 permille/bp，§2.2）
//	schema-tenant-column  登记为租户范围（T）的表必须含 `tenant_id … NOT NULL`（§2.1、§6 第 1 条）
//	schema-tenant-index   含 `tenant_id NOT NULL` 的租户表：主键之外的所有索引必须以 tenant_id 打头（§2.6、§6 第 2 条），使「漏写租户条件」
//	                      的全表扫描在 EXPLAIN 断言中可被发现。两类合法例外：① 仅含 `uid` 一列的唯一索引（ULID 全局唯一，C1 资源按 uid
//	                      解析后再校验租户，§2.3）；② 语句前注释 `-- lingedge:cross-tenant-index=<索引名> <原因>` 显式登记的跨租户索引（原因必填，
//	                      如「登录后按用户反查所属租户」）
//
// 局限：与 ddlcheck.go 一样只理解常规写法；经 PREPARE 执行的动态 SQL 不可见（由真实 MySQL 的空库全量 up 与 information_schema 断言兜底）。
const (
	RuleSchemaEnum         = "schema-enum"
	RuleSchemaLOBDefault   = "schema-lob-default"
	RuleSchemaTime         = "schema-time"
	RuleSchemaFloat        = "schema-float"
	RuleSchemaTenantColumn = "schema-tenant-column"
	RuleSchemaTenantIndex  = "schema-tenant-index"
)

// tenantTables 是文档登记为租户范围（T，tenant_id NOT NULL）的 M1 表（database/*.md「通用列：… + T」）。
// 这些表一旦被创建，必须含 `tenant_id NOT NULL`。T?（可空租户，NULL=平台级）与平台级表不在此列。
var tenantTables = map[string]bool{
	"tenant_members": true, "tenant_quotas": true,
	"site_groups": true, "sites": true, "site_domains": true, "site_settings": true,
	"origin_groups": true, "origins": true, "site_versions": true,
	"site_rules": true, "rule_versions": true,
	"cache_policies": true, "cache_tasks": true, "certificates": true,
	"certificate_deployments": true, "origin_health": true, "site_cluster_bindings": true, "cache_task_nodes": true,
}

var crossTenantRe = regexp.MustCompile(`lingedge:cross-tenant-index=(\w+)(?:[ \t]+(\S[^\n]*))?`)

type schemaIndex struct {
	name     string
	cols     []string // 各键列首个标识符；函数索引表达式为空串
	unique   bool
	primary  bool
	reported bool // 已判定（违规已报告，或已由豁免注释放行）
}

type schemaTable struct {
	name          string
	cols          map[string]bool // 列名 → 是否 NOT NULL（主键列隐含 NOT NULL）
	columnFlagged bool            // 「缺少 tenant_id」已报告
	line          int
	indexes       []*schemaIndex
}

func newSchemaTable(name string, line int) *schemaTable {
	return &schemaTable{name: name, line: line, cols: map[string]bool{}}
}

// tenantNotNull 报告表是否含 `tenant_id … NOT NULL`（租户表的定义）。
func (t *schemaTable) tenantNotNull() bool { return t.cols["tenant_id"] }

// exemptions 是一条语句前注释登记的跨租户索引豁免：索引名 → 是否被本语句里的某个索引消费（未消费的豁免是死豁免，会被报告）。
type exemptions map[string]bool

// take 报告索引名是否已登记豁免，并标记为已消费。
func (e exemptions) take(name string) bool {
	if _, ok := e[name]; ok && name != "" {
		e[name] = true
		return true
	}
	return false
}

// schemaState 按迁移顺序累积表的租户相关事实（含后续 ALTER/CREATE INDEX 追加的索引）。
type schemaState struct {
	tables map[string]*schemaTable
}

func newSchemaState() *schemaState { return &schemaState{tables: map[string]*schemaTable{}} }

type schemaFinding struct {
	Line    int
	Rule    string
	Message string
}

// check 处理一条 Up 语句，返回新发现并更新模型。
func (s *schemaState) check(in stmtInfo) []schemaFinding {
	var out []schemaFinding
	add := func(rule, format string, a ...any) {
		out = append(out, schemaFinding{Line: in.st.Line, Rule: rule, Message: fmt.Sprintf(format, a...)})
	}
	exempt := exemptions{}
	for _, m := range crossTenantRe.FindAllStringSubmatch(in.st.Lead, -1) {
		if strings.TrimSpace(m[2]) == "" {
			add(RuleSchemaTenantIndex, "跨租户索引豁免 `lingedge:cross-tenant-index=%s` 必须写明原因（如「登录后按用户反查所属租户」）", m[1])
			continue
		}
		exempt[strings.ToLower(m[1])] = false
	}
	toks := in.toks
	var touched []*schemaTable
	switch in.kind {
	case "CREATE TABLE":
		if in.temporary || in.object == "" {
			return out
		}
		touched = append(touched, s.createTable(in, exempt, add))
	case "ALTER TABLE":
		touched = append(touched, s.alterTable(in, exempt, add))
	case "CREATE INDEX":
		touched = append(touched, s.createIndex(toks, exempt))
	case "DROP INDEX":
		touched = append(touched, s.dropIndexStmt(toks))
	case "DROP TABLE":
		delete(s.tables, in.object)
	case "RENAME TABLE":
		touched = s.renameTables(toks)
	}
	for _, t := range touched {
		if t != nil {
			out = append(out, s.evaluate(t, in.st.Line)...)
		}
	}
	var dead []string
	for name, used := range exempt {
		if !used {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	for _, name := range dead {
		add(RuleSchemaTenantIndex, "跨租户索引豁免 `lingedge:cross-tenant-index=%s` 没有对应本语句里的任何索引（索引名拼写错误，或索引已不在此处）：死豁免会掩盖之后的违规，请删除或改正", name)
	}
	return out
}

func (s *schemaState) createTable(in stmtInfo, exempt exemptions, add func(rule, format string, a ...any)) *schemaTable {
	toks := in.toks
	i := 1
	if i < len(toks) && toks[i].is("TEMPORARY") {
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
	if name == "" || i >= len(toks) || !toks[i].isPunct("(") {
		return nil // CREATE TABLE … LIKE / AS SELECT：不在静态规则范围
	}
	j := matchParen(toks, i)
	if j < 0 {
		return nil
	}
	if _, exists := s.tables[name]; exists && hasIfNotExists(toks) {
		return nil // 已存在则语句无效果，保持原模型
	}
	t := newSchemaTable(name, in.st.Line)
	s.tables[name] = t
	for _, def := range splitTopLevel(toks[i+1 : j]) {
		s.applyDef(t, def, exempt, add)
	}
	return t
}

func (s *schemaState) alterTable(in stmtInfo, exempt exemptions, add func(rule, format string, a ...any)) *schemaTable {
	if in.object == "" {
		return nil
	}
	t, known := s.tables[in.object]
	if !known {
		// 迁移之外已存在的表：模型未知，不做租户索引判定；列类型规则仍对新增/修改的列生效（用不登记的临时模型承接）。
		t = newSchemaTable(in.object, in.st.Line)
	}
	var renamed *schemaTable
	for _, spec := range in.alterSpecTokens() {
		if len(spec) == 0 {
			continue
		}
		switch {
		case spec[0].is("ADD"):
			rest := spec[1:]
			if len(rest) > 0 && rest[0].is("COLUMN") {
				rest = rest[1:]
			}
			if len(rest) > 0 && rest[0].isPunct("(") {
				if k := matchParen(rest, 0); k > 0 {
					for _, def := range splitTopLevel(rest[1:k]) {
						s.applyDef(t, def, exempt, add)
					}
				}
			} else {
				s.applyDef(t, rest, exempt, add)
			}
		case spec[0].is("MODIFY"), spec[0].is("CHANGE"):
			rest := spec[1:]
			if len(rest) > 0 && rest[0].is("COLUMN") {
				rest = rest[1:]
			}
			if spec[0].is("CHANGE") && len(rest) > 1 {
				delete(t.cols, rest[0].ident()) // CHANGE 旧名 新定义：旧列不再存在
				rest = rest[1:]
			}
			s.applyDef(t, rest, exempt, add)
		case spec[0].is("DROP"):
			t.alterDrop(spec[1:])
		case spec[0].is("RENAME"):
			if nt := s.alterRename(t, known, spec[1:]); nt != nil {
				renamed = nt
			}
		}
	}
	if !known {
		return nil
	}
	if renamed != nil {
		return renamed
	}
	return t
}

// alterDrop 处理 DROP COLUMN/INDEX/KEY/PRIMARY KEY（外键与 CHECK 与本规则无关）。
func (t *schemaTable) alterDrop(spec []token) {
	if len(spec) == 0 {
		return
	}
	switch {
	case spec[0].is("COLUMN") && len(spec) > 1:
		delete(t.cols, spec[1].ident())
	case (spec[0].is("INDEX") || spec[0].is("KEY")) && len(spec) > 1:
		t.removeIndex(spec[1].ident())
	case spec[0].is("PRIMARY"):
		out := t.indexes[:0]
		for _, idx := range t.indexes {
			if !idx.primary {
				out = append(out, idx)
			}
		}
		t.indexes = out
	case spec[0].is("FOREIGN") || spec[0].is("CHECK") || spec[0].is("CONSTRAINT") || spec[0].is("PARTITION"):
	case spec[0].kind == tWord:
		delete(t.cols, spec[0].ident()) // DROP <列>（COLUMN 可省）
	}
}

func (t *schemaTable) removeIndex(name string) {
	out := t.indexes[:0]
	for _, idx := range t.indexes {
		if idx.name != name {
			out = append(out, idx)
		}
	}
	t.indexes = out
}

// alterRename 处理 ALTER 的 RENAME 子句：RENAME COLUMN a TO b、RENAME INDEX|KEY a TO b、RENAME [TO|AS] 新表名。
// 改名后的表按新名重新登记（并在 evaluate 时按新名重新匹配租户表登记清单）；返回改名后的表。
func (s *schemaState) alterRename(t *schemaTable, known bool, spec []token) *schemaTable {
	if len(spec) == 0 {
		return nil
	}
	switch {
	case spec[0].is("COLUMN") && len(spec) >= 4 && spec[2].is("TO"):
		oldN, newN := spec[1].ident(), spec[3].ident()
		if nn, ok := t.cols[oldN]; ok {
			delete(t.cols, oldN)
			t.cols[newN] = nn
		} else {
			delete(t.cols, newN)
		}
		for _, idx := range t.indexes {
			for i, c := range idx.cols {
				if c == oldN {
					idx.cols[i] = newN
				}
			}
		}
	case (spec[0].is("INDEX") || spec[0].is("KEY")) && len(spec) >= 4 && spec[2].is("TO"):
		for _, idx := range t.indexes {
			if idx.name == spec[1].ident() {
				idx.name = spec[3].ident()
			}
		}
	default:
		rest := spec
		if rest[0].is("TO") || rest[0].is("AS") {
			rest = rest[1:]
		}
		to, _ := parseTableName(rest, 0)
		if to == "" || !known {
			return nil
		}
		s.moveTable(t, to)
		return t
	}
	return nil
}

// moveTable 把表模型改登记到新名（RENAME TABLE / ALTER … RENAME）。
func (s *schemaState) moveTable(t *schemaTable, to string) {
	delete(s.tables, t.name)
	t.name = to
	t.columnFlagged = false
	s.tables[to] = t
}

// renameTables 处理 RENAME TABLE a TO b[, c TO d …]，返回被改名的表。
func (s *schemaState) renameTables(toks []token) []*schemaTable {
	var out []*schemaTable
	if len(toks) < 2 {
		return nil
	}
	for _, part := range splitTopLevel(toks[2:]) {
		from, k := parseTableName(part, 0)
		if from == "" || k >= len(part) || !part[k].is("TO") {
			continue
		}
		to, _ := parseTableName(part, k+1)
		if t := s.tables[from]; t != nil && to != "" {
			s.moveTable(t, to)
			out = append(out, t)
		}
	}
	return out
}

// dropIndexStmt 处理 DROP INDEX name ON tbl。
func (s *schemaState) dropIndexStmt(toks []token) *schemaTable {
	i := 1
	for i < len(toks) && !toks[i].is("INDEX") {
		i++
	}
	i++
	if i >= len(toks) {
		return nil
	}
	name := toks[i].ident()
	for i < len(toks) && !toks[i].is("ON") {
		i++
	}
	tbl, _ := parseTableName(toks, i+1)
	if t := s.tables[tbl]; t != nil {
		t.removeIndex(name)
	}
	return nil
}

// createIndex 处理 CREATE [UNIQUE|FULLTEXT|SPATIAL] INDEX name [USING x] ON tbl (cols)。
func (s *schemaState) createIndex(toks []token, exempt exemptions) *schemaTable {
	idx := &schemaIndex{}
	i := 1
	for ; i < len(toks) && !toks[i].is("INDEX"); i++ {
		if toks[i].is("UNIQUE") {
			idx.unique = true
		}
	}
	i++
	if i >= len(toks) {
		return nil
	}
	idx.name = toks[i].ident()
	on := i
	for on < len(toks) && !toks[on].is("ON") {
		on++
	}
	tbl, k := parseTableName(toks, on+1)
	t := s.tables[tbl]
	if t == nil || k >= len(toks) || !toks[k].isPunct("(") {
		return nil
	}
	if j := matchParen(toks, k); j > 0 {
		idx.cols = keyCols(toks[k+1 : j])
	}
	idx.reported = exempt.take(idx.name)
	t.indexes = append(t.indexes, idx)
	return t
}

// keyCols 返回键列清单：每个逗号分隔项的首个标识符（带长度/排序的写法照常；函数索引表达式记为空串）。
func keyCols(toks []token) []string {
	var out []string
	for _, part := range splitTopLevel(toks) {
		switch {
		case len(part) == 0:
		case part[0].kind == tWord || part[0].kind == tQuoted:
			out = append(out, part[0].ident())
		default:
			out = append(out, "")
		}
	}
	return out
}

// applyDef 处理一条表定义：列（类型规则 + 内联索引）或索引/约束。
func (s *schemaState) applyDef(t *schemaTable, def []token, exempt exemptions, add func(rule, format string, a ...any)) {
	if len(def) == 0 {
		return
	}
	switch {
	case def[0].is("PRIMARY") || def[0].is("UNIQUE") || def[0].is("KEY") || def[0].is("INDEX") || def[0].is("FULLTEXT") || def[0].is("SPATIAL") || def[0].is("CONSTRAINT"):
		t.addIndexDef(def, exempt)
		return
	case def[0].is("FOREIGN") || def[0].is("CHECK") || def[0].is("LIKE") || def[0].is("PARTITION"):
		return
	}
	s.applyColumn(t, def, exempt, add)
}

// addIndexDef 解析表内索引定义：[CONSTRAINT [名]] PRIMARY KEY|UNIQUE [KEY|INDEX] [名] (…) | KEY|INDEX|FULLTEXT|SPATIAL [名] (…)。
func (t *schemaTable) addIndexDef(def []token, exempt exemptions) {
	i := 0
	idx := &schemaIndex{}
	if def[i].is("CONSTRAINT") {
		i++
		if i < len(def) && !def[i].is("PRIMARY") && !def[i].is("UNIQUE") && !def[i].is("FOREIGN") && !def[i].is("CHECK") {
			idx.name = def[i].ident()
			i++
		}
	}
	if i >= len(def) {
		return
	}
	switch {
	case def[i].is("FOREIGN"), def[i].is("CHECK"):
		return
	case def[i].is("PRIMARY"):
		idx.primary, idx.unique = true, true
		i++
		if i < len(def) && def[i].is("KEY") {
			i++
		}
	case def[i].is("UNIQUE"):
		idx.unique = true
		i++
		if i < len(def) && (def[i].is("KEY") || def[i].is("INDEX")) {
			i++
		}
	case def[i].is("FULLTEXT"), def[i].is("SPATIAL"):
		i++
		if i < len(def) && (def[i].is("KEY") || def[i].is("INDEX")) {
			i++
		}
	case def[i].is("KEY"), def[i].is("INDEX"):
		i++
	default:
		return
	}
	if i < len(def) && !def[i].isPunct("(") && !def[i].is("USING") {
		idx.name = def[i].ident()
		i++
	}
	for i < len(def) && !def[i].isPunct("(") {
		i++ // USING BTREE 等
	}
	if i >= len(def) {
		return
	}
	if j := matchParen(def, i); j > 0 {
		idx.cols = keyCols(def[i+1 : j])
	}
	if exempt.take(idx.name) {
		idx.reported = true // 已登记豁免：不再判定
	}
	t.indexes = append(t.indexes, idx)
}

var floatTypes = map[string]bool{"FLOAT": true, "FLOAT4": true, "FLOAT8": true, "DOUBLE": true, "REAL": true, "DECIMAL": true, "DEC": true, "NUMERIC": true, "FIXED": true}
var lobTypes = map[string]bool{
	"TEXT": true, "TINYTEXT": true, "MEDIUMTEXT": true, "LONGTEXT": true,
	"BLOB": true, "TINYBLOB": true, "MEDIUMBLOB": true, "LONGBLOB": true, "JSON": true,
	"LONG": true, "GEOMETRY": true, "POINT": true, "LINESTRING": true, "POLYGON": true,
	"MULTIPOINT": true, "MULTILINESTRING": true, "MULTIPOLYGON": true, "GEOMETRYCOLLECTION": true, "GEOMCOLLECTION": true,
}

func (s *schemaState) applyColumn(t *schemaTable, def []token, exempt exemptions, add func(rule, format string, a ...any)) {
	if def[0].kind != tWord && def[0].kind != tQuoted {
		return
	}
	col := def[0].ident()
	if len(def) < 2 || def[1].kind != tWord {
		return
	}
	typ := def[1].up
	switch {
	case typ == "ENUM" || typ == "SET":
		add(RuleSchemaEnum, "表 %s 的列 %s 使用了 %s：不得使用 ENUM/SET，请用 VARCHAR(n) + CHECK（新增取值走 expand 迁移，database/README §2.2）", t.name, col, typ)
	case floatTypes[typ]:
		add(RuleSchemaFloat, "表 %s 的列 %s 使用了 %s：数据库内不得有浮点/定点列；金额用 BIGINT 最小货币单位 + currency CHAR(3)，比率用整数（*_permille / *_bp），见 database/README §2.2", t.name, col, typ)
	case typ == "TIMESTAMP" || typ == "TIME":
		add(RuleSchemaTime, "表 %s 的列 %s 使用了 %s：时间列一律 DATETIME(3)（UTC；TIMESTAMP 有 2038 与时区问题），见 database/README §2.2", t.name, col, typ)
	case typ == "DATETIME":
		if len(def) <= 4 || !def[2].isPunct("(") || def[3].text != "3" || !def[4].isPunct(")") {
			add(RuleSchemaTime, "表 %s 的列 %s 是 DATETIME 但未写 (3)：时间列一律 DATETIME(3)（毫秒精度，UTC），见 database/README §2.2", t.name, col)
		}
	case lobTypes[typ]:
		if hasTopLevel(def[2:], "DEFAULT") {
			add(RuleSchemaLOBDefault, "表 %s 的列 %s 是 %s，不得带 DEFAULT（字面量会报 ERROR 1101，表达式默认值亦不采用）：应用层必须显式赋值，见 database/README §2.2", t.name, col, typ)
		}
	}
	inlinePK := false
	// 内联索引：PRIMARY KEY / 裸 KEY（= PRIMARY KEY）/ UNIQUE [KEY]。
	depth := 0
	for k := 2; k < len(def); k++ {
		switch {
		case def[k].isPunct("("):
			depth++
		case def[k].isPunct(")"):
			depth--
		case depth == 0 && def[k].is("PRIMARY") && k+1 < len(def) && def[k+1].is("KEY"):
			inlinePK = true
			t.indexes = append(t.indexes, &schemaIndex{cols: []string{col}, unique: true, primary: true})
		case depth == 0 && def[k].is("UNIQUE"):
			t.indexes = append(t.indexes, &schemaIndex{cols: []string{col}, unique: true})
		case depth == 0 && def[k].is("KEY") && (k == 0 || (!def[k-1].is("PRIMARY") && !def[k-1].is("UNIQUE"))):
			inlinePK = true
			t.indexes = append(t.indexes, &schemaIndex{cols: []string{col}, unique: true, primary: true})
		}
	}
	t.cols[col] = notNull(def) || inlinePK
}

// hasTopLevel 报告 toks 在括号深度 0 处是否含关键字 kw。
func hasTopLevel(toks []token, kw string) bool {
	depth := 0
	for _, t := range toks {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case depth == 0 && t.is(kw):
			return true
		}
	}
	return false
}

// evaluate 判定表是否满足租户表约定；每个违规索引只报告一次。
func (s *schemaState) evaluate(t *schemaTable, line int) []schemaFinding {
	var out []schemaFinding
	if tenantTables[t.name] && !t.tenantNotNull() {
		if !t.columnFlagged {
			t.columnFlagged = true
			out = append(out, schemaFinding{Line: line, Rule: RuleSchemaTenantColumn, Message: fmt.Sprintf("表 %s 登记为租户范围表（database/*.md「+ T」），必须含 `tenant_id BIGINT UNSIGNED NOT NULL`（database/README §2.1、§6）", t.name)})
		}
	}
	if !t.tenantNotNull() {
		return out
	}
	for _, idx := range t.indexes {
		if idx.primary || idx.reported {
			continue
		}
		idx.reported = true
		if len(idx.cols) > 0 && idx.cols[0] == "tenant_id" {
			continue
		}
		if idx.unique && len(idx.cols) == 1 && idx.cols[0] == "uid" {
			continue // 例外 ①：ULID 全局唯一，C1 资源按 uid 解析
		}
		label := idx.name
		if label == "" {
			label = "(" + strings.Join(idx.cols, ",") + ")"
		}
		out = append(out, schemaFinding{Line: line, Rule: RuleSchemaTenantIndex, Message: fmt.Sprintf("租户表 %s 的索引 %s 未以 tenant_id 打头：租户范围表的所有索引（主键与仅含 uid 的唯一索引除外）必须以 tenant_id 打头，使漏写租户条件的全表扫描可被 EXPLAIN 断言发现（database/README §2.6）；确属跨租户查询的索引请在语句前加 `-- lingedge:cross-tenant-index=<索引名> <原因>`", t.name, label)})
	}
	return out
}
