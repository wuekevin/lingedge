package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// change 是一条被判定为破坏性的契约变更。
type change struct {
	Source string // oasdiff | contract
	ID     string
	Where  string
	Text   string
}

func (c change) String() string {
	return fmt.Sprintf("[%s:%s] %s: %s", c.Source, c.ID, c.Where, c.Text)
}

// oasdiffEntry 对应 `oasdiff breaking -f json` 的元素（仅取用到的字段）。
type oasdiffEntry struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	Level       int    `json:"level"`
	Operation   string `json:"operation"`
	Path        string `json:"path"`
	OperationID string `json:"operationId"`
}

// 级别：oasdiff 的 info=1、warn=2、error=3；`oasdiff breaking` 只输出 warn 与 error，二者都视为破坏性
// （warn 含「删除请求参数/属性」「正则收紧」等，对已冻结契约同样不可无说明地发生；等价于 `--fail-on WARN`）。
const oasdiffBreakingLevel = 2

// runOasdiff 执行 `oasdiff breaking base cur`（argv 由 Makefile 固定版本后传入，不经 shell）。
// 返回 warn 及以上级别的变更。若输出不是 JSON 数组则视为工具失败（例如文件无法加载）。
func runOasdiff(argv []string, base, cur, severityFile string) (breaking []change, err error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("未指定 oasdiff 命令")
	}
	args := append(append([]string{}, argv[1:]...), "breaking", base, cur, "-f", "json")
	if severityFile != "" {
		args = append(args, "--severity-levels", severityFile)
	}
	cmd := exec.Command(argv[0], args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run() // 退出码在 `go run` 下被折叠，改以 JSON 能否解析判定成败
	var entries []oasdiffEntry
	if jerr := json.Unmarshal(stdout.Bytes(), &entries); jerr != nil {
		return nil, fmt.Errorf("oasdiff 未返回 JSON（工具失败）: %s%s", strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()))
	}
	for _, e := range entries {
		if e.Level >= oasdiffBreakingLevel {
			breaking = append(breaking, change{"oasdiff", e.ID, strings.TrimSpace(e.Operation + " " + e.Path), e.Text})
		}
	}
	return breaking, nil
}

// operationKey 以 operationId 为键保存 operation 的契约元数据，用于 oasdiff 未覆盖的 x- 扩展比较。
type opMeta struct {
	Where string
	Shape string // 方法 + 路径模板（参数名归一，仅改参数名不算移动）
	Perms []string
	Scope string
	Reau  bool
	Audit bool
	Phase string
}

var pathParamRe = regexp.MustCompile(`\{[^}]*\}`)

func opMetas(doc *openapi3.T) map[string]opMeta {
	out := map[string]opMeta{}
	for _, o := range operations(doc) {
		m := opMeta{Where: o.String(), Shape: o.Method + " " + pathParamRe.ReplaceAllString(o.Path, "{}")}
		if v, ok := o.Op.Extensions["x-permissions"].([]any); ok {
			for _, p := range v {
				if s, ok := p.(string); ok {
					m.Perms = append(m.Perms, s)
				}
			}
		}
		sort.Strings(m.Perms)
		m.Scope, _ = extString(o.Op, "x-tenant-scope")
		m.Reau, _ = extBool(o.Op, "x-reauth")
		m.Audit, _ = extBool(o.Op, "x-audit")
		m.Phase, _ = extString(o.Op, "x-phase")
		out[o.Op.OperationID] = m
	}
	return out
}

// contractChanges 比较 operationId 与 x- 扩展：oasdiff 不跟踪这些项，但它们是契约的一部分（FR-20、IMP-12、ADR-009）。
// 破坏性定义：operationId 删除/改名（含路径模板或方法变更，仅改路径参数名不算）、x-permissions 新增要求、x-tenant-scope 改变、
// x-reauth 增删、x-audit 由 true 变 false、x-phase 变更。
func contractChanges(base, cur *openapi3.T) []change {
	var out []change
	bm, cm := opMetas(base), opMetas(cur)
	ids := make([]string, 0, len(bm))
	for id := range bm {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b := bm[id]
		c, ok := cm[id]
		if !ok {
			out = append(out, change{"contract", "operation-id-removed", b.Where, fmt.Sprintf("operationId %q 已删除或改名", id)})
			continue
		}
		if b.Shape != c.Shape {
			out = append(out, change{"contract", "operation-moved", b.Where, fmt.Sprintf("operationId %q 的路径/方法改为 %s", id, c.Where)})
		}
		_, newPerms := diffSets(b.Perms, c.Perms) // extra = 现有契约比基线多出的权限
		if len(newPerms) > 0 {
			out = append(out, change{"contract", "permission-added", c.Where, fmt.Sprintf("x-permissions 新增必需权限 %v（收紧授权）", newPerms)})
		}
		if b.Scope != c.Scope {
			out = append(out, change{"contract", "tenant-scope-changed", c.Where, fmt.Sprintf("x-tenant-scope %q → %q", b.Scope, c.Scope)})
		}
		if !b.Reau && c.Reau {
			out = append(out, change{"contract", "reauth-added", c.Where, "新增 x-reauth: true（收紧再认证要求）"})
		}
		if b.Reau && !c.Reau {
			out = append(out, change{"contract", "reauth-removed", c.Where, "取消 x-reauth: true（降低 IMP-12 的再认证要求，须 ADR）"})
		}
		if b.Audit && !c.Audit {
			out = append(out, change{"contract", "audit-removed", c.Where, "x-audit 由 true 改为 false（审计要求被取消）"})
		}
		if b.Phase != c.Phase {
			out = append(out, change{"contract", "phase-changed", c.Where, fmt.Sprintf("x-phase %q → %q（交付阶段变更）", b.Phase, c.Phase)})
		}
	}
	return out
}

