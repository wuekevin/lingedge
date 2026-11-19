package migrate

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 迁移 lint（Story 1.6 AC2、AC4、AC6，ADR-016 §3、ADR-013）。规则 ID（Finding.Rule）：
//
//	name / sequence            六位序号 NNNNNN_name.sql；序号从 000001 起连续且唯一
//	header / goose-annotation  第一行 `-- lingedge:class=reversible|expand|irreversible`；goose 指令合法
//	syntax                     语句以分号结束
//	class-expand               expand 的 Up 不含 DROP / RENAME / MODIFY / CHANGE（及 TRUNCATE）
//	class-reversible           reversible 必须有可执行的 Down
//	class-irreversible         irreversible 的 Down 只能为空或 `SELECT 'irreversible'`，且非空时必须有原因注释
//	idempotency                可重入（AC4）：CREATE TABLE IF NOT EXISTS、DROP ... IF EXISTS、触发器先 DROP IF EXISTS、
//	                           无原生守卫的 ALTER/CREATE INDEX 须为单语句文件或带 `-- lingedge:guard=<说明>`、种子 INSERT 须可重放
//	ddl-dml-split              数据回填与 DDL 分文件（AC4）
//	compound-statement         含 BEGIN...END 的触发器/存储过程须用 StatementBegin/End 包裹
//	grant-app / grant-maint    逐表授权（database/README §5.1、Story 1.5 发现）：每张新表须授权 lingedge_app，
//	                           分区表须授权 lingedge_maint 表级 SELECT, ALTER, DROP
//	ddl-*                      DDL 预检（ddlcheck.go，AC6）
//	schema-*                   schema lint（schemalint.go，Story 1.7 AC2）：无 ENUM、TEXT/JSON/BLOB 无 DEFAULT、时间列 DATETIME(3)、
//	                           无浮点金额、租户表含 tenant_id 且索引以其打头
//
// 本 lint 校验迁移结构、DDL 组合与列类型/租户索引约定。

const (
	// AppUser / MaintUser 是迁移里授权的运行期账号名（database/README §5.1）。
	AppUser   = "lingedge_app"
	MaintUser = "lingedge_maint"
)

// appendOnlyTables 是 lingedge_app 仅有 SELECT, INSERT 的表（审计与钱包流水，database/README §5.1）。
var appendOnlyTables = map[string]bool{"audit_logs": true, "wallet_entries": true}

var guardRe = regexp.MustCompile(`lingedge:guard=\S`)

type stmtInfo struct {
	st     Statement
	toks   []token
	kind   string // 如 "CREATE TABLE"、"ALTER TABLE"、"DROP TRIGGER"、"GRANT"、"INSERT"
	object string // 被创建/删除对象名（小写）
	// 修饰
	ifNotExists, ifExists, orReplace, temporary bool
}

var createObjects = map[string]bool{"TABLE": true, "TRIGGER": true, "INDEX": true, "VIEW": true, "PROCEDURE": true, "FUNCTION": true, "EVENT": true, "DATABASE": true, "SCHEMA": true, "USER": true, "ROLE": true}

func analyze(st Statement) stmtInfo {
	in := stmtInfo{st: st, toks: tokenize(st.Text)}
	t := in.toks
	if len(t) == 0 {
		in.kind = "EMPTY"
		return in
	}
	first := t[0].up
	switch first {
	case "CREATE", "DROP":
		i := 1
		for ; i < len(t) && i < 16; i++ {
			switch {
			case t[i].is("TEMPORARY"):
				in.temporary = true
			case t[i].is("OR") && i+1 < len(t) && t[i+1].is("REPLACE"):
				in.orReplace = true
			}
			if t[i].kind == tWord && createObjects[t[i].up] {
				break
			}
		}
		if i >= len(t) || t[i].kind != tWord || !createObjects[t[i].up] {
			in.kind = first
			return in
		}
		in.kind = first + " " + t[i].up
		i++
		if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
			in.ifNotExists = true
			i += 3
		} else if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
			in.ifExists = true
			i += 2
		}
		if in.kind == "CREATE INDEX" || in.kind == "DROP INDEX" {
			if i < len(t) {
				in.object = t[i].ident()
			}
		} else {
			in.object, _ = parseTableName(t, i)
		}
	case "ALTER":
		in.kind = "ALTER"
		if len(t) > 1 && t[1].is("ONLINE") {
			t = t[1:]
		}
		if len(t) > 2 && t[1].is("TABLE") {
			in.kind = "ALTER TABLE"
			in.object, _ = parseTableName(t, 2)
		} else if len(t) > 1 && (t[1].is("USER") || t[1].is("DATABASE") || t[1].is("SCHEMA")) {
			in.kind = "ALTER " + t[1].up
		}
	default:
		in.kind = first
		if first == "RENAME" && len(t) > 1 && t[1].is("TABLE") {
			in.kind = "RENAME TABLE"
		}
	}
	return in
}

