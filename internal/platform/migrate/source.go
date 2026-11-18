package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Class 是迁移分类（ADR-013 §1、ADR-016 §3、PRD R-3/NFR-23），由文件第一行 `-- lingedge:class=<值>` 标注。
type Class string

const (
	// ClassReversible：附经测试的 Down，二进制与数据库可一起回退。
	ClassReversible Class = "reversible"
	// ClassExpand：只增不破（新表/列/索引），旧二进制仍可运行；升级失败的即时回滚可只回退二进制。
	ClassExpand Class = "expand"
	// ClassIrreversible：破坏性变更或数据转换；升级前强制备份并要求显式确认，回滚 = 恢复备份 + 旧二进制。
	ClassIrreversible Class = "irreversible"
)

func (c Class) valid() bool {
	return c == ClassReversible || c == ClassExpand || c == ClassIrreversible
}

// Statement 是迁移中的一条可执行语句。
type Statement struct {
	Text  string // 原文（含前导注释，与 goose 交给 MySQL 的内容一致）
	Line  int    // 起始行号（1 起）
	Lead  string // 首个有效词之前的注释（lingedge:guard 标注从这里取）
	Block bool   // 来自 `-- +goose StatementBegin/End` 块（可含语句内分号，如触发器体）

	// goose 与 MySQL 会「看到不同东西」的写法（见 splitStatements），由 lint 报告。
	SameLine    bool // 与上一条语句写在同一行
	ExecComment bool // 含 /*! … */ 可执行注释
	SplitRisk   bool // 引号内某行以分号结尾
}

// Migration 是一个已解析的迁移文件。
type Migration struct {
	Version int64  // 由文件名六位序号得到
	Name    string // 文件名中序号之后的部分（snake_case）
	File    string // 文件名
	Class   Class
	// 文件头可选标注：lingedge:summary / lingedge:contract-after / lingedge:reason。
	Summary       string
	ContractAfter string
	Reason        string
	Checksum      string // 文件原始字节的 SHA-256（十六进制），与 migrations/CHECKSUMS 同口径

	Up, Down      []Statement
	NoTransaction bool
	// HasDown 表示存在「可执行」的回退语句；irreversible 的 `SELECT 'irreversible'` 标记不算。
	HasDown bool
	// DownMarker 表示 Down 段只含 `SELECT 'irreversible'` 标记。
	DownMarker     bool
	hasUp, hasDown bool
}

// Finding 是一条 lint 发现。
type Finding struct {
	File    string
	Line    int // 0 表示整个文件/集合
	Rule    string
	Message string
}

func (f Finding) String() string {
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc == "" {
		loc = "migrations"
	}
	return fmt.Sprintf("%s: [%s] %s", loc, f.Rule, f.Message)
}

var (
	fileNameRe = regexp.MustCompile(`^(\d{6})_([a-z][a-z0-9_]*)\.sql$`)
	gooseRe    = regexp.MustCompile(`(?i)^--\s*\+goose\s+(.+?)\s*$`)
	classRe    = regexp.MustCompile(`^--\s*lingedge:class=(\S*)\s*$`)
	attrRe     = regexp.MustCompile(`^--\s*lingedge:(summary|contract-after|reason)=(.*?)\s*$`)
)

