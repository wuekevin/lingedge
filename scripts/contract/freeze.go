package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// 冻结前置复核（Story 1.3 AC7、AC9）：只读校验，任一项不成立即返回违规，调用方据此拒绝打 contract-v1。

// protoRequiredSymbols 是修订后协议必须包含的符号（AC7b），以「定义形态」匹配（剥离注释后）：消息/RPC 的声明、字段的声明；
// 仅在注释或其它字段名里出现不算。
var protoRequiredSymbols = []struct {
	Name string
	Re   *regexp.Regexp
}{
	{"NodeRollback", regexp.MustCompile(`(?m)^\s*message\s+NodeRollback\s*\{`)},
	{"held_seq", regexp.MustCompile(`(?m)^\s*(?:repeated\s+|optional\s+)?[\w.]+\s+held_seq\s*=\s*\d+`)},
	{"GetCaBundle", regexp.MustCompile(`(?m)^\s*rpc\s+GetCaBundle\s*\(`)},
	{"lineage", regexp.MustCompile(`(?m)^\s*(?:repeated\s+|optional\s+)?[\w.]+\s+lineage\s*=\s*\d+`)},
}

var (
	protoBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	protoLineComment  = regexp.MustCompile(`(?m)//.*$`)
)

func stripProtoComments(s string) string {
	return protoLineComment.ReplaceAllString(protoBlockComment.ReplaceAllString(s, ""), "")
}

// internalPeriodicJobs 是「内部周期作业」清单（AA-8 方案二）：它们不是 TaskType，不对外暴露、不可经 API 入队。
var internalPeriodicJobs = []string{"tasks.reap", "idempotency.cleanup", "sessions.cleanup", "partition.maintain"}

// checkProtoSymbols 确认 6 个 .proto 存在且含必需符号。
func checkProtoSymbols(protoDir string) []finding {
	var fs []finding
	files, _ := filepath.Glob(filepath.Join(protoDir, "*.proto"))
	if len(files) != 6 {
		fs = append(fs, finding{"proto", protoDir, fmt.Sprintf("应有 6 个 .proto，实际 %d 个", len(files))})
	}
	var all strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fs = append(fs, finding{"proto", f, err.Error()})
			continue
		}
		all.WriteString(stripProtoComments(string(b)))
		all.WriteByte('\n')
	}
	for _, sym := range protoRequiredSymbols {
		if !sym.Re.MatchString(all.String()) {
			fs = append(fs, finding{"proto", sym.Name, "6 个 .proto 中缺少必需符号的定义（注释中提及不算）"})
		}
	}
	return fs
}

// checkM1Operations：M1 operation 数量（AC7a：200 个）。
func checkM1Operations(doc *openapi3.T, want int) []finding {
	n := 0
	for _, o := range operations(doc) {
		if p, _ := extString(o.Op, "x-phase"); p == "M1" {
			n++
		}
	}
	if n != want {
		return []finding{{"m1-ops", "x-phase: M1", fmt.Sprintf("M1 operation 应为 %d 个，实际 %d 个", want, n)}}
	}
	return nil
}

var (
	readmeRowRe  = regexp.MustCompile("^\\|\\s*(\\d{6})\\s*\\|\\s*`([a-z0-9_]+)`\\s*\\|(.*)$")
	skelHeaderRe = regexp.MustCompile(`^-- ==== (\d{6}) ([a-z0-9_]+)`)
	createRe     = regexp.MustCompile("(?i)^\\s*CREATE TABLE\\s+(?:IF NOT EXISTS\\s+)?`?([a-z0-9_]+)`?")
	identRe      = regexp.MustCompile("[a-z][a-z0-9_]*")
)

type migSection struct {
	Num    string
	Name   string
	Tables []string
}

var parenRe = regexp.MustCompile(`（[^）]*）|\([^)]*\)`)

// tableList 取「内容」列中真正建表的部分：在首个「；」处截断（其后是 ALTER 追加外键等说明），
// 并去掉括号内的补充说明（其中的标识符多是被引用的外部表，如 `→certificates`）。
func tableList(content string) string {
	if i := strings.Index(content, "；"); i >= 0 {
		content = content[:i]
	}
	return parenRe.ReplaceAllString(content, "")
}