func (in stmtInfo) isDDL() bool {
	switch {
	case strings.HasPrefix(in.kind, "CREATE"), strings.HasPrefix(in.kind, "ALTER"), strings.HasPrefix(in.kind, "DROP"),
		in.kind == "RENAME TABLE", in.kind == "TRUNCATE":
		return !in.temporary
	}
	return false
}

func (in stmtInfo) isDML() bool {
	switch in.kind {
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "LOAD":
		return true
	}
	return false
}

// alterSpecTokens 返回 ALTER TABLE 各子句的词序列。
func (in stmtInfo) alterSpecTokens() [][]token {
	if in.kind != "ALTER TABLE" {
		return nil
	}
	i := 2
	if len(in.toks) > 1 && in.toks[1].is("ONLINE") {
		i = 3
	}
	_, i = parseTableName(in.toks, i)
	return splitTopLevel(in.toks[i:])
}

// alterSpecs 返回 ALTER TABLE 各子句的首词（大写）。
func (in stmtInfo) alterSpecs() []string {
	var out []string
	for _, spec := range in.alterSpecTokens() {
		if len(spec) > 0 && spec[0].kind == tWord {
			out = append(out, spec[0].up)
		}
	}
	return out
}

// guardNeeded 报告语句是否属于 MySQL 无原生 IF [NOT] EXISTS 守卫的 DDL（重跑会报重复/不存在）。
func (in stmtInfo) guardNeeded() bool {
	switch in.kind {
	case "CREATE INDEX", "DROP INDEX":
		return true
	case "RENAME TABLE":
		return true
	case "ALTER TABLE":
		for _, s := range in.alterSpecs() {
			if s == "ADD" || s == "DROP" || s == "RENAME" {
				return true
			}
		}
	}
	return false
}

func lintSet(set *Set) []Finding {
	var out []Finding
	// 序号：从 1 起连续且唯一。
	seen := map[int64]string{}
	for _, m := range set.Migrations {
		if prev, ok := seen[m.Version]; ok {
			out = append(out, Finding{File: m.File, Rule: "sequence", Message: fmt.Sprintf("序号 %06d 与 %s 重复", m.Version, prev)})
		}
		seen[m.Version] = m.File
	}
	var uniq []*Migration
	last := int64(0)
	for _, m := range set.Migrations {
		if m.Version == last {
			continue
		}
		if m.Version != last+1 {
			out = append(out, Finding{File: m.File, Rule: "sequence", Message: fmt.Sprintf("序号不连续：%06d 之后应为 %06d（迁移序号从 000001 起连续，已发布迁移不得删除或跳号）", last, last+1)})
		}
		last = m.Version
		uniq = append(uniq, m)
	}
	checker := NewChecker()
	for _, m := range uniq {
		out = append(out, lintMigration(m, checker)...)
	}
	return out
}

// LintMigration 对单个迁移运行全部规则（checker 为此前迁移累积的表模型；传 NewChecker() 即孤立检查）。
func LintMigration(m *Migration, checker *Checker) []Finding { return lintMigration(m, checker) }