// ParseFile 解析一个迁移文件，返回迁移与解析/头部层面的发现。name 为文件名（不含目录）。
// 解析与 goose 的 SQL 语义保持一致（Up/Down 段、StatementBegin/End、NO TRANSACTION），使 lint 看到的语句就是执行的语句。
func ParseFile(name string, data []byte) (*Migration, []Finding) {
	var out []Finding
	add := func(line int, rule, format string, a ...any) {
		out = append(out, Finding{File: name, Line: line, Rule: rule, Message: fmt.Sprintf(format, a...)})
	}
	m := &Migration{File: name}
	sum := sha256.Sum256(data)
	m.Checksum = hex.EncodeToString(sum[:])

	if sm := fileNameRe.FindStringSubmatch(name); sm == nil {
		add(0, "name", "文件名必须是 NNNNNN_<snake_name>.sql（六位序号 + 小写蛇形名称，ADR-016 §2）：%q", name)
	} else {
		v, _ := strconv.ParseInt(sm[1], 10, 64)
		if v == 0 {
			add(0, "name", "序号不得为 000000（goose 的 0 号版本保留）")
		}
		m.Version, m.Name = v, sm[2]
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	// 文件头：第一行必须是 class 标注（ADR-016 §3）。
	if len(lines) == 0 || !classRe.MatchString(strings.TrimRight(lines[0], " \t")) {
		add(1, "header", "第一行必须是 `-- lingedge:class=reversible|expand|irreversible`（缺少或写错分类标注；注意是 `class=<值>`，不是 `class <值>`，且不得有前导空白）")
	}

	type section int
	const (
		none section = iota
		up
		down
	)
	cur := none
	inBlock := false
	var blockLines []string
	blockLine := 0
	// 每段的非块内容按行累积，遇到块或段切换时 flush 切分。
	var pending []string
	pendingLine := 0
	flush := func(sec section) {
		if len(pending) == 0 {
			return
		}
		stmts, rest := splitStatements(strings.Join(pending, "\n"), pendingLine)
		if rest != "" {
			add(pendingLine, "syntax", "语句缺少结尾分号（goose 将拒绝执行）：%.60s", rest)
		}
		switch sec {
		case up:
			m.Up = append(m.Up, stmts...)
		case down:
			m.Down = append(m.Down, stmts...)
		default:
			if len(stmts) > 0 {
				add(stmts[0].Line, "goose-annotation", "语句出现在 `-- +goose Up` 之前")
			}
		}
		pending = nil
	}

	var downComments []string
	for i, ln := range lines {
		no := i + 1
		trim := strings.TrimSpace(ln)
		if g := gooseRe.FindStringSubmatch(trim); g != nil {
			dir := strings.ToUpper(strings.Join(strings.Fields(g[1]), " "))
			switch dir {
			case "UP":
				if inBlock {
					add(no, "goose-annotation", "StatementBegin 未用 StatementEnd 结束")
					inBlock = false
				}
				flush(cur)
				if m.hasUp {
					add(no, "goose-annotation", "重复的 `-- +goose Up`")
				}
				m.hasUp, cur = true, up
			case "DOWN":
				if inBlock {
					add(no, "goose-annotation", "StatementBegin 未用 StatementEnd 结束")
					inBlock = false
				}
				flush(cur)
				if m.hasDown {
					add(no, "goose-annotation", "重复的 `-- +goose Down`")
				}
				m.hasDown, cur = true, down
			case "STATEMENTBEGIN":
				if cur == none || inBlock {
					add(no, "goose-annotation", "StatementBegin 位置不合法")
					continue
				}
				flush(cur)
				inBlock, blockLines, blockLine = true, nil, no+1
			case "STATEMENTEND":
				if !inBlock {
					add(no, "goose-annotation", "StatementEnd 没有对应的 StatementBegin")
					continue
				}
				inBlock = false
				body := strings.TrimSpace(strings.Join(blockLines, "\n"))
				if body != "" {
					st := newStatement(body, blockLine)
					st.Block = true
					if cur == up {
						m.Up = append(m.Up, st)
					} else {
						m.Down = append(m.Down, st)
					}
				}
			case "NO TRANSACTION":
				m.NoTransaction = true
			default:
				add(no, "goose-annotation", "不支持的 goose 指令 `-- +goose %s`（含 ENVSUB：迁移内容不得受环境变量影响）", g[1])
			}
			continue
		}
		if cur == none {
			if c := classRe.FindStringSubmatch(trim); c != nil {
				if no != 1 {
					add(no, "header", "class 标注必须在第一行")
				}
				m.Class = Class(c[1])
				if !m.Class.valid() {
					add(no, "header", "未知分类 %q，取值只能是 reversible|expand|irreversible", c[1])
				}
			} else if a := attrRe.FindStringSubmatch(trim); a != nil {
				switch a[1] {
				case "summary":
					m.Summary = a[2]
				case "contract-after":
					m.ContractAfter = a[2]
				case "reason":
					m.Reason = a[2]
				}
			}
			if strings.HasPrefix(trim, "--") || trim == "" {
				continue
			}
			// 注释/空行之外的内容在 Up 之前：走 flush(none) 报告。
			if len(pending) == 0 {
				pendingLine = no
			}
			pending = append(pending, ln)
			continue
		}
		if inBlock {
			blockLines = append(blockLines, ln)
			continue
		}
		if len(pending) == 0 {
			pendingLine = no
		}
		pending = append(pending, ln)
		if cur == down {
			if a := attrRe.FindStringSubmatch(trim); a != nil && a[1] == "reason" {
				downComments = append(downComments, a[2])
			}
		}
	}
	if inBlock {
		add(blockLine, "goose-annotation", "StatementBegin 未用 StatementEnd 结束")
	}
	flush(cur)
	flush(none)

	if !m.hasUp {
		add(0, "goose-annotation", "缺少 `-- +goose Up`（goose 无法识别该迁移）")
	}
	if m.hasUp && len(m.Up) == 0 {
		add(0, "syntax", "Up 段没有任何可执行语句")
	}
	if len(downComments) > 0 && m.Reason == "" {
		m.Reason = downComments[0]
	}

	// Down 段：区分「可执行回退」与 irreversible 标记。
	for _, st := range m.Down {
		if isIrreversibleMarker(st) {
			m.DownMarker = true
			continue
		}
		m.HasDown = true
	}
	return m, out
}

// isIrreversibleMarker 判断语句是否恰为 `SELECT 'irreversible'`（大小写不敏感，可带别名无关）。
func isIrreversibleMarker(st Statement) bool {
	t := tokenize(st.Text)
	return len(t) == 2 && t[0].is("SELECT") && t[1].kind == tString && strings.EqualFold(t[1].text, "irreversible")
}

// Set 是一组按序号排序的迁移。
type Set struct {
	Migrations []*Migration
}

// ByVersion 返回指定序号的迁移。
func (s *Set) ByVersion(v int64) *Migration {
	for _, m := range s.Migrations {
		if m.Version == v {
			return m
		}
	}
	return nil
}

// Load 读取 fsys 根目录下全部 *.sql，解析并 lint，返回迁移集合与全部发现（发现为空即通过）。
// 非 .sql 文件（CHECKSUMS、embed.go、.gitkeep 等）被忽略；子目录不被支持（迁移平铺在根目录）。
func Load(fsys fs.FS) (*Set, []Finding, error) {
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: 读取迁移目录: %w", err)
	}
	var all []Finding
	set := &Set{}
	for _, e := range ents {
		if e.IsDir() {
			all = append(all, Finding{File: e.Name(), Rule: "name", Message: "迁移目录不得含子目录（迁移文件平铺在根目录）"})
			continue
		}
		if !strings.HasSuffix(e.Name(), ".sql") {
			switch ext := path.Ext(e.Name()); {
			case strings.EqualFold(ext, ".sql"):
				// 契约工具按不区分大小写把它当迁移登记，而 goose 只认 .sql：必须报出来，否则它被登记为已发布却永远不会执行。
				all = append(all, Finding{File: e.Name(), Rule: "name", Message: "迁移文件扩展名必须是小写 .sql"})
			case ext == ".go" && e.Name() != "embed.go" && !strings.HasSuffix(e.Name(), "_test.go"):
				all = append(all, Finding{File: e.Name(), Rule: "name", Message: "Go 迁移暂不支持（ADR-016 允许的数据回填 Go 迁移须先扩展框架：注册、幂等与校验和）；请用 SQL 迁移"})
			}
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, nil, fmt.Errorf("migrate: 读取 %s: %w", e.Name(), err)
		}
		m, fnd := ParseFile(e.Name(), data)
		all = append(all, fnd...)
		if fileNameRe.MatchString(e.Name()) {
			set.Migrations = append(set.Migrations, m)
		}
	}
	sort.SliceStable(set.Migrations, func(i, j int) bool { return set.Migrations[i].Version < set.Migrations[j].Version })
	all = append(all, lintSet(set)...)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].File != all[j].File {
			return all[i].File < all[j].File
		}
		return all[i].Line < all[j].Line
	})
	return set, all, nil
}
