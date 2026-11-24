package rollback

import (
	"sort"

	"github.com/wuekevin/lingedge/internal/tools/registry"
)

// 迁移级别（与 lingedge-server migrate plan 的 class 一致）。
const (
	ClassReversible   = "reversible"
	ClassExpand       = "expand"
	ClassIrreversible = "irreversible"
)

// Decision 是决策表（tools-design §17.2）对一次升级的结论。多类并存时取最严格者：irreversible > expand > reversible > 无迁移。
type Decision struct {
	// Classes 是升级所跨越的迁移级别（升序去重，固定顺序 reversible/expand/irreversible；无迁移为空）。
	Classes []string `json:"classes"`
	// Path 是失败/回滚路径（registry.PathBinary 等）。
	Path string `json:"path"`
	// BackupForced：升级前必须备份（server 恒为 true；无迁移时仍备份 DB，--no-backup 对 server 不适用）。
	BackupForced bool `json:"backup_forced"`
	// BackupVerified：备份写完后须再做一次完整可读校验（含不可逆迁移）。
	BackupVerified bool `json:"backup_verified"`
	// NeedConfirm：须用户确认（交互键入 confirm-irreversible；非交互 --accept-irreversible，否则退出码 9，此时尚未改动任何东西）。
	NeedConfirm bool `json:"need_confirm"`
	// Irreversible/Expand：便于输出。
	Irreversible bool `json:"irreversible"`
	HasExpand    bool `json:"has_expand"`
	// Promise 是对用户的承诺（原文，逐字展示）。
	Promise string `json:"promise"`
}

// 对用户的承诺（§17.2 决策表「对用户的承诺」列）。
const (
	PromiseBinary       = "二进制回退即恢复"
	PromiseReversible   = "二进制+库可一起回退"
	PromiseExpand       = "仅限升级失败的即时回滚（新二进制尚未写入新语义数据）可回退二进制；新版本已运行并写入新列/新枚举值之后的回退不在此承诺内，须依赖 N-1 兼容性测试（ADR-016 §10）并按“恢复备份”路径处理"
	PromiseIrreversible = "明示：备份点之后产生的数据将丢失，不承诺简单回退二进制即可恢复；回滚路径是「恢复升级前备份 + 旧版本二进制」"
)

// Decide 计算决策。classes 为空表示无迁移；未知级别按 irreversible 处理（失败关闭：不认识的级别不能被当成可逆）。isServer：server 的 DB 备份恒为强制。
func Decide(classes []string, isServer bool) Decision {
	seen := map[string]bool{}
	for _, c := range classes {
		switch c {
		case ClassReversible, ClassExpand, ClassIrreversible:
			seen[c] = true
		default:
			seen[ClassIrreversible] = true
		}
	}
	d := Decision{Classes: []string{}, BackupForced: isServer}
	for _, c := range []string{ClassReversible, ClassExpand, ClassIrreversible} {
		if seen[c] {
			d.Classes = append(d.Classes, c)
		}
	}
	d.Irreversible, d.HasExpand = seen[ClassIrreversible], seen[ClassExpand]
	switch {
	case d.Irreversible:
		d.Path, d.Promise = registry.PathRestoreBackup, PromiseIrreversible
		d.BackupForced, d.BackupVerified, d.NeedConfirm = true, true, true
	case d.HasExpand:
		d.Path, d.Promise = registry.PathBinaryExpand, PromiseExpand
		d.BackupForced = true
	case seen[ClassReversible]:
		d.Path, d.Promise = registry.PathMigrateDown, PromiseReversible
		d.BackupForced = true
	default:
		d.Path, d.Promise = registry.PathBinary, PromiseBinary
	}
	return d
}

// MergeClasses 合并多个来源（清单预告与 migrate plan 的实际结果）的级别集合：「与清单预告取更严格者」（§17.2），即并集。
func MergeClasses(sets ...[]string) []string {
	seen := map[string]bool{}
	for _, s := range sets {
		for _, c := range s {
			seen[c] = true
		}
	}
	var out []string
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// RefusesPlainRollback 报告上一次升级的记录是否要求拒绝「简单回退」（手动 `rollback` 不带 --from-backup）：含不可逆迁移。
// 含 expand 的升级也不承诺「升级之后」的二进制回退（PromiseExpand）：返回 needBackupHint=true 提示改用备份，但不拒绝（由用户承担 N-1 兼容性前提）。
func RefusesPlainRollback(u *registry.UpgradeRecord) (refuse bool, needBackupHint bool) {
	if u == nil {
		return false, false
	}
	if u.Irreversible || u.Path == registry.PathRestoreBackup {
		return true, true
	}
	return false, u.Path == registry.PathBinaryExpand
}
