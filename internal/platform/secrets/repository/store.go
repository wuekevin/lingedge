package repository

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/scope"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// 构造失败的原因。
var (
	ErrNeedPlatformScope = errors.New("secrets/repository: 重包装是跨租户批处理，须使用平台范围 Scope（PlatformForJob）")
	ErrWrongRole         = errors.New("secrets/repository: 只接受 app 连接池（四张目标表均已授予 lingedge_app 的 UPDATE；maint/migrate 账号不得用于此路径）")
	ErrUnknownTarget     = errors.New("secrets/repository: 记录的（表,列）不在受控白名单内")
	ErrBadRecord         = errors.New("secrets/repository: 记录非法（uid/密文/kek_id）")
)

// target 是一个受控的（表,列）目标。四条 SQL 都是编译期常量；hasKeyCol 表示该表有 enc_key_id 列。
type target struct {
	table, column string
	hasKeyCol     bool
	list          db.Query // 参数：kekID, prefixLen, prefix, afterUID, limit（有 enc_key_id 列的表前两项为 kekID）
	replace       db.Query
	count         db.Query
}

// targets 按（表,列）升序排列（RecordStore 约定的全序）；init 校验。
//
// 判定「仍以某 KEK 包装」：密文第 2 段即 kek_id，故以 BINARY LEFT(col, n) = 'v1:<kek>:' 为准（BINARY 避免 ai_ci 排序规则下大小写等价；
// 前缀作为参数绑定，kek_id 里的 `_` 不会被当作 LIKE 通配符）。有 enc_key_id 列的表同时核对该列（OR）：两者任一指向旧 KEK 都算引用，
// 保证「移除 KEK」这一不可逆操作只会因漂移而更保守，不会更激进。
// CAS 的相等比较同样用 BINARY：base64url 区分大小写，而库默认排序规则 utf8mb4_0900_ai_ci 不区分。
var targets = []target{
	{
		table: "certificates", column: "private_key_enc", hasKeyCol: true,
		list:    db.Q("SELECT uid, private_key_enc AS enc FROM certificates WHERE uid > ? AND (enc_key_id = ? OR BINARY LEFT(private_key_enc, ?) = ?) ORDER BY uid LIMIT ?"),
		replace: db.Q("UPDATE certificates SET private_key_enc = ?, enc_key_id = ? WHERE uid = ? AND BINARY private_key_enc = ?"),
		count:   db.Q("SELECT COUNT(*) FROM certificates WHERE enc_key_id = ? OR BINARY LEFT(private_key_enc, ?) = ?"),
	},
	{
		table: "signing_keys", column: "private_key_enc", hasKeyCol: true,
		list:    db.Q("SELECT uid, private_key_enc AS enc FROM signing_keys WHERE uid > ? AND (enc_key_id = ? OR BINARY LEFT(private_key_enc, ?) = ?) ORDER BY uid LIMIT ?"),
		replace: db.Q("UPDATE signing_keys SET private_key_enc = ?, enc_key_id = ? WHERE uid = ? AND BINARY private_key_enc = ?"),
		count:   db.Q("SELECT COUNT(*) FROM signing_keys WHERE enc_key_id = ? OR BINARY LEFT(private_key_enc, ?) = ?"),
	},
	{
		table: "system_settings", column: "value_enc",
		list:    db.Q("SELECT uid, value_enc AS enc FROM system_settings WHERE uid > ? AND BINARY LEFT(value_enc, ?) = ? ORDER BY uid LIMIT ?"),
		replace: db.Q("UPDATE system_settings SET value_enc = ? WHERE uid = ? AND BINARY value_enc = ?"),
		count:   db.Q("SELECT COUNT(*) FROM system_settings WHERE BINARY LEFT(value_enc, ?) = ?"),
	},
	{
		table: "users", column: "totp_secret_enc",
		list:    db.Q("SELECT uid, totp_secret_enc AS enc FROM users WHERE uid > ? AND BINARY LEFT(totp_secret_enc, ?) = ? ORDER BY uid LIMIT ?"),
		replace: db.Q("UPDATE users SET totp_secret_enc = ? WHERE uid = ? AND BINARY totp_secret_enc = ?"),
		count:   db.Q("SELECT COUNT(*) FROM users WHERE BINARY LEFT(totp_secret_enc, ?) = ?"),
	},
}

func init() {
	for i := 1; i < len(targets); i++ {
		a, b := targets[i-1], targets[i]
		if a.table > b.table || (a.table == b.table && a.column >= b.column) {
			panic("secrets/repository: targets 必须按（表,列）严格升序")
		}
	}
}

func find(table, column string) (target, bool) {
	for _, t := range targets {
		if t.table == table && t.column == column {
			return t, true
		}
	}
	return target{}, false
}

// Targets 返回白名单中的（表,列）清单（供文档与测试核对 migrations 实际列）。
func Targets(sc scope.Scope) [][2]string {
	if platformOnly(sc) != nil {
		return nil
	}
	out := make([][2]string, len(targets))
	for i, t := range targets {
		out[i] = [2]string{t.table, t.column}
	}
	return out
}

// Repo 是密文列的跨租户仓储。方法名含 AcrossTenants 并以平台范围 Scope 为首参（ADR-009 §2；repo-scope 门禁）；
// secretbox.RecordStore 接口没有 Scope 参数，由 internal/platform/secrets.NewRecordStore 绑定一个平台 Scope 后适配。
type Repo struct {
	pool db.Pool
}