// parseReadmeMigrations 解析 database/README §8.1 的迁移表；content 为「内容」列原文。
func parseReadmeMigrations(readme string) (rows []migSection, content map[string]string) {
	content = map[string]string{}
	in := false
	for _, line := range strings.Split(readme, "\n") {
		if strings.HasPrefix(line, "### ") {
			in = strings.HasPrefix(line, "### 8.1")
			continue
		}
		if !in {
			continue
		}
		if m := readmeRowRe.FindStringSubmatch(line); m != nil {
			rows = append(rows, migSection{Num: m[1], Name: m[2]})
			cells := strings.Split(m[3], "|")
			content[m[1]] = tableList(cells[0])
		}
	}
	return rows, content
}

// parseSkeleton 解析骨架 SQL：按「-- ==== NNNNNN name」分节，节内 CREATE TABLE 归属该节；
// 遇到「后续阶段」标记后的建表计入 extra（不属 M1 迁移）。
func parseSkeleton(sql string) (secs []migSection, extra []string) {
	cur := -1
	later := false
	for _, line := range strings.Split(sql, "\n") {
		if m := skelHeaderRe.FindStringSubmatch(line); m != nil {
			secs = append(secs, migSection{Num: m[1], Name: m[2]})
			cur = len(secs) - 1
			continue
		}
		if strings.HasPrefix(line, "-- ==== 后续阶段") {
			later = true
			continue
		}
		if m := createRe.FindStringSubmatch(line); m != nil {
			switch {
			case later:
				extra = append(extra, m[1])
			case cur >= 0:
				secs[cur].Tables = append(secs[cur].Tables, m[1])
			}
		}
	}
	return secs, extra
}

// checkMigrationOrder：README §8.1 的迁移顺序、命名与各迁移所含表，必须与骨架 SQL 一致（AC7c）。
// 000004 仅含种子数据（无 DDL），骨架中无对应分节，属允许的缺口。
func checkMigrationOrder(readme, skeleton string, wantM1Tables int) []finding {
	var fs []finding
	rows, content := parseReadmeMigrations(readme)
	secs, _ := parseSkeleton(skeleton)
	if len(rows) != 13 {
		fs = append(fs, finding{"db-order", "README §8.1", fmt.Sprintf("应有 13 个迁移文件，实际 %d 个", len(rows))})
	}
	for i, r := range rows {
		if want := fmt.Sprintf("%06d", i+1); r.Num != want {
			fs = append(fs, finding{"db-order", "README §8.1", fmt.Sprintf("第 %d 行序号应为 %s，实际 %s", i+1, want, r.Num)})
		}
	}
	byNum := map[string]migSection{}
	for _, s := range secs {
		byNum[s.Num] = s
	}
	// 骨架分节顺序须递增。
	prev := ""
	for _, s := range secs {
		if s.Num <= prev {
			fs = append(fs, finding{"db-order", "m1-fk-skeleton.sql", fmt.Sprintf("分节 %s 未按序号递增", s.Num)})
		}
		prev = s.Num
	}
	// 全部骨架表的归属表。
	owner := map[string]string{}
	total := 0
	for _, s := range secs {
		for _, t := range s.Tables {
			owner[t] = s.Num
			total++
		}
	}
	if total != wantM1Tables {
		fs = append(fs, finding{"db-order", "m1-fk-skeleton.sql", fmt.Sprintf("M1 建表应为 %d 张，实际 %d 张", wantM1Tables, total)})
	}
	for _, r := range rows {
		sec, has := byNum[r.Num]
		if !has {
			// 无 DDL 的种子迁移：「内容」列不得出现任何骨架表名。
			for _, id := range identRe.FindAllString(content[r.Num], -1) {
				if o, isTable := owner[id]; isTable {
					fs = append(fs, finding{"db-order", r.Num + " " + r.Name, fmt.Sprintf("README 称其含表 %s，但骨架中该表属于 %s", id, o)})
				}
			}
			continue
		}
		if sec.Name != r.Name {
			fs = append(fs, finding{"db-order", r.Num, fmt.Sprintf("README 命名 %q 与骨架 %q 不一致", r.Name, sec.Name)})
		}
		// README「内容」列提到的、且在骨架中确有建表的标识符，归属必须等于本行。
		mentioned := map[string]bool{}
		for _, id := range identRe.FindAllString(content[r.Num], -1) {
			if o, isTable := owner[id]; isTable {
				mentioned[id] = true
				if o != r.Num {
					fs = append(fs, finding{"db-order", r.Num + " " + r.Name, fmt.Sprintf("README 称其含表 %s，但骨架中该表属于 %s", id, o)})
				}
			}
		}
		for _, t := range sec.Tables {
			if !mentioned[t] {
				fs = append(fs, finding{"db-order", r.Num + " " + r.Name, fmt.Sprintf("骨架建表 %s 未出现在 README §8.1 该行的内容列", t)})
			}
		}
	}
	return fs
}

