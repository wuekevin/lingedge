// Package repository 是 signing_keys 的数据访问，以及初始化时写入/读取 system_settings 两个键（deploy.lineage、grpc.tls.san_extra）所需的最小语句（Story 1.23）。
// signing_keys、system_settings 都是平台级表（无 tenant_id）：方法的文档注释登记 lingedge:platform-table（repo-check 的 Scope 首参豁免）。
//
// 权限（核对迁移 000005、000002、000003）：lingedge_app 对 signing_keys、system_settings 均有 SELECT/INSERT/UPDATE/DELETE，对 audit_logs 有 INSERT；
// 初始化只需 app 池，不需要也不接受 migrate/maint 账号（AA-7：安装向导的迁移凭据交接未决，本包不依赖它）。
//
// 「每个 purpose 至多一个 active」由数据库保证：迁移 000005 的生成列 active_key = IF(status='active', purpose, NULL) 上的唯一索引 uk_signing_keys_active
// （NULL 不参与唯一性，故 pending/retiring/revoked 可以有多行），无需新迁移。并发初始化时后到者在 INSERT 处得到重复键错误（db.IsDuplicate）。
// 所有写方法必须在事务内调用（Service 用 WithTx 划定边界：密钥入库、lineage 与审计同一事务）。
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
)

// ErrWrongRole：只接受 app 连接池。
var ErrWrongRole = errors.New("signingkeys/repository: 只接受 app 连接池")

// Repo 是 signing_keys 的 Repository。
type Repo struct{ pool db.Pool }

// New 创建 Repository。
func New(pool db.Pool) (*Repo, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	return &Repo{pool: pool}, nil
}

func (r *Repo) ex(ctx context.Context) db.Executor { return r.pool.Executor(ctx) }

// Key 是 signing_keys 的一行（含密文列；密文只在 Service 内用于解密，不对外暴露）。
type Key struct {
	ID            uint64     `db:"id"`
	UID           string     `db:"uid"`
	CreatedAt     time.Time  `db:"created_at"`
	Purpose       string     `db:"purpose"`
	KeyID         string     `db:"key_id"`
	Algorithm     string     `db:"algorithm"`
	PublicKey     []byte     `db:"public_key"`
	CertPEM       *string    `db:"cert_pem"`
	PrivateKeyEnc string     `db:"private_key_enc"`
	EncKeyID      string     `db:"enc_key_id"`
	Status        string     `db:"status"`
	ActivatedAt   *time.Time `db:"activated_at"`
	RetireAfter   *time.Time `db:"retire_after"`
	RevokedAt     *time.Time `db:"revoked_at"`
}

const keyCols = `id, uid, created_at, purpose, key_id, algorithm, public_key, cert_pem, private_key_enc, enc_key_id, status, activated_at, retire_after, revoked_at`

// GetActive 读取某个 purpose 当前的 active 行；不存在返回 (零值, false, nil)。普通（非锁定）读取。
// lingedge:platform-table=signing_keys
func (r *Repo) GetActive(ctx context.Context, purpose string) (Key, bool, error) {
	var k Key
	err := r.ex(ctx).Get(ctx, &k, db.Q(`SELECT `+keyCols+` FROM signing_keys WHERE purpose = ? AND status = 'active'`), purpose)
	if errors.Is(err, db.ErrNotFound) {
		return Key{}, false, nil
	}
	return k, err == nil, err
}

// Insert 新增一行。active 的唯一性由 uk_signing_keys_active 保证：重复时返回 db.IsDuplicate 为真的错误。
// lingedge:platform-table=signing_keys
func (r *Repo) Insert(ctx context.Context, k Key) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO signing_keys (uid, purpose, key_id, algorithm, public_key, cert_pem, private_key_enc, enc_key_id, status, activated_at) `+
		`VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		k.UID, k.Purpose, k.KeyID, k.Algorithm, k.PublicKey, k.CertPEM, k.PrivateKeyEnc, k.EncKeyID, k.Status, k.ActivatedAt)
	return err
}

// List 分页读取全部行（按创建顺序），返回行与总数。含密文列只为复用 Key 结构；Service 对外只暴露公开字段。
// lingedge:platform-table=signing_keys
func (r *Repo) List(ctx context.Context, limit, offset int) ([]Key, int64, error) {
	var total int64
	if err := r.ex(ctx).Get(ctx, &total, db.Q(`SELECT COUNT(*) FROM signing_keys`)); err != nil {
		return nil, 0, err
	}
	var rows []Key
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT `+keyCols+` FROM signing_keys ORDER BY id LIMIT ? OFFSET ?`), limit, offset)
	return rows, total, err
}

// EnsureSetting 在设置项不存在时写入 value（JSON 文本），已存在则不改；返回是否本次写入。先做普通读取再 INSERT：并发调用时后到者在 INSERT 处得到重复键错误
// （db.IsDuplicate），由 Service 整体重试，重试时读到已提交的行——因此恰有一个调用返回 true。
// （不用 ON DUPLICATE KEY UPDATE / INSERT IGNORE：连接开启 clientFoundRows 时前者无法区分「已存在」，后者会把其他错误降级为警告。）
// lingedge:platform-table=system_settings
func (r *Repo) EnsureSetting(ctx context.Context, uid, key, group, valueJSON string) (bool, error) {
	cur, err := r.GetSettingJSON(ctx, key)
	if err != nil || cur != nil {
		return false, err
	}
	if _, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO system_settings (uid, setting_key, group_name, value_json) VALUES (?, ?, ?, CAST(? AS JSON))`), uid, key, group, valueJSON); err != nil {
		return false, err
	}
	return true, nil
}

// GetSettingJSON 读取设置项的 JSON 文本；不存在返回 (nil, nil)。
// lingedge:platform-table=system_settings
func (r *Repo) GetSettingJSON(ctx context.Context, key string) (*string, error) {
	var v *string
	err := r.ex(ctx).Get(ctx, &v, db.Q(`SELECT CAST(value_json AS CHAR) FROM system_settings WHERE setting_key = ?`), key)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	return v, err
}