// New 构造 Repo。pool 必须是 app 池（经 pool.Executor(ctx) 取执行器，事务内外一致）。
func New(pool db.Pool) (*Repo, error) {
	if pool == nil {
		return nil, errors.New("secrets/repository: pool 不能为空")
	}
	if pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	return &Repo{pool: pool}, nil
}

func platformOnly(sc scope.Scope) error {
	if !sc.IsPlatform() {
		return ErrNeedPlatformScope
	}
	return nil
}

type row struct {
	UID string `db:"uid"`
	Enc string `db:"enc"`
}

func prefix(kek string) (string, error) {
	if !secretbox.ValidKEKID(kek) {
		return "", fmt.Errorf("%w: kek_id", ErrBadRecord)
	}
	return "v1:" + kek + ":", nil
}

// ListByKEKAcrossTenants 对应 secretbox.RecordStore.ListByKEK：按（表,列,uid）升序、keyset 游标分批；after 为零值从头开始。
func (s *Repo) ListByKEKAcrossTenants(ctx context.Context, sc scope.Scope, kekID string, after secretbox.Record, limit int) ([]secretbox.Record, error) {
	if err := platformOnly(sc); err != nil {
		return nil, err
	}
	pfx, err := prefix(kekID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit", ErrBadRecord)
	}
	if after != (secretbox.Record{}) {
		if _, ok := find(after.Table, after.Column); !ok {
			return nil, fmt.Errorf("%w: %s.%s", ErrUnknownTarget, after.Table, after.Column)
		}
	}
	ex := s.pool.Executor(ctx)
	var out []secretbox.Record
	for _, t := range targets {
		if len(out) >= limit {
			break
		}
		// 游标：早于 after 所在目标的目标整体跳过；同一目标从 after.UID 之后继续；其后的目标从头。
		afterUID := ""
		if after != (secretbox.Record{}) {
			if t.table < after.Table || (t.table == after.Table && t.column < after.Column) {
				continue
			}
			if t.table == after.Table && t.column == after.Column {
				afterUID = after.UID
			}
		}
		var rows []row
		n := limit - len(out)
		if t.hasKeyCol {
			err = ex.Select(ctx, &rows, t.list, afterUID, kekID, utf8.RuneCountInString(pfx), pfx, n)
		} else {
			err = ex.Select(ctx, &rows, t.list, afterUID, utf8.RuneCountInString(pfx), pfx, n)
		}
		if err != nil {
			return nil, fmt.Errorf("secrets/repository: 扫描 %s.%s: %w", t.table, t.column, err)
		}
		for _, r := range rows {
			out = append(out, secretbox.Record{Table: t.table, Column: t.column, UID: r.UID, Enc: r.Enc})
		}
	}
	return out, nil
}

// ReplaceAcrossTenants 对应 secretbox.RecordStore.Replace：仅当库中密文仍与 old.Enc 逐字节相等时写入（CAS），有 enc_key_id 列的表同一条 UPDATE 里一并更新。
// 返回 false 表示记录已被并发改动或已删除（调用方记为 Skipped）。
func (s *Repo) ReplaceAcrossTenants(ctx context.Context, sc scope.Scope, old secretbox.Record, newEnc, newKEK string) (bool, error) {
	if err := platformOnly(sc); err != nil {
		return false, err
	}
	t, ok := find(old.Table, old.Column)
	if !ok {
		return false, fmt.Errorf("%w: %s.%s", ErrUnknownTarget, old.Table, old.Column)
	}
	if old.UID == "" || old.Enc == "" || newEnc == "" {
		return false, ErrBadRecord
	}
	// 写回前核对：新密文的 kek_id 必须等于 newKEK，避免 enc_key_id 与密文漂移。
	if id, err := secretbox.KeyID(newEnc); err != nil || id != newKEK {
		return false, fmt.Errorf("%w: 新密文的 kek_id 与目标不一致", ErrBadRecord)
	}
	ex := s.pool.Executor(ctx)
	var (
		res db.Result
		err error
	)
	if t.hasKeyCol {
		res, err = ex.Exec(ctx, t.replace, newEnc, newKEK, old.UID, old.Enc)
	} else {
		res, err = ex.Exec(ctx, t.replace, newEnc, old.UID, old.Enc)
	}
	if err != nil {
		return false, fmt.Errorf("secrets/repository: 写回 %s.%s: %w", t.table, t.column, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// CountByKEKAcrossTenants 对应 secretbox.RecordStore.CountByKEK：全部目标中仍以 kekID 包装 DEK 的记录数（含软删除行：行仍在库、备份里也在，不能忽略）。
func (s *Repo) CountByKEKAcrossTenants(ctx context.Context, sc scope.Scope, kekID string) (int64, error) {
	if err := platformOnly(sc); err != nil {
		return 0, err
	}
	pfx, err := prefix(kekID)
	if err != nil {
		return 0, err
	}
	ex := s.pool.Executor(ctx)
	var total int64
	for _, t := range targets {
		var n int64
		if t.hasKeyCol {
			err = ex.Get(ctx, &n, t.count, kekID, utf8.RuneCountInString(pfx), pfx)
		} else {
			err = ex.Get(ctx, &n, t.count, utf8.RuneCountInString(pfx), pfx)
		}
		if err != nil {
			return 0, fmt.Errorf("secrets/repository: 统计 %s.%s: %w", t.table, t.column, err)
		}
		total += n
	}
	return total, nil
}