func lintMigration(m *Migration, checker *Checker) []Finding {
	var out []Finding
	add := func(line int, rule, format string, a ...any) {
		out = append(out, Finding{File: m.File, Line: line, Rule: rule, Message: fmt.Sprintf(format, a...)})
	}
	up := make([]stmtInfo, len(m.Up))
	for i, s := range m.Up {
		up[i] = analyze(s)
	}
	down := make([]stmtInfo, len(m.Down))
	for i, s := range m.Down {
		down[i] = analyze(s)
	}

	// ---- goose 与 MySQL 看到不同东西的写法、被禁止的语句 ----
	hazards := func(infos []stmtInfo, sec string) {
		for _, in := range infos {
			if in.st.ExecComment {
				add(in.st.Line, "executable-comment", "%s：含 `/*! … */` 可执行注释：MySQL 会执行其内容，而 lint 把它当注释，其中的 DROP/外键开关等规则将失效；请去掉可执行注释", sec)
			}
			if in.st.SameLine {
				add(in.st.Line, "syntax", "%s：语句与上一条写在同一行：goose 只在行尾分号处切分，会把它们作为一条多语句发送并失败；每条语句另起一行", sec)
			}
			if in.st.SplitRisk {
				add(in.st.Line, "syntax", "%s：字符串字面量内有以分号结尾的行：goose 会在该行把语句切断；请改写字面量（如拼接或去掉行尾分号）", sec)
			}
			if why := forbiddenStatement(in, sec == "Down"); why != "" {
				add(in.st.Line, "forbidden-statement", "%s：迁移不得执行 %s（%s）", sec, why, "账号由安装向导创建与授权、库名由连接决定、全局设置属运维；迁移只做本库的表结构与种子数据")
			}
		}
	}
	hazards(up, "Up")
	hazards(down, "Down")

	// ---- 分类规则 ----
	switch m.Class {
	case ClassExpand:
		for i, in := range up {
			if what := destructive(in); what != "" && !recreatesImmediately(up, i) {
				add(in.st.Line, "class-expand", "expand 迁移只增不破，不得含 %s（ADR-013 §1）；破坏性变更拆成后续版本的 irreversible/reversible 迁移", what)
			}
			if what := breakingForOldBinary(in); what != "" {
				add(in.st.Line, "class-expand", "expand 迁移必须让旧二进制照常工作（ADR-013 §1）：%s", what)
			}
		}
	case ClassReversible:
		if !m.HasDown {
			add(0, "class-reversible", "reversible 迁移必须有可执行的 `-- +goose Down`（经测试的回退语句；仅含 SELECT 'irreversible' 不算）")
		}
	case ClassIrreversible:
		for _, in := range down {
			if !isIrreversibleMarker(in.st) {
				add(in.st.Line, "class-irreversible", "irreversible 迁移的 Down 必须为空或仅 `SELECT 'irreversible'`（不得提供假的回退语句，ADR-013 §1）")
			}
		}
		if len(m.Down) > 0 && m.Reason == "" {
			add(0, "class-irreversible", "irreversible 迁移的 Down 非空时必须有原因注释：`-- lingedge:reason=<为什么不可逆>`")
		}
	}

	// ---- 可重入（AC4）----
	idem := func(infos []stmtInfo, sec string) {
		droppedBefore := map[string]bool{}
		for _, in := range infos {
			switch in.kind {
			case "CREATE TABLE":
				if !in.temporary && !in.ifNotExists {
					add(in.st.Line, "idempotency", "%s：CREATE TABLE 必须带 IF NOT EXISTS，使中断后重跑与一次性执行等价（AC4）", sec)
				}
			case "DROP TABLE", "DROP VIEW", "DROP TRIGGER", "DROP PROCEDURE", "DROP FUNCTION", "DROP EVENT", "DROP DATABASE", "DROP SCHEMA":
				if !in.temporary && !in.ifExists {
					add(in.st.Line, "idempotency", "%s：%s 必须带 IF EXISTS（AC4）", sec, in.kind)
				}
				if in.ifExists {
					droppedBefore[strings.TrimPrefix(in.kind, "DROP ")+" "+in.object] = true
				}
			case "CREATE TRIGGER", "CREATE PROCEDURE", "CREATE FUNCTION", "CREATE EVENT", "CREATE VIEW":
				obj := strings.TrimPrefix(in.kind, "CREATE ")
				if (!in.orReplace || obj != "VIEW") && !droppedBefore[obj+" "+in.object] {
					add(in.st.Line, "idempotency", "%s：%s %s 前必须有 `DROP %s IF EXISTS %s`（MySQL 无 CREATE ... IF NOT EXISTS，重跑才不会报已存在，AC4）", sec, obj, in.object, obj, in.object)
				}
				if !in.st.Block && hasWord(in.toks, "BEGIN") {
					add(in.st.Line, "compound-statement", "%s：含 BEGIN...END 的 %s 必须用 `-- +goose StatementBegin/StatementEnd` 包裹（否则 goose 会在体内分号处拆断）", sec, obj)
				}
			}
			if in.guardNeeded() && len(infos) != 1 && !guardRe.MatchString(in.st.Lead) {
				add(in.st.Line, "idempotency", "%s：%s 在 MySQL 没有 IF [NOT] EXISTS 原生守卫，重跑会失败；请把它独立成单语句迁移文件（MySQL 8 的单条 DDL 是原子的），或在其前加注释 `-- lingedge:guard=<重跑安全的依据>`（如经 information_schema 判断的 PREPARE 守卫）", sec, in.kind)
			}
			if sec == "Up" && in.kind == "INSERT" && !insertReplayable(in) {
				add(in.st.Line, "idempotency", "种子/回填 INSERT 必须可重放：使用 INSERT IGNORE、ON DUPLICATE KEY UPDATE 或 ... WHERE NOT EXISTS（AC4）")
			}
		}
	}
	idem(up, "Up")
	idem(down, "Down")

	// ---- DDL 与 DML 分文件（AC4）----
	var firstDDL, firstDML *stmtInfo
	for i := range up {
		if up[i].isDDL() && firstDDL == nil {
			firstDDL = &up[i]
		}
		if up[i].isDML() && firstDML == nil {
			firstDML = &up[i]
		}
	}
	if firstDDL != nil && firstDML != nil {
		add(firstDML.st.Line, "ddl-dml-split", "同一迁移同时含 DDL（第 %d 行）与 DML：数据回填必须拆成「DDL」与「DML」两个文件（DDL 隐式提交，混写会让 DML 无法随失败回滚；AC4）", firstDDL.st.Line)
	}

	// ---- DDL 预检（AC6）：按迁移顺序累积表模型 ----
	for _, in := range up {
		for _, v := range checker.Check(in.st.Text) {
			add(in.st.Line, v.Rule, "%s", v.Message)
		}
	}

	// ---- schema lint（Story 1.7 AC2）：列类型与租户索引约定 ----
	for _, in := range up {
		for _, f := range checker.schema.check(in) {
			add(f.Line, f.Rule, "%s", f.Message)
		}
	}

	// ---- 逐表授权 ----
	out = append(out, lintGrants(m, up, checker)...)
	return out
}

