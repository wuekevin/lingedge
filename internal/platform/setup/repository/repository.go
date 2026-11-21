// Package repository 是安装向导状态的数据访问（Story 1.24）：system_installations 的一行即「安装状态」（tools-design §13：
// 向导任一步中断可从 system_installations 续做）。表 000002 没有单独的 state 列，向导进度存于 meta_json 的 "setup" 键（只含步骤名/布尔/版本号/
// 公开配置如 base_url，**绝不含任何口令、哈希或其长度**，ADR-026 §2）；完成的判据是 install_locked_at 非空。无需新迁移。
//
// 权限（核对迁移 000002）：lingedge_app 对 system_installations 有 SELECT/INSERT/UPDATE/DELETE。system_installations 是平台级表（无 tenant_id）：
// 方法的文档注释登记 lingedge:platform-table（repo-check 的 Scope 首参豁免）。写方法必须在事务内调用（Service 用 WithTx 划定边界：状态与审计同一事务）。
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
)

// ErrWrongRole：只接受 app 连接池。
var ErrWrongRole = errors.New("setup/repository: 只接受 app 连接池")

// Component/Instance/Host 是向导行的固定键（unique (component, host, instance_name)）：Server 自身，安装完成后 host 更新为真实主机名。
const (
	Component    = "server"
	InstanceName = "default"
	setupHost    = "setup"
)

// Repo 是 system_installations 的 Repository。
type Repo struct{ pool db.Pool }

// New 创建 Repository。
func New(pool db.Pool) (*Repo, error) {
	if pool == nil || pool.Role() != db.RoleApp {
		return nil, ErrWrongRole
	}
	return &Repo{pool: pool}, nil
}

func (r *Repo) ex(ctx context.Context) db.Executor { return r.pool.Executor(ctx) }

// Row 是向导行。
type Row struct {
	ID             uint64     `db:"id"`
	UID            string     `db:"uid"`
	InstallationID string     `db:"installation_id"`
	Status         string     `db:"status"`
	InstalledAt    time.Time  `db:"installed_at"`
	InstallLocked  *time.Time `db:"install_locked_at"`
	Meta           *string    `db:"meta_json"`
}

const cols = `id, uid, installation_id, status, installed_at, install_locked_at, meta_json`

// Completed 报告是否存在已完成安装的行（install_locked_at 非空）。
// lingedge:platform-table=system_installations
func (r *Repo) Completed(ctx context.Context) (bool, error) {
	var n int
	err := r.ex(ctx).Get(ctx, &n, db.Q(`SELECT COUNT(*) FROM system_installations WHERE install_locked_at IS NOT NULL`))
	return n > 0, err
}

// Get 读取向导行；不存在返回 (零值, false, nil)。forUpdate 为真时锁行（须在事务内）。
// lingedge:platform-table=system_installations
func (r *Repo) Get(ctx context.Context, forUpdate bool) (Row, bool, error) {
	var row Row
	var err error
	if forUpdate {
		err = r.ex(ctx).Get(ctx, &row, db.Q(`SELECT `+cols+` FROM system_installations WHERE component = ? AND instance_name = ? ORDER BY id LIMIT 1 FOR UPDATE`), Component, InstanceName)
	} else {
		err = r.ex(ctx).Get(ctx, &row, db.Q(`SELECT `+cols+` FROM system_installations WHERE component = ? AND instance_name = ? ORDER BY id LIMIT 1`), Component, InstanceName)
	}
	if errors.Is(err, db.ErrNotFound) {
		return Row{}, false, nil
	}
	return row, err == nil, err
}

// Insert 创建进行中的向导行（status=upgrading 表示「安装进行中」，meta_json 含进度；完成时由 Complete 置为 installed）。
// lingedge:platform-table=system_installations
func (r *Repo) Insert(ctx context.Context, uid, installationID, version string, now time.Time, meta string) error {
	_, err := r.ex(ctx).Exec(ctx, db.Q(`INSERT INTO system_installations (uid, installation_id, component, instance_name, host, version, status, installed_at, meta_json)
		VALUES (?, ?, ?, ?, ?, ?, 'upgrading', ?, ?)`), uid, installationID, Component, InstanceName, setupHost, version, now, meta)
	return err
}

// UpdateMeta 更新进度（仅未完成的行）；返回是否匹配到行。
// lingedge:platform-table=system_installations
func (r *Repo) UpdateMeta(ctx context.Context, id uint64, meta string) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE system_installations SET meta_json = ? WHERE id = ? AND install_locked_at IS NULL`), meta, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Complete 把向导行置为已安装并写入 install_locked_at（条件更新：已锁定则不匹配）；返回是否由本次调用完成。
// lingedge:platform-table=system_installations
func (r *Repo) Complete(ctx context.Context, id uint64, host, version string, now time.Time, meta string) (bool, error) {
	res, err := r.ex(ctx).Exec(ctx, db.Q(`UPDATE system_installations SET status = 'installed', host = ?, version = ?, installed_at = ?, install_locked_at = ?, meta_json = ?
		WHERE id = ? AND install_locked_at IS NULL`), host, version, now, now, meta, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
