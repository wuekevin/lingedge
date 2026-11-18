package migrate

import (
	"encoding/json"
	"io"
)

// PlanItem 是 `migrate plan` 输出中的一项（AC1：序号、名称、分类、是否含 Down）。
type PlanItem struct {
	Version       int64  `json:"version"`
	Name          string `json:"name"`
	File          string `json:"file"`
	Class         Class  `json:"class"`
	HasDown       bool   `json:"has_down"`
	Checksum      string `json:"checksum"`
	Summary       string `json:"summary,omitempty"`
	ContractAfter string `json:"contract_after,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// Plan 是 `lingedge-server migrate plan` 的 JSON 输出，供 Tools 升级前展示并决定是否强制备份（ADR-013 §2、ADR-016 §7）：
// 「是否含不可逆」以 has_irreversible 为唯一判据。
type Plan struct {
	// Mode：pending = 连库后只列待执行迁移；all = 未连库（离线）时列出二进制内嵌的全部迁移。
	Mode string `json:"mode"`
	// MySQLVersion 与 DBVersion 仅在连库时有值。DBVersion 是数据库已应用的最大序号。
	MySQLVersion string     `json:"mysql_version,omitempty"`
	DBVersion    *int64     `json:"db_version,omitempty"`
	LatestKnown  int64      `json:"latest_version"`
	Count        int        `json:"count"`
	Migrations   []PlanItem `json:"migrations"`
	// HasIrreversible 为 true 时，升级前必须强制备份并要求显式确认（--accept-irreversible）。
	HasIrreversible bool `json:"has_irreversible"`
	// MaxClass 是待执行迁移中最严重的分类（irreversible > reversible > expand），无迁移时为空。
	MaxClass Class `json:"max_class,omitempty"`
}

// BuildPlan 由迁移集合与「已应用序号集合」生成计划；applied 为 nil 表示离线（列出全部）。
func BuildPlan(set *Set, applied map[int64]bool) *Plan {
	p := &Plan{Mode: "all", Migrations: []PlanItem{}}
	if applied != nil {
		p.Mode = "pending"
	}
	for _, m := range set.Migrations {
		p.LatestKnown = m.Version
		if applied != nil && applied[m.Version] {
			continue
		}
		p.Migrations = append(p.Migrations, PlanItem{
			Version: m.Version, Name: m.Name, File: m.File, Class: m.Class, HasDown: m.HasDown, Checksum: m.Checksum,
			Summary: m.Summary, ContractAfter: m.ContractAfter, Reason: m.Reason,
		})
		if m.Class == ClassIrreversible {
			p.HasIrreversible = true
		}
		p.MaxClass = maxClass(p.MaxClass, m.Class)
	}
	p.Count = len(p.Migrations)
	return p
}

func classRank(c Class) int {
	switch c {
	case ClassIrreversible:
		return 3
	case ClassReversible:
		return 2
	case ClassExpand:
		return 1
	}
	return 0
}

func maxClass(a, b Class) Class {
	if classRank(b) > classRank(a) {
		return b
	}
	return a
}

// WriteJSON 以缩进 JSON 写出。
func (p *Plan) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}