// envPending 描述修订日志 §4 的一项「待环境验证」及其登记位置（AC7d）。
type envPending struct {
	Item    string
	Stories []string // epics.md 中的 Story 编号
	Needles []string // 该 Story 小节须同时包含
}

var envPendingItems = []envPending{
	{"① MySQL 8.0.16 最低版本空库迁移", []string{"1.6", "1.9"}, []string{"8.0.16", "等待环境验证"}},
	{"② PoC-5：PCRE JIT 与解释执行下 lua_regex_match_limit", []string{"3.24"}, []string{"PoC-5", "等待环境验证"}},
	{"③ PoC-3：lua_shared_dict 容量变化 reload 是否新建 zone", []string{"3.23"}, []string{"PoC-3", "等待环境验证"}},
	{"④ Agent 单元内 openresty -t 与生产启动一致", []string{"1.32", "3.22"}, []string{"openresty -t", "等待环境验证"}},
	{"⑤ PoC-1：ngx.var.le_* 跨 ngx.exec 保留与 access 不重入", []string{"3.23"}, []string{"PoC-1", "等待环境验证"}},
}

// storySection 返回 epics.md 中「### Story N：…」小节全文。
func storySection(epics, num string) string {
	start := strings.Index(epics, "### Story "+num+"：")
	if start < 0 {
		return ""
	}
	rest := epics[start+1:]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		return rest[:end]
	}
	return rest
}

func checkEnvPendingRegistered(epics string) []finding {
	var fs []finding
	for _, it := range envPendingItems {
		for _, st := range it.Stories {
			sec := storySection(epics, st)
			if sec == "" {
				fs = append(fs, finding{"env-pending", it.Item, "epics.md 中找不到 Story " + st})
				continue
			}
			for _, n := range it.Needles {
				if !strings.Contains(sec, n) {
					fs = append(fs, finding{"env-pending", it.Item, fmt.Sprintf("Story %s 未登记（缺少 %q）", st, n)})
				}
			}
		}
	}
	return fs
}

// checkCF1Record：评审记录 CF-1.md 存在且引用修订日志（AC5、AC7e）。
func checkCF1Record(path string) []finding {
	b, err := os.ReadFile(path)
	if err != nil {
		return []finding{{"cf1-record", path, "评审记录不存在: " + err.Error()}}
	}
	if !strings.Contains(string(b), "architecture-revision-log.md") {
		return []finding{{"cf1-record", path, "评审记录未引用 architecture-revision-log.md"}}
	}
	return nil
}

// taskTypeEnum 返回 components.schemas.TaskType 的枚举值。
func taskTypeEnum(doc *openapi3.T) []string {
	if doc.Components == nil {
		return nil
	}
	ref := doc.Components.Schemas["TaskType"]
	if ref == nil || ref.Value == nil {
		return nil
	}
	var out []string
	for _, v := range ref.Value.Enum {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// checkTaskTypeScope（AA-8、AC9 方案二）：TaskType 恰为 wantCount 项且不含内部周期作业；
// 文档中凡提到内部周期作业名的行，须同时标注「内部周期作业」，使 1.14 的「注册表 ⊆ TaskType」检查范围无歧义。
// docs 为 路径→内容。
func checkTaskTypeScope(doc *openapi3.T, wantCount int, docs map[string]string) []finding {
	var fs []finding
	enum := taskTypeEnum(doc)
	if len(enum) != wantCount {
		fs = append(fs, finding{"task-type", "TaskType", fmt.Sprintf("TaskType 应为 %d 项，实际 %d 项", wantCount, len(enum))})
	}
	set := map[string]bool{}
	for _, v := range enum {
		set[v] = true
	}
	for _, job := range internalPeriodicJobs {
		if set[job] {
			fs = append(fs, finding{"task-type", job, "内部周期作业不得出现在 OpenAPI TaskType（AA-8 方案二）"})
		}
	}
	paths := make([]string, 0, len(docs))
	for p := range docs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for i, line := range strings.Split(docs[p], "\n") {
			for _, job := range internalPeriodicJobs {
				if strings.Contains(line, job) && !strings.Contains(line, "内部周期作业") {
					fs = append(fs, finding{"task-type", fmt.Sprintf("%s:%d", p, i+1), fmt.Sprintf("提到内部周期作业 %s 但未标注「内部周期作业」", job)})
				}
			}
		}
	}
	return fs
}