func hasWord(toks []token, w string) bool {
	for _, t := range toks {
		if t.is(w) {
			return true
		}
	}
	return false
}

func insertReplayable(in stmtInfo) bool {
	t := in.toks
	if len(t) > 1 && (t[1].is("IGNORE")) {
		return true
	}
	for i := 0; i+3 < len(t); i++ {
		if t[i].is("ON") && t[i+1].is("DUPLICATE") && t[i+2].is("KEY") {
			return true
		}
	}
	for i := 0; i+1 < len(t); i++ {
		if t[i].is("NOT") && t[i+1].is("EXISTS") {
			return true
		}
	}
	return false
}

// recreatesImmediately 判断 infos[i] 是否是「DROP <对象> IF EXISTS <名>」且紧跟同名 CREATE：这是可重入重建（触发器/视图/过程/函数/事件）的守卫，
// 不是破坏（lint 的可重入规则恰恰要求有它）。孤立的 DROP 仍然是破坏。
func recreatesImmediately(infos []stmtInfo, i int) bool {
	in := infos[i]
	switch in.kind {
	case "DROP TRIGGER", "DROP VIEW", "DROP PROCEDURE", "DROP FUNCTION", "DROP EVENT":
	default:
		return false
	}
	if !in.ifExists || i+1 >= len(infos) {
		return false
	}
	next := infos[i+1]
	return next.kind == "CREATE "+strings.TrimPrefix(in.kind, "DROP ") && next.object == in.object
}