var (
	codeCellRe = regexp.MustCompile("`([a-z][a-z0-9_]*(?:\\.[a-z0-9_]+)+)`")
	httpCellRe = regexp.MustCompile(`^\s*(\d{3}|—|-)\s*$`)
)

// parseErrorCodes 从 docs/api/README.md「错误码索引」表提取 code → HTTP 状态（「—」表示无固定 HTTP）。
func parseErrorCodes(readme string) map[string]string {
	out := map[string]string{}
	in := false
	for _, line := range strings.Split(readme, "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.Contains(line, "错误码索引")
			continue
		}
		if !in || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 || !httpCellRe.MatchString(cells[1]) {
			continue
		}
		status := strings.TrimSpace(cells[1])
		for _, m := range codeCellRe.FindAllStringSubmatch(cells[0], -1) {
			out[m[1]] = status
		}
	}
	return out
}

// errorCodeChanges：错误码被删除，或其 HTTP 状态改变，均为破坏性（AC2「改错误码」）；新增错误码为向后兼容。
func errorCodeChanges(baseReadme, curReadme string) []change {
	b, c := parseErrorCodes(baseReadme), parseErrorCodes(curReadme)
	codes := make([]string, 0, len(b))
	for k := range b {
		codes = append(codes, k)
	}
	sort.Strings(codes)
	var out []change
	for _, code := range codes {
		cs, ok := c[code]
		switch {
		case !ok:
			out = append(out, change{"contract", "error-code-removed", code, "错误码已删除或改名（docs/api/README.md 错误码索引）"})
		case cs != b[code]:
			out = append(out, change{"contract", "error-code-status-changed", code, fmt.Sprintf("HTTP 状态 %s → %s", b[code], cs)})
		}
	}
	return out
}

// gitShow 读取某个 ref 下的文件内容；ref 或文件不存在返回 (nil, false, nil)。
func gitShow(ref, path string) ([]byte, bool, error) {
	if err := exec.Command("git", "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run(); err != nil {
		return nil, false, fmt.Errorf("基线 %q 不存在（CI 需 fetch-depth: 0 并拉取标签；contract-v1 由 Story 1.3 创建）", ref)
	}
	out, err := exec.Command("git", "show", ref+":"+path).Output()
	if err != nil {
		return nil, false, nil
	}
	return out, true, nil
}

// 变更说明（docs/bmad/contract-changes/_template.md）：破坏性变更须有 ADR/变更说明才可放行（AC2、ADR-017 §10）。
var (
	noteBreakRe = regexp.MustCompile(`(?m)^- \*\*是否破坏\*\*：\s*(是|否)`)
	noteADRRe   = regexp.MustCompile(`(?m)^- \*\*关联 ADR\*\*：\s*(ADR-\d{3}\S*)`)
	noteScopeRe = regexp.MustCompile(`(?m)^- \*\*影响范围\*\*：\s*(\S.*)$`)
)

// noteDir 是变更说明唯一允许的目录；模板本身不能作为说明。
const noteDir = "docs/bmad/contract-changes/"

// validateChangeNote 校验变更说明足以放行破坏性变更：位于 docs/bmad/contract-changes/（且不是模板/CF 评审记录）、
// 影响范围已填、声明破坏、关联具体 ADR 编号、无未替换的占位符，并且逐项列出本次检出的全部变更 ID（说明与变更绑定，
// 旧说明不能顺带放行新的破坏）。changes 为本次检出的变更；baseHasNote 返回该说明在基线中是否已存在（已存在=旧说明，不得复用）。
func validateChangeNote(path string, changes []change, baseHasNote func(rel string) bool) error {
	rel := filepath.ToSlash(path)
	if i := strings.Index(rel, noteDir); i >= 0 {
		rel = rel[i:]
	}
	if !strings.HasPrefix(rel, noteDir) || strings.HasSuffix(rel, "/_template.md") || strings.HasSuffix(rel, "/CF-1.md") {
		return fmt.Errorf("变更说明 %s 必须位于 %s 下，且不能是模板或 CF 评审记录", path, noteDir)
	}
	if baseHasNote != nil && baseHasNote(rel) {
		return fmt.Errorf("变更说明 %s 在基线中已存在（旧说明不能放行新的破坏性变更，请新建说明）", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("变更说明 %s 无法读取: %w", path, err)
	}
	s := string(b)
	if strings.Contains(s, "<待填>") || strings.Contains(s, "TODO") {
		return fmt.Errorf("变更说明 %s 含未替换的占位符（<待填>/TODO）", path)
	}
	m := noteBreakRe.FindStringSubmatch(s)
	if m == nil || m[1] != "是" {
		return fmt.Errorf("变更说明 %s 未声明「是否破坏：是」，不能放行破坏性变更", path)
	}
	if !noteADRRe.MatchString(s) {
		return fmt.Errorf("变更说明 %s 缺少「关联 ADR：ADR-NNN」", path)
	}
	if !noteScopeRe.MatchString(s) {
		return fmt.Errorf("变更说明 %s 缺少「影响范围」", path)
	}
	missing := map[string]bool{}
	for _, c := range changes {
		if !strings.Contains(s, c.ID) {
			missing[c.ID] = true
		}
	}
	if len(missing) > 0 {
		ids := make([]string, 0, len(missing))
		for id := range missing {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return fmt.Errorf("变更说明 %s 未逐项列出本次检出的变更类型 %v（须在说明中写出这些 ID）", path, ids)
	}
	return nil
}
