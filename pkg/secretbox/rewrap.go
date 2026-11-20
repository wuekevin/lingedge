package secretbox

import (
	"context"
	"errors"
	"fmt"
)

// Record 是一条带信封密文的库记录（表/列/uid + 密文）。
type Record struct {
	Table, Column, UID string
	Enc                string
}

func (r Record) context() RecordContext {
	return RecordContext{Table: r.Table, Column: r.Column, UID: r.UID}
}

func (r Record) less(o Record) bool {
	if r.Table != o.Table {
		return r.Table < o.Table
	}
	if r.Column != o.Column {
		return r.Column < o.Column
	}
	return r.UID < o.UID
}

// RecordStore 是 secrets.rewrap 所需的存储抽象：由持有各 *_enc 列的仓储层实现（后续 Story 接线 worker 任务）。
type RecordStore interface {
	// ListByKEK 返回 enc_key_id=kekID 且排序键（表,列,uid）大于 after 的至多 limit 条记录，按（表,列,uid）升序；after 为零值表示从头。
	ListByKEK(ctx context.Context, kekID string, after Record, limit int) ([]Record, error)
	// Replace 仅当库中该记录的密文仍等于 old.Enc 时，把密文写成 newEnc 并把 enc_key_id 置为 newKEK；返回是否发生写入（并发改动则 false）。
	Replace(ctx context.Context, old Record, newEnc, newKEK string) (bool, error)
	// CountByKEK 返回仍以 kekID 包装 DEK 的记录数（enc_key_id=kekID），KEK 移除前核对用。
	CountByKEK(ctx context.Context, kekID string) (int64, error)
}

// RewrapReport 是一次 RewrapAll 的结果。Failures 只含「表|列|uid: 原因」，不含密文或密钥。
type RewrapReport struct {
	Scanned   int
	Rewrapped int
	Skipped   int // 并发修改导致未写入（下一轮会再被扫描到）
	Failures  []string
}

// RewrapAll 把所有以 fromKEK 包装的记录重包装到 toKEK（仅 DEK，ADR-011 §3）。可重入、可中断后续跑：
// 已重包装的记录不会再被列出；单条失败不终止整体，记入 Failures。batch<=0 取 100。
func (b *Box) RewrapAll(ctx context.Context, store RecordStore, fromKEK, toKEK string, batch int) (RewrapReport, error) {
	var rep RewrapReport
	if fromKEK == toKEK || !ValidKEKID(fromKEK) || !ValidKEKID(toKEK) {
		return rep, errors.New("secretbox: 重包装的源/目标 kek_id 非法或相同")
	}
	if batch <= 0 {
		batch = 100
	}
	var after Record
	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		recs, err := store.ListByKEK(ctx, fromKEK, after, batch)
		if err != nil {
			return rep, fmt.Errorf("secretbox: 列出待重包装记录失败: %w", err)
		}
		if len(recs) == 0 {
			return rep, nil
		}
		for _, r := range recs {
			rep.Scanned++
			if !after.less(r) && after != (Record{}) {
				return rep, errors.New("secretbox: RecordStore 未按（表,列,uid）升序返回，已中止以免死循环")
			}
			after = r
			newEnc, changed, err := b.Rewrap(ctx, r.Enc, r.context(), toKEK)
			if err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s|%s|%s: %v", r.Table, r.Column, r.UID, err))
				continue
			}
			if !changed {
				continue
			}
			ok, err := store.Replace(ctx, r, newEnc, toKEK)
			if err != nil {
				return rep, fmt.Errorf("secretbox: 写回 %s|%s|%s 失败: %w", r.Table, r.Column, r.UID, err)
			}
			if ok {
				rep.Rewrapped++
			} else {
				rep.Skipped++
			}
		}
	}
}

// 移除 KEK 被拒的原因。
var (
	ErrKEKInUse            = errors.New("secretbox: 仍有记录以该 KEK 包装 DEK，须先完成 secrets.rewrap，旧 KEK 不可移除")
	ErrKEKIsCurrent        = errors.New("secretbox: 不能移除当前 KEK")
	ErrKEKRequiredByBackup = errors.New("secretbox: 该 KEK 仍被保留备份需要（kek_ids_required）；确需移除请显式 --drop-backup-compat（此后这些备份不带旧主密钥无法解密）")
)

// RemovalCheck 是移除 KEK 前的核对输入。
type RemovalCheck struct {
	KEKID            string   // 拟移除
	CurrentKEKID     string   // 当前 KEK
	BackupRequired   []string // 保留备份清单中 kek_ids_required 的并集
	DropBackupCompat bool     // --drop-backup-compat
}

// CheckKEKRemovable 返回 nil 才允许从主密钥文件移除 kekID：非当前、库内零引用（CountByKEK==0）、
// 不被保留备份需要（除非显式放弃备份兼容，ADR-011 §3、MIN-05）。
func CheckKEKRemovable(ctx context.Context, store RecordStore, c RemovalCheck) error {
	if !ValidKEKID(c.KEKID) {
		return ErrInvalidFormat
	}
	if c.KEKID == c.CurrentKEKID {
		return ErrKEKIsCurrent
	}
	n, err := store.CountByKEK(ctx, c.KEKID)
	if err != nil {
		return fmt.Errorf("secretbox: 统计 KEK 引用失败: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w（剩余 %d 条）", ErrKEKInUse, n)
	}
	if !c.DropBackupCompat {
		for _, id := range c.BackupRequired {
			if id == c.KEKID {
				return ErrKEKRequiredByBackup
			}
		}
	}
	return nil
}