// destructive 返回 expand 迁移里不允许的破坏性操作描述，没有则为空。
func destructive(in stmtInfo) string {
	switch in.kind {
	case "RENAME TABLE":
		return "RENAME"
	case "TRUNCATE":
		return "TRUNCATE"
	case "DELETE":
		return "DELETE（删除数据）"
	case "ALTER TABLE":
		for _, s := range in.alterSpecs() {
			switch s {
			case "DROP", "RENAME", "MODIFY", "CHANGE":
				return s
			}
		}
		return ""
	}
	if strings.HasPrefix(in.kind, "DROP") {
		return in.kind
	}
	return ""
}

// breakingForOldBinary 返回 expand 迁移里会让「上一版本二进制」出错的新增：NOT NULL 且无默认值的新列
// （旧二进制的 INSERT 不会提供该列）。README §8：expand 只允许「加可空列/带默认列」。
func breakingForOldBinary(in stmtInfo) string {
	for _, spec := range in.alterSpecTokens() {
		if len(spec) < 2 || !spec[0].is("ADD") {
			continue
		}
		rest := spec[1:]
		if rest[0].is("COLUMN") {
			rest = rest[1:]
		}
		var defs [][]token
		switch {
		case len(rest) > 0 && rest[0].isPunct("("):
			if j := matchParen(rest, 0); j > 0 {
				defs = splitTopLevel(rest[1:j])
			}
		case len(rest) > 0 && (rest[0].is("CONSTRAINT") || rest[0].is("FOREIGN") || rest[0].is("CHECK") || rest[0].is("PRIMARY") || rest[0].is("UNIQUE") ||
			rest[0].is("KEY") || rest[0].is("INDEX") || rest[0].is("FULLTEXT") || rest[0].is("SPATIAL") || rest[0].is("PARTITION")):
			continue
		default:
			defs = [][]token{rest}
		}
		for _, d := range defs {
			if len(d) == 0 || !notNull(d) || hasWord(d, "DEFAULT") || hasWord(d, "AUTO_INCREMENT") || hasWord(d, "AS") {
				continue
			}
			return "ADD COLUMN " + d[0].text + " 为 NOT NULL 且没有 DEFAULT（旧二进制的 INSERT 不提供该列会失败）；请改为可空或带默认值"
		}
	}
	return ""
}

func notNull(d []token) bool {
	for i := 0; i+1 < len(d); i++ {
		if d[i].is("NOT") && d[i+1].is("NULL") {
			return true
		}
	}
	return false
}

// forbiddenStatement 返回迁移里不允许执行的语句描述（账号/库/全局设置管理与会话切换）；没有则为空。
// Down 段允许 REVOKE（撤销本迁移授予的权限）。
func forbiddenStatement(in stmtInfo, down bool) string {
	t := in.toks
	if len(t) == 0 {
		return ""
	}
	switch t[0].up {
	case "REVOKE":
		if down {
			return ""
		}
		return "REVOKE"
	case "USE", "FLUSH", "RESET", "PURGE", "KILL", "SHUTDOWN", "INSTALL", "UNINSTALL", "LOAD", "RENAME":
		if t[0].up == "RENAME" && in.kind == "RENAME TABLE" {
			return ""
		}
		return t[0].up
	case "SET":
		if len(t) > 1 {
			switch t[1].up {
			case "GLOBAL", "PERSIST", "PERSIST_ONLY", "PASSWORD", "ROLE", "DEFAULT":
				return "SET " + t[1].up
			}
			if len(t) > 1 && strings.HasPrefix(t[1].up, "@@GLOBAL") {
				return "SET @@GLOBAL"
			}
		}
	}
	switch in.kind {
	case "CREATE USER", "DROP USER", "ALTER USER", "CREATE ROLE", "DROP ROLE", "CREATE DATABASE", "DROP DATABASE", "CREATE SCHEMA", "DROP SCHEMA", "ALTER DATABASE", "ALTER SCHEMA":
		return in.kind
	}
	return ""
}

