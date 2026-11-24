package doctor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// Level 是检查结果级别。
type Level string

// 级别取值（tools-design §19）。
const (
	LevelPass Level = "pass"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
	LevelSkip Level = "skip"
)

// Valid 报告 l 是否是已知级别。
func (l Level) Valid() bool {
	switch l {
	case LevelPass, LevelWarn, LevelFail, LevelSkip:
		return true
	}
	return false
}

// Finding 是一个检查项的结果（`doctor` 的 data.checks[] 项）。ID 与 Category 稳定、小写；Message/Hint 为中文人类可读文本。
type Finding struct {
	ID        string         `json:"id"`
	Category  string         `json:"category"`
	Level     Level          `json:"level"`
	Message   string         `json:"message"`
	Hint      string         `json:"hint,omitempty"`
	Component string         `json:"component,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
	// Fixed 为 true 表示 --fix-perms 已修复该项（Level 为修复并复检之后的结果）。
	Fixed bool `json:"fixed,omitempty"`
}

// Group 是一组同前缀的检查项。
type Group struct {
	// Name 是注册名（唯一，小写）。
	Name string
	// Category 是 Finding.Category 的缺省值（Run 返回的 Finding 未填时补上）。
	Category string
	// Prefixes 是本组结果 id 允许的前缀（含结尾的点，如 "sys."）；不得与其他组重叠。
	Prefixes []string
	// PerComponent 为 true 表示本组检查项按组件划分：`doctor <组件>` 只运行（并只返回）该组件的结果；为 false 的组在指定组件时不运行。
	PerComponent bool
	// Run 执行检查。filter.Component 非空时仅检查该组件（仅 PerComponent 组会收到）。
	Run func(ctx context.Context, w *World, filter Filter) []Finding
}

// Filter 限定一次运行的范围。
type Filter struct {
	// Component 非空 = 只检查该组件。
	Component string
}

var (
	reGroupName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	rePrefix    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}\.$`)
	reFindingID = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_-]+)+$`)
)

// Registry 是检查项注册表。
type Registry struct {
	groups []Group
}

// New 返回空注册表。
func New() *Registry { return &Registry{} }

// Register 登记一个 Group：名称与前缀必须合规且不与已登记的冲突。
func (r *Registry) Register(g Group) error {
	if !reGroupName.MatchString(g.Name) {
		return fmt.Errorf("doctor: 检查组名 %q 不合法", g.Name)
	}
	if g.Run == nil {
		return fmt.Errorf("doctor: 检查组 %s 没有 Run", g.Name)
	}
	if len(g.Prefixes) == 0 {
		return fmt.Errorf("doctor: 检查组 %s 没有声明 id 前缀", g.Name)
	}
	for _, p := range g.Prefixes {
		if !rePrefix.MatchString(p) {
			return fmt.Errorf("doctor: 检查组 %s 的前缀 %q 不合法（形如 \"sys.\"）", g.Name, p)
		}
	}
	for _, e := range r.groups {
		if e.Name == g.Name {
			return fmt.Errorf("doctor: 检查组 %s 已登记", g.Name)
		}
		for _, p := range g.Prefixes {
			for _, q := range e.Prefixes {
				if strings.HasPrefix(p, q) || strings.HasPrefix(q, p) {
					return fmt.Errorf("doctor: 检查组 %s 的前缀 %s 与已登记的 %s（%s）重叠", g.Name, p, e.Name, q)
				}
			}
		}
	}
	r.groups = append(r.groups, g)
	return nil
}

// MustRegister 同 Register，失败 panic（用于包内常量级的内置登记）。
func (r *Registry) MustRegister(g Group) {
	if err := r.Register(g); err != nil {
		panic(err)
	}
}

// Groups 返回已登记组名（按登记顺序）。
func (r *Registry) Groups() []string {
	out := make([]string, 0, len(r.groups))
	for _, g := range r.groups {
		out = append(out, g.Name)
	}
	return out
}

// Summary 是各级别计数。
type Summary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

// Report 是一次运行的结果。
type Report struct {
	Findings []Finding `json:"checks"`
	Summary  Summary   `json:"summary"`
}

// Failed 报告是否应以退出码 3 结束：有 fail；strict 时 warn 同样算。
func (rp Report) Failed(strict bool) bool {
	return rp.Summary.Fail > 0 || (strict && rp.Summary.Warn > 0)
}

// Run 运行全部（适用的）检查组。结果顺序确定：按登记顺序，组内按 id；每个结果都经过形态校验与脱敏——
// 不合规的结果（id 不在本组前缀内、级别非法、同 id 重复）不会被悄悄丢弃或放行，而是转为一条 doctor.internal.* 的 fail，使缺陷可见。
func (r *Registry) Run(ctx context.Context, w *World, f Filter) Report {
	var all []Finding
	seen := map[string]bool{}
	for _, g := range r.groups {
		if err := ctx.Err(); err != nil {
			break
		}
		if f.Component != "" && !g.PerComponent {
			continue
		}
		fs := runGroup(ctx, g, w, f)
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].ID < fs[j].ID })
		for _, x := range fs {
			x = normalize(g, x)
			if seen[x.ID] {
				x = Finding{ID: "doctor.internal.duplicate_id", Category: "doctor", Level: LevelFail,
					Message: "检查组 " + g.Name + " 产生了重复的检查项 id（实现缺陷）", Hint: "请报告该缺陷", Details: map[string]any{"id": x.ID}}
				if seen[x.ID] {
					continue
				}
			}
			seen[x.ID] = true
			all = append(all, x)
		}
	}
	rp := Report{Findings: all}
	if rp.Findings == nil {
		rp.Findings = []Finding{}
	}
	for _, x := range rp.Findings {
		switch x.Level {
		case LevelPass:
			rp.Summary.Pass++
		case LevelWarn:
			rp.Summary.Warn++
		case LevelFail:
			rp.Summary.Fail++
		case LevelSkip:
			rp.Summary.Skip++
		}
	}
	return rp
}

func runGroup(ctx context.Context, g Group, w *World, f Filter) (out []Finding) {
	defer func() {
		if p := recover(); p != nil {
			out = []Finding{{ID: g.Prefixes[0] + "internal_error", Category: g.Category, Level: LevelFail,
				Message: "检查组 " + g.Name + " 内部错误：" + security.RedactString(fmt.Sprint(p)), Hint: "请报告该缺陷（附 lingedge doctor --json 输出）"}}
		}
	}()
	return g.Run(ctx, w, f)
}

func normalize(g Group, x Finding) Finding {
	bad := ""
	switch {
	case !reFindingID.MatchString(x.ID):
		bad = "id 形态不合法"
	case !hasAnyPrefix(x.ID, g.Prefixes):
		bad = "id 不在本组声明的前缀内"
	case !x.Level.Valid():
		bad = "级别不合法"
	}
	if bad != "" {
		return Finding{ID: "doctor.internal.bad_finding", Category: "doctor", Level: LevelFail,
			Message: "检查组 " + g.Name + " 产生了不合规的结果（" + bad + "，实现缺陷）", Hint: "请报告该缺陷",
			Details: map[string]any{"group": g.Name}}
	}
	if x.Category == "" {
		x.Category = g.Category
	}
	x.Message = security.RedactString(x.Message)
	x.Hint = security.RedactString(x.Hint)
	if x.Details != nil {
		if v, ok := security.RedactValue(x.Details).(map[string]any); ok {
			x.Details = v
		}
	}
	return x
}

func hasAnyPrefix(id string, ps []string) bool {
	for _, p := range ps {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// Default 返回登记了全部内置检查组的注册表。extra 是调用方追加的组（后续 Story 的 node/dns/disk 检查）。
func Default(extra ...Group) (*Registry, error) {
	r := New()
	for _, g := range builtinGroups() {
		if err := r.Register(g); err != nil {
			return nil, err
		}
	}
	for _, g := range extra {
		if err := r.Register(g); err != nil {
			return nil, err
		}
	}
	return r, nil
}
