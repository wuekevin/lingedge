// Package repository 是系统设置与安装记录的数据访问（Story 1.22）。system_settings、system_installations 都是平台级表（无 tenant_id）：
// 方法的文档注释登记 lingedge:platform-table（repo-check 的 Scope 首参豁免）。lingedge_app 对两表均有 SELECT/INSERT/UPDATE/DELETE（迁移 000002）。
// 所有写方法必须在事务内调用（Service 用 WithTx 划定边界：设置变更与审计同一事务）。
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
)

// ErrWrongRole：只接受 app 连接池。
var ErrWrongRole = errors.New("sysconfig/repository: 只接受 app 连接池")

// Repo 是设置与安装记录的 Repository。
type Repo struct{ pool db.Pool }

// New 创建 Repository。
func New(pool db.Pool) (*Repo, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	return &Repo{pool: pool}, nil
}

func (r *Repo) ex(ctx context.Context) db.Executor { return r.pool.Executor(ctx) }

// Setting 是 system_settings 的一行。
type Setting struct {
	ID        uint64    `db:"id"`
	UID       string    `db:"uid"`
	Key       string    `db:"setting_key"`
	Group     string    `db:"group_name"`
	ValueJSON *string   `db:"value_json"`
	ValueEnc  *string   `db:"value_enc"`
	UpdatedAt time.Time `db:"updated_at"`
	// UpdatedByUID 是最后修改者的用户 uid（用户被删除后为 nil）。
	UpdatedByUID *string `db:"updated_by_uid"`
}

const settingCols = `s.id, s.uid, s.setting_key, s.group_name, CAST(s.value_json AS CHAR) AS value_json, s.value_enc, s.updated_at, u.uid AS updated_by_uid`
const settingFrom = ` FROM system_settings s LEFT JOIN users u ON u.id = s.updated_by`

// ListSettings 读取全部设置行。
// lingedge:platform-table=system_settings,users
func (r *Repo) ListSettings(ctx context.Context) ([]Setting, error) {
	var rows []Setting
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT `+settingCols+` `+settingFrom+` ORDER BY s.setting_key`))
	return rows, err
}

// GetSettingForUpdate 读取一行并加行锁（须在事务内）；不存在返回 (零值, false, nil)。
// lingedge:platform-table=system_settings,users
func (r *Repo) GetSettingForUpdate(ctx context.Context, key string) (Setting, bool, error) {
	var s Setting
	err := r.ex(ctx).Get(ctx, &s, db.Q(`SELECT `+settingCols+` `+settingFrom+` WHERE s.setting_key = ? FOR UPDATE OF s`), key)
	if errors.Is(err, db.ErrNotFound) {
		return Setting{}, false, nil
	}
	return s, err == nil, err
}

// InsertSetting 新增一行（valueJSON 与 valueEnc 恰有一个非 nil，CHECK 约束保证）。
// lingedge:platform-table=system_settings
func (r *Repo) InsertSetting(ctx context.Context, uid, key, group string, valueJSON, valueEnc *string, updatedBy *uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO system_settings (uid, setting_key, group_name, value_json, value_enc, updated_by) VALUES (?, ?, ?, ?, ?, ?)`),
		uid, key, group, valueJSON, valueEnc, updatedBy)
	return err
}

// UpdateSetting 按 id 改值（同样恰有一个非 nil；另一列显式置 NULL）。
// lingedge:platform-table=system_settings
func (r *Repo) UpdateSetting(ctx context.Context, id uint64, valueJSON, valueEnc *string, updatedBy *uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE system_settings SET value_json = ?, value_enc = ?, updated_by = ? WHERE id = ?`), valueJSON, valueEnc, updatedBy, id)
	return err
}

// DeleteSetting 删除一行（= 恢复默认）。
// lingedge:platform-table=system_settings
func (r *Repo) DeleteSetting(ctx context.Context, id uint64) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`DELETE FROM system_settings WHERE id = ?`), id)
	return err
}

// Installation 是 system_installations 的一行（对外字段，不含 meta_json/install_locked_at）。
type Installation struct {
	UID            string     `db:"uid"`
	InstallationID string     `db:"installation_id"`
	Component      string     `db:"component"`
	InstanceName   string     `db:"instance_name"`
	Host           string     `db:"host"`
	Version        string     `db:"version"`
	Channel        string     `db:"channel"`
	Status         string     `db:"status"`
	InstalledAt    time.Time  `db:"installed_at"`
	LastUpgradedAt *time.Time `db:"last_upgraded_at"`
}

// ListInstallations 分页列出安装记录（按组件、实例名稳定排序）并返回总数。
// lingedge:platform-table=system_installations
func (r *Repo) ListInstallations(ctx context.Context, limit, offset int) ([]Installation, int64, error) {
	var total int64
	if err := r.ex(ctx).Get(ctx, &total, db.Q(`SELECT COUNT(*) FROM system_installations`)); err != nil {
		return nil, 0, err
	}
	var rows []Installation
	err := r.ex(ctx).Select(ctx, &rows, db.Q(`SELECT uid, installation_id, component, instance_name, host, version, channel, status, installed_at, last_upgraded_at
		FROM system_installations ORDER BY component, instance_name, id LIMIT ? OFFSET ?`), limit, offset)
	return rows, total, err
}

// InstallationID 返回平台安装 ID（同一部署内相同；取最早的一行）；没有记录返回空串。
// lingedge:platform-table=system_installations
func (r *Repo) InstallationID(ctx context.Context) (string, error) {
	var id string
	err := r.ex(ctx).Get(ctx, &id, db.Q(`SELECT installation_id FROM system_installations ORDER BY id LIMIT 1`))
	if errors.Is(err, db.ErrNotFound) {
		return "", nil
	}
	return id, err
}

// ConsoleInstalled 报告是否存在状态为 installed 的 console 组件记录。
// lingedge:platform-table=system_installations
func (r *Repo) ConsoleInstalled(ctx context.Context) (bool, error) {
	var n int64
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM system_installations WHERE component = 'console' AND status = 'installed'`))
	return n > 0, err
}