type grantInfo struct {
	line   int
	table  string
	users  []string // 被授权账号（仅用户名部分，小写）
	user   string   // 唯一的被授权账号（lintGrants 校验 len(users)==1 之后填入）
	privs  []string
	scoped bool // 目标是否为「单个不带库名的表」
	// 授权后的多余子句（WITH GRANT OPTION / AS / REQUIRE …）。
	trailing string
}

// parseGrant 解析 `GRANT <权限> ON [TABLE] <目标> TO <账号>[, <账号>…] [WITH …]`。
func parseGrant(in stmtInfo) (grantInfo, bool) {
	t := in.toks
	g := grantInfo{line: in.st.Line}
	i := 1
	// 权限列表：直到 ON。
	var cur []string
	for ; i < len(t) && !t[i].is("ON"); i++ {
		switch {
		case t[i].isPunct(","):
			if len(cur) > 0 {
				g.privs = append(g.privs, strings.Join(cur, " "))
			}
			cur = nil
		case t[i].isPunct("("), t[i].isPunct(")"):
			cur = append(cur, t[i].text) // 列级授权：权限名带括号，后面会被判为不合法权限集合
		default:
			cur = append(cur, strings.ToUpper(t[i].text))
		}
	}
	if len(cur) > 0 {
		g.privs = append(g.privs, strings.Join(cur, " "))
	}
	if i >= len(t) {
		return g, false
	}
	i++ // ON
	if i < len(t) && (t[i].is("TABLE")) {
		i++
	}
	var target []token
	for ; i < len(t) && !t[i].is("TO"); i++ {
		target = append(target, t[i])
	}
	if len(target) == 1 && (target[0].kind == tWord || target[0].kind == tQuoted) {
		g.table, g.scoped = target[0].ident(), true
	}
	if i >= len(t) {
		return g, false
	}
	i++ // TO
	// 账号列表：以逗号分隔；每个账号是 user 或 user@host（引号/反引号/裸词均可）。WITH/AS/REQUIRE 起是授权选项，一律不允许。
	end := len(t)
	for k := i; k < len(t); k++ {
		if t[k].is("WITH") || t[k].is("AS") || t[k].is("REQUIRE") {
			end = k
			g.trailing = strings.ToUpper(t[k].text)
			for _, x := range t[k+1:] {
				if x.kind == tWord {
					g.trailing += " " + x.up
				}
			}
			break
		}
	}
	for _, acct := range splitTopLevel(t[i:end]) {
		if len(acct) == 0 || (acct[0].kind != tWord && acct[0].kind != tQuoted && acct[0].kind != tString) {
			continue
		}
		u := acct[0].text
		if acct[0].kind == tWord {
			if at := strings.IndexByte(u, '@'); at >= 0 { // 裸词形态 lingedge_app@localhost
				u = u[:at]
			}
		}
		g.users = append(g.users, strings.ToLower(u))
	}
	return g, len(g.users) > 0
}

