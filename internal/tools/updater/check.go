package updater

import (
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// 状态取值。
const (
	StatusUpToDate         = "up_to_date"
	StatusUpgradeAvailable = "upgrade_available"
	StatusBlocked          = "blocked"
	StatusUnreleased       = "unreleased"
	StatusBuiltin          = "builtin"
	StatusNotInManifest    = "not_in_manifest"
	StatusNoCandidate      = "no_candidate"
)

// Migrations 是清单对一次升级的迁移预告（以新二进制 migrate plan 的实际结果为准，取更严格者；升级时的 Plan 会再核对）。
type Migrations struct {
	Classes           []string `json:"classes"`
	HasIrreversible   bool     `json:"has_irreversible"`
	IrreversibleCount int      `json:"irreversible_count"`
	BackupRequired    bool     `json:"backup_required"`
	SchemaFrom        string   `json:"schema_from,omitempty"`
	SchemaTo          string   `json:"schema_to,omitempty"`
}

// Entry 是一个已装组件的检查结果。
type Entry struct {
	Component string `json:"component"`
	Installed string `json:"installed"`
	// Latest 是通道内最高的稳定、未被吊销的版本（没有则为空）。
	Latest string `json:"latest,omitempty"`
	Status string `json:"status"`
	// MinUpgradeFrom 在 blocked 时给出目标版本要求的最低已装版本；UpgradePath 是逐级升级的路径（每一步都满足 min_upgrade_from；无路为空）。
	MinUpgradeFrom string      `json:"min_upgrade_from,omitempty"`
	UpgradePath    []string    `json:"upgrade_path,omitempty"`
	Migrations     *Migrations `json:"migrations,omitempty"`
	NotesURL       string      `json:"notes_url,omitempty"`
	// InstalledRevoked：已装版本已被发行方吊销，建议尽快升级。
	InstalledRevoked bool   `json:"installed_revoked,omitempty"`
	Message          string `json:"message,omitempty"`
}

type cand struct {
	v   manifest.Version
	e   manifest.VersionEntry
	min *manifest.Version
}

// Check 为 state 里的每个已装组件给出结论（按组件名排序）。清单必须来自 manifest.Verifier.Verify（版本已解析、吊销表就绪）。
// 纯函数、只读。
func Check(m *manifest.Manifest, st registry.State) []Entry {
	out := make([]Entry, 0, len(st.Components))
	for _, name := range st.Names() {
		out = append(out, checkOne(m, name, st.Components[name].Version))
	}
	return out
}

func checkOne(m *manifest.Manifest, name, installed string) Entry {
	e := Entry{Component: name, Installed: installed}
	comp, ok := m.Components[name]
	switch {
	case !ok:
		e.Status, e.Message = StatusNotInManifest, "清单里没有该组件"
		return e
	case comp.Status == manifest.CompBuiltin:
		e.Status, e.Message = StatusBuiltin, "内置于 "+comp.BuiltinIn+"，随其升级"
		return e
	case comp.Status == manifest.CompUnreleased:
		e.Status, e.Message = StatusUnreleased, "尚未发布"
		return e
	}
	iv, ierr := manifest.ParseVersion(installed)
	if ierr == nil {
		if reason, rev := m.RevokedVersion(name, iv); rev {
			e.InstalledRevoked = true
			e.Message = "已装版本已被发行方吊销，建议尽快升级" + reasonSuffix(reason)
		}
	}
	var cands []cand
	for i := range comp.Versions {
		v, err := manifest.ParseVersion(comp.Versions[i].Version)
		if err != nil || v.Prerelease() {
			continue
		}
		if _, rev := m.RevokedVersion(name, v); rev {
			continue
		}
		c := cand{v: v, e: comp.Versions[i]}
		if s := comp.Versions[i].MinUpgradeFrom; s != "" {
			if mv, err := manifest.ParseVersion(s); err == nil {
				c.min = &mv
			}
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 || ierr != nil {
		e.Status = StatusNoCandidate
		if e.Message == "" {
			e.Message = "清单里没有可用的稳定版本"
		}
		return e
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].v.Compare(cands[j].v) > 0 })
	top := cands[0]
	e.Latest = top.v.String()
	if top.v.Compare(iv) <= 0 {
		e.Status = StatusUpToDate
		return e
	}
	e.NotesURL = top.e.NotesURL
	e.Migrations = noticeOf(top.e.Migrations)
	if top.min != nil && iv.Compare(*top.min) < 0 {
		e.Status = StatusBlocked
		e.MinUpgradeFrom = top.min.String()
		e.UpgradePath = stepwise(cands, iv, top.v)
		msg := e.Latest + " 要求已装版本 ≥ " + top.min.String() + "，不能直接升级"
		if len(e.UpgradePath) > 0 {
			msg += "；须逐级升级：" + strings.Join(e.UpgradePath, " → ")
			if next := e.UpgradePath[0]; next != e.Latest {
				// 迁移预告改用「下一步」的版本：那才是下一次 update 真正会执行的。
				for _, c := range cands {
					if c.v.String() == next {
						e.Migrations, e.NotesURL = noticeOf(c.e.Migrations), c.e.NotesURL
					}
				}
			}
		}
		e.Message = strings.TrimSpace(e.Message + " " + msg)
		return e
	}
	e.Status = StatusUpgradeAvailable
	return e
}

func reasonSuffix(r string) string {
	if r == "" {
		return ""
	}
	return "（" + r + "）"
}

func noticeOf(n *manifest.MigrationsNotice) *Migrations {
	if n == nil {
		return nil
	}
	classes := append([]string{}, n.Classes...)
	has := n.IrreversibleCount > 0
	for _, c := range classes {
		if c == "irreversible" {
			has = true
		}
	}
	return &Migrations{Classes: classes, HasIrreversible: has, IrreversibleCount: n.IrreversibleCount, BackupRequired: n.BackupRequired || has,
		SchemaFrom: n.SchemaFrom, SchemaTo: n.SchemaTo}
}

// stepwise 给出从 from 逐级升到 target 的路径（每一步都满足 min_upgrade_from；贪心取每步可达的最高版本）；无路返回 nil。
func stepwise(cands []cand, from, target manifest.Version) []string {
	reach := func(c cand, cur manifest.Version) bool { return c.min == nil || cur.Compare(*c.min) >= 0 }
	cur := from
	var path []string
	for i := 0; i <= len(cands); i++ {
		for _, c := range cands {
			if c.v.Compare(target) == 0 && reach(c, cur) {
				return append(path, target.String())
			}
		}
		var best *cand
		for k := range cands {
			c := cands[k]
			if c.v.Compare(cur) > 0 && c.v.Compare(target) < 0 && reach(c, cur) && (best == nil || c.v.Compare(best.v) > 0) {
				best = &cands[k]
			}
		}
		if best == nil {
			return nil
		}
		path = append(path, best.v.String())
		cur = best.v
	}
	return nil
}