func lintGrants(m *Migration, up []stmtInfo, checker *Checker) []Finding {
	var out []Finding
	add := func(line int, rule, format string, a ...any) {
		out = append(out, Finding{File: m.File, Line: line, Rule: rule, Message: fmt.Sprintf(format, a...)})
	}
	created := map[string]int{} // 表 → 创建语句下标
	var order []string
	for i, in := range up {
		if in.kind == "CREATE TABLE" && !in.temporary && in.object != "" {
			if _, dup := created[in.object]; !dup {
				created[in.object] = i
				order = append(order, in.object)
			}
		}
	}
	// RENAME TABLE：MySQL 的表级权限不随表改名，新名字的表同样需要逐表授权。
	for i, in := range up {
		if in.kind != "RENAME TABLE" {
			continue
		}
		for _, part := range splitTopLevel(in.toks[2:]) {
			_, k := parseTableName(part, 0)
			if k < len(part) && part[k].is("TO") {
				if to, _ := parseTableName(part, k+1); to != "" {
					if _, dup := created[to]; !dup {
						created[to] = i
						order = append(order, to)
					}
				}
			}
		}
	}
	appGranted := map[string]int{}
	maintGranted := map[string]int{}
	for i, in := range up {
		if in.kind != "GRANT" {
			continue
		}
		g, ok := parseGrant(in)
		if !ok {
			add(in.st.Line, "grant-app", "无法解析的 GRANT 语句：%.80s", in.st.Text)
			continue
		}
		if g.trailing != "" {
			add(g.line, "grant-app", "授权不得带 %s 子句（迁移只能授予固定的表级权限，不得转授或附加选项）", g.trailing)
			continue
		}
		if len(g.users) != 1 {
			add(g.line, "grant-app", "每条 GRANT 只能授权一个账号（实际 %d 个：%s）；请拆成各自一条语句", len(g.users), strings.Join(g.users, ", "))
			continue
		}
		g.user = g.users[0]
		switch g.user {
		case AppUser, MaintUser:
		default:
			add(g.line, "grant-app", "迁移只允许向 %s / %s 授权（实际：%s）", AppUser, MaintUser, g.user)
			continue
		}
		rule := "grant-app"
		if g.user == MaintUser {
			rule = "grant-maint"
		}
		if !g.scoped {
			add(g.line, rule, "授权必须是表级且不带库名（`GRANT ... ON <表> TO %s`；库名由连接决定，禁止 ON db.* / *.* / 列级授权）", g.user)
			continue
		}
		sort.Strings(g.privs)
		got := strings.Join(g.privs, ",")
		if g.user == AppUser {
			want := "DELETE,INSERT,SELECT,UPDATE"
			desc := "SELECT, INSERT, UPDATE, DELETE"
			if appendOnlyTables[g.table] {
				want, desc = "INSERT,SELECT", "SELECT, INSERT（追加只写表，database/README §5.1）"
			}
			if got != want {
				add(g.line, rule, "表 %s 对 %s 的授权必须恰为 %s（实际：%s）", g.table, AppUser, desc, strings.ReplaceAll(got, ",", ", "))
			}
			if _, known := checker.tables[g.table]; !known {
				add(g.line, rule, "授权目标表 %s 尚未创建", g.table)
			}
			if ci, ok := created[g.table]; ok && ci > i {
				add(g.line, rule, "对表 %s 的授权必须出现在其 CREATE TABLE 之后", g.table)
			}
			appGranted[g.table] = i
		} else {
			if got != "ALTER,DROP,SELECT" {
				add(g.line, rule, "表 %s 对 %s 的授权必须恰为 SELECT, ALTER, DROP（实际：%s）", g.table, MaintUser, strings.ReplaceAll(got, ",", ", "))
			}
			if tm := checker.tables[g.table]; tm == nil || !tm.partitioned {
				add(g.line, rule, "%s 只授权分区表（database/README §5.1）；%s 不是分区表", MaintUser, g.table)
			}
			maintGranted[g.table] = i
		}
	}
	for _, tbl := range order {
		if _, ok := appGranted[tbl]; !ok {
			add(up[created[tbl]].st.Line, "grant-app", "新表 %s 缺少逐表授权：库级授权无法在表级收窄，迁移必须在建表后执行 `GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO %s`（追加只写表为 SELECT, INSERT；Story 1.5 实测）", tbl, tbl, AppUser)
		}
		if tm := checker.tables[tbl]; tm != nil && tm.partitioned {
			if _, ok := maintGranted[tbl]; !ok {
				add(up[created[tbl]].st.Line, "grant-maint", "分区表 %s 缺少 `GRANT SELECT, ALTER, DROP ON %s TO %s`（保留期清理与滚动预建分区所需，ADR-016 §6）", tbl, tbl, MaintUser)
			}
		}
	}
	return out
}
